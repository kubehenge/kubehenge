#!/bin/sh
# shellcheck disable=SC2154 # ns and work are the sourcing test's.
# Backups with Velero for the end-to-end tests that switch them on: an S3 store the test serves, and
# Velero's objects made in a cluster as the interface makes them, through the interface's own
# identities there. Sourced after common.sh by a test whose Clusters are in ${ns} and whose own
# folder is ${work}.
#
# KUBEHENGE_E2E_BACKUP_TARGET names a backup target of the management cluster's whose endpoint is on
# this machine, at an https:// address the clusters' nodes reach; the test serves SeaweedFS's S3
# gateway there with KUBEHENGE_E2E_BACKUP_CERT and KUBEHENGE_E2E_BACKUP_KEY, which
# KUBEHENGE_E2E_BACKUP_CA, the target's CA, signed.

s3_pid=

# s3_store <ClusterClass JSON>: serves the target the class offers as KUBEHENGE_E2E_BACKUP_TARGET
# with SeaweedFS, in s3_pid, with a bucket made: the target in target, its address, bucket and
# region in s3_endpoint, bucket and region, and s3 reaching it with the store's keys.
s3_store() {
    target=$(printf '%s' "$1" | jq -c --arg t "${KUBEHENGE_E2E_BACKUP_TARGET}" \
        '.metadata.annotations["kubehenge.dev/backup-targets"] // "[]" | fromjson | .[] | select(.name == $t)')
    [ -n "${target}" ] || fail "the class offers no backup target ${KUBEHENGE_E2E_BACKUP_TARGET}"
    s3_endpoint=$(printf '%s' "${target}" | jq -r .endpoint)
    bucket=$(printf '%s' "${target}" | jq -r .bucket)
    region=$(printf '%s' "${target}" | jq -r '.region // "us-east-1"')
    s3_port=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.urlsplit(sys.argv[1]).port or 443)' "${s3_endpoint}")
    seaweedfs "${work}"
    s3_key=e2e$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')
    s3_secret=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
    jq -n --arg k "${s3_key}" --arg s "${s3_secret}" \
        '{identities: [{name: "e2e", credentials: [{accessKey: $k, secretKey: $s}], actions: ["Admin", "Read", "Write", "List", "Tagging"]}]}' \
        > "${work}/s3.json"
    seaweedfs_server "${work}" "${s3_port}" "${work}/s3.json" "${KUBEHENGE_E2E_BACKUP_CERT}" "${KUBEHENGE_E2E_BACKUP_KEY}"
    # shellcheck disable=SC2034 # the sourcing test stops the store with it.
    s3_pid=$!
    until_true 120 "the S3 store takes a bucket" s3 -X PUT -o /dev/null "${s3_endpoint}/${bucket}"
}
s3() { curl -sS --fail --cacert "${KUBEHENGE_E2E_BACKUP_CA}" --aws-sigv4 "aws:amz:${region}:s3" --user "${s3_key}:${s3_secret}" "$@"; }

# target_folder <cluster> <folder>: where a folder of the cluster's is in the bucket, as Kubehenge
# names it.
target_folder() {
    printf '%s' "${target}" | jq -r --arg own "${ns}.$1/$2" 'if (.prefix // "") == "" then $own else .prefix + "/" + $own end'
}

# backup_credential <cluster>: the store's keys for the cluster, kept beside it as the interface
# keeps a cluster's own key for its target.
backup_credential() {
    kubectl -n "${ns}" create secret generic "$1.backup" --type=kubehenge.dev/user-credential \
        --from-literal=accessKeyID="${s3_key}" --from-literal=secretAccessKey="${s3_secret}"
    kubectl -n "${ns}" label secret "$1.backup" kubehenge.dev/user-credential=backup "kubehenge.dev/credential-cluster=$1"
}

# velero_ready <kubeconfig>: Velero and its node agent running in the cluster, reaching the store.
velero_ready() {
    until_true 900 "Velero installed" env KUBECONFIG="$1" kubectl -n velero rollout status deployment/velero --timeout=10s
    until_true 600 "Velero's node agent on every node" env KUBECONFIG="$1" kubectl -n velero rollout status daemonset/node-agent --timeout=10s
    until_true 300 "Velero reaches the S3 store" velero_location_available "$1" default
}
# velero_location_available <kubeconfig> <location>: whether Velero reaches the location.
velero_location_available() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    phase=$(KUBECONFIG="$1" kubectl -n velero get backupstoragelocation "$2" -o jsonpath='{.status.phase}') && [ "${phase}" = Available ]
}

# as_identity <kubeconfig> <Secret> <kubectl arguments...>: kubectl in the kubeconfig's cluster as
# the interface's identity whose token the Secret beside the Cluster holds.
as_identity() {
    identity_config=$1
    identity_token=$(kubectl -n "${ns}" get secret "$2" -o jsonpath='{.data.token}' | base64 -d)
    shift 2
    identity_server=$(KUBECONFIG="${identity_config}" kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    KUBECONFIG="${identity_config}" kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' |
        base64 -d > "${identity_config}.identity-ca"
    kubectl --kubeconfig=/dev/null --server="${identity_server}" --certificate-authority="${identity_config}.identity-ca" \
        --token="${identity_token}" "$@"
}

# velero_phase <kubeconfig> <kind> <name> <phase>: whether a Velero object has reached the phase;
# any phase it ends in otherwise fails the test.
velero_phase() {
    # shellcheck disable=SC2310 # polled with set -e off, so a failed request is only a retry.
    phase=$(KUBECONFIG="$1" kubectl -n velero get "$2" "$3" -o jsonpath='{.status.phase}') || return 1
    case ${phase} in
        "$4") ;;
        Failed | FailedValidation | PartiallyFailed)
            KUBECONFIG="$1" kubectl -n velero get "$2" "$3" -o yaml >&2
            # The Deployment's selector takes in the node agents' pods too, so by the server's own
            # label; its CLI fetches what went wrong from the store.
            velero_server=$(KUBECONFIG="$1" kubectl -n velero get pods -l name=velero -o name | head -n 1)
            # shellcheck disable=SC2310 # what it prints is only for the reader; the test fails below.
            KUBECONFIG="$1" kubectl -n velero exec "${velero_server}" -c velero -- /velero "$2" describe "$3" --details >&2 || true
            # shellcheck disable=SC2310 # as above.
            KUBECONFIG="$1" kubectl -n velero logs "${velero_server}" -c velero --tail=100 >&2 || true
            fail "$2 $3 ended ${phase}"
            ;;
        *) return 1 ;;
    esac
}

# back_up_now <kubeconfig> <cluster> <backup>: a backup asked for as the interface's Back up now asks
# for one, by the cluster's backups identity: the schedule's own, made at once.
back_up_now() {
    backup_template=$(KUBECONFIG="$1" kubectl -n velero get schedule velero-scheduled -o json | jq -c .spec.template)
    jq -n --arg n "$3" --argjson spec "${backup_template}" \
        '{apiVersion: "velero.io/v1", kind: "Backup", metadata: {name: $n, namespace: "velero"}, spec: $spec}' |
        as_identity "$1" "$2-backups" create -f -
}

# restore_backup <kubeconfig> <cluster> <restore> <spec>: a restore as the interface asks for one, by
# the cluster's restores identity, waited for until it has finished.
restore_backup() {
    jq -n --arg n "$3" --argjson spec "$4" \
        '{apiVersion: "velero.io/v1", kind: "Restore", metadata: {name: $n, namespace: "velero"}, spec: $spec}' |
        as_identity "$1" "$2-restores" create -f -
    until_true 1200 "restore $3 completed" velero_phase "$1" restore "$3" Completed
}

# volume_claim <kubeconfig> <namespace>: a claim named data of the smallest volume.
volume_claim() {
    KUBECONFIG="$1" kubectl apply -f - << EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: $2
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
EOF
}

# volume_pod <kubeconfig> <namespace> <name>: a pod with the claim data mounted at /data.
volume_pod() {
    KUBECONFIG="$1" kubectl apply -f - << EOF
apiVersion: v1
kind: Pod
metadata:
  name: $3
  namespace: $2
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: data
      image: ${agnhost}
      args: [pause]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
EOF
}
