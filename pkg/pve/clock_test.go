package pve

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNodeTimeIsTheNodesClockInUTC(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	alice, _, err := a.SignIn(ctx, "alice@pve", "secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.NodeTime(ctx, alice, "one")
	if err != nil || got.Unix() != 1760000000 {
		t.Fatalf("NodeTime = %v, %v; want the UTC seconds, not the local time", got, err)
	}
	other := &Ticket{Username: "bob@pve", Ticket: "PVE:bob@pve:1", Issued: time.Now().Add(-time.Hour)}
	if _, err := a.NodeTime(ctx, other, "one"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a user without Sys.Audit on the node: %v, want ErrForbidden", err)
	}
}

// A ticket one node signed and another refuses seconds later is the nodes' clocks, not a session
// that ended. The fake refuses every ticket but alice's own, as a node does that finds a ticket
// from the future.
func TestARefusedFreshTicketBlamesTheNodesClocks(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	ctx := context.Background()
	signed, _, err := a.SignIn(ctx, "alice@pve", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Privileges(ctx, signed, "/"); err != nil {
		t.Fatalf("a good ticket: %v", err)
	}

	for name, c := range map[string]struct {
		ticket *Ticket
		want   error
	}{
		"fresh":               {&Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:2", Issued: time.Now()}, ErrNodesDisagree},
		"nine seconds":        {&Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:2", Issued: time.Now().Add(-9 * time.Second)}, ErrNodesDisagree},
		"eleven seconds":      {&Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:2", Issued: time.Now().Add(-11 * time.Second)}, ErrAuthentication},
		"an hour":             {&Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:2", Issued: time.Now().Add(-time.Hour)}, ErrAuthentication},
		"a token, so no node": {&Ticket{Username: "alice@pve!t", Token: "alice@pve!t=x", Issued: time.Now()}, ErrAuthentication},
	} {
		_, err := a.Privileges(ctx, c.ticket, "/")
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if !errors.Is(err, ErrAuthentication) {
			t.Errorf("%s: %v is no authentication failure", name, err)
		}
		if c.want == ErrAuthentication && errors.Is(err, ErrNodesDisagree) {
			t.Errorf("%s: blamed the nodes' clocks", name)
		}
	}
}
