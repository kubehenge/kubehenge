package pve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// closeHook runs after a response's body is closed, which is once the client has read the answer.
type closeHook struct {
	next    http.RoundTripper
	matches func(*http.Request) bool
	after   func()
}

func (h closeHook) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := h.next.RoundTrip(req)
	if err != nil || !h.matches(req) {
		return resp, err
	}
	resp.Body = hookedBody{resp.Body, h.after}
	return resp, nil
}

type hookedBody struct {
	io.ReadCloser

	after func()
}

func (b hookedBody) Close() error {
	err := b.ReadCloser.Close()
	b.after()
	return err
}

// guestAgent is VM 100 on node one with its guest agent, which starts commands as pid 4242.
type guestAgent struct {
	mu      sync.Mutex
	started map[string]any
	polls   []time.Time
	// status answers the n-th poll, from 0.
	status func(n int) (int, any)
	// startFails has the agent refuse to start the command.
	startFails bool
}

func (g *guestAgent) vm(t *testing.T, wrap func(http.RoundTripper) http.RoundTripper) (*proxmox.Client, *proxmox.VirtualMachine) {
	t.Helper()
	if g.status == nil {
		g.status = func(int) (int, any) { return http.StatusOK, map[string]any{"exited": 0} }
	}
	reply := func(w http.ResponseWriter, data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/nodes/one/status", func(w http.ResponseWriter, _ *http.Request) { reply(w, map[string]any{}) })
	mux.HandleFunc("GET /api2/json/nodes/one/qemu/100/status/current", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"vmid": 100, "name": "web-cp-a", "status": "running"})
	})
	mux.HandleFunc("GET /api2/json/nodes/one/qemu/100/config", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"name": "web-cp-a", "agent": "1"})
	})
	mux.HandleFunc("POST /api2/json/nodes/one/qemu/100/agent/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=kubehenge@pve!controller=s3cret" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&g.started); err != nil {
			t.Error(err)
		}
		if g.startFails {
			http.Error(w, "QEMU guest agent is not running", http.StatusInternalServerError)
			return
		}
		reply(w, map[string]any{"pid": 4242})
	})
	mux.HandleFunc("GET /api2/json/nodes/one/qemu/100/agent/exec-status", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pid") != "4242" {
			t.Errorf("status asked for pid %s", r.URL.Query().Get("pid"))
		}
		g.mu.Lock()
		g.polls = append(g.polls, time.Now())
		code, data := g.status(len(g.polls) - 1)
		g.mu.Unlock()
		if code != http.StatusOK {
			http.Error(w, "agent error", code)
			return
		}
		reply(w, data)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	transport := srv.Client().Transport
	if wrap != nil {
		transport = wrap(transport)
	}
	c := proxmox.NewClient(srv.URL+"/api2/json", proxmox.WithHTTPClient(&http.Client{Transport: transport}), proxmox.WithAPIToken("kubehenge@pve!controller", "s3cret"))
	node, err := c.Node(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := node.VirtualMachine(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return c, vm
}

func TestGuestExecWaitsLongerEachTimeAndReportsTheExit(t *testing.T) {
	t.Parallel()
	g := &guestAgent{status: func(n int) (int, any) {
		if n < 2 {
			return http.StatusOK, map[string]any{"exited": 0}
		}
		// Proxmox VE sends the truncation flags as numbers.
		return http.StatusOK, map[string]any{"exited": 1, "exitcode": 3, "out-data": "partial\n", "err-data": "failed\n", "err-truncated": 1}
	}}
	c, vm := g.vm(t, nil)
	status, err := GuestExec(context.Background(), c, vm, []string{"/bin/bash", "-s"}, "echo hi", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if status.ExitCode != 3 || status.Output() != "partial\n\nfailed" {
		t.Errorf("exit %d, output %q", status.ExitCode, status.Output())
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if want := map[string]any{"command": []any{"/bin/bash", "-s"}, "input-data": "echo hi"}; !reflect.DeepEqual(g.started, want) {
		t.Errorf("started %v, want %v", g.started, want)
	}
	if len(g.polls) != 3 {
		t.Fatalf("%d polls, want 3", len(g.polls))
	}
	if first, second := g.polls[1].Sub(g.polls[0]), g.polls[2].Sub(g.polls[1]); first < PollFirst || second < Backoff(PollFirst) {
		t.Errorf("polled after %s and %s, want at least %s and %s", first, second, PollFirst, Backoff(PollFirst))
	}
}

func TestGuestExecGivesUpOnceTheTimeoutHasPassed(t *testing.T) {
	t.Parallel()
	g := &guestAgent{}
	c, vm := g.vm(t, nil)
	_, err := GuestExec(context.Background(), c, vm, []string{"sleep", "600"}, "", 0)
	if err == nil || !strings.Contains(err.Error(), "still running after 0s") {
		t.Errorf("got %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.polls) != 1 {
		t.Errorf("%d polls, want the status read once", len(g.polls))
	}
}

func TestGuestExecStopsWaitingWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &guestAgent{}
	// Cancelled once the first status has been read, so the wait before the next poll sees it.
	c, vm := g.vm(t, func(next http.RoundTripper) http.RoundTripper {
		return closeHook{next: next, matches: func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/exec-status") }, after: cancel}
	})
	_, err := GuestExec(ctx, c, vm, []string{"sleep", "600"}, "", time.Hour)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "waiting for it to end") {
		t.Errorf("got %v", err)
	}
}

func TestGuestExecFailsWhenTheAgentCannotStartTheCommand(t *testing.T) {
	t.Parallel()
	g := &guestAgent{startFails: true}
	c, vm := g.vm(t, nil)
	_, err := GuestExec(context.Background(), c, vm, []string{"true"}, "", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "starting it through the guest agent") {
		t.Errorf("got %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.polls) != 0 {
		t.Errorf("polled %d times for a command that never started", len(g.polls))
	}
}

func TestGuestExecFailsWhenItsStatusCannotBeRead(t *testing.T) {
	t.Parallel()
	g := &guestAgent{status: func(int) (int, any) { return http.StatusInternalServerError, nil }}
	c, vm := g.vm(t, nil)
	_, err := GuestExec(context.Background(), c, vm, []string{"true"}, "", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "reading its status from the guest agent") {
		t.Errorf("got %v", err)
	}
}

func TestBackoffDoublesUpToTheLongestWait(t *testing.T) {
	t.Parallel()
	var waits []time.Duration
	for interval := PollFirst; len(waits) < 7; interval = Backoff(interval) {
		waits = append(waits, interval)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, PollLongest, PollLongest}
	if !reflect.DeepEqual(waits, want) {
		t.Errorf("waits %v, want %v", waits, want)
	}
}

func TestOutputJoinsBothStreamsWithoutSurroundingSpace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		out, err, want string
	}{
		{"done\n", "", "done"},
		{"", "no such file\n", "no such file"},
		{"line one\nline two\n", "warning\n", "line one\nline two\n\nwarning"},
		{"", "", ""},
	} {
		s := &ExecStatus{OutData: c.out, ErrData: c.err}
		if got := s.Output(); got != c.want {
			t.Errorf("out %q, err %q: %q, want %q", c.out, c.err, got, c.want)
		}
	}
}

func TestFileBytesUndoesProxmoxVEsEncoding(t *testing.T) {
	t.Parallel()
	// Each byte of the file as one character, as Proxmox VE sends a file the agent read.
	asSent := func(file string) string {
		runes := make([]rune, 0, len(file))
		for _, b := range []byte(file) {
			runes = append(runes, rune(b))
		}
		return string(runes)
	}
	cut := asSent("done ✓")
	for _, tc := range []struct{ name, content, want string }{
		{"ASCII", "k3s[812]: started\n", "k3s[812]: started\n"},
		{"UTF-8", asSent("cloud-init … done ✓\n"), "cloud-init … done ✓\n"},
		{"cut in a character", cut[:len(cut)-2], "done �"},
		{"not sent byte by byte", "Ölfeld ✓", "Ölfeld ✓"},
	} {
		if got := fileBytes(tc.content); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
