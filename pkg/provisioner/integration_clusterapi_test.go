//go:build integration

package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kubehenge/kubehenge/test/integration"
)

// classVersions are a Kubernetes version of each class's form, with the variables its clusters
// cannot go without beyond the Proxmox VE ones.
var classVersions = map[string]struct {
	version string
	vars    map[string]string
}{
	"kubehenge-kubeadm": {"v1.36.5", map[string]string{"cni": `"calico"`}},
	"kubehenge-rke2":    {"v1.36.5+rke2r1", nil},
	"kubehenge-k3s":     {"v1.36.5+k3s1", nil},
	"kubehenge-talos":   {"v1.36.5", map[string]string{TalosVersionVariable: `"v1.14.5"`}},
}

// startClusterAPI starts an API server with Cluster API's Cluster and ClusterClass webhooks and
// ClusterClass controller and Kubehenge's own Cluster validation beside them, as a management
// cluster runs them, and installs the charts with them answering, so that Cluster API checks
// Kubehenge's classes as they are applied.
func startClusterAPI(t *testing.T, values ...string) (*integration.API, *Hooks) {
	t.Helper()
	objects, webhooks := apiObjects(t, values...)
	var hooks *Hooks
	api := integration.Start(t, &integration.Options{
		Objects: objects, Namespaces: apiNamespaces(), Webhooks: webhooks, ClusterAPI: true,
		Serve: func(c client.Client) map[string]http.Handler {
			hooks = &Hooks{Reader: c, ClassNamespace: classNamespace, RancherKubernetes: KubernetesMinors{Min: 34, Max: 37}}
			return map[string]http.Handler{"/validate-cluster": admission.WithValidator[*clusterv1.Cluster](c.Scheme(), &ClusterAdmission{Hooks: hooks})}
		},
	})
	return api, hooks
}

// clusterOf is a cluster of class with one control plane and one worker on a network of its own,
// 10.0.<net>.0/24, with vars on top.
func clusterOf(class, name string, net int, vars map[string]string) *clusterv1.Cluster {
	c := &clusterv1.Cluster{
		Namespace: "team-a", Name: name,
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef:     clusterv1.ClusterClassRef{Name: class, Namespace: classNamespace},
			Version:      classVersions[class].version,
			ControlPlane: clusterv1.ControlPlaneTopology{Replicas: new(int32(1))},
			Workers: clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{
				{Class: "worker", Name: "md-0", Replicas: new(int32(1))},
			}},
		}},
	}
	withAddons(c, map[string]string{
		"network": fmt.Sprintf(`{"bridge":"vmbr0","addresses":["10.0.%d.10-10.0.%d.20"],"prefix":24,"gateway":"10.0.%d.1","dnsServers":["10.0.%d.1"]}`,
			net, net, net, net),
		"controlPlaneEndpoint": fmt.Sprintf(`"10.0.%d.5"`, net),
		"storage":              `"local-lvm"`,
	})
	withAddons(c, classVersions[class].vars)
	return withAddons(c, vars)
}

// roomy gives a cluster machines with room for any add-on, Rancher included.
var roomy = map[string]string{
	"controlPlaneMachine": `{"cores":8,"memoryMiB":16384}`,
	"workerMachine":       `{"cores":8,"memoryMiB":32768}`,
}

func storedVariable(c *clusterv1.Cluster, name string) string {
	for _, v := range c.Spec.Topology.Variables {
		if v.Name == name {
			return string(v.Value.Raw)
		}
	}
	return ""
}

// TestIntegrationClusterAPIChecksTheClasses holds every CEL rule of the classes' variables to what
// Cluster API makes of it with its own defaulting and checks, and Kubehenge's validation beside it:
// each refuses what it should and lets through a cluster that meets it, a cluster of the class's
// defaults passes every rule, and the rules on changes hold on update, also for values Cluster API
// filled in from a default. A class Cluster API has not passed yet, as every class is before its
// first cluster brings up its providers and templates, has its clusters' variables checked by
// Kubehenge instead.
func TestIntegrationClusterAPIChecksTheClasses(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, _ := startClusterAPI(t, "testdata/clusterapi-values.yaml")
	c := api.Client
	ctx := context.Background()

	// Cluster API's webhook lets a cluster of a class it has not passed through unchecked.
	deployKey := map[string]string{gitopsVariable: `{"enabled":true,"auth":"deployKey",` + repository + `}`}
	if err := c.Create(ctx, clusterOf("kubehenge-k3s", "unpassed", 1, deployKey), client.DryRunAll); !apierrors.IsInvalid(err) ||
		!strings.Contains(err.Error(), "A deploy key reads the repository over ssh://.") {
		t.Errorf("a deploy key over https://, before Cluster API has passed the class: %v", err)
	}
	if err := c.Create(ctx, clusterOf("kubehenge-k3s", "unpassed", 1, map[string]string{"cni": `"weave"`}), client.DryRunAll); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "weave") {
		t.Errorf("a CNI the class does not offer, before Cluster API has passed the class: %v", err)
	}
	// Made before Cluster API has passed the class, it never had its defaults stored.
	early := clusterOf("kubehenge-k3s", "early", 1, nil)
	if err := c.Create(ctx, early); err != nil {
		t.Fatalf("a cluster of the class's defaults, before Cluster API has passed the class: %v", err)
	}

	for class := range classVersions {
		integration.ApplyClassTemplates(t, c, classNamespace, class, templatesKey)
	}
	for class := range classVersions {
		integration.WaitReconciled(t, c, classNamespace, class)
	}

	// The CNI and CIS hardening it runs are the defaults it was made with, which stay.
	for name, value := range map[string]string{"cni": `"cilium"`, "cis": `true`} {
		got := &clusterv1.Cluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(early), got); err != nil {
			t.Fatal(err)
		}
		if storedVariable(got, name) != "" {
			t.Fatalf("%s was stored before Cluster API passed the class", name)
		}
		withAddons(got, map[string]string{name: value})
		if err := c.Update(ctx, got); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "cannot be changed") {
			t.Errorf("%s set to %s on a cluster made with its default: %v", name, value, err)
		}
	}

	net := 10
	for class := range classVersions {
		t.Run(class+" at its defaults", func(t *testing.T) {
			net++
			cluster := clusterOf(class, "defaults-"+strings.TrimPrefix(class, "kubehenge-"), net, nil)
			if err := c.Create(ctx, cluster); err != nil {
				t.Fatalf("a cluster of the class's defaults: %v", err)
			}
			got := &clusterv1.Cluster{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), got); err != nil {
				t.Fatal(err)
			}
			// Cluster API stores each default it filled in, which the rules on changes then hold.
			for _, name := range []string{"cni", "cis", "controlPlaneMachine", "workerMachine"} {
				if storedVariable(got, name) == "" {
					t.Errorf("%s was not filled in from its default", name)
				}
			}
			if class != "kubehenge-talos" && storedVariable(got, NodeOSVariable) != `{"family":"almalinux","release":"10"}` {
				t.Errorf("node OS %s, want the class's default", storedVariable(got, NodeOSVariable))
			}
			if err := c.Delete(ctx, got); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, rule := range celRules {
		t.Run(rule.message, func(t *testing.T) {
			for class := range classVersions {
				if rule.variable == NodeOSVariable && class == "kubehenge-talos" {
					continue
				}
				bad := clusterOf(class, "bad", 2, roomy)
				withAddons(bad, rule.bad)
				if err := c.Create(ctx, bad, client.DryRunAll); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), rule.message) {
					t.Errorf("%s: %v", class, err)
				}
				good := clusterOf(class, "good", 2, roomy)
				withAddons(good, rule.good)
				if err := c.Create(ctx, good, client.DryRunAll); err != nil {
					t.Errorf("%s, the rule met: %v", class, err)
				}
			}
		})
	}

	checkRulesOnChanges(ctx, t, c)
}

// celRule is one of the classes' CEL rules: variables that break it, which Cluster API refuses with
// its message, and variables that meet it, which pass both webhooks.
type celRule struct {
	variable  string
	message   string
	bad, good map[string]string
}

const repository = `"repository":{"url":"https://git.example.org/team/cluster.git"}`

func certificates(dns01 string) map[string]string {
	return map[string]string{"certificates": `{"enabled":true,"issuer":"acme-dns01","domain":"example.org","dns01":` + dns01 + `}`}
}

var celRules = []celRule{
	{
		backupsVariable, "Backups need a target.",
		map[string]string{backupsVariable: `{"enabled":true}`},
		map[string]string{backupsVariable: `{"enabled":true,"target":"store"}`},
	},
	{
		etcdSnapshotsVariable, "etcd snapshots need a target.",
		map[string]string{etcdSnapshotsVariable: `{"enabled":true}`},
		map[string]string{etcdSnapshotsVariable: `{"enabled":true,"target":"store"}`},
	},
	{
		etcdSnapshotsVariable, "The distribution's own upload takes the target, schedule and keep of etcd snapshots, which are off.",
		map[string]string{etcdSnapshotsVariable: `{"distributionUpload":true}`},
		map[string]string{etcdSnapshotsVariable: `{"enabled":false,"distributionUpload":false}`},
	},
	{
		"loadBalancer", "A load balancer needs addresses.",
		map[string]string{"loadBalancer": `{"enabled":true}`},
		map[string]string{"loadBalancer": `{"enabled":true,"addresses":["10.0.2.100-10.0.2.110"]}`},
	},
	{
		"rancher", "Rancher needs a hostname.",
		map[string]string{"rancher": `{"install":true}`},
		map[string]string{"rancher": `{"install":true,"hostname":"rancher.example.org"}`},
	},
	{
		"certificates", "Staging is Let's Encrypt's own directory; it cannot go with another server.",
		map[string]string{"certificates": `{"enabled":true,"issuer":"acme-http01","acme":{"server":"https://acme.example.org/directory","staging":true}}`},
		map[string]string{"certificates": `{"enabled":true,"issuer":"acme-http01","acme":{"server":"https://acme.example.org/directory"}}`},
	},
	{
		"certificates", "The credential comes from an existing Secret or from the one shared by every cluster, not both.",
		certificates(`{"provider":"cloudflare","existingSecret":"cloudflare","sharedCredential":true}`), certificates(`{"provider":"cloudflare","existingSecret":"cloudflare"}`),
	},
	{
		"certificates", "Akamai's three secrets come only from an existing Secret.",
		certificates(`{"provider":"akamai","akamai":{"serviceConsumerDomain":"akab.example.net"}}`),
		certificates(`{"provider":"akamai","existingSecret":"akamai","akamai":{"serviceConsumerDomain":"akab.example.net"}}`),
	},
	{
		"certificates", "Akamai needs its service consumer domain.",
		certificates(`{"provider":"akamai","existingSecret":"akamai"}`),
		certificates(`{"provider":"akamai","existingSecret":"akamai","akamai":{"serviceConsumerDomain":"akab.example.net"}}`),
	},
	{
		"certificates", "Route 53 needs its region and access key ID.",
		certificates(`{"provider":"route53","route53":{"region":"eu-central-2"}}`),
		certificates(`{"provider":"route53","route53":{"region":"eu-central-2","accessKeyID":"AKIAEXAMPLE"}}`),
	},
	{
		"certificates", "Azure DNS needs its subscription, resource group, tenant and client IDs.",
		certificates(`{"provider":"azuredns","azureDNS":{"subscriptionID":"s","resourceGroupName":"g","tenantID":"t"}}`),
		certificates(`{"provider":"azuredns","azureDNS":{"subscriptionID":"s","resourceGroupName":"g","tenantID":"t","clientID":"c"}}`),
	},
	{
		"certificates", "Google Cloud DNS needs its project.",
		certificates(`{"provider":"clouddns"}`), certificates(`{"provider":"clouddns","cloudDNS":{"project":"dns"}}`),
	},
	{
		"certificates", "RFC 2136 needs its nameserver and TSIG key name.",
		certificates(`{"provider":"rfc2136","rfc2136":{"nameserver":"10.0.2.53:53"}}`),
		certificates(`{"provider":"rfc2136","rfc2136":{"nameserver":"10.0.2.53:53","tsigKeyName":"kubehenge"}}`),
	},
	{
		"certificates", "ACME-DNS needs its host.",
		certificates(`{"provider":"acmedns"}`), certificates(`{"provider":"acmedns","acmeDNS":{"host":"https://auth.example.org"}}`),
	},
	{
		"certificates", "DNS-01 and the cluster's CA need a domain.",
		map[string]string{"certificates": `{"enabled":true,"issuer":"ca"}`},
		map[string]string{"certificates": `{"enabled":true,"issuer":"ca","domain":"example.org"}`},
	},
	{
		"certificates", "DNS-01 needs a DNS provider.",
		map[string]string{"certificates": `{"enabled":true,"issuer":"acme-dns01","domain":"example.org"}`},
		certificates(`{"provider":"cloudflare"}`),
	},
	{
		gitopsVariable, "A token goes into the cluster's gitops credential, not into the URL.",
		map[string]string{gitopsVariable: `{"enabled":true,"auth":"token","repository":{"url":"https://user:token@git.example.org/team/cluster.git"}}`}, //nolint:gosec // the credential in a URL the rule refuses
		map[string]string{gitopsVariable: `{"enabled":true,"auth":"token",` + repository + `}`},
	},
	{
		gitopsVariable, "The path stays inside the repository.",
		map[string]string{gitopsVariable: `{"enabled":true,"repository":{"url":"https://git.example.org/team/cluster.git","path":"clusters/../secrets"}}`},
		map[string]string{gitopsVariable: `{"enabled":true,"repository":{"url":"https://git.example.org/team/cluster.git","path":"clusters/..prod"}}`},
	},
	{
		gitopsVariable, "GitOps needs a repository.",
		map[string]string{gitopsVariable: `{"enabled":true}`},
		map[string]string{gitopsVariable: `{"enabled":true,` + repository + `}`},
	},
	{
		gitopsVariable, "A deploy key reads the repository over ssh://.",
		map[string]string{gitopsVariable: `{"enabled":true,"auth":"deployKey",` + repository + `}`},
		map[string]string{gitopsVariable: `{"enabled":true,"auth":"deployKey","repository":{"url":"ssh://git@git.example.org/team/cluster.git"}}`},
	},
	{
		gitopsVariable, "Without a key, the repository is read over https://.",
		map[string]string{gitopsVariable: `{"enabled":true,"auth":"none","repository":{"url":"ssh://git@git.example.org/team/cluster.git"}}`},
		map[string]string{gitopsVariable: `{"enabled":true,"auth":"none",` + repository + `}`},
	},
	{
		gitopsVariable, "Credentials from a Secret of your own need the Secret's name.",
		map[string]string{gitopsVariable: `{"enabled":true,"auth":"existingSecret",` + repository + `}`},
		map[string]string{gitopsVariable: `{"enabled":true,"auth":"existingSecret","existingSecret":"git",` + repository + `}`},
	},
	{
		gitopsVariable, "A client secret from a Secret of your own needs the Secret's name.",
		map[string]string{gitopsVariable: `{"enabled":true,` + repository + `,"signIn":{"clientSecret":"existingSecret"}}`},
		map[string]string{gitopsVariable: `{"enabled":true,` + repository + `,"signIn":{"clientSecret":"existingSecret","existingSecret":"argocd-sso"}}`},
	},
	{
		gitopsVariable, "The web interface is Flux's, and needs a hostname.",
		map[string]string{gitopsVariable: `{"enabled":true,"engine":"argocd","ui":true,"hostname":"argocd.example.org",` + repository + `,"signIn":{"clientSecret":"stored"}}`},
		map[string]string{
			gitopsVariable: `{"enabled":true,"engine":"flux","ui":true,"hostname":"flux.example.org",` + repository + `,"signIn":{"clientSecret":"stored"}}`,
			"oidc":         `[{"issuerURL":"https://sso.example.org","clientID":"kubernetes"}]`,
		},
	},
	{
		gitopsVariable, "Flux's web interface signs in with a client secret.",
		map[string]string{gitopsVariable: `{"enabled":true,"engine":"flux","ui":true,"hostname":"flux.example.org",` + repository + `}`},
		map[string]string{
			gitopsVariable: `{"enabled":true,"engine":"flux","ui":true,"hostname":"flux.example.org",` + repository + `,"signIn":{"clientSecret":"stored"}}`,
			"oidc":         `[{"issuerURL":"https://sso.example.org","clientID":"kubernetes"}]`,
		},
	},
	{
		"oidc", "Kubernetes keeps system: for its own users and groups.",
		map[string]string{"oidc": `[{"issuerURL":"https://sso.example.org","clientID":"kubernetes","usernamePrefix":"system:sso:"}]`},
		map[string]string{"oidc": `[{"issuerURL":"https://sso.example.org","clientID":"kubernetes","usernamePrefix":"sso:","groupsPrefix":"sso:"}]`},
	},
	{
		"oidc", "Kubernetes keeps system: for its own users and groups.",
		map[string]string{"oidc": `[{"issuerURL":"https://sso.example.org","clientID":"kubernetes","groupsPrefix":"system:masters"}]`},
		map[string]string{"oidc": `[{"issuerURL":"https://sso.example.org","clientID":"kubernetes"}]`},
	},
	{
		NodeOSVariable, "a release of the family Kubehenge builds templates of",
		map[string]string{NodeOSVariable: `{"family":"rocky","release":"9"}`},
		map[string]string{NodeOSVariable: `{"family":"rocky","release":"10"}`},
	},
}

// checkRulesOnChanges holds the rules on changes to a cluster: the CNI and CIS hardening stay as
// created, also where Cluster API filled them in from their defaults, and Loki's volume only grows.
func checkRulesOnChanges(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	cluster := clusterOf("kubehenge-k3s", "changes", 3, roomy)
	withAddons(cluster, map[string]string{"monitoring": `{"enabled":true}`, logsAddon: `{"enabled":true,"sizeGiB":20}`})
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	change := func(name, value string) error {
		t.Helper()
		got := &clusterv1.Cluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), got); err != nil {
			t.Fatal(err)
		}
		for i := range got.Spec.Topology.Variables {
			if got.Spec.Topology.Variables[i].Name == name {
				got.Spec.Topology.Variables[i].Value = apiextensionsv1.JSON{Raw: []byte(value)}
				return c.Update(ctx, got)
			}
		}
		t.Fatalf("the cluster has no %s", name)
		return nil
	}
	for _, tc := range []struct{ name, value, message string }{
		{"cni", `"cilium"`, "the CNI is chosen when the cluster is created and cannot be changed"},
		{"cis", `true`, "CIS hardening is chosen when the cluster is created and cannot be changed"},
		{logsAddon, `{"enabled":true,"sizeGiB":10}`, "Loki's volume only grows."},
	} {
		if err := change(tc.name, tc.value); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("%s changed to %s: %v", tc.name, tc.value, err)
		}
	}
	if err := change(logsAddon, `{"enabled":true,"sizeGiB":40}`); err != nil {
		t.Errorf("Loki's volume grown: %v", err)
	}
	if err := change("cni", `"flannel"`); err != nil {
		t.Errorf("the CNI given as it was: %v", err)
	}
}
