package provisioner

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// MaxTimeServers is how many time servers a cluster's nodes are given. chrony and Talos use a few,
// and more only lengthen the wait on a blocked network.
const MaxTimeServers = 8

// TimeServerProblem says what is wrong with a time server, or "" when the nodes can use it: a DNS
// name or an IP address, never a URL and never an address that leads back to the node itself.
func TimeServerProblem(server string) string {
	ip, err := netip.ParseAddr(server)
	switch {
	case err == nil && (ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast()):
		return server + " is no address the nodes can reach"
	case err != nil && !validHostName(server):
		return server + " is no host name or IP address"
	}
	return ""
}

// validHostName tells whether s is a DNS name: dot-separated labels of letters, digits and hyphens,
// none starting or ending with a hyphen, at most 253 characters.
func validHostName(s string) bool {
	const longestName, longestLabel = 253, 63
	s = strings.TrimSuffix(s, ".")
	if s == "" || len(s) > longestName {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > longestLabel || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-') {
				return false
			}
		}
	}
	return true
}

// validateTimeServers holds the time servers of the network variable to TimeServerProblem, to
// MaxTimeServers and to being listed once each. Unset, the nodes keep their image's own servers.
func validateTimeServers(c *clusterv1.Cluster) field.ErrorList {
	network, set, err := optionalVariable[nodeNetwork](c, "network")
	if err != nil || !set {
		return nil
	}
	path := field.NewPath("spec", "topology", "variables").Key("network")
	if problem := timeServersProblem(network.NTP); problem != nil {
		return field.ErrorList{field.Invalid(path, "ntp", problem.Error())}
	}
	return nil
}

func timeServersProblem(servers []string) error {
	if len(servers) > MaxTimeServers {
		return fmt.Errorf("ntp: at most %d time servers, not %d", MaxTimeServers, len(servers))
	}
	seen := make(map[string]bool, len(servers))
	var problems []error
	for _, s := range servers {
		switch p := TimeServerProblem(s); {
		case seen[s]:
			problems = append(problems, fmt.Errorf("ntp: %s is listed twice", s))
		case p != "":
			problems = append(problems, fmt.Errorf("ntp: %s", p))
		}
		seen[s] = true
	}
	return errors.Join(problems...)
}
