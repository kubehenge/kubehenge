#!/bin/sh
# Sourced by test/e2e/ci/run.sh and test/unit/nightly.sh.
#
# variant <test>: the settings an end-to-end test's name carries, as exports to evaluate. A workload
# distribution's test (kubeadm, rke2, k3s, talos), the halves of rke2's and k3s's (rke2-etcd,
# rke2-machines and so on) and its add-on parts (kubeadm-addons-a and so on) take options after
# their name, each after a +: its CNI, its node OS as one word such as rocky-10, or cis-off for a cluster without CIS hardening, as in kubeadm+calico+rocky-10+cis-off; the test
# itself is the name before the first +. Every test run.sh applies a management cluster for takes
# management-<MiB>, its node's memory, as in management-cluster-upgrade+management-3584. Refuses an
# option it does not know, or one the test does not take.
variant() {
    variant_test=${1%%+*}
    [ "$1" != "${variant_test}" ] || return 0
    variant_distribution=${variant_test%-addons-?}
    case ${variant_distribution} in
        kubeadm | rke2 | k3s | talos) variant_workload=true ;;
        rke2-etcd | rke2-machines | k3s-etcd | k3s-machines)
            variant_distribution=${variant_distribution%-*}
            variant_workload=true
            ;;
        *) variant_workload=false ;;
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
            management-*)
                case ${variant_option#management-} in
                    '' | 0* | *[!0-9]*)
                        echo "option ${variant_option} of test ${variant_test} is not management-<MiB>" >&2
                        return 2
                        ;;
                    *) ;;
                esac
                if [ "${variant_test}" = appliance ] || [ "${variant_test}" = appliance-upgrade ]; then
                    echo "the appliance sets its management node up itself, so ${variant_test} takes no ${variant_option}" >&2
                    return 2
                fi
                printf 'export KUBEHENGE_E2E_MANAGEMENT_MEMORY=%s\n' "${variant_option#management-}"
                continue
                ;;
            *) ;;
        esac
        if [ "${variant_workload}" = false ]; then
            echo "test ${variant_test} takes no option but management-<MiB>, not ${variant_option}" >&2
            return 2
        fi
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
