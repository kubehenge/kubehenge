#!/bin/sh
# The VM templates the end-to-end tests need, and the keys the Actions cache keeps the baked ones
# under. run.sh sources this file.
#
#   test/e2e/ci/templates.sh keys <lane>       # key1=<key> and key2=<key>
#   test/e2e/ci/templates.sh current           # the keys of the merge queue's tests' templates
#   test/e2e/ci/templates.sh missing <ref>     # those the cache lacks on <ref>, as JSON
#   test/e2e/ci/templates.sh bake <work directory> <key> <options>...
#   test/e2e/ci/templates.sh made <work directory> <slot> <options>...
#
# keys prints one key for each template of kubeadm, RKE2 or k3s the test makes, in the order run.sh
# makes them. Talos's templates are only downloaded, not baked, and get none. missing lists, from
# every test the merge queue runs, the templates whose keys the repository's cache lacks on <ref>,
# for e2e-templates.yml (needs the GitHub CLI with actions: read). bake makes one template on the
# Proxmox VE up.sh started and backs it up to <work directory>/vz/template-1 for the cache. made
# restores a test's template in the slot or makes it, as run.sh does, during the test.
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

# baked <lane>: the options of each template of the lane's tests a bake makes, one per line, each
# once. A lane is a test or two, as in k3s-etcd,cluster-mesh (scope.sh).
baked() {
    for baked_test in $(printf '%s' "$1" | tr , ' '); do
        templates_of "${baked_test}"
    done | sed -n '/^talos/!s/ *$//p' | awk '!seen[$0]++'
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
# pve_in: pve with this shell's standard input.
pve_in() { docker exec -i kubehenge-e2e-pve "$@"; }

# vzdump leaves out every disk set to backup=0, as a template's disks are, so a template is backed
# up with the option cleared on a copy of its config, and the restore sets it again. These read a
# VM config on standard input.
# no_backup_drives: the drives of the config that backups leave out, one per line.
no_backup_drives() { sed -n '/^\[/q; s/^\([a-z]*[0-9]*\): .*,backup=0\(,.*\)\{0,1\}$/\1/p'; }
# backed_up: the config with every drive in its backups.
backed_up() { sed 's/,backup=0//'; }
# backup_off <drive>...: the config with the drives given left out of backups again.
backup_off() {
    backup_off_script=
    for backup_off_drive in "$@"; do
        backup_off_script="${backup_off_script}s/^\(${backup_off_drive}: [^,]*\)/\1,backup=0/;"
    done
    sed "${backup_off_script}"
}

# made <slot> <options>: restores the template or makes it. Slot 0 is never kept.
made() {
    made_slot=$1
    shift
    made_kept=/var/lib/vz/template-${made_slot}
    # qmrestore reads the archive's format from vzdump's own file name, so the backup keeps it.
    made_archive=
    if [ "${made_slot}" != 0 ]; then
        for made_file in "${work}/vz/template-${made_slot}"/vzdump-qemu-*.vma.zst; do
            [ ! -s "${made_file}" ] || made_archive=${made_kept}/${made_file##*/}
        done
    fi
    if [ -n "${made_archive}" ]; then
        made_id=$(pve pvesh get /cluster/nextid)
        pve qmrestore "${made_archive}" "${made_id}" --storage "${KUBEHENGE_E2E_SHARED_STORAGE:-local}" \
            --pool kubehenge-templates > /dev/null
        made_conf=/etc/pve/qemu-server/${made_id}.conf
        made_drives=$(cat "${work}/vz/template-${made_slot}/no-backup")
        # shellcheck disable=SC2086 # one drive per word
        pve cat "${made_conf}" | backup_off ${made_drives} |
            pve_in sh -c "cat > /tmp/restored.conf && cat /tmp/restored.conf > ${made_conf}"
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
    # The builder adds the VM's hardware after the ID: "template <tags> is VM 100 (host, SSD, discard)".
    made_id=$(printf '%s\n' "${made_out}" | sed -n 's/^template .* is VM \([0-9][0-9]*\)\( (.*)\)\{0,1\}$/\1/p' | tail -n 1)
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
    made_conf=/etc/pve/qemu-server/${made_id}.conf
    pve cp "${made_conf}" "${made_kept}.conf"
    pve cat "${made_kept}.conf" | no_backup_drives | pve_in sh -c "cat > ${made_kept}/no-backup"
    pve cat "${made_kept}.conf" | backed_up | pve_in sh -c "cat > ${made_conf}"
    made_dump=0
    # shellcheck disable=SC2310 # the config is put back before the status is returned.
    pve vzdump "${made_id}" --dumpdir "${made_kept}" --compress zstd --quiet 1 || made_dump=$?
    pve sh -c "cat ${made_kept}.conf > ${made_conf} && rm ${made_kept}.conf"
    [ "${made_dump}" = 0 ] || return "${made_dump}"
    pve sh -c "rm -f ${made_kept}/vzdump-qemu-*.log ${made_kept}/vzdump-qemu-*.notes"
    made_size=$(du -ch "${work}/vz/template-${made_slot}"/vzdump-qemu-*.vma.zst | tail -n 1)
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
        current)
            cli_inputs=$(bake_inputs "${repo}")
            cli_tests=$(sh "${here}/scope.sh" --all | jq -r '.[]' | tr -d '\r')
            for cli_test in ${cli_tests}; do
                baked "${cli_test}"
            done | sort -u | while read -r cli_options; do
                key_of "${cli_options}" "${cli_inputs}"
            done
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
        made)
            work=$(readlink -f "${2:?usage: $0 made <work directory> <slot> <options>...}")
            cli_slot=${3:?usage: $0 made <work directory> <slot> <options>...}
            shift 3
            # shellcheck source=/dev/null
            . "${work}/env"
            cli_out=$(made "${cli_slot}" "$@")
            printf '%s\n' "${cli_out}"
            # A restored template showed its boot log when it was baked.
            case "${cli_out}" in
                *"restored from the cache as VM"* | *"hands out its boot log and cloud-init's output through the guest agent"*) ;;
                *)
                    echo "template $* never showed its boot log" >&2
                    exit 1
                    ;;
            esac
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
            echo "usage: $0 keys <test> | current | missing <ref> | bake <work directory> <key> <options>..." \
                "| made <work directory> <slot> <options>..." >&2
            exit 2
            ;;
    esac
fi
