// Package authproxy implements an authenticating proxy in front of the K8s
// API server converting Metropolis credentials into authentication headers.
package authproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/kubernetes/pki"
	"source.monogon.dev/metropolis/pkg/supervisor"
)

type Service struct {
	// KPKI is a reference to the Kubernetes PKI
	KPKI *pki.PKI
	// Node contains the node identity
	Node *identity.Node
}

func (s *Service) getTLSCert(ctx context.Context, name pki.KubeCertificateName) (*tls.Certificate, error) {
	cert, key, err := s.KPKI.Certificate(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("could not load certificate %q from PKI: %w", name, err)
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key for cert %q: %w", name, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{cert},
		PrivateKey:  parsedKey,
	}, nil
}

func respondWithK8sStatus(w http.ResponseWriter, status *metav1.Status) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(int(status.Code))
	return json.NewEncoder(w).Encode(status)
}

func (s *Service) Run(ctx context.Context) error {
	logger := supervisor.Logger(ctx)

	k8sCAs := x509.NewCertPool()
	cert, _, err := s.KPKI.Certificate(ctx, pki.IdCA)
	parsedCert, err := x509.ParseCertificate(cert)
	if err != nil {
		return fmt.Errorf("failed to parse K8s CA certificate: %w", err)
	}
	k8sCAs.AddCert(parsedCert)

	clientCert, err := s.getTLSCert(ctx, pki.MetropolisAuthProxyClient)
	if err != nil {
		return err
	}

	proxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Host:   net.JoinHostPort("localhost", node.KubernetesAPIPort.PortString()),
		Scheme: "https",
	})
	proxy.Transport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			RootCAs:      k8sCAs,
			Certificates: []tls.Certificate{*clientCert},
			NextProtos:   []string{"h2", "http/1.1"},
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		logger.Infof("Proxy error: %v", err)
		respondWithK8sStatus(w, &metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusBadGateway,
			Reason:  metav1.StatusReasonServiceUnavailable,
			Message: "authproxy could not reach apiserver",
		})
	}

	serverCert, err := s.getTLSCert(ctx, pki.APIServer)
	if err != nil {
		return err
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(s.Node.ClusterCA())
	server := &http.Server{
		Addr: ":" + node.KubernetesAPIWrappedPort.PortString(),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"h2", "http/1.1"},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    clientCAs,
			Certificates: []tls.Certificate{*serverCert},
		},
		// Limits match @io_k8s_apiserver/pkg/server:secure_serving.go Serve()
		MaxHeaderBytes:    1 << 20,
		IdleTimeout:       90 * time.Second,
		ReadHeaderTimeout: 32 * time.Second,

		Handler: http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			// Guaranteed to exist because of RequireAndVerifyClientCert
			clientCert := req.TLS.VerifiedChains[0][0]
			clientIdentity, err := identity.VerifyUserInCluster(clientCert, s.Node.ClusterCA())
			if err != nil {
				respondWithK8sStatus(rw, &metav1.Status{
					Status:  metav1.StatusFailure,
					Code:    http.StatusUnauthorized,
					Reason:  metav1.StatusReasonUnauthorized,
					Message: fmt.Sprintf("Metropolis authentication failed: %v", err),
				})
				return
			}
			// Clone the request as otherwise modifying it is not allowed
			newReq := req.Clone(req.Context())
			// Drop any X-Remote headers to prevent injection
			for k := range newReq.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), http.CanonicalHeaderKey("X-Remote-")) {
					newReq.Header.Del(k)
				}
			}
			newReq.Header.Set("X-Remote-User", clientIdentity)
			newReq.Header.Set("X-Remote-Group", "")

			proxy.ServeHTTP(rw, newReq)
		}),
	}
	go server.ListenAndServeTLS("", "")
	logger.Info("K8s AuthProxy running")
	<-ctx.Done()
	return server.Close()
}