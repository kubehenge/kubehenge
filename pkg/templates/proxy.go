package templates

import (
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
)

// GuestProxy is the HTTP(S) proxy a bake's VMs download through, for a site without direct internet
// access. The zero value, or nil, is no proxy, and a bake without one produces the same cloud-config
// it always did.
type GuestProxy struct {
	// URL is the proxy's address, with credentials if it needs them.
	URL string
	// NoProxy is the comma-separated list of hosts, domains and networks that bypass it.
	NoProxy string
	// CABundle is the PEM authority of a proxy that re-signs TLS.
	CABundle string
}

// Enabled tells whether there is a proxy.
func (p *GuestProxy) Enabled() bool { return p != nil && p.URL != "" }

// CAFileEnv names the variable that holds the path of the proxy's authority, where the chart
// mounts it.
const CAFileEnv = "KUBEHENGE_PROXY_CA_FILE"

// ProxyFromEnvironment reads the proxy the pod was given: the standard variables, and the
// authority from the file CAFileEnv names. Without a proxy it returns one that is not Enabled.
func ProxyFromEnvironment() (*GuestProxy, error) {
	first := func(names ...string) string {
		for _, n := range names {
			if v := os.Getenv(n); v != "" {
				return v
			}
		}
		return ""
	}
	address := first("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy")
	if address == "" {
		return &GuestProxy{}, nil
	}
	p := &GuestProxy{URL: address, NoProxy: first("NO_PROXY", "no_proxy")}
	if file := os.Getenv(CAFileEnv); file != "" {
		pem, err := os.ReadFile(file) //nolint:gosec // the chart mounts it there
		if err != nil {
			return nil, fmt.Errorf("the proxy's certificate authority: %w", err)
		}
		p.CABundle = string(pem)
	}
	return p, nil
}

// proxyDir is where a bake's proxy scripts are, below buildDir.
const proxyDir = "proxy"

// proxyBuild is the command that runs a bake's build with the proxy set up before it and taken away
// after it, whatever the build's outcome.
func proxyBuild(build []string) []string {
	return append([]string{"sh", path.Join(buildDir, proxyDir, "run.sh")}, build...)
}

// proxyFiles are the scripts and authority that set the proxy up in a guest and take it away again,
// by their path below buildDir.
func proxyFiles(p *GuestProxy) map[string]string {
	files := map[string]string{
		proxyDir + "/run.sh":  proxyRun,
		proxyDir + "/down.sh": proxyDown,
		proxyDir + "/up.sh":   proxyUp(p),
	}
	if p.CABundle != "" {
		files[proxyDir+"/ca.crt"] = p.CABundle
	}
	return files
}

// proxyRun sets the proxy up, runs the command it is given, and takes the proxy away again.
const proxyRun = `#!/bin/sh
dir=$(dirname "$0")
sh "${dir}/up.sh" || exit 1
# up.sh runs in a shell of its own, so the environment it wrote for logins is taken up here: curl,
# podman and everything the build starts go through the proxy.
. /etc/profile.d/kubehenge-proxy.sh
"$@"
status=$?
sh "${dir}/down.sh"
exit "${status}"
`

// proxyDown removes everything up.sh put in the guest, so that the credentials and the authority
// are in no image a cluster boots from.
//
//nolint:dupword // the shell's nested fi
const proxyDown = `#!/bin/sh
dir=$(dirname "$0")
sed -i '/^proxy=/d;/^proxy_username=/d;/^proxy_password=/d' /etc/dnf/dnf.conf 2> /dev/null
if [ -f /etc/sysconfig/proxy ]; then
    sed -i '/^PROXY_ENABLED=/d;/^HTTP_PROXY=/d;/^HTTPS_PROXY=/d;/^NO_PROXY=/d' /etc/sysconfig/proxy
    printf 'PROXY_ENABLED="no"\nHTTP_PROXY=""\nHTTPS_PROXY=""\nNO_PROXY=""\n' >> /etc/sysconfig/proxy
fi
sed -i '/^# kubehenge proxy$/,/^# end kubehenge proxy$/d' /etc/environment 2> /dev/null
rm -f /etc/profile.d/kubehenge-proxy.sh /etc/systemd/system/containerd.service.d/kubehenge-proxy.conf
systemctl daemon-reload 2> /dev/null
for anchor in /etc/pki/ca-trust/source/anchors /etc/pki/trust/anchors /usr/local/share/ca-certificates; do
    if [ -f "${anchor}/kubehenge-proxy.crt" ]; then
        rm -f "${anchor}/kubehenge-proxy.crt"
        removed=1
    fi
done
if [ -n "${removed}" ]; then
    if command -v update-ca-trust > /dev/null; then update-ca-trust; else update-ca-certificates; fi
fi
`

// proxyUp is the script that sets the proxy up: for dnf and zypper, for curl and everything that
// reads the environment, and for containerd's image pulls, and that trusts the proxy's authority.
func proxyUp(p *GuestProxy) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\nset -eu\ndir=$(dirname \"$0\")\n")
	fmt.Fprintf(&sb, "proxy=%s\nno_proxy=%s\n", shellQuote(p.URL), shellQuote(p.NoProxy))
	// dnf takes the credentials as options of their own.
	sb.WriteString("if [ -f /etc/dnf/dnf.conf ]; then\n")
	sb.WriteString("    sed -i '/^proxy=/d;/^proxy_username=/d;/^proxy_password=/d' /etc/dnf/dnf.conf\n")
	address, user, password := splitCredentials(p.URL)
	fmt.Fprintf(&sb, "    printf 'proxy=%%s\\n' %s >> /etc/dnf/dnf.conf\n", shellQuote(address))
	if user != "" {
		fmt.Fprintf(&sb, "    printf 'proxy_username=%%s\\nproxy_password=%%s\\n' %s %s >> /etc/dnf/dnf.conf\n", shellQuote(user), shellQuote(password))
	}
	sb.WriteString("fi\n")
	sb.WriteString(`if [ -f /etc/sysconfig/proxy ]; then
    sed -i '/^PROXY_ENABLED=/d;/^HTTP_PROXY=/d;/^HTTPS_PROXY=/d;/^NO_PROXY=/d' /etc/sysconfig/proxy
    {
        echo 'PROXY_ENABLED="yes"'
        echo "HTTP_PROXY=\"${proxy}\""
        echo "HTTPS_PROXY=\"${proxy}\""
        echo "NO_PROXY=\"${no_proxy}\""
    } >> /etc/sysconfig/proxy
fi
{
    echo '# kubehenge proxy'
    for name in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy; do echo "${name}=\"${proxy}\""; done
    echo "NO_PROXY=\"${no_proxy}\""
    echo "no_proxy=\"${no_proxy}\""
    echo '# end kubehenge proxy'
} >> /etc/environment
{
    for name in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy; do echo "export ${name}='${proxy}'"; done
    echo "export NO_PROXY='${no_proxy}' no_proxy='${no_proxy}'"
} > /etc/profile.d/kubehenge-proxy.sh
for name in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy; do export "${name}=${proxy}"; done
export NO_PROXY="${no_proxy}" no_proxy="${no_proxy}"
# A drop-in for containerd, which crictl's pulls go through. systemd reads % as a specifier.
mkdir -p /etc/systemd/system/containerd.service.d
escaped=$(printf '%s' "${proxy}" | sed 's/%/%%/g;s/\\/\\\\/g;s/"/\\"/g')
escaped_no=$(printf '%s' "${no_proxy}" | sed 's/%/%%/g;s/\\/\\\\/g;s/"/\\"/g')
{
    echo '[Service]'
    echo "Environment=\"HTTP_PROXY=${escaped}\" \"HTTPS_PROXY=${escaped}\" \"NO_PROXY=${escaped_no}\""
} > /etc/systemd/system/containerd.service.d/kubehenge-proxy.conf
systemctl daemon-reload
`)
	if p.CABundle != "" {
		sb.WriteString(`for anchor in /etc/pki/ca-trust/source/anchors /etc/pki/trust/anchors /usr/local/share/ca-certificates; do
    if [ -d "${anchor}" ]; then
        cp "${dir}/ca.crt" "${anchor}/kubehenge-proxy.crt"
        break
    fi
done
if command -v update-ca-trust > /dev/null; then update-ca-trust; else update-ca-certificates; fi
systemctl try-restart containerd 2> /dev/null || true
`)
	}
	return sb.String()
}

// splitCredentials separates a proxy address from the user and password it carries.
func splitCredentials(raw string) (string, string, string) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw, "", ""
	}
	user := u.User.Username()
	password, _ := u.User.Password()
	u.User = nil
	return u.String(), user, password
}

// shellQuote quotes s as one word for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// errProxyBuild is a proxy that cannot go into a bake.
var errProxyBuild = errors.New("the proxy address is not one a bake can use")

// checkGuestProxy refuses a proxy a bake's scripts could not carry: one with a line break, which
// would end the shell word.
func checkGuestProxy(p *GuestProxy) error {
	if !p.Enabled() {
		return nil
	}
	if strings.ContainsAny(p.URL+p.NoProxy, "\n\r") {
		return errProxyBuild
	}
	return nil
}

// pullUserData is the cloud-config of the booted node that pulls a kubeadm node's images, with the
// proxy up for the pulls and taken away after them.
func pullUserData(p *GuestProxy, version string) string {
	if !p.Enabled() {
		return fmt.Sprintf("#cloud-config\nruncmd:\n  - [%s, %s]\n", imagesPath, version)
	}
	const dir = "/run/kubehenge-proxy"
	var sb strings.Builder
	sb.WriteString("#cloud-config\nwrite_files:\n")
	files := proxyFiles(p)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content := files[name]
		fmt.Fprintf(&sb, "  - path: %s\n    permissions: \"0600\"\n    encoding: b64\n    content: %s\n",
			path.Join(dir, path.Base(name)), base64.StdEncoding.EncodeToString([]byte(content)))
	}
	fmt.Fprintf(&sb, "runcmd:\n  - [sh, %s, %s, %s]\n", path.Join(dir, "run.sh"), imagesPath, version)
	return sb.String()
}
