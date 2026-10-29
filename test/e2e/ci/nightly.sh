#!/bin/sh
# Prints, as a JSON list, the end-to-end tests of the nightly run on a day, given as days since
# 1970-01-01 UTC, today by default:
#
#   test/e2e/ci/nightly.sh [day]
#
# Every test the merge queue may run (scope.sh --all) and those only the nightly run runs, then each
# workload distribution once more with options (variant.sh): a CNI of its class other than the one
# its own test runs, a node OS other than the default, and every other night no CIS hardening. The
# choice moves on each night, and from one distribution to the next, so that a few nights cover
# every CNI and node OS of every class, each with hardening and without.
set -eu

here=$(dirname "$0")
chart=${here}/../../../charts/kubehenge
day=${1:-$(($(date -u +%s) / 86400))}
case ${day} in
    '' | *[!0-9]*)
        echo "usage: $0 [days since 1970-01-01]" >&2
        exit 2
        ;;
    *) ;;
esac

# pick <n> <word>...: the word at n, counting round.
pick() {
    pick_n=$1
    shift
    shift $((pick_n % $#))
    printf '%s' "$1"
}
# cnis <distribution>: its class's CNIs but the one its own test runs: the class's default, or for
# kubeadm, whose class has none, Cilium (run.sh).
cnis() {
    cnis_all=$(awk '/- name: cni$/ { f = 1 } f && /enum:/ { gsub(/.*\[|\].*/, ""); gsub(/,/, " "); print; exit }' "${chart}/templates/$1.yaml")
    cnis_own=$(awk '/- name: cni$/ { f = 1 } f && /default:/ { print $2; exit } f && /include/ { exit }' "${chart}/templates/$1.yaml")
    [ -n "${cnis_all}" ] || {
        echo "no CNIs found in ${chart}/templates/$1.yaml" >&2
        exit 1
    }
    for cni in ${cnis_all}; do
        [ "${cni}" = "${cnis_own:-cilium}" ] || printf '%s ' "${cni}"
    done
}
# jq on Windows ends its lines with CR LF.
oses=$(jq -r '(.default | "\(.family)-\(.release)") as $d | .available[] | select(. != $d)' "${chart}/files/node-oses.json" | tr -d '\r')
[ -n "${oses}" ] || {
    echo "no node OS but the default in ${chart}/files/node-oses.json" >&2
    exit 1
}

variants=
i=0
for distribution in kubeadm rke2 k3s talos; do
    n=$((day + i))
    others=$(cnis "${distribution}")
    # The CNI moves every night, hardening once the CNIs have come round, and the node OS once
    # both have: every combination comes in turn.
    # shellcheck disable=SC2086 # one CNI per word
    count=$(printf '%s ' ${others} | wc -w)
    # shellcheck disable=SC2086 # one CNI per word
    name=${distribution}+$(pick "${n}" ${others})
    # shellcheck disable=SC2086 # one node OS per word
    [ "${distribution}" = talos ] || name=${name}+$(pick $((n / (count * 2))) ${oses})
    [ $((n / count % 2)) = 1 ] || name=${name}+cis-off
    variants="${variants} ${name}"
    i=$((i + 1))
done

# shellcheck disable=SC2086 # one test per word
"${here}/scope.sh" --all | jq -c '. + $ARGS.positional' --args management-cluster-upgrade rancher control-plane-ha ${variants}
