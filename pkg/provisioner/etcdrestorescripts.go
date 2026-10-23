package provisioner

import (
	"bytes"
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// RestoreWorkDir is where a restore keeps its files on a control-plane node: the snapshot, and
	// what it held back of the control plane while it was stopped.
	RestoreWorkDir = "/var/lib/kubehenge-restore"
	// RestoreSnapshotPath is where the restore reads the plain snapshot from on the node it restores.
	RestoreSnapshotPath = RestoreWorkDir + "/snapshot.db"

	// The transcoded snapshot is encrypted as openssl's enc reads it, since every node has openssl
	// and none has age. The iterations only slow guessing a password, and this one is random.
	opensslMagic      = "Salted__"
	opensslSaltSize   = 8
	opensslIterations = 10000
	opensslKeySize    = 32
	opensslPassword   = 32
)

// The pieces of the restore scripts. Each runs as root under bash, after restoreCommon.
const (
	restoreCommon = `set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/rke2/bin:/var/lib/rancher/rke2/bin
work=` + RestoreWorkDir + `
umask 077
mkdir -p "$work"
wait_for() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    if ((SECONDS >= deadline)); then
      echo "timed out waiting for $*" >&2
      return 1
    fi
    sleep 2
  done
}
`

	// kubeadm runs etcd and the API server as static pods: the kubelet stops them once their
	// manifests are gone.
	kubeadmStop = `mkdir -p "$work/manifests"
for m in etcd kube-apiserver; do
  if [ -f "/etc/kubernetes/manifests/$m.yaml" ]; then
    mv "/etc/kubernetes/manifests/$m.yaml" "$work/manifests/"
  fi
done
stopped() { [ -z "$(crictl ps -q --name '^(etcd|kube-apiserver)$')" ]; }
wait_for 300 stopped
echo "etcd and the API server are stopped"
`

	// etcdutl comes from the etcd image the node already runs, so the restore writes the database
	// the version of etcd that will open it.
	kubeadmRestore = `manifest="$work/manifests/etcd.yaml"
if [ ! -f "$manifest" ]; then
  echo "etcd is not stopped here: $manifest is missing" >&2
  exit 1
fi
flag() { sed -n "s/^ *- --$1=//p" "$manifest" | head -n 1; }
name=$(flag name)
peer=$(flag initial-advertise-peer-urls)
data=$(flag data-dir)
image=$(sed -n 's/^ *image: *//p' "$manifest" | head -n 1)
if [ -z "$name" ] || [ -z "$peer" ] || [ -z "$data" ] || [ -z "$image" ]; then
  echo "cannot read etcd's settings from $manifest" >&2
  exit 1
fi
rm -rf "$work/data"
# A run cut short leaves its container behind, whose name the next run needs.
ctr -n k8s.io container rm kubehenge-etcd-restore >/dev/null 2>&1 || true
ctr -n k8s.io run --rm --mount "type=bind,src=$work,dst=$work,options=rbind:rw" "$image" kubehenge-etcd-restore \
  etcdutl snapshot restore "$work/snapshot.db" --name "$name" --initial-cluster "$name=$peer" \
  --initial-advertise-peer-urls "$peer" --data-dir "$work/data"
old="$data.before-restore-$(date -u +%Y%m%dT%H%M%SZ)"
mv "$data" "$old"
mv "$work/data" "$data"
touch "$work/restored"
echo "restored etcd into $data; its database from before is in $old"
`

	kubeadmStart = `for m in etcd kube-apiserver; do
  if [ -f "$work/manifests/$m.yaml" ]; then
    mv "$work/manifests/$m.yaml" /etc/kubernetes/manifests/
  fi
done
ready() { kubectl --kubeconfig /etc/kubernetes/admin.conf --server https://127.0.0.1:6443 --tls-server-name kubernetes get --raw /readyz >/dev/null 2>&1; }
`

	// rke2-killall.sh stops the static pods, etcd among them, that stopping the service leaves.
	rke2Stop = `systemctl stop rke2-server
rke2-killall.sh >/dev/null
echo "RKE2 is stopped"
`
	k3sStop = `systemctl stop k3s
echo "k3s is stopped"
`

	// RKE2 and k3s refuse to reset a server that names another to join, so the server line leaves
	// the configuration for the reset alone.
	rancherRestore = `config=/etc/rancher/%[1]s/config.yaml
if [ ! -f "$work/config.yaml" ]; then
  cp -p "$config" "$work/config.yaml"
fi
trap 'cp -p "$work/config.yaml" "$config"' EXIT
sed -i '/^server:/d' "$config"
%[1]s server --cluster-reset --cluster-reset-restore-path="$work/snapshot.db" --etcd-s3=false
touch "$work/restored"
echo "restored etcd from the snapshot"
`
	// The service starts without waiting: on a node of several that are all stopped, it would wait
	// for the others.
	rancherStart = `if [ -f "$work/config.yaml" ]; then
  cp -p "$work/config.yaml" /etc/rancher/%[1]s/config.yaml
fi
systemctl start --no-block %[2]s
ready() { %[3]s get --raw /readyz >/dev/null 2>&1; }
`

	startWait = `wait_for 600 ready
rm -rf "$work"
echo "the API server is ready"
`
	startNoWait = `rm -f "$work/snapshot.db" "$work/snapshot.enc" "$work/restored"
echo "started the control plane"
`

	// fetchScript downloads the transcoded snapshot from a link, decrypts it and checks it against
	// the checksum the snapshot was stored with. The link, password and the target's CA, empty when
	// it has none, are set before it: a store with a CA of its own is one the node does not trust.
	fetchScript = `trust=()
if [ -n "$ca" ]; then
  printf '%%s\n' "$ca" >"$work/store-ca.pem"
  trust=(--cacert "$work/store-ca.pem")
fi
curl -fsS --retry 3 "${trust[@]}" --config - -o "$work/snapshot.enc" <<<"url = \"$url\""
rm -f "$work/store-ca.pem"
KUBEHENGE_PASSWORD="$password" openssl enc -d -aes-256-ctr -pbkdf2 -iter %d -md sha256 \
  -pass env:KUBEHENGE_PASSWORD -in "$work/snapshot.enc" -out "$work/snapshot.db"
rm -f "$work/snapshot.enc"
echo "$sha256  $work/snapshot.db" | sha256sum -c --quiet -
echo "fetched the snapshot: $(stat -c %%s "$work/snapshot.db") bytes"
`
	markerCheck = `test -e "$work/restored"
`
	cleanScript = `rm -rf "$work"
`
)

// RestoreScripts are the bash scripts a restore runs as root on control-plane nodes.
type RestoreScripts struct {
	// Stop stops etcd and the API server; it runs on every control-plane node.
	Stop string
	// Restore restores etcd on one stopped node from RestoreSnapshotPath.
	Restore string
	// Start starts the control plane again and waits for its API server.
	Start string
	// StartNoWait starts it without waiting, as on nodes whose etcd has no quorum alone.
	StartNoWait string
	// Restored exits 0 once Restore has replaced the node's etcd database.
	Restored string
	// Clean removes what a restore that went no further than fetching left on the node.
	Clean string
}

// restoreScriptsFor are a distribution's restore scripts.
func restoreScriptsFor(d templates.Distribution) (RestoreScripts, error) {
	var stop, restore, start string
	switch d {
	case templates.Kubeadm:
		stop, restore, start = kubeadmStop, kubeadmRestore, kubeadmStart
	case templates.RKE2:
		stop = rke2Stop
		restore = fmt.Sprintf(rancherRestore, "rke2")
		start = fmt.Sprintf(rancherStart, "rke2", "rke2-server", "kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml")
	case templates.K3s:
		stop = k3sStop
		restore = fmt.Sprintf(rancherRestore, "k3s")
		start = fmt.Sprintf(rancherStart, "k3s", "k3s", "k3s kubectl")
	case templates.Talos:
	}
	if stop == "" {
		return RestoreScripts{}, fmt.Errorf("restoring etcd is not offered for %s clusters", d)
	}
	return RestoreScripts{
		Stop:        script(stop),
		Restore:     script(restore),
		Start:       script(start + startWait),
		StartNoWait: script(start + startNoWait),
		Restored:    script(markerCheck),
		Clean:       script(cleanScript),
	}, nil
}

// script makes a script whole. Its body is one group with nothing on its standard input, so that
// bash reads all of it before running any, and no command inside reads the script itself, which
// reaches bash on its standard input.
func script(body string, vars ...string) string {
	var sb strings.Builder
	for i := 0; i+1 < len(vars); i += 2 {
		fmt.Fprintf(&sb, "%s=%s\n", vars[i], shellQuote(vars[i+1]))
	}
	sb.WriteString("{\n")
	sb.WriteString(restoreCommon)
	sb.WriteString(body)
	sb.WriteString("} </dev/null\n")
	return sb.String()
}

// shellQuote quotes s for bash as one word, taken literally.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fetchScriptFor downloads a transcoded snapshot from link onto a node, trusting the store by ca
// when it is set, and checks it.
func fetchScriptFor(link, password, sum, ca string) string {
	return script(fmt.Sprintf(fetchScript, opensslIterations), "url", link, "password", password, "sha256", sum, "ca", ca)
}

// newRestorePassword is the password a snapshot is transcoded with for one restore.
func newRestorePassword() (string, error) {
	b := make([]byte, opensslPassword)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("making a password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// opensslSeal encrypts r as "openssl enc -aes-256-ctr -pbkdf2 -iter 10000 -md sha256" does, as it
// is read.
func opensslSeal(r io.Reader, password string) (io.Reader, error) {
	salt := make([]byte, opensslSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("making a salt: %w", err)
	}
	stream, err := opensslStream(password, salt)
	if err != nil {
		return nil, err
	}
	header := append([]byte(opensslMagic), salt...)
	return io.MultiReader(bytes.NewReader(header), cipher.StreamReader{S: stream, R: r}), nil
}

// opensslStream is the AES-256-CTR stream openssl's enc derives from a password and salt with
// PBKDF2: the key, then the counter's start.
func opensslStream(password string, salt []byte) (cipher.Stream, error) {
	keyIV, err := pbkdf2.Key(sha256.New, password, salt, opensslIterations, opensslKeySize+aes.BlockSize)
	if err != nil {
		return nil, fmt.Errorf("deriving the key: %w", err)
	}
	block, err := aes.NewCipher(keyIV[:opensslKeySize])
	if err != nil {
		return nil, fmt.Errorf("deriving the key: %w", err)
	}
	return cipher.NewCTR(block, keyIV[opensslKeySize:]), nil
}

// EtcdRestorePlan is how a cluster's etcd is restored from a snapshot by hand, step by step, for
// a management cluster that does not restore through the guest agent.
type EtcdRestorePlan struct {
	Snapshot     EtcdSnapshot  `json:"snapshot"`
	Distribution string        `json:"distribution"`
	Steps        []RestoreStep `json:"steps"`
	// Replaced are the control-plane machines Cluster API makes again, empty for a cluster of one.
	Replaced []string `json:"replaced,omitempty"`
}

// RestoreStep is one step of a restore by hand: a command and where it runs.
type RestoreStep struct {
	Title string `json:"title"`
	// Where is the management cluster, or the control-plane nodes the command runs on as root.
	Where   string `json:"where"`
	Command string `json:"command"`
}

// RestoreNode is a control-plane machine of a cluster and the node it runs.
type RestoreNode struct {
	Machine string
	Node    string
	VMID    int64
	// Address is where a Talos node serves its API.
	Address string

	object *clusterv1.Machine
}

// controlPlaneMachines lists a cluster's control-plane machines that are not being deleted, by
// name.
func controlPlaneMachines(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) ([]RestoreNode, error) {
	var machines clusterv1.MachineList
	err := c.List(ctx, &machines, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return nil, fmt.Errorf("listing the control-plane machines: %w", err)
	}
	var out []RestoreNode
	for i := range machines.Items {
		m := &machines.Items[i]
		if m.DeletionTimestamp.IsZero() {
			out = append(out, RestoreNode{Machine: m.Name, Node: m.Status.NodeRef.Name, object: m})
		}
	}
	slices.SortFunc(out, func(a, b RestoreNode) int { return strings.Compare(a.Machine, b.Machine) })
	return out, nil
}

// controlPlaneNodes are controlPlaneMachines with their VMs.
func controlPlaneNodes(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) ([]RestoreNode, error) {
	nodes, err := controlPlaneMachines(ctx, c, cluster)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if _, nodes[i].VMID, err = proxmoxMachineOf(ctx, c, nodes[i].object); err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

// PlanEtcdRestore writes out the steps that restore a cluster's etcd from a snapshot by hand, the
// snapshot downloaded onto its first control-plane node.
func PlanEtcdRestore(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster, snapshot *EtcdSnapshot) (*EtcdRestorePlan, error) {
	if err := Restorable(cluster, snapshot); err != nil {
		return nil, err
	}
	if isTalos(cluster) {
		return planTalosRestore(ctx, c, cluster, snapshot)
	}
	scripts, err := restoreScriptsFor(distributionOf(cluster))
	if err != nil {
		return nil, err
	}
	nodes, err := controlPlaneMachines(ctx, c, cluster)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New("the cluster has no control-plane machines")
	}
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = cmp.Or(n.Node, n.Machine)
	}
	first := names[0]
	plan := &EtcdRestorePlan{Snapshot: *snapshot, Distribution: string(distributionOf(cluster))}
	patch := fmt.Sprintf("kubectl --namespace %s patch cluster %s --type merge --patch ", cluster.Namespace, cluster.Name)
	const mgmt = "the management cluster"
	plan.Steps = append(plan.Steps,
		RestoreStep{"Pause the cluster, so that Cluster API replaces no machine meanwhile", mgmt, patch + `'{"spec":{"paused":true}}'`},
		RestoreStep{"Stop etcd and the API server", strings.Join(names, ", "), asRoot(scripts.Stop)},
		RestoreStep{
			"Copy the downloaded snapshot to " + RestoreSnapshotPath + " as root, and check it", first,
			fmt.Sprintf("echo '%s  %s' | sudo sha256sum -c -", snapshot.SHA256, RestoreSnapshotPath),
		},
		RestoreStep{"Restore etcd from the snapshot", first, asRoot(scripts.Restore)},
		RestoreStep{"Start the control plane again", first, asRoot(scripts.Start)},
	)
	if len(nodes) > 1 {
		for _, n := range nodes[1:] {
			plan.Replaced = append(plan.Replaced, n.Machine)
		}
		plan.Steps = append(plan.Steps, RestoreStep{
			"Replace the other control-plane machines, whose etcd still holds the cluster from before", mgmt,
			fmt.Sprintf("kubectl --namespace %s delete machine --wait=false %s", cluster.Namespace, strings.Join(plan.Replaced, " ")),
		})
	}
	plan.Steps = append(plan.Steps, RestoreStep{"Let Cluster API look after the cluster again", mgmt, patch + `'{"spec":{"paused":false}}'`})
	return plan, nil
}

// asRoot is a script as a command to paste into a shell on a node.
func asRoot(s string) string {
	return "sudo bash <<'KUBEHENGE'\n" + s + "KUBEHENGE"
}

// Restorable refuses a snapshot a cluster cannot be restored from: one of another distribution, or
// of another Kubernetes minor, whose objects the cluster's API server may not read.
func Restorable(cluster *clusterv1.Cluster, s *EtcdSnapshot) error {
	if d := string(distributionOf(cluster)); s.Distribution != "" && s.Distribution != d {
		return fmt.Errorf("snapshot %s is of a %s cluster, and this one is %s", s.Name, s.Distribution, d)
	}
	want, known := kubernetesMinor(s.KubernetesVersion)
	if !known {
		return nil
	}
	if have, ok := kubernetesMinor(cluster.Spec.Topology.Version); !ok || have != want {
		return fmt.Errorf("snapshot %s holds the objects of Kubernetes v1.%d and the cluster runs %s: restore one taken on its minor",
			s.Name, want, cluster.Spec.Topology.Version)
	}
	return nil
}
