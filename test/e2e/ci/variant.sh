#!/bin/sh
# Sourced by test/e2e/ci/run.sh and test/unit/nightly.sh.
#
# variant <test>: the settings an end-to-end test's name carries, as exports to evaluate. A workload
# distribution's test (kubeadm, rke2, k3s, talos) and its add-on half (kubeadm-addons and so on)
# take options after their name, each after a +: its CNI, its node OS as one word such as rocky-10,
# or cis-off for a cluster without CIS hardening, as in kubeadm+calico+rocky-10+cis-off; the test
# itself is the name before the first +. Refuses an option it does not know, or one the test does
# not take.
variant() {
    variant_test=${1%%+*}
    [ "$1" != "${variant_test}" ] || return 0
    variant_distribution=${variant_test%-addons}
    case ${variant_distribution} in
        kubeadm | rke2 | k3s | talos) ;;
        *)
            echo "test ${variant_test} takes no options: $1" >&2
            return 2
            ;;
    esac
    variant_ifs=${IFS}
    IFS=+
    # shellcheck disable=SC2086 # split on IFS, set to + for the options.
    set -- ${1#*+}
    IFS=${variant_ifs}
    for variant_option in "$@"; do
        # What is printed is evaluated.
        case ${variant_option} in
            '' | *[!a-z0-9.-]*)
                echo "option '${variant_option}' of test ${variant_test} is not a word of a-z, 0-9, . and -" >&2
                return 2
                ;;
            *) ;;
        esac
        case ${variant_option} in
            flannel | canal | calico | cilium) printf 'export KUBEHENGE_E2E_CNI=%s\n' "${variant_option}" ;;
            cis-off) echo 'export KUBEHENGE_E2E_CIS=false' ;;
            [a-z]*-[0-9]*)
                if [ "${variant_distribution}" = talos ]; then
                    echo "Talos nodes run Talos, not ${variant_option}" >&2
                    return 2
                fi
                printf 'export KUBEHENGE_E2E_NODE_OS=%s\n' "${variant_option}"
                ;;
            *)
                echo "unknown option ${variant_option} of test ${variant_test}" >&2
                return 2
                ;;
        esac
    done
}
