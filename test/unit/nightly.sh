#!/bin/sh
# Unit test of test/e2e/ci/nightly.sh, the nightly run's list of end-to-end tests, and of
# test/e2e/ci/variant.sh, which reads the options in a test's name: over enough nights every
# combination of each class's other CNIs, the other node OSes and hardening comes up, every name
# reads as run.sh reads it, and a name variant.sh cannot read is refused.
#
#   test/unit/nightly.sh
set -eu

here=$(dirname "$0")
ci=${here}/../e2e/ci
chart=${here}/../../charts/kubehenge
# shellcheck source=../e2e/ci/variant.sh
. "${ci}/variant.sh"

passes=0 failures=0
ok() { passes=$((passes + 1)); }
bad() {
    printf 'FAIL: %s\n' "$*"
    failures=$((failures + 1))
}

seen=$(mktemp)
scratch=$(mktemp)
trap 'rm -f "${seen}" "${scratch}"' EXIT
# slurp <file>: sets slurped to the file's text without its last line end, as $(cat file) reads it.
slurp() {
    slurped=''
    slurp_sep=''
    while IFS= read -r slurp_line || [ -n "${slurp_line}" ]; do
        slurped=${slurped}${slurp_sep}${slurp_line}
        slurp_sep='
'
    done < "$1"
}
# variant_of <test>: runs variant in this shell, as a command substitution would start a process for
# it. Sets variant_status to its status and settings to what it printed, the errors included.
variant_of() {
    # shellcheck disable=SC2310 # its status is the answer.
    if variant "$1" > "${scratch}" 2>&1; then variant_status=0; else variant_status=$?; fi
    slurp "${scratch}"
    settings=${slurped}
}
# bare <lane>: sets bare to the lane's tests without their options.
strip_options() {
    bare='' rest=$1,
    while [ -n "${rest}" ]; do
        part=${rest%%,*}
        rest=${rest#*,}
        bare=${bare:+${bare},}${part%%+*}
    done
}

# The lists below are read once and matched in the shell, as each process started costs most of
# this test's time on Windows. Each holds its words between spaces. jq -j prints no line ends,
# which jq on Windows ends with CR LF.
lanes_and_parts=$(sh "${ci}/scope.sh" --all | jq -j '(.[] | . + " "), "|", (.[] | gsub(","; " ") + " ")')
lanes=${lanes_and_parts%%|*}
# The dual-stack lane is the nightly run's own, kept out of the rotation's checks below.
dual_stack=kubeadm+calico+dual-stack
queue=" ${lanes} management-cluster-upgrade rancher control-plane-ha two-nodes appliance-upgrade "
all_lanes=" ${lanes_and_parts#*|}"
node_oses=" $(jq -j '.available[] | . + " "' "${chart}/files/node-oses.json")"
# cnis <distribution>: the CNIs its class offers.
cnis() {
    awk '/- name: cni$/ { f = 1 } f && /enum:/ { gsub(/.*\[|\].*/, ""); gsub(/,/, " "); print; exit }' "${chart}/templates/$1.yaml"
}
cnis_kubeadm=$(cnis kubeadm)
cnis_rke2=$(cnis rke2)
cnis_k3s=$(cnis k3s)
cnis_talos=$(cnis talos)
nights=24
night=0
while [ "${night}" -lt "${nights}" ]; do
    sh "${ci}/nightly.sh" $((20000 + night)) > "${scratch}"
    parsed=$(jq -j 'if type == "array" and length == (unique | length) then "ok " else "bad " end, (if type == "array" then .[] | . + " " else empty end)' "${scratch}")
    case ${parsed} in
        'ok '*) ok ;;
        *)
            slurp "${scratch}"
            bad "night ${night}: not a list of distinct tests: ${slurped}"
            ;;
    esac
    tests=" ${parsed#* }"
    for t in ${queue} ${dual_stack}; do
        case ${tests} in
            *" ${t} "*) ok ;;
            *) bad "night ${night} leaves ${t} out" ;;
        esac
    done
    # Every lane is one the merge queue runs, its tests without their options, or one of the
    # nightly run's own: none runs only at night by accident.
    for lane in ${tests}; do
        strip_options "${lane}"
        case ${queue} in
            *" ${bare} "*) ok ;;
            *) bad "night ${night} runs the lane ${lane}, which the merge queue does not run as ${bare}" ;;
        esac
    done
    for lane in ${tests}; do
        case ${lane} in
            "${dual_stack}") continue ;;
            *+*) ;;
            *) continue ;;
        esac
        # The options are the first test's, and the lane's other test runs as in the merge queue.
        name=${lane%%,*}
        case ${lane#"${name}"} in
            *+*) bad "night ${night}: ${lane} gives options to the test after ${name}" ;;
            *) ok ;;
        esac
        # shellcheck disable=SC2310 # its refusal is the failure reported.
        variant_of "${name}"
        if [ "${variant_status}" -ne 0 ]; then
            bad "${name} does not read: ${settings}"
            continue
        fi
        # The settings carry nothing but what was asked for.
        KUBEHENGE_E2E_CNI='' KUBEHENGE_E2E_NODE_OS='' KUBEHENGE_E2E_CIS='' KUBEHENGE_E2E_RELEASES=''
        eval "${settings}"
        test=${name%%+*}
        test=${test%-addons-?}
        test=${test%-etcd}
        test=${test%-machines}
        # RKE2's and k3s's rotation downloads from GitHub, the path of a cluster without a mirror.
        case ${test}:${KUBEHENGE_E2E_RELEASES} in
            rke2:github | k3s:github | kubeadm: | talos:) ok ;;
            *) bad "${name} downloads its releases from '${KUBEHENGE_E2E_RELEASES:-the runner}'" ;;
        esac
        case ${test} in
            kubeadm) enum=${cnis_kubeadm} ;;
            rke2) enum=${cnis_rke2} ;;
            k3s) enum=${cnis_k3s} ;;
            talos) enum=${cnis_talos} ;;
            *) enum='' ;;
        esac
        case " ${enum} " in
            *" ${KUBEHENGE_E2E_CNI} "*) ok ;;
            *) bad "${name}: ${test}'s class has no CNI ${KUBEHENGE_E2E_CNI}" ;;
        esac
        if [ "${test}" = talos ] && [ -z "${KUBEHENGE_E2E_NODE_OS}" ]; then
            ok
        elif [ "${test}" = talos ]; then
            bad "${name} gives Talos a node OS"
        else
            case ${node_oses} in
                *" ${KUBEHENGE_E2E_NODE_OS} "*) ok ;;
                *) bad "${name}: no node OS ${KUBEHENGE_E2E_NODE_OS}" ;;
            esac
        fi
        printf '%s %s %s %s\n' "${test}" "${KUBEHENGE_E2E_CNI}" "${KUBEHENGE_E2E_NODE_OS:--}" "${KUBEHENGE_E2E_CIS:-true}" >> "${seen}"
        # Each variant runs as every part of its distribution's test, in the part's own lane.
        parts=0
        for part in ${lanes}; do
            first=${part%%,*}
            case ${first} in
                "${test}" | "${test}"-etcd | "${test}"-machines | "${test}"-addons-?) ;;
                *) continue ;;
            esac
            parts=$((parts + 1))
            twin=${first}+${name#*+}${part#"${first}"}
            case ${tests} in
                *" ${twin} "*) ok ;;
                *) bad "night ${night} runs ${name} without ${twin}" ;;
            esac
        done
        if [ "${parts}" -ge 3 ]; then ok; else bad "${test} runs in ${parts} parts, not its lifecycle and at least two add-on parts"; fi
        # Every step of the lifecycle runs with the options: whole, or as both its halves.
        lifecycle=no
        case ${all_lanes} in
            *" ${test} "*) lifecycle=yes ;;
            *" ${test}-etcd "*)
                case ${all_lanes} in
                    *" ${test}-machines "*) lifecycle=yes ;;
                    *) ;;
                esac
                ;;
            *) ;;
        esac
        if [ "${lifecycle}" = yes ]; then ok; else bad "${test} runs no lifecycle, whole or in halves"; fi
    done
    night=$((night + 1))
done

# Every other CNI of each class, on every other node OS, hardened and not.
others=$(jq -j '(.default | "\(.family)-\(.release)") as $d | .available[] | select(. != $d) | . + " "' "${chart}/files/node-oses.json")
slurp "${seen}"
seen_lines="
${slurped}
"
for distribution in kubeadm rke2 k3s talos; do
    case ${distribution} in
        kubeadm) enum=${cnis_kubeadm} ;;
        rke2) enum=${cnis_rke2} ;;
        k3s) enum=${cnis_k3s} ;;
        *) enum=${cnis_talos} ;;
    esac
    own=$(awk '/- name: cni$/ { f = 1 } f && /default:/ { print $2; exit } f && /include/ { exit }' "${chart}/templates/${distribution}.yaml")
    oses=${others}
    [ "${distribution}" != talos ] || oses=-
    for cni in ${enum}; do
        [ "${cni}" != "${own:-cilium}" ] || continue
        for os in ${oses}; do
            for cis in true false; do
                case ${seen_lines} in
                    *"
${distribution} ${cni} ${os} ${cis}
"*) ok ;;
                    *) bad "in ${nights} nights no ${distribution} on ${cni}, node OS ${os}, CIS ${cis}" ;;
                esac
            done
        done
    done
done

refused() {
    variant_of "$1"
    if [ "${variant_status}" -eq 0 ]; then
        bad "$1 was read: ${settings}"
    else
        ok
    fi
}
refused talos+rocky-10
refused talos-addons-a+rocky-10
refused appliance-addons-a+calico
refused k3s-addons+calico
refused k3s-addons-ab+calico
refused talos-machines+calico
refused kubeadm-etcd+calico
refused appliance+calico
refused control-plane-ha+calico
refused k3s+weave
refused 'k3s+cilium;true'
# shellcheck disable=SC2016 # the substitution is the text refused.
refused 'k3s+$(true)'
refused 'k3s++cilium'
refused 'k3s+Cilium'
refused appliance+management-3584
refused appliance-upgrade+management-3584
refused appliance-upgrade+calico
refused management-cluster+management-
refused management-cluster+management-0
refused management-cluster+management-035
refused management-cluster+management-35x
refused management-cluster+management-3584+calico
refused appliance+dual-stack
refused management-cluster+ipv6-first
refused control-plane-ha+dual-stack
refused kubeadm+github-releases
refused talos-addons-a+github-releases
refused two-nodes+github-releases
# reads <name> <settings>: the settings the name carries, one per line.
reads() {
    variant_of "$1"
    if [ "${variant_status}" -eq 0 ] && [ "${settings}" = "$2" ]; then ok; else bad "$1 reads as '${settings}', not '$2'"; fi
}
reads management-cluster-upgrade+management-3584 'export KUBEHENGE_E2E_MANAGEMENT_MEMORY=3584'
reads control-plane-ha+management-5120 'export KUBEHENGE_E2E_MANAGEMENT_MEMORY=5120'
reads kubeadm-addons-a+calico+management-4096 'export KUBEHENGE_E2E_CNI=calico
export KUBEHENGE_E2E_MANAGEMENT_MEMORY=4096'
reads rke2+calico 'export KUBEHENGE_E2E_CNI=calico'
reads kubeadm+calico+dual-stack 'export KUBEHENGE_E2E_CNI=calico
export KUBEHENGE_E2E_IP_FAMILIES=IPv4,IPv6'
reads rke2-etcd+cilium+ipv6-first 'export KUBEHENGE_E2E_CNI=cilium
export KUBEHENGE_E2E_IP_FAMILIES=IPv6,IPv4'
reads k3s-addons-b+flannel+github-releases 'export KUBEHENGE_E2E_CNI=flannel
export KUBEHENGE_E2E_RELEASES=github'
reads rke2-machines+github-releases 'export KUBEHENGE_E2E_RELEASES=github'
# A test without options carries no settings.
# shellcheck disable=SC2310 # a refusal is reported as the failure.
variant_of control-plane-ha
if [ "${variant_status}" -eq 0 ] && [ -z "${settings}" ]; then ok; else bad "control-plane-ha carries settings: ${settings}"; fi
runs=' '
while IFS= read -r line; do
    case ${line} in
        '    control-plane-ha)'*) runs="${runs}control-plane-ha " ;;
        '    two-nodes)'*) runs="${runs}two-nodes " ;;
        '    appliance-upgrade)'*) runs="${runs}appliance-upgrade " ;;
        *) ;;
    esac
done < "${ci}/run.sh"
for t in control-plane-ha two-nodes appliance-upgrade; do
    case ${runs} in
        *" ${t} "*) ok ;;
        *) bad "run.sh does not run ${t}" ;;
    esac
done

printf 'nightly: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
