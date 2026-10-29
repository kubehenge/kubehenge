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
trap 'rm -f "${seen}"' EXIT
nights=24
night=0
while [ "${night}" -lt "${nights}" ]; do
    list=$(sh "${ci}/nightly.sh" $((20000 + night)))
    if printf '%s' "${list}" | jq -e 'type == "array" and length == (unique | length)' > /dev/null; then ok; else bad "night ${night}: not a list of distinct tests: ${list}"; fi
    # jq on Windows ends its lines with CR LF.
    for t in $(sh "${ci}/scope.sh" --all | jq -r '.[]' | tr -d '\r') management-cluster-upgrade rancher control-plane-ha; do
        if printf '%s' "${list}" | jq -e --arg t "${t}" 'index($t) != null' > /dev/null; then ok; else bad "night ${night} leaves ${t} out"; fi
    done
    for name in $(printf '%s' "${list}" | jq -r '.[] | select(contains("+"))' | tr -d '\r'); do
        # shellcheck disable=SC2310 # its refusal is the failure reported.
        if ! settings=$(variant "${name}" 2>&1); then
            bad "${name} does not read: ${settings}"
            continue
        fi
        # The settings carry nothing but what was asked for.
        KUBEHENGE_E2E_CNI='' KUBEHENGE_E2E_NODE_OS='' KUBEHENGE_E2E_CIS=''
        eval "${settings}"
        test=${name%%+*}
        enum=$(awk '/- name: cni$/ { f = 1 } f && /enum:/ { gsub(/.*\[|\].*/, ""); gsub(/,/, " "); print; exit }' "${chart}/templates/${test}.yaml")
        case " ${enum} " in
            *" ${KUBEHENGE_E2E_CNI} "*) ok ;;
            *) bad "${name}: ${test}'s class has no CNI ${KUBEHENGE_E2E_CNI}" ;;
        esac
        if [ "${test}" = talos ] && [ -z "${KUBEHENGE_E2E_NODE_OS}" ]; then
            ok
        elif [ "${test}" = talos ]; then
            bad "${name} gives Talos a node OS"
        elif jq -e --arg os "${KUBEHENGE_E2E_NODE_OS}" '.available | index($os) != null' "${chart}/files/node-oses.json" > /dev/null; then
            ok
        else
            bad "${name}: no node OS ${KUBEHENGE_E2E_NODE_OS}"
        fi
        printf '%s %s %s %s\n' "${test}" "${KUBEHENGE_E2E_CNI}" "${KUBEHENGE_E2E_NODE_OS:--}" "${KUBEHENGE_E2E_CIS:-true}" >> "${seen}"
    done
    night=$((night + 1))
done

# Every other CNI of each class, on every other node OS, hardened and not.
others=$(jq -r '(.default | "\(.family)-\(.release)") as $d | .available[] | select(. != $d)' "${chart}/files/node-oses.json" | tr -d '\r')
for distribution in kubeadm rke2 k3s talos; do
    enum=$(awk '/- name: cni$/ { f = 1 } f && /enum:/ { gsub(/.*\[|\].*/, ""); gsub(/,/, " "); print; exit }' "${chart}/templates/${distribution}.yaml")
    own=$(awk '/- name: cni$/ { f = 1 } f && /default:/ { print $2; exit } f && /include/ { exit }' "${chart}/templates/${distribution}.yaml")
    oses=${others}
    [ "${distribution}" != talos ] || oses=-
    for cni in ${enum}; do
        [ "${cni}" != "${own:-cilium}" ] || continue
        for os in ${oses}; do
            for cis in true false; do
                if grep -qxF "${distribution} ${cni} ${os} ${cis}" "${seen}"; then ok; else bad "in ${nights} nights no ${distribution} on ${cni}, node OS ${os}, CIS ${cis}"; fi
            done
        done
    done
done

refused() {
    # shellcheck disable=SC2310 # its refusal is what is looked for.
    if out=$(variant "$1" 2>&1); then
        bad "$1 was read: ${out}"
    else
        ok
    fi
}
refused talos+rocky-10
refused appliance+calico
refused control-plane-ha+calico
refused k3s+weave
refused 'k3s+cilium;true'
# shellcheck disable=SC2016 # the substitution is the text refused.
refused 'k3s+$(true)'
refused 'k3s++cilium'
refused 'k3s+Cilium'
# A test without options carries no settings.
# shellcheck disable=SC2310 # a refusal is reported as the failure.
if out=$(variant control-plane-ha 2>&1) && [ -z "${out}" ]; then ok; else bad "control-plane-ha carries settings: ${out}"; fi
if grep -q 'control-plane-ha)' "${ci}/run.sh"; then ok; else bad "run.sh does not run control-plane-ha"; fi

printf 'nightly: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
