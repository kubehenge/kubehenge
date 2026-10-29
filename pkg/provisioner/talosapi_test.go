package provisioner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/api/common"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// fakeTalosAPI is a Talos node's API: it records every call with its request and the client
// certificate it came with, and answers as its fields say.
type fakeTalosAPI struct {
	mu        sync.Mutex
	calls     []string
	requests  map[string]any
	client    string
	recovered []byte

	// fail is what a method answers instead, by its name.
	fail      map[string]error
	pulls     []*machine.ImageServicePullResponse
	installer []*machine.LifecycleServiceInstallProgress
	snapshot  []byte
	protocol  string
	bootID    string
	services  []*machine.ServiceInfo
}

func (a *fakeTalosAPI) record(ctx context.Context, method string, req any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, method)
	if a.requests == nil {
		a.requests = map[string]any{}
	}
	a.requests[method] = req
	if p, ok := peer.FromContext(ctx); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			a.client = info.State.PeerCertificates[0].Subject.CommonName
		}
	}
	return a.fail[method]
}

func (a *fakeTalosAPI) called() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

func (a *fakeTalosAPI) caller() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

func (a *fakeTalosAPI) request(t *testing.T, method string) any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	req, ok := a.requests[method]
	if !ok {
		t.Fatalf("the node was never asked for %s; it was asked for %v", method, a.calls)
	}
	return req
}

// unary answers every unary call of the node before its handler would: the handlers' signatures
// name protobuf's well-known types, which this package does not otherwise depend on.
func (a *fakeTalosAPI) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
	method := path.Base(info.FullMethod)
	if err := a.record(ctx, method, req); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch method {
	case "Reboot":
		return &machine.RebootResponse{}, nil
	case "Reset":
		return &machine.ResetResponse{}, nil
	case "Bootstrap":
		return &machine.BootstrapResponse{}, nil
	case "EtcdStatus":
		return &machine.EtcdStatusResponse{Messages: []*machine.EtcdStatus{{MemberStatus: &machine.EtcdMemberStatus{ProtocolVersion: a.protocol}}}}, nil
	case "ServiceList":
		return &machine.ServiceListResponse{Messages: []*machine.ServiceList{{Services: a.services}}}, nil
	}
	return nil, status.Errorf(codes.Unimplemented, "%s", info.FullMethod)
}

type fakeMachineService struct {
	machine.UnimplementedMachineServiceServer

	api *fakeTalosAPI
}

func sendChunks(srv grpc.ServerStreamingServer[common.Data], data []byte) error {
	const chunk = 4000
	for off := 0; off < len(data); off += chunk {
		if err := srv.Send(&common.Data{Bytes: data[off:min(off+chunk, len(data))]}); err != nil {
			return err
		}
	}
	return nil
}

func (m fakeMachineService) EtcdSnapshot(req *machine.EtcdSnapshotRequest, srv grpc.ServerStreamingServer[common.Data]) error {
	if err := m.api.record(srv.Context(), "EtcdSnapshot", req); err != nil {
		return err
	}
	return sendChunks(srv, m.api.snapshot)
}

func (m fakeMachineService) Read(req *machine.ReadRequest, srv grpc.ServerStreamingServer[common.Data]) error {
	if err := m.api.record(srv.Context(), "Read", req); err != nil {
		return err
	}
	return sendChunks(srv, []byte(m.api.bootID))
}

func (m fakeMachineService) EtcdRecover(srv grpc.ClientStreamingServer[common.Data, machine.EtcdRecoverResponse]) error {
	if err := m.api.record(srv.Context(), "EtcdRecover", nil); err != nil {
		return err
	}
	var got []byte
	for {
		d, err := srv.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		got = append(got, d.GetBytes()...)
	}
	m.api.mu.Lock()
	m.api.recovered = got
	m.api.mu.Unlock()
	return srv.SendAndClose(&machine.EtcdRecoverResponse{})
}

type fakeImageService struct {
	machine.UnimplementedImageServiceServer

	api *fakeTalosAPI
}

func (s fakeImageService) Pull(req *machine.ImageServicePullRequest, srv grpc.ServerStreamingServer[machine.ImageServicePullResponse]) error {
	if err := s.api.record(srv.Context(), "Pull", req); err != nil {
		return err
	}
	for _, r := range s.api.pulls {
		if err := srv.Send(r); err != nil {
			return err
		}
	}
	return nil
}

type fakeLifecycleService struct {
	machine.UnimplementedLifecycleServiceServer

	api *fakeTalosAPI
}

func (s fakeLifecycleService) Upgrade(req *machine.LifecycleServiceUpgradeRequest, srv grpc.ServerStreamingServer[machine.LifecycleServiceUpgradeResponse]) error {
	if err := s.api.record(srv.Context(), "Upgrade", req); err != nil {
		return err
	}
	for _, p := range s.api.installer {
		if err := srv.Send(&machine.LifecycleServiceUpgradeResponse{Progress: p}); err != nil {
			return err
		}
	}
	return nil
}

// talosAddresses gives each test a loopback address of its own, as DialTalos always dials the
// Talos API's fixed port.
var talosAddresses atomic.Uint32

// listenTalosAPI listens on the Talos API's port of a loopback address no other test uses.
func listenTalosAPI(t *testing.T) (net.Listener, string) {
	t.Helper()
	var errs []error
	for range 20 {
		n := talosAddresses.Add(1)
		address := fmt.Sprintf("127.77.%d.%d", n/250, n%250+1)
		lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(address, strconv.Itoa(talosAPIPort)))
		if err == nil {
			return lis, address
		}
		errs = append(errs, err)
	}
	t.Fatalf("no loopback address takes the Talos API's port: %v", errors.Join(errs...))
	return nil, ""
}

// sign issues a certificate for template from the CA, in PEM with its key.
func (ca *testCA) sign(t *testing.T, template *x509.Certificate) (*tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template.SerialNumber = big.NewInt(time.Now().UnixNano())
	template.NotBefore, template.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	template.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// testTalosconfig is a talosconfig as CABPT writes it: the cluster's Talos CA and an admin
// certificate, with an endpoint DialTalos must not use, as it reaches each node by its own address.
func testTalosconfig(t *testing.T, ca, signer *testCA) []byte {
	t.Helper()
	_, crt, key := signer.sign(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "os:admin"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Appendf(nil, "context: web\ncontexts:\n  web:\n    endpoints:\n      - 192.0.2.10\n    ca: %s\n    crt: %s\n    key: %s\n",
		b64(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})), b64(crt), b64(key))
}

// startTalosAPI serves api on a loopback address with a serving certificate from a new Talos CA,
// accepting only clients that CA signed, and returns the address and a talosconfig of that CA.
func startTalosAPI(t *testing.T, api *fakeTalosAPI) (string, []byte, *testCA) {
	t.Helper()
	lis, address := listenTalosAPI(t)
	ca := newTestCA(t)
	serving, _, _ := ca.sign(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "web-cp-0"}, IPAddresses: []net.IP{net.ParseIP(address)},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	clients := x509.NewCertPool()
	clients.AddCert(ca.cert)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*serving},
		ClientCAs: clients, ClientAuth: tls.RequireAndVerifyClientCert,
	})), grpc.UnaryInterceptor(api.unary))
	machine.RegisterMachineServiceServer(srv, fakeMachineService{api: api})
	machine.RegisterImageServiceServer(srv, fakeImageService{api: api})
	machine.RegisterLifecycleServiceServer(srv, fakeLifecycleService{api: api})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return address, testTalosconfig(t, ca, ca), ca
}

// dialFake starts a node serving api and reaches it as the controller does.
func dialFake(t *testing.T, api *fakeTalosAPI) (*talosNode, string) {
	t.Helper()
	address, config, _ := startTalosAPI(t, api)
	dialled, err := DialTalos(t.Context(), config, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialled.Close() })
	node, ok := dialled.(*talosNode)
	if !ok {
		t.Fatalf("DialTalos made a %T", dialled)
	}
	return node, address
}

// sentTo is the request the node was last sent for method.
func sentTo[T any](t *testing.T, api *fakeTalosAPI, method string) T {
	t.Helper()
	req, ok := api.request(t, method).(T)
	if !ok {
		t.Fatalf("%s was sent a %T", method, api.request(t, method))
	}
	return req
}

func TestNotReadyMarksOnlyABootingNodesRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code    codes.Code
		booting bool
	}{
		{codes.FailedPrecondition, true},
		{codes.Unavailable, true},
		{codes.NotFound, false},
		{codes.PermissionDenied, false},
		{codes.Internal, false},
	} {
		err := notReady(status.Error(tc.code, "refused"))
		if errors.Is(err, errTalosNotReady) != tc.booting {
			t.Errorf("%s: not ready %v", tc.code, errors.Is(err, errTalosNotReady))
		}
		if status.Code(err) != tc.code {
			t.Errorf("%s: the gRPC code became %s", tc.code, status.Code(err))
		}
	}
	if err := notReady(errors.New("disk full")); errors.Is(err, errTalosNotReady) || err.Error() != "disk full" {
		t.Errorf("a failure that is not gRPC's came back as %v", err)
	}
	if err := notReady(nil); err != nil {
		t.Errorf("no error became %v", err)
	}
}

func TestLastMessagesKeepsTheInstallersLastFive(t *testing.T) {
	t.Parallel()
	if got := lastMessages(nil); got != "" {
		t.Errorf("no messages: %q", got)
	}
	if got := lastMessages([]string{"a", "b"}); got != "a; b" {
		t.Errorf("two messages: %q", got)
	}
	if got := lastMessages([]string{"one", "two", "three", "four", "five", "six", "seven"}); got != "three; four; five; six; seven" {
		t.Errorf("seven messages: %q", got)
	}
}

// The node is reached at its own address with the talosconfig's admin certificate, never at the
// talosconfig's endpoint.
func TestDialTalosReachesTheNodeItselfWithTheTalosconfigsCertificate(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{bootID: "  3f1c0a2e-6c1b-4e0f-9d7a-2b8c5e4f1a90\n"}
	node, _ := dialFake(t, api)
	id, err := node.BootID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if id != "3f1c0a2e-6c1b-4e0f-9d7a-2b8c5e4f1a90" {
		t.Errorf("boot ID %q", id)
	}
	if got := sentTo[*machine.ReadRequest](t, api, "Read").GetPath(); got != "/proc/sys/kernel/random/boot_id" {
		t.Errorf("read %s", got)
	}
	if who := api.caller(); who != "os:admin" {
		t.Errorf("the node was reached as %q", who)
	}
}

func TestDialTalosRefusesATalosconfigItCannotUse(t *testing.T) {
	t.Parallel()
	if _, err := DialTalos(t.Context(), []byte("context: [not, a, name"), "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "reading the talosconfig") {
		t.Errorf("unreadable talosconfig: %v", err)
	}
	missing := []byte("context: gone\ncontexts:\n  web:\n    endpoints: [192.0.2.10]\n")
	if _, err := DialTalos(t.Context(), missing, "192.0.2.20"); err == nil || !strings.Contains(err.Error(), "reaching the Talos API of 192.0.2.20") {
		t.Errorf("talosconfig without its context: %v", err)
	}
}

// A node whose serving certificate the talosconfig's CA did not sign is never trusted with a call.
func TestDialTalosTrustsOnlyTheTalosconfigsCA(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{bootID: "boot"}
	address, _, ca := startTalosAPI(t, api)
	other := newTestCA(t)
	node, err := DialTalos(t.Context(), testTalosconfig(t, other, ca), address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = node.Close() }()
	if id, err := node.BootID(t.Context()); err == nil {
		t.Errorf("an untrusted node answered %q", id)
	}
	if calls := api.called(); len(calls) != 0 {
		t.Errorf("the untrusted node was asked for %v", calls)
	}
}

func pulled(name string) *machine.ImageServicePullResponse {
	return &machine.ImageServicePullResponse{Response: &machine.ImageServicePullResponse_Name{Name: name}}
}

func pulling(layer string) *machine.ImageServicePullResponse {
	return &machine.ImageServicePullResponse{Response: &machine.ImageServicePullResponse_PullProgress{
		PullProgress: &machine.ImageServicePullProgress{LayerId: layer},
	}}
}

func said(message string) *machine.LifecycleServiceInstallProgress {
	return &machine.LifecycleServiceInstallProgress{Response: &machine.LifecycleServiceInstallProgress_Message{Message: message}}
}

func exited(code int32) *machine.LifecycleServiceInstallProgress {
	return &machine.LifecycleServiceInstallProgress{Response: &machine.LifecycleServiceInstallProgress_ExitCode{ExitCode: code}}
}

const testInstaller = "factory.talos.dev/installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.12.4"

// The installer goes into Talos's own containerd, and the upgrade installs the image by the name
// the pull gave it.
func TestInstallPullsIntoTalossContainerdAndInstallsThePulledImage(t *testing.T) {
	t.Parallel()
	const name = "factory.talos.dev/installer@sha256:9a3c"
	api := &fakeTalosAPI{
		pulls:     []*machine.ImageServicePullResponse{pulling("sha256:aa"), pulled(name), pulling("sha256:bb")},
		installer: []*machine.LifecycleServiceInstallProgress{said("writing the boot loader"), exited(0)},
	}
	node, _ := dialFake(t, api)
	if err := node.Install(t.Context(), testInstaller); err != nil {
		t.Fatal(err)
	}
	if calls := api.called(); !slices.Equal(calls, []string{"Pull", "Upgrade"}) {
		t.Errorf("calls %v", calls)
	}
	pull := sentTo[*machine.ImageServicePullRequest](t, api, "Pull")
	if pull.GetImageRef() != testInstaller || pull.GetContainerd().GetNamespace() != common.ContainerdNamespace_NS_SYSTEM ||
		pull.GetContainerd().GetDriver() != common.ContainerDriver_CRI {
		t.Errorf("pull %v", pull)
	}
	upgrade := sentTo[*machine.LifecycleServiceUpgradeRequest](t, api, "Upgrade")
	if upgrade.GetSource().GetImageName() != name || upgrade.GetContainerd().GetNamespace() != common.ContainerdNamespace_NS_SYSTEM {
		t.Errorf("upgrade %v", upgrade)
	}
}

// A failed install says why with the installer's last words, which are the ones that matter.
func TestInstallFailureCarriesTheInstallersLastMessages(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{
		pulls: []*machine.ImageServicePullResponse{pulled("installer")},
		installer: []*machine.LifecycleServiceInstallProgress{
			said("alpha"), said("bravo"), said("charlie"), said("delta"), said("echo"), said("foxtrot"), said("disk not found"), exited(1),
		},
	}
	node, address := dialFake(t, api)
	err := node.Install(t.Context(), testInstaller)
	if err == nil {
		t.Fatal("a failed install succeeded")
	}
	for _, want := range []string{address, "exited with 1", "charlie; delta; echo; foxtrot; disk not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "bravo") {
		t.Errorf("%v carries more than the last five messages", err)
	}
}

func TestInstallWithoutAnExitCodeFails(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{
		pulls:     []*machine.ImageServicePullResponse{pulled("installer")},
		installer: []*machine.LifecycleServiceInstallProgress{said("writing the boot loader")},
	}
	node, _ := dialFake(t, api)
	err := node.Install(t.Context(), testInstaller)
	if err == nil || !strings.Contains(err.Error(), "ended without an exit code: writing the boot loader") {
		t.Errorf("an install cut short: %v", err)
	}
}

func TestInstallStopsWhenThePullNamesNoImage(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{pulls: []*machine.ImageServicePullResponse{pulling("sha256:aa")}}
	node, _ := dialFake(t, api)
	if err := node.Install(t.Context(), testInstaller); err == nil || !strings.Contains(err.Error(), "named no image") {
		t.Errorf("a pull without a name: %v", err)
	}
	if calls := api.called(); !slices.Equal(calls, []string{"Pull"}) {
		t.Errorf("calls %v", calls)
	}
}

func TestInstallReportsTheNodesRefusals(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{fail: map[string]error{"Pull": status.Error(codes.NotFound, "no such image")}}
	node, _ := dialFake(t, api)
	err := node.Install(t.Context(), testInstaller)
	if status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), "pulling "+testInstaller) {
		t.Errorf("a refused pull: %v", err)
	}

	api = &fakeTalosAPI{
		pulls: []*machine.ImageServicePullResponse{pulled("installer")},
		fail:  map[string]error{"Upgrade": status.Error(codes.FailedPrecondition, "an upgrade is running")},
	}
	node, _ = dialFake(t, api)
	err = node.Install(t.Context(), testInstaller)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "installing "+testInstaller) {
		t.Errorf("a refused install: %v", err)
	}
}

func TestRebootAsksForAnOrdinaryReboot(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{}
	node, _ := dialFake(t, api)
	if err := node.Reboot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if mode := sentTo[*machine.RebootRequest](t, api, "Reboot").GetMode(); mode != machine.RebootRequest_DEFAULT {
		t.Errorf("reboot mode %s", mode)
	}

	api = &fakeTalosAPI{fail: map[string]error{"Reboot": status.Error(codes.PermissionDenied, "reader role")}}
	node, _ = dialFake(t, api)
	if err := node.Reboot(t.Context()); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "rebooting") {
		t.Errorf("a refused reboot: %v", err)
	}
}

func TestEtcdSnapshotStreamsTheSnapshotWithTheMembersVersion(t *testing.T) {
	t.Parallel()
	snapshot := make([]byte, 300<<10)
	for i := range snapshot {
		snapshot[i] = byte(i * 7 % 251)
	}
	api := &fakeTalosAPI{snapshot: snapshot, protocol: "3.6.0"}
	node, _ := dialFake(t, api)
	r, version, err := node.EtcdSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Error(err)
	}
	if !bytes.Equal(got, snapshot) {
		t.Errorf("the snapshot came through as %d bytes of %d, or changed", len(got), len(snapshot))
	}
	if version != "3.6.0" {
		t.Errorf("version %q", version)
	}
	if calls := api.called(); !slices.Equal(calls, []string{"EtcdStatus", "EtcdSnapshot"}) {
		t.Errorf("calls %v", calls)
	}
}

func TestEtcdSnapshotOfANodeWithoutEtcdFails(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{fail: map[string]error{"EtcdStatus": status.Error(codes.Unimplemented, "not a control-plane node")}}
	node, _ := dialFake(t, api)
	if _, _, err := node.EtcdSnapshot(t.Context()); err == nil || !strings.Contains(err.Error(), "etcd status") {
		t.Errorf("a node without etcd: %v", err)
	}
	if calls := api.called(); !slices.Equal(calls, []string{"EtcdStatus"}) {
		t.Errorf("calls %v", calls)
	}
}

func TestBootIDOfABootingNodeIsNotReady(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{fail: map[string]error{"Read": status.Error(codes.Unavailable, "apid is starting")}}
	node, _ := dialFake(t, api)
	if _, err := node.BootID(t.Context()); !errors.Is(err, errTalosNotReady) {
		t.Errorf("a booting node: %v", err)
	}

	api = &fakeTalosAPI{fail: map[string]error{"Read": status.Error(codes.PermissionDenied, "reader role")}}
	node, _ = dialFake(t, api)
	if _, err := node.BootID(t.Context()); err == nil || errors.Is(err, errTalosNotReady) {
		t.Errorf("a refusal for good: %v", err)
	}
}

// Wiping etcd resets the node without leaving etcd, which has lost its quorum, and wipes only
// EPHEMERAL, so the machine configuration stays.
func TestWipeEtcdResetsOnlyTheEphemeralPartitionAndReboots(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{}
	node, _ := dialFake(t, api)
	if err := node.WipeEtcd(t.Context()); err != nil {
		t.Fatal(err)
	}
	reset := sentTo[*machine.ResetRequest](t, api, "Reset")
	parts := reset.GetSystemPartitionsToWipe()
	if !reset.GetReboot() || reset.GetGraceful() || len(reset.GetUserDisksToWipe()) != 0 ||
		len(parts) != 1 || parts[0].GetLabel() != constants.EphemeralPartitionLabel || !parts[0].GetWipe() {
		t.Errorf("reset %v", reset)
	}

	api = &fakeTalosAPI{fail: map[string]error{"Reset": status.Error(codes.FailedPrecondition, "still booting")}}
	node, _ = dialFake(t, api)
	if err := node.WipeEtcd(t.Context()); !errors.Is(err, errTalosNotReady) || !strings.Contains(err.Error(), "wiping etcd") {
		t.Errorf("a booting node: %v", err)
	}
}

func TestEtcdReportsTheServicesStateAndHealth(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{services: []*machine.ServiceInfo{
		{Id: "apid", State: "Running", Health: &machine.ServiceHealth{Healthy: true}},
		{Id: "etcd", State: "Preparing", Health: &machine.ServiceHealth{Healthy: false}},
	}}
	node, _ := dialFake(t, api)
	state, healthy, err := node.Etcd(t.Context())
	if err != nil || state != "Preparing" || healthy {
		t.Errorf("etcd %q, healthy %v, %v", state, healthy, err)
	}

	api.mu.Lock()
	api.services[1] = &machine.ServiceInfo{Id: "etcd", State: "Running", Health: &machine.ServiceHealth{Healthy: true}}
	api.mu.Unlock()
	if state, healthy, err := node.Etcd(t.Context()); err != nil || state != "Running" || !healthy {
		t.Errorf("etcd %q, healthy %v, %v", state, healthy, err)
	}
}

// A node that lists no etcd yet, or refuses while it boots, is not ready rather than broken.
func TestEtcdOfANodeStillBootingIsNotReady(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{services: []*machine.ServiceInfo{{Id: "apid", State: "Running"}}}
	node, _ := dialFake(t, api)
	if _, _, err := node.Etcd(t.Context()); !errors.Is(err, errTalosNotReady) || !strings.Contains(err.Error(), "etcd is not among its services") {
		t.Errorf("no etcd yet: %v", err)
	}

	api = &fakeTalosAPI{fail: map[string]error{"ServiceList": status.Error(codes.Unavailable, "machined is starting")}}
	node, _ = dialFake(t, api)
	if _, _, err := node.Etcd(t.Context()); !errors.Is(err, errTalosNotReady) {
		t.Errorf("a booting node: %v", err)
	}
}

// Restoring etcd on a node wipes it, uploads the snapshot unchanged, then bootstraps etcd from it.
func TestRestoreUploadsTheSnapshotThenBootstrapsFromIt(t *testing.T) {
	t.Parallel()
	snapshot := bytes.Repeat([]byte("etcd snapshot "), 10_000)
	api := &fakeTalosAPI{}
	node, _ := dialFake(t, api)
	if err := node.WipeEtcd(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := node.UploadEtcdSnapshot(t.Context(), bytes.NewReader(snapshot)); err != nil {
		t.Fatal(err)
	}
	if err := node.BootstrapRecovered(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls := api.called(); !slices.Equal(calls, []string{"Reset", "EtcdRecover", "Bootstrap"}) {
		t.Errorf("calls %v", calls)
	}
	api.mu.Lock()
	recovered := api.recovered
	api.mu.Unlock()
	if !bytes.Equal(recovered, snapshot) {
		t.Errorf("the node received %d bytes of %d, or changed", len(recovered), len(snapshot))
	}
	if !sentTo[*machine.BootstrapRequest](t, api, "Bootstrap").GetRecoverEtcd() {
		t.Error("the bootstrap did not recover etcd from the snapshot")
	}
}

func TestRestoreOnABootingNodeIsNotReady(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{fail: map[string]error{
		"EtcdRecover": status.Error(codes.FailedPrecondition, "etcd is not waiting for a snapshot"),
		"Bootstrap":   status.Error(codes.Unavailable, "machined is starting"),
	}}
	node, _ := dialFake(t, api)
	if err := node.UploadEtcdSnapshot(t.Context(), strings.NewReader("snapshot")); !errors.Is(err, errTalosNotReady) || !strings.Contains(err.Error(), "uploading the snapshot") {
		t.Errorf("upload: %v", err)
	}
	if err := node.BootstrapRecovered(t.Context()); !errors.Is(err, errTalosNotReady) || !strings.Contains(err.Error(), "restoring etcd") {
		t.Errorf("bootstrap: %v", err)
	}

	api = &fakeTalosAPI{fail: map[string]error{"Bootstrap": status.Error(codes.AlreadyExists, "etcd is running")}}
	node, _ = dialFake(t, api)
	if err := node.BootstrapRecovered(t.Context()); err == nil || errors.Is(err, errTalosNotReady) {
		t.Errorf("a refusal for good: %v", err)
	}
}

func TestCloseEndsTheConnection(t *testing.T) {
	t.Parallel()
	api := &fakeTalosAPI{bootID: "boot"}
	address, config, _ := startTalosAPI(t, api)
	node, err := DialTalos(t.Context(), config, address)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.BootID(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := node.BootID(t.Context()); err == nil {
		t.Error("a closed connection still reached the node")
	}
	if err := node.Close(); err == nil || !strings.Contains(err.Error(), address) {
		t.Errorf("closing twice: %v", err)
	}
}
