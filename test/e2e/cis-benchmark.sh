#!/bin/sh
# shellcheck disable=SC2154 # distribution and work are workload-cluster.sh's, which sources this.
# The CIS Kubernetes Benchmark for a workload cluster hardened with the cis variable, sourced by
# workload-cluster.sh, whose wl, work, distribution and fail it uses.
#
# kube-bench checks a control-plane node against the benchmark's control-plane, etcd and node
# sections, and a worker against its node section, and every check that fails fails the test,
# unless it is one of the exceptions below. The policies section is left out: it judges what runs
# in the cluster, which is its users', not how Kubehenge builds it. kubeadm is checked against the
# CIS benchmark itself, RKE2 and k3s against their own, as Rancher's security-scan carries them.

# kube-bench's latest release predates the benchmark's 2.0, so it runs with the 2.0 configuration
# from its main branch at this commit. security-scan carries kube-bench and the RKE2 and k3s
# benchmarks together.
kube_bench_image=docker.io/aquasec/kube-bench:v0.16.0@sha256:75506f222d1eb6ce2a751a5533bdc0a3b54c898e2e49e7751d0ee22cfb862679
kube_bench_commit=e278a5915a982f2756026de6e2f9ca77077ef9f9
security_scan_image=docker.io/rancher/security-scan:v0.10.2@sha256:2c7e270a01771d97b46842149033e272394351fa6a0729598d62d9a2d8eee9e8

# cis_exceptions: the checks the cluster fails by design, each with why, as a JSON list.
cis_exceptions() {
    case ${distribution} in
        # 1.1.12: kubeadm runs etcd as root, so its data belongs to root, not to an etcd user.
        # Kubernetes has no supported way to run kubeadm's etcd as another user.
        kubeadm) echo '["1.1.12"]' ;;
        *) echo '[]' ;;
    esac
}

# cis_job <name> <control plane: true|false> <targets>: a kube-bench Job on a node of that kind,
# reading the host's configuration through read-only mounts and its processes through its PID
# namespace.
cis_job() {
    # Only what the node has: a missing path would be made on the host, or fail the pod. /var/lib
    # holds the kubelet's, etcd's, RKE2's and k3s's data and the CNI's address leases; Rancher's
    # benchmarks read k3s's components' arguments from the journal.
    host_paths="/var/lib /var/log/journal /run/log/journal /etc/systemd /lib/systemd"
    case ${distribution} in
        kubeadm)
            image=${kube_bench_image} config=/opt/kube-bench/cfg benchmark=cis-2.0
            host_paths="${host_paths} /etc/kubernetes /etc/cni/net.d /opt/cni/bin"
            ;;
        rke2)
            image=${security_scan_image} config=/etc/kube-bench/cfg benchmark=rke2-cis-2.0
            host_paths="${host_paths} /etc/rancher /etc/cni/net.d"
            ;;
        *)
            image=${security_scan_image} config=/etc/kube-bench/cfg benchmark=${distribution}-cis-2.0
            host_paths="${host_paths} /etc/rancher"
            ;;
    esac
    operator=DoesNotExist
    [ "$2" = true ] && operator=Exists
    mounts='' volumes='' index=0
    for path in ${host_paths}; do
        mounts="${mounts}$(printf '\n            - {name: host-%s, mountPath: %s, readOnly: true}' "${index}" "${path}")"
        volumes="${volumes}$(printf '\n        - {name: host-%s, hostPath: {path: %s, type: Directory}}' "${index}" "${path}")"
        index=$((index + 1))
    done
    mounts="${mounts}$(printf '\n            - {name: host-bin, mountPath: /usr/local/mount-from-host/bin, readOnly: true}')"
    volumes="${volumes}$(printf '\n        - {name: host-bin, hostPath: {path: /usr/bin, type: Directory}}')"
    if [ "${distribution}" = kubeadm ]; then
        mounts="${mounts}$(printf '\n            - {name: config, mountPath: %s, readOnly: true}\n            - {name: benchmark, mountPath: %s/cis-2.0, readOnly: true}' "${config}" "${config}")"
        volumes="${volumes}$(printf '\n        - {name: config, configMap: {name: kube-bench-config}}\n        - {name: benchmark, configMap: {name: kube-bench-cis-2.0}}')"
    fi
    cat << EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $1
  namespace: e2e-cis
spec:
  backoffLimit: 0
  template:
    spec:
      hostPID: true
      restartPolicy: Never
      tolerations: [{operator: Exists}]
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions: [{key: node-role.kubernetes.io/control-plane, operator: ${operator}}]
      containers:
        - name: kube-bench
          image: ${image}
          command: [kube-bench, run, --benchmark, ${benchmark}, --targets, "$3", --config-dir, ${config}, --json, --noremediations]
          volumeMounts:${mounts}
      volumes:${volumes}
EOF
}

cis_benchmark() {
    wl create namespace e2e-cis
    # kube-bench reads the host's processes and files, which only Pod Security's privileged level allows.
    wl label namespace e2e-cis pod-security.kubernetes.io/enforce=privileged
    if [ "${distribution}" = kubeadm ]; then
        raw=https://raw.githubusercontent.com/aquasecurity/kube-bench/${kube_bench_commit}/cfg
        mkdir -p "${work}/kube-bench/cis-2.0"
        curl -sSfL -o "${work}/kube-bench/config.yaml" "${raw}/config.yaml"
        for f in config master node controlplane etcd policies; do
            curl -sSfL -o "${work}/kube-bench/cis-2.0/${f}.yaml" "${raw}/cis-2.0/${f}.yaml"
        done
        wl -n e2e-cis create configmap kube-bench-config --from-file="${work}/kube-bench/config.yaml"
        wl -n e2e-cis create configmap kube-bench-cis-2.0 --from-file="${work}/kube-bench/cis-2.0"
    fi
    cis_job cis-control-plane true master,controlplane,etcd,node > "${work}/cis-control-plane.yaml"
    cis_job cis-worker false node > "${work}/cis-worker.yaml"
    wl apply -f "${work}/cis-control-plane.yaml" -f "${work}/cis-worker.yaml"
    exceptions=$(cis_exceptions)
    for job in cis-control-plane cis-worker; do
        if ! wl -n e2e-cis wait --for=condition=Complete "job/${job}" --timeout=10m; then
            wl -n e2e-cis logs "job/${job}" >&2
            fail "kube-bench did not finish on the ${job#cis-}"
        fi
        # kube-bench writes its report as one JSON line, its own log lines beside it.
        wl -n e2e-cis logs "job/${job}" | grep '^{' > "${work}/${job}.json" ||
            fail "kube-bench reported nothing on the ${job#cis-}"
        jq -r '.Controls[] | "\(.text): \(.total_pass) passed, \(.total_fail) failed, \(.total_warn) to check by hand"' "${work}/${job}.json"
        failed=$(jq -r --argjson except "${exceptions}" \
            '[.Controls[].tests[].results[] | select(.status == "FAIL" and (.test_number | IN($except[]) | not)) | "\(.test_number) \(.test_desc)"] | .[]' \
            "${work}/${job}.json")
        [ -z "${failed}" ] || fail "the ${job#cis-} fails the CIS benchmark:
${failed}"
    done
    wl delete namespace e2e-cis --wait --timeout=5m
}
