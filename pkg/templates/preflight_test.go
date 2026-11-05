package templates

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// preflightPVE serves a node's storages, the datacenter options and the token's privileges.
type preflightPVE struct {
	storages []map[string]any
	options  map[string]any
	// guests are the VMs the token can audit.
	guests []map[string]any
	// granted maps a path to the privileges the token holds there.
	granted map[string][]string
}

func (p *preflightPVE) client(t *testing.T) *proxmox.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch strings.TrimPrefix(r.URL.Path, "/api2/json") {
		case "/nodes/pve1/storage":
			data = p.storages
		case "/cluster/options":
			data = p.options
		case "/cluster/resources":
			data = p.guests
		case "/access/permissions":
			path := r.URL.Query().Get("path")
			held := map[string]int{}
			for _, priv := range p.granted[path] {
				held[priv] = 1
			}
			data = map[string]any{path: held}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return proxmox.NewClient(srv.URL + "/api2/json")
}

// preflightWithISO runs the checks of a bake: the pre-flight, then the choice of ISO storage.
func preflightWithISO(t *testing.T, b *Builder, tmpl *Template) (string, error) {
	t.Helper()
	list, err := b.preflight(t.Context(), tmpl)
	if err != nil {
		return "", err
	}
	return b.chooseISO(t.Context(), list)
}

func storageOf(name, content string) map[string]any {
	return map[string]any{"storage": name, "content": content, "enabled": 1, "active": 1}
}

func TestPreflightRefusesStoragesTheBuildCannotUseBeforeDownloading(t *testing.T) {
	t.Parallel()
	writable := map[string][]string{"/storage/local": {privAllocateSpace}, "/storage/isos": {privAllocateSpace}}
	tmpl, err := Plan(&Request{Distribution: RKE2})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		storages []map[string]any
		granted  map[string][]string
		builder  Builder
		want     string
	}{
		{
			"an import storage without the import content",
			[]map[string]any{storageOf("local", "iso,images"), storageOf("local-lvm", "images")},
			writable,
			Builder{},
			`storage local does not take import content. In Proxmox VE open Datacenter, Storage, local, Edit, Content and add "Import"`,
		},
		{
			"a disk storage without images",
			[]map[string]any{storageOf("local", "import,iso"), storageOf("local-lvm", "rootdir")},
			writable,
			Builder{},
			"storage local-lvm does not take images content",
		},
		{
			"an import storage the token cannot write",
			[]map[string]any{storageOf("local", "import,iso"), storageOf("local-lvm", "images")},
			nil,
			Builder{},
			"lacks Datastore.AllocateSpace on /storage/local",
		},
		{
			"no storage for ISO images",
			[]map[string]any{storageOf("local", "import"), storageOf("local-lvm", "images")},
			writable,
			Builder{},
			`no storage on node pve1 takes ISO images. In Proxmox VE open Datacenter, Storage, a file storage, Edit, Content and add "ISO image"`,
		},
		{
			"an ISO storage named that does not take ISO images",
			[]map[string]any{storageOf("local", "import"), storageOf("local-lvm", "images")},
			writable,
			Builder{ISOStorage: "local-lvm"},
			"storage local-lvm does not take iso content",
		},
		{
			"read-only ISO storages only",
			[]map[string]any{storageOf("local", "import"), storageOf("nfs", "iso"), storageOf("local-lvm", "images")},
			writable,
			Builder{},
			"lacks Datastore.AllocateSpace on every storage of node pve1 that takes ISO images (nfs)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pve := &preflightPVE{storages: tc.storages, options: map[string]any{}, granted: tc.granted}
			b := tc.builder
			b.Client, b.Node, b.ImportStorage, b.Storage = pve.client(t), "pve1", "local", "local-lvm"
			_, err := preflightWithISO(t, &b, &tmpl)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !Final(err) {
				t.Errorf("preflight = %v, want a final error with %q", err, tc.want)
			}
		})
	}
}

func TestPreflightChoosesTheISOStorageTheTokenCanWrite(t *testing.T) {
	t.Parallel()
	pve := &preflightPVE{
		storages: []map[string]any{storageOf("local", "import"), storageOf("nfs", "iso"), storageOf("isos", "iso"), storageOf("local-lvm", "images")},
		options:  map[string]any{},
		granted:  map[string][]string{"/storage/local": {privAllocateSpace}, "/storage/isos": {privAllocateSpace}},
	}
	b := &Builder{Client: pve.client(t), Node: "pve1", ImportStorage: "local", Storage: "local-lvm"}
	tmpl, err := Plan(&Request{Distribution: RKE2})
	if err != nil {
		t.Fatal(err)
	}
	iso, err := preflightWithISO(t, b, &tmpl)
	if err != nil || iso != "isos" {
		t.Errorf("preflight = %q, %v; want isos, the first the token can write", iso, err)
	}
}

func TestTagsRefusal(t *testing.T) {
	t.Parallel()
	tags := []string{"kubehenge", "volumes"}
	for _, tc := range []struct {
		name, access, registered, want string
		visible                        []string
	}{
		{"the default", "", "", "", nil},
		{"free", "user-allow=free", "", "", nil},
		{"none", "user-allow=none", "", "user-tag-access is none", nil},
		{"a list without the tags", "user-allow=list,user-allow-list=prod", "", "add kubehenge, volumes", nil},
		{"a list with them", "user-allow=list,user-allow-list=prod;kubehenge;volumes", "", "", nil},
		{"a list matches case exactly", "user-allow=list,user-allow-list=Kubehenge;volumes", "", "add kubehenge to the list", nil},
		{"registered", "", "prod;volumes", "registered-tags lists volumes", nil},
		{"registered matches case exactly", "", "Kubehenge;prod", "", nil},
		{"registered without them", "", "prod", "", nil},
		{"existing: on a visible guest", "user-allow=existing", "", "", []string{"kubehenge", "volumes"}},
		{"existing: in the list", "user-allow=existing,user-allow-list=volumes", "", "", []string{"kubehenge"}},
		{"existing: in neither", "user-allow=existing,user-allow-list=prod", "", "add volumes to the list", []string{"kubehenge"}},
	} {
		got := TagsRefusal(tc.access, tc.registered, tags, tc.visible)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: TagsRefusal = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCheckTagsNamesTheOptionUnlessTheTokenMaySetAnyTag(t *testing.T) {
	t.Parallel()
	tags := []string{"kubehenge", "kubehenge-building", "rke2"}
	pve := &preflightPVE{options: map[string]any{"user-tag-access": "user-allow=none"}}
	err := CheckTags(t.Context(), pve.client(t), tags)
	if err == nil || !strings.Contains(err.Error(), "user-tag-access") || !Final(err) {
		t.Errorf("CheckTags = %v, want a final error naming user-tag-access", err)
	}
	pve.granted = map[string][]string{"/": {"Sys.Modify"}}
	if err = CheckTags(t.Context(), pve.client(t), tags); err != nil {
		t.Errorf("CheckTags = %v for a token with Sys.Modify", err)
	}
	// VM.Allocate does not bypass the option, only Sys.Modify on / does.
	pve.granted = map[string][]string{"/pool/kubehenge-templates": {"VM.Allocate"}}
	if err = CheckTags(t.Context(), pve.client(t), tags); err == nil {
		t.Error("CheckTags let a token without Sys.Modify through")
	}
	// Under existing, the tags on guests the token can audit are allowed.
	pve = &preflightPVE{
		options: map[string]any{"user-tag-access": "user-allow=existing"},
		guests:  []map[string]any{{"vmid": 1, "tags": "kubehenge;kubehenge-building;rke2"}},
	}
	if err = CheckTags(t.Context(), pve.client(t), tags); err != nil {
		t.Errorf("CheckTags = %v with every tag on a visible guest", err)
	}
	pve.guests = nil
	if err = CheckTags(t.Context(), pve.client(t), tags); err == nil || !strings.Contains(err.Error(), "already on a guest") {
		t.Errorf("CheckTags = %v with the tags on no guest", err)
	}
}

func TestPickNodeTakesTheFirstOnlineNodeThatRunsTheNodeOS(t *testing.T) {
	t.Parallel()
	tmpl, err := Plan(&Request{Distribution: RKE2, NodeOS: NodeOS{Family: "rocky", Release: "10"}})
	if err != nil {
		t.Fatal(err)
	}
	v3 := strings.Join(slices.Concat(cpuLevelFlags[2], cpuLevelFlags[3]), " ")
	flags := map[string]string{"pve1": "", "pve2": v3, "pve3": v3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		var data any
		switch {
		case path == "/nodes":
			data = []map[string]any{{"node": "pve1", "status": "online"}, {"node": "pve2", "status": "offline"}, {"node": "pve3", "status": "online"}}
		case strings.HasSuffix(path, "/status"):
			data = map[string]any{"cpuinfo": map[string]any{"flags": flags[strings.Split(path, "/")[2]]}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	b := &Builder{Client: proxmox.NewClient(srv.URL + "/api2/json"), Candidates: []string{"pve1", "pve2", "pve3"}, Node: "pve1"}
	if err = b.pickNode(t.Context(), &tmpl); err != nil || b.Node != "pve3" {
		t.Errorf("pickNode = %v with node %q; want pve3, the first that is online and reaches the level", err, b.Node)
	}
	b.Candidates = []string{"pve1", "pve2"}
	if err = b.pickNode(t.Context(), &tmpl); err == nil || !strings.Contains(err.Error(), "pve2 is not online") {
		t.Errorf("pickNode = %v, want why no candidate builds", err)
	}
}
