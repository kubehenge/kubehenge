package provisioner

import (
	"cmp"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

var updateAddonTables = flag.Bool("update-addon-tables", false, "write the add-on tables of docs/clusters.md from the code")

const (
	docsFolder     = "../../docs"
	clustersDoc    = "clusters.md"
	chartValues    = "../../charts/kubehenge/values.yaml"
	footprintStart = "<!-- addon-footprint:start -->"
	footprintEnd   = "<!-- addon-footprint:end -->"
	ownedStart     = "<!-- addon-owned:start -->"
	ownedEnd       = "<!-- addon-owned:end -->"
)

// footprint is what an add-on asks of a company's admission policy. The namespaces, the exempt ones
// and the registries come from the code and the chart. The privileges are what the add-on's pods
// do on the host, which no data of the code says.
type footprint struct {
	name string
	// on switches the add-on on in a cluster of the base, and cni is the CNI it adds.
	on  func(*Addons)
	cni string
	// after names the add-on whose proxies this one's include, which its row leaves out.
	after string
	// exempt names the field of PodSecurityExempt that holds the add-on's namespaces the chart
	// creates itself.
	exempt string
	// charts are the keys of the chart's addons values whose images the add-on pulls.
	charts     []string
	privileges string
}

var footprints = []footprint{
	{
		name: "Every cluster", exempt: "always",
		charts:     []string{"cloudControllerManager", "csiPlugin", "csiPluginNewest", "cilium", "traefik", "kubeVip", "snapshotController"},
		privileges: "Cilium agents are privileged on the host network. kube-vip uses the host network and `NET_ADMIN`. Traefik binds host ports 80 and 443 on kubeadm and Talos.",
	},
	{
		name: "Calico", cni: cniCalico, exempt: "calico", charts: []string{"calico"},
		privileges: "`calico-node` is privileged on the host network and mounts host paths.",
	},
	{name: "Headlamp", on: func(a *Addons) { a.Headlamp.Enabled = true }, charts: []string{"headlamp"}},
	{
		name: "cert-manager", on: func(a *Addons) { a.Certificates.Enabled = true }, charts: []string{"certManager"},
	},
	{
		name: "Monitoring", after: "cert-manager", on: func(a *Addons) { a.Monitoring.Enabled = true }, exempt: "monitoring", charts: []string{"monitoring"},
		privileges: "`node-exporter` uses the host network and PID namespace and mounts host paths.",
	},
	{
		name: "Logs", after: "Monitoring", on: func(a *Addons) { a.Monitoring.Enabled, a.Logs.Enabled = true, true }, exempt: "logs", charts: []string{"loki", "alloy"},
		privileges: "Alloy's node collectors run as root and mount host paths.",
	},
	{
		name: "Backups", on: func(a *Addons) { a.Backups.Enabled, a.backupTarget = true, &BackupTarget{Name: "t", Bucket: "b"} }, exempt: "backups", charts: []string{"velero"},
		privileges: "Velero's node agents mount host paths, and its backup pods run privileged.",
	},
	{name: "Kyverno", on: func(a *Addons) { a.PodSecurity.Kyverno = "baseline" }, charts: []string{"kyverno", "kyvernoPolicies"}},
	{
		name: "Istio", on: func(a *Addons) { a.Istio.Enabled = true }, exempt: "istio", charts: []string{"istio", "istioBase", "istioCNI", "istioZtunnel"},
		privileges: "`ztunnel` and the CNI agent are privileged on the host network and mount host paths.",
	},
	{
		name: "MetalLB", on: func(a *Addons) { a.LoadBalancer.Enabled = true }, exempt: "metallb", charts: []string{"metallb"},
		privileges: "The speakers use the host network and `NET_RAW`.",
	},
	{
		name: "Rancher", after: "cert-manager", on: func(a *Addons) { a.Rancher = Rancher{Install: true, Hostname: "rancher.example"} }, exempt: "rancher", charts: []string{"rancher"},
		privileges: "Rancher's agents and controllers run privileged.",
	},
	{
		name: "Argo CD", on: func(a *Addons) { a.GitOps = GitOps{Enabled: true, Engine: "argocd"} }, charts: []string{"argocd"},
	},
}

// exemptOf are the namespaces the chart's podSecurityExempt lists under the field named.
func exemptOf(e *PodSecurityExempt, field string) []string {
	return slices.Compact(slices.Sorted(slices.Values(map[string][]string{
		"always": e.Always, "calico": e.Calico, "monitoring": e.Monitoring, "logs": e.Logs, "backups": e.Backups,
		"istio": e.Istio, "metallb": e.MetalLB, "rancher": slices.Concat(e.Rancher, e.RancherAgent),
	}[field])))
}

type footprintRow struct {
	footprint

	namespaces, exempt, registries []string
	releases                       []releaseRow
}

type releaseRow struct{ addon, release, namespace string }

func footprintCluster(t *testing.T) (*PodSecurityExempt, map[string][]string) {
	t.Helper()
	raw, err := os.ReadFile(chartValues)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		PodSecurityExempt PodSecurityExempt `json:"podSecurityExempt"`
	}
	if err = yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	return &values.PodSecurityExempt, registriesOf(string(raw))
}

var (
	chartKey = regexp.MustCompile(`^ {2}([A-Za-z]+):\s*$`)
	depName  = regexp.MustCompile(`depName=([^/\s]+)/`)
)

// registriesOf are the registries of the images each chart of the values' addons section pulls, by
// the chart's key. Renovate's comment beside each image names its registry.
func registriesOf(values string) map[string][]string {
	out := map[string][]string{}
	in, key := false, ""
	for line := range strings.SplitSeq(values, "\n") {
		switch {
		case line == "addons:":
			in = true
		case in && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#"):
			in = false
		case in:
			if m := chartKey.FindStringSubmatch(line); len(m) > 1 {
				key = m[1]
			}
			if m := depName.FindStringSubmatch(line); len(m) > 1 && !slices.Contains(out[key], m[1]) {
				out[key] = append(out[key], m[1])
			}
		}
	}
	return out
}

// footprintRows works out every row from the code: the proxies a cluster gets with the add-on on
// beyond those it has without, with their namespaces and releases.
func footprintRows(t *testing.T) []footprintRow {
	t.Helper()
	exempt, registries := footprintCluster(t)
	c := cluster()
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.50.0.0/16"}
	proxiesOf := func(a *Addons, cni CNI) map[string]releaseRow {
		proxies, err := helmChartProxies(c, &testCharts, "kubeadm", "local-lvm", cni, a)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]releaseRow{}
		for _, p := range proxies {
			out[p.GetName()] = releaseRow{
				addon: strings.TrimPrefix(p.GetName(), c.Name+"-"), release: specText(p.Object["spec"], "releaseName"), namespace: specText(p.Object["spec"], "namespace"),
			}
		}
		return out
	}
	// proxiesWith are the proxies of a cluster with just the add-on of f on.
	proxiesWith := func(f *footprint) map[string]releaseRow {
		a := &Addons{}
		if f.on != nil {
			f.on(a)
		}
		return proxiesOf(a, CNI{Name: cmp.Or(f.cni, cniCilium)})
	}
	base := proxiesOf(&Addons{}, CNI{Name: cniCilium})
	rows := make([]footprintRow, 0, len(footprints))
	for i := range footprints {
		f := &footprints[i]
		row := footprintRow{footprint: *f}
		// An add-on's row lists what it adds to the cluster, or to the add-on it needs.
		var without map[string]releaseRow
		switch {
		case f.after != "":
			without = proxiesWith(&footprints[slices.IndexFunc(footprints, func(o footprint) bool { return o.name == f.after })])
		case f.name != "Every cluster":
			without = base
		}
		for name, r := range proxiesWith(f) {
			if was, had := without[name]; had && was.release == r.release {
				continue
			}
			row.releases = append(row.releases, r)
			if !slices.Contains(row.namespaces, r.namespace) {
				row.namespaces = append(row.namespaces, r.namespace)
			}
		}
		for _, ns := range exemptOf(exempt, f.exempt) {
			if !slices.Contains(row.namespaces, ns) {
				row.namespaces = append(row.namespaces, ns)
			}
			row.exempt = append(row.exempt, ns)
		}
		for _, k := range f.charts {
			for _, reg := range registries[k] {
				if !slices.Contains(row.registries, reg) {
					row.registries = append(row.registries, reg)
				}
			}
		}
		slices.Sort(row.namespaces)
		slices.Sort(row.registries)
		slices.SortFunc(row.releases, func(a, b releaseRow) int { return strings.Compare(a.addon, b.addon) })
		rows = append(rows, row)
	}
	return rows
}

func codeList(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "`" + s + "`"
	}
	return strings.Join(quoted, ", ")
}

// footprintTable is the table of what each add-on asks of an admission policy.
func footprintTable(rows []footprintRow) string {
	var b strings.Builder
	b.WriteString("| Add-on | Namespaces | Pod Security | Privileges | Registries |\n| --- | --- | --- | --- | --- |\n")
	for i := range rows {
		r := &rows[i]
		level := "the cluster's level"
		switch {
		case len(r.exempt) == len(r.namespaces):
			level = "exempt"
		case len(r.exempt) > 0:
			level = "exempt: " + codeList(slices.Sorted(slices.Values(r.exempt))) + ". The others follow the cluster's level"
		}
		privileges := r.privileges
		if privileges == "" {
			privileges = "none"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.name, codeList(r.namespaces), level, privileges, codeList(r.registries))
	}
	return b.String()
}

// ownedTable is the table of the releases, namespaces and custom resource definitions Kubehenge's
// add-ons own, which a cluster's own install of the same software collides with.
func ownedTable(rows []footprintRow) string {
	owned := AddonCRDs()
	var b strings.Builder
	b.WriteString("| Add-on | Release | Namespace | Custom resource definitions checked before it installs |\n| --- | --- | --- | --- |\n")
	for i := range rows {
		for _, r := range rows[i].releases {
			crds := owned[r.addon]
			if r.addon == monitoringAddon {
				crds = owned[monitoringAddon]
			}
			fmt.Fprintf(&b, "| %s | `%s` | `%s` | %s |\n", r.addon, r.release, r.namespace, codeList(crds))
		}
	}
	return b.String()
}

// replaceBetween returns doc with what lies between the markers replaced by body.
func replaceBetween(doc, start, end, body string) (string, bool) {
	from, to := strings.Index(doc, start), strings.Index(doc, end)
	if from < 0 || to < from {
		return doc, false
	}
	return doc[:from+len(start)] + "\n\n" + body + "\n" + doc[to:], true
}

func TestAddonTablesMatchTheCode(t *testing.T) {
	t.Parallel()
	rows := footprintRows(t)
	docs, err := os.OpenRoot(docsFolder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docs.Close() })
	doc, err := docs.ReadFile(clustersDoc)
	if err != nil {
		t.Fatal(err)
	}
	want := string(doc)
	for _, table := range []struct{ start, end, body string }{
		{footprintStart, footprintEnd, footprintTable(rows)},
		{ownedStart, ownedEnd, ownedTable(rows)},
	} {
		var found bool
		if want, found = replaceBetween(want, table.start, table.end, table.body); !found {
			t.Fatalf("docs/clusters.md lacks %s and %s", table.start, table.end)
		}
	}
	if *updateAddonTables {
		if err = docs.WriteFile(clustersDoc, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want != string(doc) {
		t.Fatal("the add-on tables of docs/clusters.md differ from the code. Write them again: go test ./pkg/provisioner -run TestAddonTablesMatchTheCode -update-addon-tables")
	}
}

func TestAddonTablesNameEveryNamespaceAClusterGets(t *testing.T) {
	t.Parallel()
	rows := footprintRows(t)
	exempt, registries := footprintCluster(t)
	listed := map[string]bool{}
	for i := range rows {
		for _, ns := range rows[i].namespaces {
			listed[ns] = true
		}
	}
	for _, ns := range exempt.all() {
		if !listed[ns] {
			t.Errorf("the exempt namespace %s is in no row of the add-on table", ns)
		}
	}
	// Everything switched on at once: no proxy lands in a namespace the table does not list.
	everything := &Addons{}
	for _, f := range footprints {
		if f.on != nil {
			f.on(everything)
		}
	}
	c := cluster()
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.50.0.0/16"}
	proxies, err := helmChartProxies(c, &testCharts, "kubeadm", "local-lvm", CNI{Name: cniCilium}, everything)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range proxyNamespaces(proxies) {
		if !listed[ns] {
			t.Errorf("an add-on installs into %s, which the add-on table does not list", ns)
		}
	}
	for _, f := range footprints {
		known := false
		for _, k := range f.charts {
			known = known || len(registries[k]) > 0
		}
		if !known {
			t.Errorf("%s: the chart's values name no registry for %v", f.name, f.charts)
		}
	}
}
