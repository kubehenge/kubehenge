package provisioner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func withTimeServers(t *testing.T, servers []string) *clusterv1.Cluster {
	t.Helper()
	c := networkCluster("10.0.0.1", 24)
	raw, err := json.Marshal(map[string]any{
		"bridge": "vmbr0", "addresses": []string{"10.0.0.10-10.0.0.20"}, "prefix": 24, "gateway": "10.0.0.1",
		"dnsServers": []string{"10.0.0.1"}, "ntp": servers,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range c.Spec.Topology.Variables {
		if c.Spec.Topology.Variables[i].Name == "network" {
			c.Spec.Topology.Variables[i].Value.Raw = raw
		}
	}
	return c
}

func TestValidatorChecksTheTimeServers(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: hooks}
	many := []string{"a.example", "b.example", "c.example", "d.example", "e.example", "f.example", "g.example", "h.example", "i.example"}
	for name, c := range map[string]struct {
		servers []string
		want    string
	}{
		"names and addresses": {[]string{"ntp.example.org", "192.0.2.5", "2001:db8::1", "pool.ntp.org."}, ""},
		"none":                {nil, ""},
		"eight":               {many[:8], ""},
		"nine":                {many, "at most 8"},
		"a URL":               {[]string{"http://ntp.example.org"}, "http://ntp.example.org is no host name or IP address"},
		"a space":             {[]string{"ntp example"}, "is no host name or IP address"},
		"loopback":            {[]string{"127.0.0.1"}, "is no address the nodes can reach"},
		"link-local":          {[]string{"169.254.1.1"}, "is no address the nodes can reach"},
		"leading hyphen":      {[]string{"-bad.example.org"}, "is no host name or IP address"},
		"empty label":         {[]string{"ntp..example.org"}, "is no host name or IP address"},
		"twice":               {[]string{"ntp.example.org", "ntp.example.org"}, "ntp.example.org is listed twice"},
		"a path":              {[]string{"ntp.example.org/x"}, "is no host name or IP address"},
		"empty":               {[]string{""}, "is no host name or IP address"},
	} {
		_, err := v.ValidateCreate(context.Background(), withTimeServers(t, c.servers))
		switch {
		case c.want == "" && err != nil && strings.Contains(err.Error(), "ntp"):
			t.Errorf("%s: refused: %v", name, err)
		case c.want != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want a refusal with %q", name, err, c.want)
		}
	}
}
