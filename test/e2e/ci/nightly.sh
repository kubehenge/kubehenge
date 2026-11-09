#!/bin/sh
# Prints, as a JSON list, the end-to-end tests of the nightly run on a day, given as days since
# 1970-01-01 UTC, today by default:
#
#   test/e2e/ci/nightly.sh [day]
#
# Every test a pull request may run (scope.sh --all) and those only the nightly run runs, among them
# kubeadm's test on IPv4 and IPv6 (dual-stack). Then every part of each workload distribution's test
# once more with options (variant.sh): a CNI of its class
# other than the one its own test runs, a node OS other than the default, and every other night no
# CIS hardening. The choice moves on each night, and from one distribution to the next, so that a
# few nights cover every CNI and node OS of every class, each with hardening and without.
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

# Everything below is read and split in the shell: each process started costs the unit test of
# this script most of its time on Windows, and it runs this script for many days.
#
# pick <n> <word>...: sets picked to the word at n, counting round.
pick() {
    pick_n=$1
    shift
    shift $((pick_n % $#))
    picked=$1
}
# cnis <distribution>: sets others to its class's CNIs but the one its own test runs, each followed
# by a blank, and count to how many there are. The one its own test runs is the class's default, or
# for kubeadm, whose class has none, Cilium (run.sh).
cnis() {
    cnis_file=${chart}/templates/$1.yaml
    cnis_all='' cnis_own='' cnis_in=false cnis_all_done=false cnis_own_done=false
    while IFS= read -r cnis_line; do
        case ${cnis_line} in
            *'- name: cni') cnis_in=true ;;
            *) ;;
        esac
        [ "${cnis_in}" = true ] || continue
        if [ "${cnis_all_done}" = false ]; then
            case ${cnis_line} in
                *enum:*)
                    cnis_all=${cnis_line##*\[}
                    cnis_all=${cnis_all%%\]*}
                    while :; do
                        case ${cnis_all} in
                            *,*) cnis_all="${cnis_all%%,*} ${cnis_all#*,}" ;;
                            *) break ;;
                        esac
                    done
                    cnis_all_done=true
                    ;;
                *) ;;
            esac
        fi
        if [ "${cnis_own_done}" = false ]; then
            case ${cnis_line} in
                *default:*)
                    # shellcheck disable=SC2086 # the line's blank-separated fields, as awk reads them
                    set -- ${cnis_line}
                    cnis_own=${2:-}
                    cnis_own_done=true
                    ;;
                *include*) cnis_own_done=true ;;
                *) ;;
            esac
        fi
        if [ "${cnis_all_done}" = true ] && [ "${cnis_own_done}" = true ]; then break; fi
    done < "${cnis_file}"
    [ -n "${cnis_all}" ] || {
        echo "no CNIs found in ${cnis_file}" >&2
        exit 1
    }
    others='' count=0
    for cni in ${cnis_all}; do
        if [ "${cni}" != "${cnis_own:-cilium}" ]; then
            others="${others}${cni} "
            count=$((count + 1))
        fi
    done
}
# jq -j prints no line ends, which jq on Windows ends with CR LF.
oses=$(jq -j '(.default | "\(.family)-\(.release)") as $d | .available[] | select(. != $d) | . + " "' "${chart}/files/node-oses.json")
[ -n "${oses}" ] || {
    echo "no node OS but the default in ${chart}/files/node-oses.json" >&2
    exit 1
}

# The lanes as a list of JSON strings, `["a", "b,c"]`, as scope.sh prints them: one blank-separated
# word each.
lanes_json=$("${here}/scope.sh" --all)
lanes=${lanes_json#\[\"}
lanes=${lanes%\"\]}
while :; do
    case ${lanes} in
        *'", "'*) lanes="${lanes%%\", \"*} ${lanes#*\", \"}" ;;
        *) break ;;
    esac
done
variants=
i=0
for distribution in kubeadm rke2 k3s talos; do
    n=$((day + i))
    cnis "${distribution}"
    # The CNI moves every night, hardening once the CNIs have come round, and the node OS once
    # both have: every combination comes in turn.
    # shellcheck disable=SC2086 # one CNI per word
    pick "${n}" ${others}
    name=${distribution}+${picked}
    if [ "${distribution}" != talos ]; then
        # shellcheck disable=SC2086 # one node OS per word
        pick $((n / (count * 2))) ${oses}
        name=${name}+${picked}
    fi
    [ $((n / count % 2)) = 1 ] || name=${name}+cis-off
    # Every other lane downloads RKE2 and k3s from the runner's mirror. These download from GitHub,
    # as a cluster without a mirror does.
    case ${distribution} in
        rke2 | k3s) name=${name}+github-releases ;;
        *) ;;
    esac
    # The same options on each of its parts, in the same lanes as a pull request's: a part that
    # shares one runs the test it shares it with after it, as there.
    for lane in ${lanes}; do
        first=${lane%%,*}
        case ${first} in
            "${distribution}" | "${distribution}"-etcd | "${distribution}"-machines | "${distribution}"-addons-?)
                variants="${variants} ${first}${name#"${distribution}"}${lane#"${first}"}"
                ;;
            *) ;;
        esac
    done
    i=$((i + 1))
done

# shellcheck disable=SC2086 # one test per word
jq -c '. + $ARGS.positional' --args management-cluster-upgrade rancher control-plane-ha two-nodes appliance-upgrade kubeadm+calico+dual-stack ${variants} << EOF
${lanes_json}
EOF
