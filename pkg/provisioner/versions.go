package provisioner

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/go-proxmox"
	ctrl "sigs.k8s.io/controller-runtime"
)

// versionFresh is how long a node's Proxmox VE version is kept. A node changes major version only
// through an upgrade that restarts it, so a minute is soon enough to see one.
const versionFresh = time.Minute

// ProxmoxVersionsAnnotation is on each Cluster while the Proxmox VE nodes run different major
// versions. It lists the nodes of each, in the form ParseVersionNote reads.
const ProxmoxVersionsAnnotation = "kubehenge.dev/proxmox-versions"

// NodeMTUsAnnotation is on each Cluster while its VMs, on different nodes, are set to different
// interface sizes. It lists the smallest MTU of each node's VMs as `node=mtu` pairs.
const NodeMTUsAnnotation = "kubehenge.dev/node-mtus"

// versionCache keeps each node's pve-manager version for versionFresh. The zero value works.
type versionCache struct {
	mu   sync.Mutex
	seen map[string]versionRead
	// now is the clock, time.Now when nil.
	now func() time.Time
}

type versionRead struct {
	version string
	major   int
	at      time.Time
}

// read gives the node's version and major version. A read that fails gives the last one known, and
// zero before there is one: a node whose version is unknown is never left out for it.
func (v *versionCache) read(ctx context.Context, c *proxmox.Client, node string) (string, int) {
	now := time.Now
	if v.now != nil {
		now = v.now
	}
	v.mu.Lock()
	have, ok := v.seen[node]
	v.mu.Unlock()
	if ok && now().Sub(have.at) < versionFresh {
		return have.version, have.major
	}
	var res struct {
		Version string `json:"version"`
	}
	if err := c.Get(ctx, "/nodes/"+url.PathEscape(node)+"/version", &res); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "reading a node's Proxmox VE version", "node", node)
		return have.version, have.major
	}
	got := versionRead{version: res.Version, major: majorOf(res.Version), at: now()}
	v.mu.Lock()
	if v.seen == nil {
		v.seen = map[string]versionRead{}
	}
	v.seen[node] = got
	v.mu.Unlock()
	return got.version, got.major
}

// keep forgets the nodes that are not in names.
func (v *versionCache) keep(names []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for n := range v.seen {
		if !slices.Contains(names, n) {
			delete(v.seen, n)
		}
	}
}

// majorOf is the major version of a pve-manager version such as 9.0.3, and 0 for text that is none.
func majorOf(version string) int {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// oldestMajor splits the nodes into those on the oldest major version and those on a newer one.
// A node whose version is unknown stays with the first: it is judged by nothing it did not say.
func oldestMajor(online []Node) ([]Node, []Node) {
	oldest := 0
	for _, n := range online {
		if n.Major > 0 && (oldest == 0 || n.Major < oldest) {
			oldest = n.Major
		}
	}
	var kept, left []Node
	for _, n := range online {
		if oldest > 0 && n.Major > oldest {
			left = append(left, n)
		} else {
			kept = append(kept, n)
		}
	}
	return kept, left
}

// versionGroups are the nodes of each known major version, by version.
func versionGroups(online []Node) map[int][]string {
	groups := map[int][]string{}
	for _, n := range online {
		if n.Major > 0 {
			groups[n.Major] = append(groups[n.Major], n.Name)
		}
	}
	for _, names := range groups {
		slices.Sort(names)
	}
	return groups
}

// mixedMajors tells whether the nodes run more than one major version.
func mixedMajors(online []Node) bool { return len(versionGroups(online)) > 1 }

// VersionGroup is the nodes that run one Proxmox VE major version.
type VersionGroup struct {
	Major int
	Nodes []string
}

// versionNote is ProxmoxVersionsAnnotation's value: empty while one major version runs.
func versionNote(online []Node) string {
	groups := versionGroups(online)
	if len(groups) < 2 {
		return ""
	}
	var parts []string
	for _, major := range slices.Sorted(mapKeys(groups)) {
		parts = append(parts, fmt.Sprintf("%d=%s", major, strings.Join(groups[major], ",")))
	}
	return strings.Join(parts, ";")
}

func mapKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// ParseVersionNote reads ProxmoxVersionsAnnotation, oldest version first. Text that is none gives nil.
func ParseVersionNote(note string) []VersionGroup {
	var groups []VersionGroup
	for part := range strings.SplitSeq(note, ";") {
		major, nodes, ok := strings.Cut(part, "=")
		n, err := strconv.Atoi(major)
		if !ok || err != nil || nodes == "" {
			continue
		}
		groups = append(groups, VersionGroup{Major: n, Nodes: strings.Split(nodes, ",")})
	}
	slices.SortFunc(groups, func(a, b VersionGroup) int { return a.Major - b.Major })
	return groups
}

// mixedPVE reads each online node's version and tells whether the major versions differ. It is for
// those that have no Nodes to ask.
func mixedPVE(ctx context.Context, c *proxmox.Client, cache *versionCache) (bool, error) {
	var nodes []pveNode
	if err := c.Get(ctx, "/nodes", &nodes); err != nil {
		return false, fmt.Errorf("listing nodes: %w", err)
	}
	var online []Node
	var names []string
	for _, n := range nodes {
		if n.Status != nodeOnline {
			continue
		}
		_, major := cache.read(ctx, c, n.Node)
		online = append(online, Node{Name: n.Node, Major: major})
		names = append(names, n.Node)
	}
	cache.keep(names)
	return mixedMajors(online), nil
}
