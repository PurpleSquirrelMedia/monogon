package curator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"testing"

	"go.etcd.io/etcd/tests/v3/integration"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"source.monogon.dev/metropolis/node/core/consensus"
	"source.monogon.dev/metropolis/node/core/consensus/client"
	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	ppb "source.monogon.dev/metropolis/node/core/curator/proto/private"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/core/rpc"
	"source.monogon.dev/metropolis/pkg/pki"
	apb "source.monogon.dev/metropolis/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

// fakeLeader creates a curatorLeader without any underlying leader election, in
// its own etcd namespace. It starts the gRPC listener and returns clients to
// it, from the point of view of the local node and some other external node.
//
// The gRPC listeners are replicated to behave as when running the Curator
// within Metropolis, so all calls performed will be authenticated and encrypted
// the same way.
//
// This is used to test functionality of the individual curatorLeader RPC
// implementations without the overhead of having to wait for a leader election.
func fakeLeader(t *testing.T) fakeLeaderData {
	t.Helper()
	// Set up context whose cancel function will be returned to the user for
	// terminating all harnesses started by this function.
	ctx, ctxC := context.WithCancel(context.Background())

	// Start a single-node etcd cluster.
	integration.BeforeTest(t)
	cluster := integration.NewClusterV3(t, &integration.ClusterConfig{
		Size: 1,
	})
	// Terminate the etcd cluster on context cancel.
	go func() {
		<-ctx.Done()
		cluster.Terminate(t)
	}()

	// Create etcd client to test cluster.
	curEtcd, _ := client.NewLocal(cluster.Client(0)).Sub("curator")

	// Create a fake lock key/value and retrieve its revision. This replaces the
	// leader election functionality in the curator to enable faster and more
	// focused tests.
	lockKey := "/test-lock"
	res, err := curEtcd.Put(ctx, lockKey, "fake key")
	if err != nil {
		t.Fatalf("setting fake leader key failed: %v", err)
	}
	lockRev := res.Header.Revision

	// Build cluster PKI with first node, replicating the cluster bootstrap process.
	nodePub, nodePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate node keypair: %v", err)
	}
	cNode := NewNodeForBootstrap(nil, nodePub)
	caCertBytes, nodeCertBytes, err := BootstrapNodeFinish(ctx, curEtcd, &cNode, nil)
	if err != nil {
		t.Fatalf("could not finish node bootstrap: %v", err)
	}
	nodeCredentials, err := identity.NewNodeCredentials(nodePriv, nodeCertBytes, caCertBytes)
	if err != nil {
		t.Fatalf("could not build node credentials: %v", err)
	}

	// Generate credentials for cluster manager. This doesn't go through the owner
	// credentials escrow, instead manually generating a certificate that would be
	// generated by the escrow call.
	ownerPub, ownerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate owner keypair: %v", err)
	}
	oc := pki.Certificate{
		Namespace: &pkiNamespace,
		Issuer:    pkiCA,
		Template:  identity.UserCertificate("owner"),
		Name:      "owner",
		Mode:      pki.CertificateExternal,
		PublicKey: ownerPub,
	}
	ownerCertBytes, err := oc.Ensure(ctx, curEtcd)
	if err != nil {
		t.Fatalf("could not issue owner certificate: %v", err)
	}
	ownerCreds := tls.Certificate{
		PrivateKey:  ownerPriv,
		Certificate: [][]byte{ownerCertBytes},
	}

	// Generate keypair for some third-party, unknown node.
	otherPub, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate unknown node keypair: %v", err)
	}

	// Create security interceptors for gRPC listener.
	sec := &rpc.ServerSecurity{
		NodeCredentials: nodeCredentials,
	}

	// Build a curator leader object. This implements methods that will be
	// exercised by tests.
	leadership := &leadership{
		lockKey:   lockKey,
		lockRev:   lockRev,
		etcd:      curEtcd,
		consensus: consensus.TestServiceHandle(t, cluster.Client(0)),
	}
	leader := newCuratorLeader(leadership, &nodeCredentials.Node)

	// Create a curator gRPC server which performs authentication as per the created
	// ServerSecurity and is backed by the created leader.
	externalSrv := sec.SetupExternalGRPC(nil, leader)
	// The gRPC server will listen on an internal 'loopback' buffer.
	externalLis := bufconn.Listen(1024 * 1024)
	go func() {
		if err := externalSrv.Serve(externalLis); err != nil {
			t.Fatalf("GRPC serve failed: %v", err)
		}
	}()

	// Stop the gRPC server on context cancel.
	go func() {
		<-ctx.Done()
		externalSrv.Stop()
	}()

	withLocalDialer := grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
		return externalLis.Dial()
	})
	ca := nodeCredentials.ClusterCA()

	// Create an authenticated manager gRPC client.
	mcl, err := grpc.Dial("local", withLocalDialer, grpc.WithTransportCredentials(rpc.NewAuthenticatedCredentials(ownerCreds, ca)))
	if err != nil {
		t.Fatalf("Dialing external GRPC failed: %v", err)
	}

	// Create a node gRPC client for the local node.
	lcl, err := grpc.Dial("local", withLocalDialer,
		grpc.WithTransportCredentials(rpc.NewAuthenticatedCredentials(nodeCredentials.TLSCredentials(), ca)))
	if err != nil {
		t.Fatalf("Dialing external GRPC failed: %v", err)
	}

	// Create an ephemeral node gRPC client for the 'other node'.
	otherEphCreds, err := rpc.NewEphemeralCredentials(otherPriv, ca)
	if err != nil {
		t.Fatalf("NewEphemeralCredentials: %v", err)
	}
	ocl, err := grpc.Dial("local", withLocalDialer, grpc.WithTransportCredentials(otherEphCreds))
	if err != nil {
		t.Fatalf("Dialing external GRPC failed: %v", err)
	}

	// Close the clients on context cancel.
	go func() {
		<-ctx.Done()
		mcl.Close()
		lcl.Close()
	}()

	return fakeLeaderData{
		l:             leadership,
		curatorLis:    externalLis,
		mgmtConn:      mcl,
		localNodeConn: lcl,
		localNodeID:   nodeCredentials.ID(),
		otherNodeConn: ocl,
		otherNodeID:   identity.NodeID(otherPub),
		otherNodePriv: otherPriv,
		ca:            nodeCredentials.ClusterCA(),
		cancel:        ctxC,
		etcd:          curEtcd,
	}
}

// fakeLeaderData is returned by fakeLeader and contains information about the
// newly created leader and connections to its gRPC listeners.
type fakeLeaderData struct {
	// l is a type internal to Curator, representing its ability to perform
	// actions as a leader.
	l *leadership
	// curatorLis is a listener intended for Node connections.
	curatorLis *bufconn.Listener
	// mgmtConn is a gRPC connection to the leader's public gRPC interface,
	// authenticated as a cluster manager.
	mgmtConn grpc.ClientConnInterface
	// localNodeConn is a gRPC connection to the leader's internal/local node gRPC
	// interface, which usually runs on a domain socket and is only available to
	// other Metropolis node code.
	localNodeConn grpc.ClientConnInterface
	// localNodeID is the NodeID of the fake node that the leader is running on.
	localNodeID string
	// otherNodeConn is an connection from some other node (otherNodeID) into the
	// cluster, authenticated using an ephemeral certificate.
	otherNodeConn grpc.ClientConnInterface
	// otherNodeID is the NodeID of some other node present in the curator
	// state.
	otherNodeID string
	// otherNodePriv is the private key of the other node present in the curator
	// state, ie. the private key for otherNodeID.
	otherNodePriv ed25519.PrivateKey
	ca            *x509.Certificate
	// cancel shuts down the fake leader and all client connections.
	cancel context.CancelFunc
	// etcd contains a low-level connection to the curator K/V store, which can be
	// used to perform low-level changes to the store in tests.
	etcd client.Namespaced
}

// TestWatchNodeInCluster exercises a NodeInCluster Watch, from node creation,
// through updates, to its deletion.
func TestWatchNodeInCluster(t *testing.T) {
	cl := fakeLeader(t)
	defer cl.cancel()
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	cur := ipb.NewCuratorClient(cl.localNodeConn)

	// We'll be using a fake node throughout, manually updating it in the etcd
	// cluster.
	fakeNodePub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	fakeNodeID := identity.NodeID(fakeNodePub)
	fakeNodeKey, _ := nodeEtcdPrefix.Key(fakeNodeID)

	w, err := cur.Watch(ctx, &ipb.WatchRequest{
		Kind: &ipb.WatchRequest_NodeInCluster_{
			NodeInCluster: &ipb.WatchRequest_NodeInCluster{
				NodeId: fakeNodeID,
			},
		},
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Recv() should block here, as we don't yet have a node in the cluster. We
	// can't really test that reliably, unfortunately.

	// Populate new node.
	fakeNode := &ppb.Node{
		PublicKey: fakeNodePub,
		Roles:     &cpb.NodeRoles{},
	}
	fakeNodeInit, err := proto.Marshal(fakeNode)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	_, err = cl.etcd.Put(ctx, fakeNodeKey, string(fakeNodeInit))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Receive cluster node status. This should go through immediately.
	ev, err := w.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if want, got := 1, len(ev.Nodes); want != got {
		t.Errorf("wanted %d nodes, got %d", want, got)
	} else {
		n := ev.Nodes[0]
		if want, got := fakeNodeID, n.Id; want != got {
			t.Errorf("wanted node %q, got %q", want, got)
		}
		if n.Status != nil {
			t.Errorf("wanted nil status, got %v", n.Status)
		}
	}

	// Update node status. This should trigger an update from the watcher.
	fakeNode.Status = &cpb.NodeStatus{
		ExternalAddress: "203.0.113.42",
	}
	fakeNodeInit, err = proto.Marshal(fakeNode)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	_, err = cl.etcd.Put(ctx, fakeNodeKey, string(fakeNodeInit))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Receive new node. This should go through immediately.
	ev, err = w.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if want, got := 1, len(ev.Nodes); want != got {
		t.Errorf("wanted %d nodes, got %d", want, got)
	} else {
		n := ev.Nodes[0]
		if want, got := fakeNodeID, n.Id; want != got {
			t.Errorf("wanted node %q, got %q", want, got)
		}
		if want := "203.0.113.42"; n.Status == nil || n.Status.ExternalAddress != want {
			t.Errorf("wanted status with ip address %q, got %v", want, n.Status)
		}
	}

	// Remove node. This should trigger an update from the watcher.
	k, _ := nodeEtcdPrefix.Key(fakeNodeID)
	if _, err := cl.etcd.Delete(ctx, k); err != nil {
		t.Fatalf("could not delete node from etcd: %v", err)
	}
	ev, err = w.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if want, got := 1, len(ev.NodeTombstones); want != got {
		t.Errorf("wanted %d node tombstoness, got %d", want, got)
	} else {
		n := ev.NodeTombstones[0]
		if want, got := fakeNodeID, n.NodeId; want != got {
			t.Errorf("wanted node %q, got %q", want, got)
		}
	}
}

// TestWatchNodeInCluster exercises a NodesInCluster Watch, from node creation,
// through updates, to not deletion.
func TestWatchNodesInCluster(t *testing.T) {
	cl := fakeLeader(t)
	defer cl.cancel()
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	cur := ipb.NewCuratorClient(cl.localNodeConn)

	w, err := cur.Watch(ctx, &ipb.WatchRequest{
		Kind: &ipb.WatchRequest_NodesInCluster_{
			NodesInCluster: &ipb.WatchRequest_NodesInCluster{},
		},
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	nodes := make(map[string]*ipb.Node)
	syncNodes := func() *ipb.WatchEvent {
		t.Helper()
		ev, err := w.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		for _, n := range ev.Nodes {
			n := n
			nodes[n.Id] = n
		}
		for _, nt := range ev.NodeTombstones {
			delete(nodes, nt.NodeId)
		}
		return ev
	}

	// Retrieve initial node fetch. This should yield one node.
	for {
		ev := syncNodes()
		if ev.Progress == ipb.WatchEvent_PROGRESS_LAST_BACKLOGGED {
			break
		}
	}
	if n := nodes[cl.localNodeID]; n == nil || n.Id != cl.localNodeID {
		t.Errorf("Expected node %q to be present, got %v", cl.localNodeID, nodes[cl.localNodeID])
	}
	if len(nodes) != 1 {
		t.Errorf("Expected exactly one node, got %d", len(nodes))
	}

	// Update the node status and expect a corresponding WatchEvent.
	_, err = cur.UpdateNodeStatus(ctx, &ipb.UpdateNodeStatusRequest{
		NodeId: cl.localNodeID,
		Status: &cpb.NodeStatus{
			ExternalAddress: "203.0.113.43",
		},
	})
	if err != nil {
		t.Fatalf("UpdateNodeStatus: %v", err)
	}
	for {
		syncNodes()
		n := nodes[cl.localNodeID]
		if n == nil {
			continue
		}
		if n.Status == nil || n.Status.ExternalAddress != "203.0.113.43" {
			continue
		}
		break
	}

	// Add a new (fake) node, and expect a corresponding WatchEvent.
	fakeNodePub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	fakeNodeID := identity.NodeID(fakeNodePub)
	fakeNodeKey, _ := nodeEtcdPrefix.Key(fakeNodeID)
	fakeNode := &ppb.Node{
		PublicKey: fakeNodePub,
		Roles:     &cpb.NodeRoles{},
	}
	fakeNodeInit, err := proto.Marshal(fakeNode)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	_, err = cl.etcd.Put(ctx, fakeNodeKey, string(fakeNodeInit))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	for {
		syncNodes()
		n := nodes[fakeNodeID]
		if n == nil {
			continue
		}
		if n.Id != fakeNodeID {
			t.Errorf("Wanted faked node ID %q, got %q", fakeNodeID, n.Id)
		}
		break
	}

	// Re-open watcher, resynchronize, expect two nodes to be present.
	nodes = make(map[string]*ipb.Node)
	w, err = cur.Watch(ctx, &ipb.WatchRequest{
		Kind: &ipb.WatchRequest_NodesInCluster_{
			NodesInCluster: &ipb.WatchRequest_NodesInCluster{},
		},
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	for {
		ev := syncNodes()
		if ev.Progress == ipb.WatchEvent_PROGRESS_LAST_BACKLOGGED {
			break
		}
	}
	if n := nodes[cl.localNodeID]; n == nil || n.Status == nil || n.Status.ExternalAddress != "203.0.113.43" {
		t.Errorf("Node %q should exist and have external address, got %v", cl.localNodeID, n)
	}
	if n := nodes[fakeNodeID]; n == nil {
		t.Errorf("Node %q should exist, got %v", fakeNodeID, n)
	}
	if len(nodes) != 2 {
		t.Errorf("Exptected two nodes in map, got %d", len(nodes))
	}

	// Remove fake node, expect it to be removed from synced map.
	k, _ := nodeEtcdPrefix.Key(fakeNodeID)
	if _, err := cl.etcd.Delete(ctx, k); err != nil {
		t.Fatalf("could not delete node from etcd: %v", err)
	}

	for {
		syncNodes()
		n := nodes[fakeNodeID]
		if n == nil {
			break
		}
	}
}

// TestRegistration exercises the node 'Register' (a.k.a. Registration) flow,
// which is described in the Cluster Lifecycle design document.
//
// It starts out with a node that's foreign to the cluster, and performs all
// the steps required to make that node part of a cluster. It calls into the
// Curator service as the registering node and the Management service as a
// cluster manager. The node registered into the cluster is fully fake, ie. is
// not an actual Metropolis node but instead is fully managed from within the
// test as a set of credentials.
func TestRegistration(t *testing.T) {
	cl := fakeLeader(t)
	defer cl.cancel()

	mgmt := apb.NewManagementClient(cl.mgmtConn)

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// Retrieve ticket twice.
	res1, err := mgmt.GetRegisterTicket(ctx, &apb.GetRegisterTicketRequest{})
	if err != nil {
		t.Fatalf("GetRegisterTicket failed: %v", err)
	}
	res2, err := mgmt.GetRegisterTicket(ctx, &apb.GetRegisterTicketRequest{})
	if err != nil {
		t.Fatalf("GetRegisterTicket failed: %v", err)
	}

	// Ensure tickets are set and the same.
	if len(res1.Ticket) == 0 {
		t.Errorf("Ticket is empty")
	}
	if !bytes.Equal(res1.Ticket, res2.Ticket) {
		t.Errorf("Unexpected ticket change between calls")
	}

	// Generate the node's public join key to be used in the bootstrap process.
	nodeJoinPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate node join keypair: %v", err)
	}

	// Register 'other node' into cluster.
	cur := ipb.NewCuratorClient(cl.otherNodeConn)
	_, err = cur.RegisterNode(ctx, &ipb.RegisterNodeRequest{
		RegisterTicket: res1.Ticket,
		JoinKey:        nodeJoinPub,
	})
	if err != nil {
		t.Fatalf("RegisterNode failed: %v", err)
	}

	expectOtherNode := func(state cpb.NodeState) {
		t.Helper()
		res, err := mgmt.GetNodes(ctx, &apb.GetNodesRequest{})
		if err != nil {
			t.Fatalf("GetNodes failed: %v", err)
		}

		for {
			node, err := res.Recv()
			if err != nil {
				t.Fatalf("Recv failed: %v", err)
			}
			if identity.NodeID(node.Pubkey) != cl.otherNodeID {
				continue
			}
			if node.State != state {
				t.Fatalf("Expected node to be %s, got %s", state, node.State)
			}
			return
		}
	}
	otherNodePub := cl.otherNodePriv.Public().(ed25519.PublicKey)

	// Expect node to now be 'NEW'.
	expectOtherNode(cpb.NodeState_NODE_STATE_NEW)

	// Approve node.
	_, err = mgmt.ApproveNode(ctx, &apb.ApproveNodeRequest{Pubkey: otherNodePub})
	if err != nil {
		t.Fatalf("ApproveNode failed: %v", err)
	}

	// Expect node to be 'STANDBY'.
	expectOtherNode(cpb.NodeState_NODE_STATE_STANDBY)

	// Approve call should be idempotent and not fail when called a second time.
	_, err = mgmt.ApproveNode(ctx, &apb.ApproveNodeRequest{Pubkey: otherNodePub})
	if err != nil {
		t.Fatalf("ApproveNode failed: %v", err)
	}

	// Make 'other node' commit itself into the cluster.
	_, err = cur.CommitNode(ctx, &ipb.CommitNodeRequest{
		ClusterUnlockKey: []byte("fakefakefakefakefakefakefakefake"),
	})
	if err != nil {
		t.Fatalf("CommitNode failed: %v", err)
	}

	// TODO(q3k): once the test cluster's PKI is consistent with the curator's
	// PKI, test the emitted credentials.

	// Expect node to be 'UP'.
	expectOtherNode(cpb.NodeState_NODE_STATE_UP)
}

// TestJoin exercises Join Flow, as described in "Cluster Lifecycle" design
// document, assuming the node has already completed Register Flow.
func TestJoin(t *testing.T) {
	cl := fakeLeader(t)
	defer cl.cancel()

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// Build the test node and save it into etcd.
	npub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate node keypair: %v", err)
	}
	jpub, jpriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate join keypair: %v", err)
	}
	cuk := []byte("fakefakefakefakefakefakefakefake")
	node := Node{
		clusterUnlockKey: cuk,
		pubkey:           npub,
		jkey:             jpub,
		state:            cpb.NodeState_NODE_STATE_UP,
	}
	if err := nodeSave(ctx, cl.l, &node); err != nil {
		t.Fatalf("nodeSave failed: %v", err)
	}

	// Connect to Curator using the node's Join Credentials, as opposed to the
	// node keypair, then join the cluster.
	withLocalDialer := grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
		return cl.curatorLis.Dial()
	})
	ephCreds, err := rpc.NewEphemeralCredentials(jpriv, cl.ca)
	if err != nil {
		t.Fatalf("NewEphemeralCredentials: %v", err)
	}
	eph, err := grpc.Dial("local", withLocalDialer, grpc.WithTransportCredentials(ephCreds))
	if err != nil {
		t.Fatalf("Dialing external GRPC failed: %v", err)
	}
	cur := ipb.NewCuratorClient(eph)
	jr, err := cur.JoinNode(ctx, &ipb.JoinNodeRequest{})
	if err != nil {
		t.Fatalf("JoinNode failed: %v", err)
	}

	// Compare the received CUK with the one we started out with.
	if bytes.Compare(cuk, jr.ClusterUnlockKey) != 0 {
		t.Fatal("JoinNode returned an invalid CUK.")
	}
}

// TestClusterUpdateNodeStatus exercises the Curator.UpdateNodeStatus RPC by
// sending node updates and making sure they are reflected in subsequent Watch
// events.
func TestClusterUpdateNodeStatus(t *testing.T) {
	cl := fakeLeader(t)
	defer cl.cancel()

	curator := ipb.NewCuratorClient(cl.localNodeConn)

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// Retrieve initial node data, it should have no status set.
	value, err := curator.Watch(ctx, &ipb.WatchRequest{
		Kind: &ipb.WatchRequest_NodeInCluster_{
			NodeInCluster: &ipb.WatchRequest_NodeInCluster{
				NodeId: cl.localNodeID,
			},
		},
	})
	if err != nil {
		t.Fatalf("Could not request node watch: %v", err)
	}
	ev, err := value.Recv()
	if err != nil {
		t.Fatalf("Could not receive initial node value: %v", err)
	}
	if status := ev.Nodes[0].Status; status != nil {
		t.Errorf("Initial node value contains status, should be nil: %+v", status)
	}

	// Update status...
	_, err = curator.UpdateNodeStatus(ctx, &ipb.UpdateNodeStatusRequest{
		NodeId: cl.localNodeID,
		Status: &cpb.NodeStatus{
			ExternalAddress: "192.0.2.10",
		},
	})
	if err != nil {
		t.Fatalf("UpdateNodeStatus: %v", err)
	}

	// ... and expect it to be reflected in the new node value.
	for {
		ev, err = value.Recv()
		if err != nil {
			t.Fatalf("Could not receive second node value: %v", err)
		}
		// Keep waiting until we get a status.
		status := ev.Nodes[0].Status
		if status == nil {
			continue
		}
		if want, got := "192.0.2.10", status.ExternalAddress; want != got {
			t.Errorf("Wanted external address %q, got %q", want, got)
		}
		break
	}

	// Expect updating some other node's ID to fail.
	_, err = curator.UpdateNodeStatus(ctx, &ipb.UpdateNodeStatusRequest{
		NodeId: cl.otherNodeID,
		Status: &cpb.NodeStatus{
			ExternalAddress: "192.0.2.10",
		},
	})
	if err == nil {
		t.Errorf("UpdateNodeStatus for other node (%q vs local %q) succeeded, should have failed", cl.localNodeID, cl.otherNodeID)
	}
}

// TestManagementClusterInfo exercises GetClusterInfo after setting a status.
func TestManagementClusterInfo(t *testing.T) {
	cl := fakeLeader(t)
	defer cl.cancel()

	mgmt := apb.NewManagementClient(cl.mgmtConn)
	curator := ipb.NewCuratorClient(cl.localNodeConn)

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// Update status to set an external address.
	_, err := curator.UpdateNodeStatus(ctx, &ipb.UpdateNodeStatusRequest{
		NodeId: cl.localNodeID,
		Status: &cpb.NodeStatus{
			ExternalAddress: "192.0.2.10",
		},
	})
	if err != nil {
		t.Fatalf("UpdateNodeStatus: %v", err)
	}

	// Retrieve cluster info and make sure it's as expected.
	res, err := mgmt.GetClusterInfo(ctx, &apb.GetClusterInfoRequest{})
	if err != nil {
		t.Fatalf("GetClusterInfo failed: %v", err)
	}

	nodes := res.ClusterDirectory.Nodes
	if want, got := 1, len(nodes); want != got {
		t.Fatalf("ClusterDirectory.Nodes contains %d elements, wanted %d", want, got)
	}
	node := nodes[0]

	// Address should match address set from status.
	if want, got := 1, len(node.Addresses); want != got {
		t.Fatalf("ClusterDirectory.Nodes[0].Addresses has %d elements, wanted %d", want, got)
	}
	if want, got := "192.0.2.10", node.Addresses[0].Host; want != got {
		t.Errorf("Nodes[0].Addresses[0].Host is %q, wanted %q", want, got)
	}

	// Cluster CA public key should match
	ca, err := x509.ParseCertificate(res.CaCertificate)
	if err != nil {
		t.Fatalf("CaCertificate could not be parsed: %v", err)
	}
	if want, got := cl.ca.PublicKey.(ed25519.PublicKey), ca.PublicKey.(ed25519.PublicKey); !bytes.Equal(want, got) {
		t.Fatalf("CaPublicKey mismatch (wanted %s, got %s)", hex.EncodeToString(want), hex.EncodeToString(got))
	}
}
