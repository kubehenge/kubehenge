#!/bin/sh
# The VM templates an end-to-end test needs, and the keys the Actions cache keeps the baked ones
# under:
#
#   test/e2e/ci/templates.sh keys <test>
#
# prints key1=<key> and key2=<key>, one for each template of kubeadm, RKE2 or k3s the test makes,
# in the order run.sh makes them; Talos's templates are only downloaded, not baked, and get none.
# A key is the template's options and a hash of every input of its bake: the code `kubehenge
# template create` runs (the Go packages pkg/templates imports from this repository, its command and
# the modules in go.sum), the release pins, the node OSes offered, Proxmox VE's image and the
# scripts that start it and make the templates. The mirrors' newest images and packages are no input: the nightly run bakes
# every template from them afresh. run.sh sources this file for templates_of.
set -eu

# templates_of <test>: one line per template the test needs, as `kubehenge template create` takes
# its options, from ${from}, ${to}, ${node_os}, ${talos_from} and ${talos_to}.
templates_of() {
    case "$1" in
        rke2*) echo "rke2 ${node_os}" ;;
        k3s*) echo "k3s ${node_os}" ;;
        rancher | cluster-mesh | autoscaler | control-plane-ha | two-nodes) echo k3s ;;
        # The test moves its cluster to another node OS than the default.
        node-image-refresh)
            echo k3s
            echo k3s --node-os rocky-10
            ;;
        kubeadm)
            echo "kubeadm --kubernetes-version ${from} ${node_os}"
            echo "kubeadm --kubernetes-version ${to} ${node_os}"
            ;;
        # Its add-on parts replace no machine, so they need only the one they start from.
        kubeadm-addons-?) echo "kubeadm --kubernetes-version ${from} ${node_os}" ;;
        # The release the cluster starts on, and the one its machines boot once the Kubernetes
        # upgrade replaces them.
        talos*)
            echo "talos --talos-version ${talos_from}"
            echo "talos --talos-version ${talos_to}"
            ;;
        *) ;;
    esac
}

# kubernetes_versions <repository>: sets from, the release workload clusters start on, and to, the
# management cluster's, which they are upgraded to.
kubernetes_versions() {
    from=${KUBEHENGE_E2E_FROM:-v1.36.5}
    to=$(sed -n 's/^ *kubernetes *= *"\(v[^"]*\)".*/\1/p' "$1/management-cluster/versions.tf")
    [ -n "${to}" ]
}

# bake_inputs <repository>: a hash of everything a bake depends on but its options.
bake_inputs() {
    (
        cd "$1"
        root=$(pwd)
        packages=$(go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./pkg/templates | sed -n "s|^${root}/||p")
        # shellcheck disable=SC2086 # one directory per word
        git ls-files -s -- ${packages} pkg/cli/template.go go.mod go.sum pkg/provisioner/releasepins.txt \
            charts/kubehenge/files/node-oses.json test/e2e/pve test/e2e/ci/up.sh test/e2e/ci/run.sh \
            test/e2e/ci/templates.sh ':(exclude)*_test.go'
    ) | sha256sum | cut -c1-16
}

if [ "${0##*/}" = templates.sh ] && [ "${1:-}" = keys ]; then
    script=$(readlink -f "$0")
    here=$(dirname "${script}")
    repo=$(readlink -f "${here}/../../..")
    test=${2:?usage: $0 keys <test>}
    # A test with options runs only at night, which bakes afresh, and its templates would only crowd
    # the cache.
    case "${test}" in
        *+*) exit 0 ;;
        *) ;;
    esac
    node_os=
    # Talos's templates are left out below.
    talos_from=
    talos_to=
    kubernetes_versions "${repo}"
    inputs=$(bake_inputs "${repo}")
    slot=0
    templates_of "${test}" | while read -r options; do
        case "${options}" in
            talos*) continue ;;
            *) ;;
        esac
        slot=$((slot + 1))
        name=$(printf '%s' "${options}" | sed 's/--[a-z-]* //g; s/ /-/g')
        hash=$(printf '%s\n%s\n' "${options}" "${inputs}" | sha256sum | cut -c1-16)
        echo "key${slot}=e2e-template-${name}-${hash}"
    done
fi
