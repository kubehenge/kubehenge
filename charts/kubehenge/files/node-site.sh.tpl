#!/bin/sh
set -eu
dist=@DISTRIBUTION@
site=/etc/kubehenge/site
mkdir -p "${site}"
{{ if $cas }}
cat > "${site}/trusted-cas.pem" <<'KUBEHENGE_PEM'
{{ range $pem := $cas }}{{ trim $pem | replace "\n" "\n  " }}
{{ end }}KUBEHENGE_PEM
if [ -d /etc/pki/ca-trust/source/anchors ]; then
    anchor=/etc/pki/ca-trust/source/anchors/kubehenge-trusted-cas.pem
    refresh=update-ca-trust
elif [ -d /etc/pki/trust/anchors ]; then
    anchor=/etc/pki/trust/anchors/kubehenge-trusted-cas.pem
    refresh=update-ca-certificates
elif [ -d /usr/local/share/ca-certificates ]; then
    anchor=/usr/local/share/ca-certificates/kubehenge-trusted-cas.crt
    refresh=update-ca-certificates
else
    echo "trustedCAs: the node OS has no trust store Kubehenge knows (EL, SUSE or Debian family)" >&2
    exit 1
fi
cp "${site}/trusted-cas.pem" "${anchor}"
if command -v restorecon > /dev/null; then restorecon "${anchor}"; fi
${refresh}
{{ end }}
{{ range $i, $m := $mirrors }}{{ if dig "caBundle" "" $m }}
cat > "${site}/mirror-{{ $i }}.pem" <<'KUBEHENGE_PEM'
{{ trim $m.caBundle | replace "\n" "\n  " }}
KUBEHENGE_PEM
{{ end }}{{ end }}
{{ if $mirrors }}
case "${dist}" in
kubeadm)
{{ range $i, $m := $mirrors }}
    dir=/etc/containerd/certs.d/{{ if eq $m.registry "*" }}_default{{ else }}{{ $m.registry }}{{ end }}
    mkdir -p "${dir}"
    cat > "${dir}/hosts.toml" <<'KUBEHENGE_HOSTS'
{{ range $e := $m.endpoints }}[host."{{ $e }}"]
  capabilities = ["pull", "resolve"]
{{ if dig "caBundle" "" $m }}  ca = "/etc/kubehenge/site/mirror-{{ $i }}.pem"
{{ end }}{{ if (urlParse $e).path }}  override_path = true
{{ end }}{{ end }}KUBEHENGE_HOSTS
{{ end }}
    # containerd 1.x reads certs.d only where config_path names it.
    containerd config dump | grep -q 'certs.d' || { echo "registryMirrors: containerd does not read /etc/containerd/certs.d" >&2; exit 1; }
    ;;
*)
    mkdir -p "/etc/rancher/${dist}"
    cat > "/etc/rancher/${dist}/registries.yaml" <<'KUBEHENGE_REGISTRIES'
mirrors:
{{ range $m := $mirrors }}  "{{ $m.registry }}":
    endpoint:
{{ range $e := $m.endpoints }}      - "{{ $e }}"
{{ end }}{{ end }}{{ $seen := dict }}{{ $tls := false }}{{ range $m := $mirrors }}{{ if dig "caBundle" "" $m }}{{ $tls = true }}{{ end }}{{ end }}{{ if $tls }}configs:
{{ range $i, $m := $mirrors }}{{ if dig "caBundle" "" $m }}{{ range $e := $m.endpoints }}{{ $h := (urlParse $e).host }}{{ if not (hasKey $seen $h) }}{{ $_ := set $seen $h true }}  "{{ $h }}":
    tls:
      ca_file: /etc/kubehenge/site/mirror-{{ $i }}.pem
{{ end }}{{ end }}{{ end }}{{ end }}{{ end }}KUBEHENGE_REGISTRIES
    ;;
esac
{{ end }}
{{ if $url }}
cat > "${site}/proxy.env" <<'KUBEHENGE_PROXY'
HTTP_PROXY='{{ $url }}'
http_proxy='{{ $url }}'
HTTPS_PROXY='{{ $https }}'
https_proxy='{{ $https }}'
NO_PROXY='{{ $noProxy }}'
no_proxy='{{ $noProxy }}'
KUBEHENGE_PROXY
case "${dist}" in
rke2)
    for unit in rke2-server rke2-agent; do cp "${site}/proxy.env" "/etc/default/${unit}"; done
    ;;
k3s)
    for unit in k3s k3s-agent; do cp "${site}/proxy.env" "/etc/default/${unit}"; done
    ;;
kubeadm)
    mkdir -p /etc/systemd/system/containerd.service.d
    printf '[Service]\nEnvironmentFile=%s\n' "${site}/proxy.env" > /etc/systemd/system/containerd.service.d/50-kubehenge-proxy.conf
    systemctl daemon-reload
    ;;
esac
{{ end }}
{{ if or $cas $url }}
# containerd reads the trust store and its environment once, when it starts.
if [ "${dist}" = kubeadm ]; then systemctl restart containerd; fi
{{ end }}
