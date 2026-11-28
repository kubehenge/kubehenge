package pve

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// MinAccessControl is the first libpve-access-control that stops a user with User.Modify on a
// group from making a token without privilege separation for the group's members. Older ones let
// the upgrader act as any user in its group.
const MinAccessControl = "9.2.1"

// ErrAccessControlOld is a node whose libpve-access-control is older than MinAccessControl, or
// whose version cannot be read.
var ErrAccessControlOld = errors.New("libpve-access-control is too old")

// CheckAccessControl reads libpve-access-control on every node with the user's own rights. It
// returns ErrAccessControlOld, saying which node and what to update to, unless all have
// MinAccessControl or newer. A version that cannot be read counts as too old.
func (a *Accounts) CheckAccessControl(ctx context.Context, t *Ticket) error {
	var nodes []struct {
		Node string `json:"node"`
	}
	if err := a.get(ctx, t, "/cluster/resources?type=node", &nodes); err != nil {
		return fmt.Errorf("listing the nodes: %w", err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("%w: Proxmox VE lists no node", ErrAccessControlOld)
	}
	for _, n := range nodes {
		have, err := a.accessControlOn(ctx, t, n.Node)
		switch {
		case err != nil:
			return fmt.Errorf("%w: its version on %s is unreadable (%w). Update it to %s or newer", ErrAccessControlOld, n.Node, err, MinAccessControl)
		case versionLess(have, MinAccessControl):
			return fmt.Errorf("%w: %s has %s. Update it to %s or newer", ErrAccessControlOld, n.Node, have, MinAccessControl)
		}
	}
	return nil
}

// accessControlOn is the installed version of libpve-access-control on a node.
func (a *Accounts) accessControlOn(ctx context.Context, t *Ticket, node string) (string, error) {
	var pkgs []struct {
		Package    string `json:"Package"`
		OldVersion string `json:"OldVersion"`
	}
	if err := a.get(ctx, t, "/nodes/"+url.PathEscape(node)+"/apt/versions", &pkgs); err != nil {
		return "", err
	}
	for _, p := range pkgs {
		if p.Package == "libpve-access-control" && p.OldVersion != "" {
			return p.OldVersion, nil
		}
	}
	return "", errors.New("the package is not listed")
}

// versionLess tells whether Debian version a is older than b, by their dotted numbers alone. A
// version that does not start with numbers is older than all.
func versionLess(a, b string) bool {
	x, y := versionNumbers(a), versionNumbers(b)
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p < q
		}
	}
	return false
}

func versionNumbers(v string) []int {
	if _, rest, ok := strings.Cut(v, ":"); ok {
		v = rest
	}
	end := strings.IndexFunc(v, func(r rune) bool { return r != '.' && (r < '0' || r > '9') })
	if end >= 0 {
		v = v[:end]
	}
	var out []int
	for part := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}
