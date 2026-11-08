package templates

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"

	pveapi "github.com/kubehenge/kubehenge/pkg/pve"
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
			// Each privilege maps to whether it propagates. 0, as for the storage grants of the
			// module, which do not propagate, is held all the same.
			held := map[string]int{}
			for _, priv := range p.granted[path] {
				held[priv] = 0
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
	writable := map[string][]string{"/storage/local": {privAllocateTemplate}, "/storage/isos": isoPrivileges}
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
			"lacks Datastore.AllocateTemplate on /storage/local",
		},
		{
			// AllocateSpace lets a token import from the storage, not download to it.
			"an import storage the token can only allocate disks on",
			[]map[string]any{storageOf("local", "import,iso"), storageOf("local-lvm", "images")},
			map[string][]string{"/storage/local": {"Datastore.AllocateSpace", "Datastore.Audit"}},
			Builder{},
			"lacks Datastore.AllocateTemplate on /storage/local",
		},
		{
			"an ISO storage named that the ISO can be written to but not removed from",
			[]map[string]any{storageOf("local", "import"), storageOf("isos", "iso"), storageOf("local-lvm", "images")},
			map[string][]string{"/storage/local": {privAllocateTemplate}, "/storage/isos": {privAllocateTemplate}},
			Builder{ISOStorage: "isos"},
			"needs Datastore.AllocateTemplate and Datastore.Allocate on /storage/isos",
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
			"needs Datastore.AllocateTemplate and Datastore.Allocate on a storage of node pve1 that takes ISO images (nfs)",
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
		granted:  map[string][]string{"/storage/local": {privAllocateTemplate}, "/storage/isos": isoPrivileges},
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

// pveOptions are datacenter options as /cluster/options answers them.
func pveOptions(t *testing.T, answer string) map[string]any {
	t.Helper()
	var options map[string]any
	if err := json.Unmarshal([]byte(answer), &options); err != nil {
		t.Fatal(err)
	}
	return options
}

func TestTagOptionsRefusal(t *testing.T) {
	t.Parallel()
	tags := []string{"kubehenge", "volumes"}
	for _, tc := range []struct {
		name, options, want string
		visible             []string
	}{
		{"the default", `{}`, "", nil},
		{"free", `{"user-tag-access":{"user-allow":"free"}}`, "", nil},
		{"none", `{"user-tag-access":{"user-allow":"none"}}`, "user-tag-access is none", nil},
		{"a list without the tags", `{"user-tag-access":{"user-allow":"list","user-allow-list":["prod"]}}`, "refuses kubehenge, volumes.", nil},
		{"a list with them", `{"user-tag-access":{"user-allow":"list","user-allow-list":["prod","kubehenge","volumes"]}}`, "", nil},
		{"a list matches case exactly", `{"user-tag-access":{"user-allow":"list","user-allow-list":["Kubehenge","volumes"]}}`, "refuses kubehenge.", nil},
		{"registered", `{"registered-tags":["prod","volumes"]}`, "registered-tags lists volumes", nil},
		{"registered matches case exactly", `{"registered-tags":["Kubehenge","prod"]}`, "", nil},
		{"registered without them", `{"registered-tags":["prod"]}`, "", nil},
		{"existing: on a visible guest", `{"user-tag-access":{"user-allow":"existing"}}`, "", []string{"kubehenge", "volumes"}},
		{"existing: in the list", `{"user-tag-access":{"user-allow":"existing","user-allow-list":["volumes"]}}`, "", []string{"kubehenge"}},
		{"existing: in neither", `{"user-tag-access":{"user-allow":"existing","user-allow-list":["prod"]}}`, "refuses volumes.", []string{"kubehenge"}},
	} {
		var o TagOptions
		if err := json.Unmarshal([]byte(tc.options), &o); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := o.Refusal(tags, tc.visible)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: Refusal = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCheckTagsReadsTheOptionsAsProxmoxVEAnswers feeds the answers of a Proxmox VE 9 node, as a
// token without Sys.Modify got them, beside whether that token could then tag a guest.
func TestCheckTagsReadsTheOptionsAsProxmoxVEAnswers(t *testing.T) {
	t.Parallel()
	tags := []string{"kubehenge", "building"}
	const common = `"allowed-tags":["cloud-image","kubeadm","kubehenge"],"description":"","mac_prefix":"BC:24:11"`
	for _, tc := range []struct {
		name, options string
		tagged        bool
	}{
		{"free", `{` + common + `}`, true},
		{"existing", `{` + common + `,"user-tag-access":{"user-allow":"existing"}}`, false},
		{"list without them", `{` + common + `,"user-tag-access":{"user-allow":"list","user-allow-list":["other"]}}`, false},
		{"list with both", `{` + common + `,"user-tag-access":{"user-allow":"list","user-allow-list":["kubehenge","building"]}}`, true},
		{"none", `{` + common + `,"user-tag-access":{"user-allow":"none"}}`, false},
		{"registered", `{` + common + `,"registered-tags":["kubehenge"]}`, false},
	} {
		pve := &preflightPVE{options: pveOptions(t, tc.options), guests: []map[string]any{{"vmid": 100, "tags": "cloud-image;kubeadm;kubehenge"}}}
		err := CheckTags(t.Context(), pve.client(t), tags)
		if tc.tagged != (err == nil) {
			t.Errorf("%s: CheckTags = %v, while Proxmox VE tagged the guest: %t", tc.name, err, tc.tagged)
		}
	}
}

func TestCheckTagsNamesTheOptionUnlessTheTokenMaySetAnyTag(t *testing.T) {
	t.Parallel()
	tags := []string{"kubehenge", "kubehenge-building", "rke2"}
	pve := &preflightPVE{options: pveOptions(t, `{"user-tag-access":{"user-allow":"none"}}`)}
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
		options: pveOptions(t, `{"user-tag-access":{"user-allow":"existing"}}`),
		guests:  []map[string]any{{"vmid": 1, "tags": "kubehenge;kubehenge-building;rke2"}},
	}
	if err = CheckTags(t.Context(), pve.client(t), tags); err != nil {
		t.Errorf("CheckTags = %v with every tag on a visible guest", err)
	}
	pve.guests = nil
	if err = CheckTags(t.Context(), pve.client(t), tags); err == nil || !strings.Contains(err.Error(), "refuses kubehenge, kubehenge-building, rke2.") {
		t.Errorf("CheckTags = %v with the tags on no guest", err)
	}
}

func TestPickNodeSkipsAnOnlineNodeThatAnswers596AndSaysWhy(t *testing.T) {
	t.Parallel()
	tmpl, err := Plan(&Request{Distribution: RKE2, NodeOS: DefaultNodeOS})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch path {
		case "/nodes":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"node": "pve1", "status": "online"}, {"node": "pve2", "status": "online"}}})
		case "/nodes/pve1/status":
			conn, _, hijackErr := http.NewResponseController(w).Hijack()
			if hijackErr != nil {
				t.Error(hijackErr)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.WriteString(conn, "HTTP/1.1 596 certificate verify failed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"cpuinfo": map[string]any{"flags": strings.Join(slices.Concat(cpuLevelFlags[2], cpuLevelFlags[3]), " ")}}})
		}
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: pveapi.CheckStatus(http.DefaultTransport)}
	b := &Builder{Client: proxmox.NewClient(srv.URL+"/api2/json", proxmox.WithHTTPClient(client)), Candidates: []string{"pve1", "pve2"}, Node: "pve1"}
	if err = b.pickNode(t.Context(), &tmpl); err != nil || b.Node != "pve2" {
		t.Fatalf("pickNode = %v with node %q; want pve2, since pve1 does not answer", err, b.Node)
	}
	b.Candidates = []string{"pve1"}
	if err = b.pickNode(t.Context(), &tmpl); err == nil || !strings.Contains(err.Error(), "pve1 is not reachable through the cluster (HTTP 596: certificate verify failed)") {
		t.Errorf("pickNode = %v, want pve1's 596 and its reason", err)
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
