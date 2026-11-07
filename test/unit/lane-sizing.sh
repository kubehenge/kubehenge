#!/bin/sh
# Unit test of the runner's memory room (test/e2e/ci/room.sh): the floors a node's balloon may take
# it down to, that every test in balloon_tests fits the runner with its nodes at those floors and
# the headroom left, and how test/e2e/ci/guests.sh sets the floors, against stand-ins for docker
# (Proxmox VE's qm), pgrep, date and sleep. Needs no Proxmox VE.
#
#   test/unit/lane-sizing.sh
set -eu

here=$(dirname "$0")
repo=${here}/../..
guests=${repo}/test/e2e/ci/guests.sh
# shellcheck source=../e2e/ci/room.sh
. "${repo}/test/e2e/ci/room.sh"
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# same <what> <wanted> <got>
same() {
    if [ "$2" = "$3" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}
# at_most <what> <limit> <got>
at_most() {
    if [ "$3" -le "$2" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  at most: %s\n  got:     %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}

# The floors.
balloon_floor 3120 4096
same "a node's floor is its need and the guest's reserve" 3312 "${floor}"
balloon_floor 1001 4096
same "the floor is in steps of 8" 1200 "${floor}"
balloon_floor 1000 4096
same "a floor already in a step stays" 1192 "${floor}"
balloon_floor 3904 4096
same "a node that needs all its memory keeps it" 4096 "${floor}"
balloon_floor 5000 4096
same "a node that needs more keeps its memory too" 4096 "${floor}"

# The tests' figures come from the class and the chart that sizes the nodes, as the workload test
# reads them.
rke2=${repo}/charts/kubehenge/templates/rke2.yaml
helpers=${repo}/charts/kubehenge/templates/_helpers.tpl
figure() { # <annotation> <key> <file>
    sed -n "s/^ *kubehenge.dev\/$1: '\(.*\)'\$/\1/p" "$3" | jq -r ".$2"
}
node_control_plane=$(figure node-memory controlPlane "${rke2}")
node_worker=$(figure node-memory worker "${rke2}")
load_balancer=$(figure addon-memory loadBalancer "${helpers}")
rancher_agent=$(figure addon-memory rancherAgent "${helpers}")
least_control_plane=$(figure control-plane-sizes '[0].memoryMiB' "${rke2}")
least_worker=$(sed -n 's/.*"workerMinMemoryMiB" \([0-9]*\).*/\1/p' "${rke2}")
management=$(sed -n 's/.*KUBEHENGE_E2E_MANAGEMENT_MEMORY_MIN:-\([0-9]*\)}.*/\1/p' "${repo}/test/e2e/ci/run.sh")
management_nominal=4096
[ -n "${node_control_plane}" ] && [ -n "${node_worker}" ] && [ -n "${load_balancer}" ] && [ -n "${rancher_agent}" ] &&
    [ -n "${least_control_plane}" ] && [ -n "${least_worker}" ] && [ -n "${management}" ] || {
    echo "FAIL: a sizing figure was not found in the chart or run.sh"
    exit 1
}

# These tests run no add-on, so a node needs its own use and the load balancer and Rancher's agent
# on a worker.
worker_need=$((node_worker + load_balancer + rancher_agent))
worker=$((worker_need > least_worker ? worker_need : least_worker))
control_plane=$((node_control_plane > least_control_plane ? node_control_plane : least_control_plane))
balloon_floor "${worker_need}" "${worker}"
worker_floor=${floor}
balloon_floor "${node_control_plane}" "${control_plane}"
control_plane_floor=${floor}
budget=$((runner_mib - runner_fixed_mib - runner_headroom_mib))

# fits <what> <control planes> <workers>: whether the guests at their peak fit the budget at their
# floors, and that they would not at their sizes, so that the floors are what makes the room.
fits() {
    vms=$(($2 + $3 + 1))
    sizes=$((management_nominal + $2 * control_plane + $3 * worker + vms * qemu_overhead_mib))
    floors=$((management + $2 * control_plane_floor + $3 * worker_floor + vms * qemu_overhead_mib))
    at_most "$1 at its floors leaves the headroom" "${budget}" "${floors}"
    if [ "${sizes}" -gt "${budget}" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s needs no floors, at %s MiB of %s\n' "$1" "${sizes}" "${budget}"
        failures=$((failures + 1))
    fi
}
for balloon_test in ${balloon_tests}; do
    case ${balloon_test} in
        rke2-etcd) fits "${balloon_test}, a control plane replaced" 2 1 ;;
        rke2-machines)
            fits "${balloon_test}, a control plane replaced" 2 1
            fits "${balloon_test}, two workers" 1 2
            ;;
        *)
            echo "FAIL: ${balloon_test} is in balloon_tests, and this test does not know its peak"
            failures=$((failures + 1))
            ;;
    esac
done

# guests.sh sets a floor once per guest that a pattern names, and none for the management node.
bin=${root}/bin
mkdir "${bin}" "${root}/proc" "${root}/proc/7101" "${root}/proc/7102" "${root}/proc/7103"
printf -- '-id\000101\000-name\000e2e-rke2-abc12345-xyz\000' > "${root}/proc/7101/cmdline"
printf -- '-id\000102\000-name\000e2e-rke2-abc12345-md-0-uvw-rst\000' > "${root}/proc/7102/cmdline"
printf -- '-id\000103\000-name\000kubehenge-mgmt-1\000' > "${root}/proc/7103/cmdline"
printf 'MemTotal: 16000000 kB\nMemAvailable: %s kB\n' "$((8192 * 1024))" > "${root}/proc/meminfo"
printf 'e2e-rke2-abc12345-md-* 1536\ne2e-rke2-abc12345-* 3312\n' > "${root}/balloon.txt"
cat > "${bin}/docker" << EOF
#!/bin/sh
echo "\$*" >> "${root}/asked"
EOF
cat > "${bin}/pgrep" << 'EOF'
#!/bin/sh
printf '7101\n7102\n7103\n'
EOF
cat > "${bin}/date" << EOF
#!/bin/sh
printf '00:00:%02d\n' \$((\$(cat "${root}/now") % 60))
EOF
cat > "${bin}/sleep" << EOF
#!/bin/sh
echo \$((\$(cat "${root}/now") + 15)) > "${root}/now"
rounds=\$((\$(cat "${root}/rounds" 2> /dev/null || echo 0) + 1))
echo "\${rounds}" > "${root}/rounds"
[ "\${rounds}" -lt 5 ]
EOF
chmod +x "${bin}"/*
echo 0 > "${root}/now"
env PATH="${bin}:${PATH}" KUBEHENGE_E2E_GUESTS_PROC="${root}/proc" KUBEHENGE_E2E_BALLOON_FILE="${root}/balloon.txt" \
    sh "${guests}" record "${root}/guests.txt" > /dev/null 2>&1 || true
asked=$(cat "${root}/asked" 2> /dev/null || true)
same "each node is set once, the worker by its name and the control plane by the cluster's" \
    "exec kubehenge-e2e-pve qm set 101 --balloon 3312
exec kubehenge-e2e-pve qm set 102 --balloon 1536" "${asked}"
notes=$(cat "${root}/guests.txt.balloons")
same "the notes hold each VM with its floor" "00:00:00 101 3312
00:00:00 102 1536" "${notes}"
summary=$(sh "${guests}" summary "${root}/guests.txt" 2>&1 || true)
same "the summary lists them" "  101: 3312 MiB, set at 00:00:00" "$(printf '%s\n' "${summary}" | grep '^  101:' || true)"

rm -f "${root}/asked" "${root}/guests.txt.balloons" "${root}/rounds"
echo 0 > "${root}/now"
env PATH="${bin}:${PATH}" KUBEHENGE_E2E_GUESTS_PROC="${root}/proc" KUBEHENGE_E2E_BALLOON_FILE="${root}/none" \
    sh "${guests}" record "${root}/guests.txt" > /dev/null 2>&1 || true
same "without a floors file no balloon is touched" "" "$(cat "${root}/asked" 2> /dev/null || true)"

echo "lane-sizing: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
