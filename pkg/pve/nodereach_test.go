package pve

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// listedNodes serves /cluster/resources with the nodes online, and each node's status as its mode
// says: "ok", "596", "403", "hang" or "recover" (596 until recovered is closed).
func listedNodes(t *testing.T, modes map[string]string, recovered <-chan struct{}) *Accounts {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		list := make([]map[string]any, 0, len(modes)+1)
		for name := range modes {
			list = append(list, map[string]any{"node": name, "status": "online", "mem": 1, "maxmem": 2})
		}
		list = append(list, map[string]any{"node": "down", "status": "offline"})
		replyData(t, w, list)
	})
	mux.HandleFunc("GET /api2/json/nodes/{node}/status", func(w http.ResponseWriter, r *http.Request) {
		switch mode := modes[r.PathValue("node")]; {
		case mode == "596", mode == "recover" && !closed(recovered):
			refuseInStatusLine(t, w, "596 certificate verify failed")
		case mode == "403":
			http.Error(w, "Permission check failed (/nodes/x, Sys.Audit)", http.StatusForbidden)
		case mode == "hang":
			<-r.Context().Done()
		default:
			replyData(t, w, map[string]any{})
		}
	})
	a := serveAPI(t, mux)
	a.probeTimeout = 200 * time.Millisecond
	return a
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func nodeStates(t *testing.T, a *Accounts) map[string]Node {
	t.Helper()
	nodes, err := a.Nodes(context.Background(), alice)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Node{}
	for _, n := range nodes {
		out[n.Node] = n
	}
	return out
}

func TestNodesCountOnlyThoseThatAnswerForwardedCalls(t *testing.T) {
	t.Parallel()
	got := nodeStates(t, listedNodes(t, map[string]string{"one": "ok", "two": "596"}, nil))
	if one := got["one"]; !one.Online || one.Fault != "" {
		t.Errorf("one answers: %+v", one)
	}
	if two := got["two"]; two.Online || two.Fault != "HTTP 596: certificate verify failed" {
		t.Errorf("two answers 596: %+v, want it not online, with Proxmox VE's reason", two)
	}
	if down := got["down"]; down.Online || down.Fault != "" {
		t.Errorf("down is offline: %+v, want no fault, since it is no member that fails", down)
	}
}

func TestNodesCallAnAnswerOfARefusalAnAnswer(t *testing.T) {
	t.Parallel()
	got := nodeStates(t, listedNodes(t, map[string]string{"one": "403"}, nil))
	if one := got["one"]; !one.Online || one.Fault != "" {
		t.Errorf("a user without Sys.Audit on one: %+v, want it online, as the call arrived", one)
	}
}

func TestNodesGiveUpOnANodeThatHangs(t *testing.T) {
	t.Parallel()
	start := time.Now()
	got := nodeStates(t, listedNodes(t, map[string]string{"one": "ok", "two": "hang"}, nil))
	if two := got["two"]; two.Online || two.Fault != "no answer in time" {
		t.Errorf("two hangs: %+v, want it not online, with no answer in time", two)
	}
	if !got["one"].Online {
		t.Errorf("one answers but is %+v", got["one"])
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the list took %v, want it bounded by the probe's timeout", took)
	}
}

func TestNodesTakeANodeBackOnTheNextRead(t *testing.T) {
	t.Parallel()
	recovered := make(chan struct{})
	a := listedNodes(t, map[string]string{"one": "ok", "two": "recover"}, recovered)
	if two := nodeStates(t, a)["two"]; two.Online || two.Fault == "" {
		t.Fatalf("two before it recovers: %+v", two)
	}
	close(recovered)
	if two := nodeStates(t, a)["two"]; !two.Online || two.Fault != "" {
		t.Errorf("two after it recovers: %+v, want it online with no fault", two)
	}
}

func TestReasonWordsWhyACallFailed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&APIError{Status: "596 certificate verify failed"}, "HTTP 596: certificate verify failed"},
		{&APIError{Status: "596 "}, "HTTP 596"},
		{context.DeadlineExceeded, "no answer in time"},
		{ErrForbidden, "not authorised"},
		{errors.New("connection refused"), "connection refused"},
	} {
		if got := Reason(tc.err); got != tc.want {
			t.Errorf("Reason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
