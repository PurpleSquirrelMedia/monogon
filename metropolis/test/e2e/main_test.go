// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	podv1 "k8s.io/kubernetes/pkg/api/v1/pod"

	common "git.monogon.dev/source/nexantic.git/metropolis/node"
	apb "git.monogon.dev/source/nexantic.git/metropolis/proto/api"
	"git.monogon.dev/source/nexantic.git/metropolis/test/launch"
)

const (
	// Timeout for the global test context.
	//
	// Bazel would eventually time out the test after 900s ("large") if, for some reason,
	// the context cancellation fails to abort it.
	globalTestTimeout = 600 * time.Second

	// Timeouts for individual end-to-end tests of different sizes.
	smallTestTimeout = 60 * time.Second
	largeTestTimeout = 120 * time.Second
)

// TestE2E is the main E2E test entrypoint for single-node freshly-bootstrapped E2E tests. It starts a full Metropolis node
// in bootstrap mode and then runs tests against it. The actual tests it performs are located in the RunGroup subtest.
func TestE2E(t *testing.T) {
	// Run pprof server for debugging
	go func() {
		addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
		if err != nil {
			panic(err)
		}

		l, err := net.ListenTCP("tcp", addr)
		if err != nil {
			log.Fatalf("Failed to listen on pprof port: %s", l.Addr())
		}
		defer l.Close()

		log.Printf("pprof server listening on %s", l.Addr())
		log.Printf("pprof server returned an error: %v", http.Serve(l, nil))
	}()

	// Set a global timeout to make sure this terminates
	ctx, cancel := context.WithTimeout(context.Background(), globalTestTimeout)
	portMap, err := launch.ConflictFreePortMap(launch.NodePorts)
	if err != nil {
		t.Fatalf("Failed to acquire ports for e2e test: %v", err)
	}

	procExit := make(chan struct{})

	go func() {
		if err := launch.Launch(ctx, launch.Options{Ports: portMap, SerialPort: os.Stdout}); err != nil {
			panic(err)
		}
		close(procExit)
	}()
	grpcClient, err := portMap.DialGRPC(common.DebugServicePort, grpc.WithInsecure())
	if err != nil {
		fmt.Printf("Failed to dial debug service (is it running): %v\n", err)
	}
	debugClient := apb.NewNodeDebugServiceClient(grpcClient)

	// This exists to keep the parent around while all the children race
	// It currently tests both a set of OS-level conditions and Kubernetes Deployments and StatefulSets
	t.Run("RunGroup", func(t *testing.T) {
		t.Run("Get Kubernetes Debug Kubeconfig", func(t *testing.T) {
			t.Parallel()
			selfCtx, cancel := context.WithTimeout(ctx, largeTestTimeout)
			defer cancel()
			clientSet, err := GetKubeClientSet(selfCtx, debugClient, portMap[common.KubernetesAPIPort])
			if err != nil {
				t.Fatal(err)
			}
			testEventual(t, "Node is registered and ready", ctx, largeTestTimeout, func(ctx context.Context) error {
				nodes, err := clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
				if err != nil {
					return err
				}
				if len(nodes.Items) < 1 {
					return errors.New("node not registered")
				}
				if len(nodes.Items) > 1 {
					return errors.New("more than one node registered (but there is only one)")
				}
				node := nodes.Items[0]
				for _, cond := range node.Status.Conditions {
					if cond.Type != corev1.NodeReady {
						continue
					}
					if cond.Status != corev1.ConditionTrue {
						return fmt.Errorf("node not ready: %v", cond.Message)
					}
				}
				return nil
			})
			testEventual(t, "Simple deployment", ctx, largeTestTimeout, func(ctx context.Context) error {
				_, err := clientSet.AppsV1().Deployments("default").Create(ctx, makeTestDeploymentSpec("test-deploy-1"), metav1.CreateOptions{})
				return err
			})
			testEventual(t, "Simple deployment is running", ctx, largeTestTimeout, func(ctx context.Context) error {
				res, err := clientSet.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: "name=test-deploy-1"})
				if err != nil {
					return err
				}
				if len(res.Items) == 0 {
					return errors.New("pod didn't get created")
				}
				pod := res.Items[0]
				if podv1.IsPodAvailable(&pod, 1, metav1.NewTime(time.Now())) {
					return nil
				}
				events, err := clientSet.CoreV1().Events("default").List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=default", pod.Name)})
				if err != nil || len(events.Items) == 0 {
					return fmt.Errorf("pod is not ready: %v", pod.Status.Phase)
				} else {
					return fmt.Errorf("pod is not ready: %v", events.Items[0].Message)
				}
			})
			testEventual(t, "Simple deployment with runc", ctx, largeTestTimeout, func(ctx context.Context) error {
				deployment := makeTestDeploymentSpec("test-deploy-2")
				var runcStr = "runc"
				deployment.Spec.Template.Spec.RuntimeClassName = &runcStr
				_, err := clientSet.AppsV1().Deployments("default").Create(ctx, deployment, metav1.CreateOptions{})
				return err
			})
			testEventual(t, "Simple deployment is running on runc", ctx, largeTestTimeout, func(ctx context.Context) error {
				res, err := clientSet.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: "name=test-deploy-2"})
				if err != nil {
					return err
				}
				if len(res.Items) == 0 {
					return errors.New("pod didn't get created")
				}
				pod := res.Items[0]
				if podv1.IsPodAvailable(&pod, 1, metav1.NewTime(time.Now())) {
					return nil
				}
				events, err := clientSet.CoreV1().Events("default").List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=default", pod.Name)})
				if err != nil || len(events.Items) == 0 {
					return fmt.Errorf("pod is not ready: %v", pod.Status.Phase)
				} else {
					var errorMsg strings.Builder
					for _, msg := range events.Items {
						errorMsg.WriteString(" | ")
						errorMsg.WriteString(msg.Message)
					}
					return fmt.Errorf("pod is not ready: %v", errorMsg.String())
				}
			})
			testEventual(t, "Simple StatefulSet with PVC", ctx, largeTestTimeout, func(ctx context.Context) error {
				_, err := clientSet.AppsV1().StatefulSets("default").Create(ctx, makeTestStatefulSet("test-statefulset-1"), metav1.CreateOptions{})
				return err
			})
			testEventual(t, "Simple StatefulSet with PVC is running", ctx, largeTestTimeout, func(ctx context.Context) error {
				res, err := clientSet.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: "name=test-statefulset-1"})
				if err != nil {
					return err
				}
				if len(res.Items) == 0 {
					return errors.New("pod didn't get created")
				}
				pod := res.Items[0]
				if podv1.IsPodAvailable(&pod, 1, metav1.NewTime(time.Now())) {
					return nil
				}
				events, err := clientSet.CoreV1().Events("default").List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=default", pod.Name)})
				if err != nil || len(events.Items) == 0 {
					return fmt.Errorf("pod is not ready: %v", pod.Status.Phase)
				} else {
					return fmt.Errorf("pod is not ready: %v", events.Items[0].Message)
				}
			})
			testEventual(t, "Pod with preseeded image", ctx, smallTestTimeout, func(ctx context.Context) error {
				_, err := clientSet.CoreV1().Pods("default").Create(ctx, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "preseed-test-1",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:            "preseed-test-1",
							ImagePullPolicy: corev1.PullNever,
							Image:           "bazel/metropolis/test/e2e/preseedtest:preseedtest",
						}},
						RestartPolicy: corev1.RestartPolicyNever,
					},
				}, metav1.CreateOptions{})
				return err
			})
			testEventual(t, "Pod with preseeded image is completed", ctx, largeTestTimeout, func(ctx context.Context) error {
				pod, err := clientSet.CoreV1().Pods("default").Get(ctx, "preseed-test-1", metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("failed to get pod: %w", err)
				}
				if pod.Status.Phase == corev1.PodSucceeded {
					return nil
				}
				events, err := clientSet.CoreV1().Events("default").List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=default", pod.Name)})
				if err != nil || len(events.Items) == 0 {
					return fmt.Errorf("pod is not ready: %v", pod.Status.Phase)
				} else {
					return fmt.Errorf("pod is not ready: %v", events.Items[len(events.Items)-1].Message)
				}
			})
		})
	})

	// Cancel the main context and wait for our subprocesses to exit
	// to avoid leaking them and blocking the parent.
	cancel()
	<-procExit
}
