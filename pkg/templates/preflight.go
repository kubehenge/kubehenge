package templates

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/luthermonson/go-proxmox"
)

// listedNode is a node as /nodes lists it.
type listedNode struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

// pickNode sets Node to the first of Candidates that is online and whose CPU runs the template's
// node OS, so that a node in maintenance costs no build.
func (b *Builder) pickNode(ctx context.Context, t *Template) error {
	if len(b.Candidates) == 0 {
		return nil
	}
	need := 1
	if t.Bake != nil {
		need = CPULevel(t.Bake.NodeOS)
	}
	var listed []listedNode
	if err := b.Client.Get(ctx, "/nodes", &listed); err != nil {
		return fmt.Errorf("listing the nodes to build on: %w", err)
	}
	var why []string
	for _, name := range b.Candidates {
		i := slices.IndexFunc(listed, func(n listedNode) bool { return n.Node == name })
		if i < 0 || listed[i].Status != "online" {
			why = append(why, name+" is not online")
			continue
		}
		var status nodeStatus
		if err := b.Client.Get(ctx, "/nodes/"+url.PathEscape(name)+"/status", &status); err != nil {
			why = append(why, name+"'s status cannot be read")
			continue
		}
		if have := CPULevelOf(status.CPUInfo.Flags); have < need {
			why = append(why, fmt.Sprintf("%s reaches only x86-64-v%d", name, have))
			continue
		}
		if name != b.Node {
			b.logf("building on node %s", name)
		}
		b.Node = name
		return nil
	}
	return fmt.Errorf("none of the nodes %s can build %s: %s", strings.Join(b.Candidates, ", "), t.LineName, strings.Join(why, ", "))
}

// privAllocateSpace is what writing a file to a storage takes.
const privAllocateSpace = "Datastore.AllocateSpace"

// nodeStorage is a storage as a node lists it.
type nodeStorage struct {
	Storage string `json:"storage"`
	Content string `json:"content"`
	Active  int    `json:"active"`
	Enabled int    `json:"enabled"`
	Shared  int    `json:"shared"`
}

func (s *nodeStorage) takes(content string) bool {
	return slices.Contains(strings.Split(s.Content, ","), content)
}

// StorageNode is one storage of a node as Proxmox VE lists it, for the caller that has to choose
// one.
type StorageNode struct {
	Name    string
	Content []string
	Shared  bool
	Active  bool
}

// NodeStorages lists the storages on a node.
func NodeStorages(ctx context.Context, c *proxmox.Client, node string) ([]StorageNode, error) {
	list, err := listNodeStorages(ctx, c, node)
	if err != nil {
		return nil, err
	}
	out := make([]StorageNode, len(list))
	for i, s := range list {
		out[i] = StorageNode{Name: s.Storage, Content: strings.Split(s.Content, ","), Shared: s.Shared != 0, Active: s.Active != 0 && s.Enabled != 0}
	}
	return out, nil
}

func listNodeStorages(ctx context.Context, c *proxmox.Client, node string) ([]nodeStorage, error) {
	var list []nodeStorage
	if err := c.Get(ctx, "/nodes/"+url.PathEscape(node)+"/storage", &list); err != nil {
		return nil, fmt.Errorf("listing the storages of node %s: %w", node, err)
	}
	return list, nil
}

// holds reports whether the token holds priv on path. A token that cannot read its own privileges
// is taken to hold it: Proxmox VE judges the real call.
func holds(ctx context.Context, c *proxmox.Client, path, priv string) bool {
	var held map[string]map[string]int
	if err := c.Get(ctx, "/access/permissions?"+url.Values{"path": {path}}.Encode(), &held); err != nil {
		return true
	}
	return held[path][priv] != 0
}

// storageContentName is how Proxmox VE's storage settings name a content type.
var storageContentName = map[string]string{"import": "Import", "iso": "ISO image", "images": "Disk image"}

func wrongContent(storage, content string) error {
	return lasting(fmt.Errorf("storage %s does not take %s content. In Proxmox VE open Datacenter, Storage, %s, Edit, Content and add %q, or choose another storage",
		storage, content, storage, storageContentName[content]))
}

// checkBuild runs the pre-flight, and for a bake, which writes a cloud-init ISO, chooses where.
func (b *Builder) checkBuild(ctx context.Context, t *Template, recipe *Recipe) error {
	list, err := b.preflight(ctx, t)
	if err != nil || recipe == nil {
		return err
	}
	b.isoChosen, err = b.chooseISO(ctx, list)
	return err
}

// preflight refuses, before anything is downloaded, a build that Proxmox VE would refuse later
// with its own text: a storage without the content type the build puts there, a token that cannot
// write to it, and tags the datacenter does not let the builder set. It returns the node's storages.
func (b *Builder) preflight(ctx context.Context, t *Template) ([]nodeStorage, error) {
	list, err := listNodeStorages(ctx, b.Client, b.Node)
	if err != nil {
		return nil, err
	}
	find := func(name string) (nodeStorage, bool) {
		i := slices.IndexFunc(list, func(s nodeStorage) bool { return s.Storage == name })
		if i < 0 {
			return nodeStorage{}, false
		}
		return list[i], true
	}
	for _, check := range []struct{ name, content string }{{b.ImportStorage, "import"}, {b.Storage, "images"}} {
		s, ok := find(check.name)
		switch {
		case !ok:
			return nil, lasting(fmt.Errorf("storage %s is not on node %s or not visible to the builder's token", check.name, b.Node))
		case !s.takes(check.content):
			return nil, wrongContent(check.name, check.content)
		}
	}
	if !holds(ctx, b.Client, "/storage/"+b.ImportStorage, privAllocateSpace) {
		return nil, lasting(fmt.Errorf("the builder's token lacks %s on /storage/%s, where the image is downloaded", privAllocateSpace, b.ImportStorage))
	}
	return list, CheckTags(ctx, b.Client, append(slices.Clone(t.Tags), kubehengeTag, buildingTag))
}

// chooseISO is the storage for a bake's cloud-init ISO: the one asked for, which has to take ISO
// images and be writable, else the first that does.
func (b *Builder) chooseISO(ctx context.Context, list []nodeStorage) (string, error) {
	if b.ISOStorage != "" {
		i := slices.IndexFunc(list, func(s nodeStorage) bool { return s.Storage == b.ISOStorage })
		switch {
		case i < 0:
			return "", lasting(fmt.Errorf("ISO storage %s is not on node %s or not visible to the builder's token", b.ISOStorage, b.Node))
		case !list[i].takes("iso"):
			return "", wrongContent(b.ISOStorage, "iso")
		case !holds(ctx, b.Client, "/storage/"+b.ISOStorage, privAllocateSpace):
			return "", lasting(fmt.Errorf("the builder's token lacks %s on /storage/%s, where the cloud-init ISO is written", privAllocateSpace, b.ISOStorage))
		}
		return b.ISOStorage, nil
	}
	var readOnly []string
	for _, s := range list {
		if !s.takes("iso") || s.Active == 0 || s.Enabled == 0 {
			continue
		}
		if holds(ctx, b.Client, "/storage/"+s.Storage, privAllocateSpace) {
			return s.Storage, nil
		}
		readOnly = append(readOnly, s.Storage)
	}
	if len(readOnly) > 0 {
		return "", lasting(fmt.Errorf("the builder's token lacks %s on every storage of node %s that takes ISO images (%s). Grant it on one, or pass --iso-storage",
			privAllocateSpace, b.Node, strings.Join(readOnly, ", ")))
	}
	return "", lasting(fmt.Errorf("no storage on node %s takes ISO images. In Proxmox VE open Datacenter, Storage, a file storage, Edit, Content and add %q",
		b.Node, storageContentName["iso"]))
}

// datacenterTags are the datacenter options that decide which tags a user may set.
type datacenterTags struct {
	Access     string `json:"user-tag-access"`
	Registered string `json:"registered-tags"`
}

// TagsRefusal says why the datacenter's tag options would refuse tags to a user without Sys.Modify
// on /, or returns "". access and registered are the user-tag-access and registered-tags options,
// and visible the tags already on guests the token can audit. Tags match exactly, as Proxmox VE's
// own check does, and only Sys.Modify on / bypasses it.
func TagsRefusal(access, registered string, tags, visible []string) string {
	var blocked []string
	for _, r := range strings.FieldsFunc(registered, isTagSeparator) {
		if slices.Contains(tags, r) {
			blocked = append(blocked, r)
		}
	}
	if len(blocked) > 0 {
		return fmt.Sprintf("the datacenter option registered-tags lists %s, which only Sys.Modify on / may set. "+
			"Remove them from Datacenter, Options, Registered Tags, or let the token hold Sys.Modify", strings.Join(blocked, ", "))
	}
	allow, list := "free", ""
	for part := range strings.SplitSeq(access, ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch key {
		case "user-allow":
			allow = value
		case "user-allow-list":
			list = value
		}
	}
	switch allow {
	case "none":
		return "the datacenter option user-tag-access is none, so users cannot set tags. " +
			"Choose list in Datacenter, Options, User Tag Access and add " + strings.Join(tags, ", ")
	case "list", "existing":
		allowed := strings.FieldsFunc(list, isTagSeparator)
		if allow == "existing" {
			allowed = append(allowed, visible...)
		}
		var missing []string
		for _, tag := range tags {
			if !slices.Contains(allowed, tag) {
				missing = append(missing, tag)
			}
		}
		if len(missing) > 0 {
			return "the datacenter option user-tag-access refuses " + strings.Join(missing, ", ") +
				". Add them to its list in Datacenter, Options, User Tag Access"
		}
	}
	return ""
}

// visibleTags are the tags on the guests the token can audit.
func visibleTags(ctx context.Context, c *proxmox.Client) []string {
	var guests []struct {
		Tags string `json:"tags"`
	}
	if err := c.Get(ctx, "/cluster/resources?type=vm", &guests); err != nil {
		return nil
	}
	var out []string
	for _, g := range guests {
		out = append(out, strings.FieldsFunc(g.Tags, isTagSeparator)...)
	}
	return out
}

func isTagSeparator(r rune) bool { return r == ';' || r == ',' || r == ' ' }

// CheckTags refuses tags that the datacenter's tag options would keep a user without Sys.Modify
// on / from setting. A token that cannot read the options, or holds Sys.Modify, is not judged.
func CheckTags(ctx context.Context, c *proxmox.Client, tags []string) error {
	var options datacenterTags
	if err := c.Get(ctx, "/cluster/options", &options); err != nil {
		return nil //nolint:nilerr // a token that cannot read the options cannot be judged by them
	}
	var visible []string
	if strings.Contains(options.Access, "user-allow=existing") {
		visible = visibleTags(ctx, c)
	}
	why := TagsRefusal(options.Access, options.Registered, tags, visible)
	if why == "" || holds(ctx, c, "/", "Sys.Modify") {
		return nil
	}
	return lasting(errors.New(why))
}
