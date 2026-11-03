#!/bin/sh
# The VM templates the end-to-end tests need, and the keys the Actions cache keeps the baked ones
# under. run.sh sources this file.
#
#   test/e2e/ci/templates.sh keys <test>       # key1=<key> and key2=<key>
#   test/e2e/ci/templates.sh missing <ref>     # the keys the cache lacks on <ref>, as JSON
#   test/e2e/ci/templates.sh bake <work directory> <key> <options>...
#
# keys prints one key for each template of kubeadm, RKE2 or k3s the test makes, in the order run.sh
# makes them. Talos's templates are only downloaded, not baked, and get none. missing lists, from
# every test the merge queue runs, the templates whose keys the repository's cache lacks on <ref>,
# for e2e-templates.yml (needs the GitHub CLI with actions: read). bake makes one template on the
# Proxmox VE up.sh started and backs it up to <work directory>/vz/template-1 for the cache.
#
# A key is the template's options and a hash of every input of its bake. Those are the code
# `kubehenge template create` runs (the Go packages pkg/templates imports from this repository, its
# command and the modules in go.sum), the release pins, the node OSes offered, Proxmox VE's image
# and the scripts that start it and make the templates. The mirrors' newest images and packages are
# no input: the nightly run bakes every template from them afresh.
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
            charts/kubehenge/files/node-oses.json test/e2e/pve test/e2e/ci/up.sh test/e2e/ci/templates.sh \
            ':(exclude)*_test.go'
    ) | sha256sum | cut -c1-16
}

# key_of <options> <inputs>: the cache key of a template.
key_of() {
    key_name=$(printf '%s' "$1" | sed 's/--[a-z-]* //g; s/ /-/g')
    key_hash=$(printf '%s\n%s\n' "$1" "$2" | sha256sum | cut -c1-16)
    echo "e2e-template-${key_name}-${key_hash}"
}

# baked <test>: the options of each template of the test a bake makes, one per line.
baked() {
    templates_of "$1" | grep -v '^talos'
}

# The host's CPU: told of a generic one, the guests' kernels run Meltdown's and MDS's mitigations,
# which under nested virtualisation slow them on Intel runners until a worker loses its API server.
template() { # distribution [flags]
    : "${KUBEHENGE_E2E_NODE:?}" "${KUBEHENGE_E2E_ADDRESS:?}" "${KUBEHENGE_E2E_GATEWAY:?}" "${KUBEHENGE_E2E_DNS:?}"
    "${repo}/dist/kubehenge" template create "$@" --cpu-type host --node "${KUBEHENGE_E2E_NODE}" \
        --storage "${KUBEHENGE_E2E_SHARED_STORAGE:-local}" \
        --build-address "${KUBEHENGE_E2E_ADDRESS}" --build-gateway "${KUBEHENGE_E2E_GATEWAY}" \
        --build-dns "${KUBEHENGE_E2E_DNS}"
}

# A baked template the Actions cache kept is restored into the pool from ${work}/vz/template-<n>,
# the n-th baked template of the test, where `kubehenge template create` then finds it. One baked
# here whose slot KUBEHENGE_E2E_KEEP_TEMPLATES lists (as in "1 2") is backed up there for the cache.
pve() { docker exec kubehenge-e2e-pve "$@"; }
# made <slot> <options>: restores the template or makes it. Slot 0 is never kept.
made() {
    made_slot=$1
    shift
    made_kept=/var/lib/vz/template-${made_slot}
    if [ "${made_slot}" != 0 ] && [ -s "${work}/vz/template-${made_slot}/template.vma.zst" ]; then
        made_id=$(pve pvesh get /cluster/nextid)
        pve qmrestore "${made_kept}/template.vma.zst" "${made_id}" --storage "${KUBEHENGE_E2E_SHARED_STORAGE:-local}" \
            --pool kubehenge-templates > /dev/null
        echo "template $* restored from the cache as VM ${made_id}"
        template "$@"
        return
    fi
    made_status=0
    # shellcheck disable=SC2310 # the status is returned below, once the output is shown.
    made_out=$(template "$@" 2>&1) || made_status=$?
    printf '%s\n' "${made_out}"
    [ "${made_status}" = 0 ] || return "${made_status}"
    case " ${KUBEHENGE_E2E_KEEP_TEMPLATES:-} " in
        *" ${made_slot} "*) ;;
        *) return 0 ;;
    esac
    made_id=$(printf '%s\n' "${made_out}" | sed -n 's/^template .* is VM \([0-9][0-9]*\)$/\1/p' | tail -n 1)
    [ -n "${made_id}" ] || {
        echo "no VM ID for template $* to keep" >&2
        return 1
    }
    # Only a bake that proved the guest agent hands out the boot log is kept.
    printf '%s\n' "${made_out}" | grep -q 'hands out its boot log and cloud-init.s output through the guest agent$' || {
        echo "template $* never showed its boot log, so it is not kept" >&2
        return 1
    }
    pve rm -rf "${made_kept}"
    pve mkdir -p "${made_kept}"
    pve vzdump "${made_id}" --dumpdir "${made_kept}" --compress zstd --quiet 1
    pve sh -c "mv ${made_kept}/vzdump-qemu-*.vma.zst ${made_kept}/template.vma.zst && rm -f ${made_kept}/vzdump-qemu-*"
    made_size=$(du -h "${work}/vz/template-${made_slot}/template.vma.zst")
    echo "template $* kept for the cache: ${made_size%%[[:space:]]*}"
}

if [ "${0##*/}" = templates.sh ]; then
    script=$(readlink -f "$0")
    here=$(dirname "${script}")
    repo=$(readlink -f "${here}/../../..")
    node_os=
    # Talos's templates are never baked.
    talos_from=
    talos_to=
    kubernetes_versions "${repo}"
    case "${1:-}" in
        keys)
            cli_test=${2:?usage: $0 keys <test>}
            # A test with options runs only at night, which bakes afresh, and its templates would
            # only crowd the cache.
            case "${cli_test}" in
                *+*) exit 0 ;;
                *) ;;
            esac
            cli_inputs=$(bake_inputs "${repo}")
            cli_slot=0
            baked "${cli_test}" > "${TMPDIR:-/tmp}/templates.$$"
            while read -r cli_options; do
                cli_slot=$((cli_slot + 1))
                cli_key=$(key_of "${cli_options}" "${cli_inputs}")
                echo "key${cli_slot}=${cli_key}"
            done < "${TMPDIR:-/tmp}/templates.$$"
            rm -f "${TMPDIR:-/tmp}/templates.$$"
            ;;
        missing)
            cli_ref=${2:?usage: $0 missing <ref>}
            cli_inputs=$(bake_inputs "${repo}")
            cli_tests=$(sh "${here}/scope.sh" --all | jq -r '.[]' | tr -d '\r')
            for cli_test in ${cli_tests}; do
                baked "${cli_test}"
            done | sort -u | while read -r cli_options; do
                cli_key=$(key_of "${cli_options}" "${cli_inputs}")
                cli_found=$(gh api -X GET "repos/${GITHUB_REPOSITORY:?}/actions/caches" -f key="${cli_key}" \
                    -f ref="${cli_ref}" --jq "[.actions_caches[] | select(.key == \"${cli_key}\")] | length")
                [ "${cli_found}" != 0 ] || jq -nc --arg o "${cli_options}" --arg k "${cli_key}" '{options: $o, key: $k}'
            done | jq -sc .
            ;;
        bake)
            work=$(readlink -f "${2:?usage: $0 bake <work directory> <key> <options>...}")
            cli_key=${3:?usage: $0 bake <work directory> <key> <options>...}
            shift 3
            cli_inputs=$(bake_inputs "${repo}")
            cli_want=$(key_of "$*" "${cli_inputs}")
            [ "${cli_want}" = "${cli_key}" ] || {
                echo "template $* has the key ${cli_want} on this tree, not ${cli_key}" >&2
                exit 1
            }
            # shellcheck source=/dev/null
            . "${work}/env"
            # The pool the management cluster's module would make.
            pve pvesh create /pools --poolid kubehenge-templates
            [ -x "${repo}/dist/kubehenge" ] || make -s -C "${repo}" build
            KUBEHENGE_E2E_KEEP_TEMPLATES=1
            made 1 "$@"
            ;;
        *)
            echo "usage: $0 keys <test> | missing <ref> | bake <work directory> <key> <options>..." >&2
            exit 2
            ;;
    esac
fi
