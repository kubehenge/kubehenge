package provisioner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// WorkloadClients reaches the workload clusters' API servers. found is false until Cluster API has
// made a cluster's kubeconfig.
type WorkloadClients interface {
	Client(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, bool, error)
}

// A snapshot on the backup volume reaches a kubeadm, RKE2 or k3s node through the cluster's own API
// server, the way the controller pulls its snapshots: nothing on the management cluster opens to the
// workload network. The controller writes it, encrypted for this restore, into Secrets in
// kube-system, and the node reads them back with its admin kubeconfig before etcd stops. etcd's
// images hold no shell, so no pod could take it in from an exec instead.
const (
	// handOverChunk is the bytes of the snapshot one Secret holds, well within a Secret's 1 MiB.
	handOverChunk = 512 << 10
	// maxHandOver bounds a snapshot handed over this way: its Secrets sit in the cluster's etcd
	// beside what it holds, within a quarter of the distributions' default quota of 2 GiB.
	maxHandOver = 512 << 20
	// handOverLabel marks the Secrets of one hand-over, by its ID.
	handOverLabel = "kubehenge.dev/etcd-restore"
	handOverKey   = "chunk"
)

// errVolumeUnreached refuses a kubeadm, RKE2 or k3s restore from the volume where the controller
// reaches no workload API server.
var errVolumeUnreached = errors.New("the controller reaches no workload API server, through which a snapshot on the backup volume gets to the node. Download it and restore by hand")

// ErrHandOverAPIDown says why a kubeadm, RKE2 or k3s restore from the volume cannot start: the
// cluster's API server, the snapshot's way to the node, does not answer.
var ErrHandOverAPIDown = errors.New("the cluster's API server does not answer, and a snapshot on the backup volume reaches the node only through it. Download the snapshot and restore by hand, or restore from a bucket target")

// adminKubeconfig is where a control-plane node keeps its distribution's admin kubeconfig.
func adminKubeconfig(d templates.Distribution) string {
	switch d {
	case templates.RKE2:
		return "/etc/rancher/rke2/rke2.yaml"
	case templates.K3s:
		return "/etc/rancher/k3s/k3s.yaml"
	default:
		return "/etc/kubernetes/admin.conf"
	}
}

// handOver writes the snapshot, encrypted with password, into Secrets of the cluster, has the first
// node read and check it, and deletes the Secrets again.
func (run *restoreRun) handOver(ctx context.Context, password string) error {
	if run.r.Workloads == nil {
		return errVolumeUnreached
	}
	workload, found, err := run.r.Workloads.Client(ctx, run.cluster)
	if err != nil {
		return fmt.Errorf("reaching the cluster's API server: %w", err)
	}
	if !found {
		return ErrHandOverAPIDown
	}
	raw := make([]byte, restoreObjectID)
	if _, err = rand.Read(raw); err != nil {
		return fmt.Errorf("naming the hand-over: %w", err)
	}
	id := hex.EncodeToString(raw)
	defer func() {
		_ = workload.DeleteAllOf(context.WithoutCancel(ctx), &corev1.Secret{}, client.InNamespace(metav1.NamespaceSystem), client.MatchingLabels{handOverLabel: id})
	}()
	body, sum, done, err := run.sealedForNode(ctx, password)
	if err != nil {
		return err
	}
	defer done()
	count, err := writeChunks(ctx, workload, id, body)
	if err != nil {
		return err
	}
	return run.on(ctx, run.nodes[0], "fetching the snapshot", apiFetchScriptFor(adminKubeconfig(distributionOf(run.cluster)), handOverName(id), count, password, sum))
}

// handOverName is the start of the names of a hand-over's Secrets.
func handOverName(id string) string { return "kubehenge-restore-" + id }

// writeChunks stores body in Secrets of handOverChunk bytes, named handOverName(id)-0000 on, and
// returns how many.
func writeChunks(ctx context.Context, workload client.Client, id string, body io.Reader) (int, error) {
	buf := make([]byte, handOverChunk)
	total := 0
	for i := 0; ; i++ {
		n, err := io.ReadFull(body, buf)
		if n > 0 {
			total += n
			if total > maxHandOver {
				return 0, fmt.Errorf("the snapshot is over %d MiB, too large to pass through the cluster's API server. Download it and restore by hand", maxHandOver>>20)
			}
			s := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: metav1.NamespaceSystem, Name: fmt.Sprintf("%s-%04d", handOverName(id), i),
					Labels: map[string]string{handOverLabel: id},
				},
				Data: map[string][]byte{handOverKey: append([]byte(nil), buf[:n]...)},
			}
			if cerr := workload.Create(ctx, s); cerr != nil {
				return 0, fmt.Errorf("%w: %w", ErrHandOverAPIDown, cerr)
			}
		}
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			if n > 0 {
				return i + 1, nil
			}
			return i, nil
		case err != nil:
			return 0, fmt.Errorf("reading the snapshot: %w", err)
		}
	}
}

// apiFetchScript reads a hand-over's Secrets through the cluster's API server with the node's admin
// kubeconfig, then decrypts and checks the snapshot as fetchScript does.
const apiFetchScript = `field() { sed -n "0,/^ *$1: /s/^ *$1: *//p" "$kubeconfig"; }
server=$(field server)
field certificate-authority-data | base64 -d >"$work/api-ca.pem"
field client-certificate-data | base64 -d >"$work/api-cert.pem"
field client-key-data | base64 -d >"$work/api-key.pem"
: >"$work/snapshot.enc"
for ((i = 0; i < count; i++)); do
  curl -fsS --retry 3 --cacert "$work/api-ca.pem" --cert "$work/api-cert.pem" --key "$work/api-key.pem" \
    "$server/api/v1/namespaces/kube-system/secrets/$(printf '%%s-%%04d' "$name" "$i")" |
    sed -n 's/.*"chunk":"\([^"]*\)".*/\1/p' | base64 -d >>"$work/snapshot.enc"
done
rm -f "$work/api-ca.pem" "$work/api-cert.pem" "$work/api-key.pem"
KUBEHENGE_PASSWORD="$password" openssl enc -d -aes-256-ctr -pbkdf2 -iter %d -md sha256 \
  -pass env:KUBEHENGE_PASSWORD -in "$work/snapshot.enc" -out "$work/snapshot.db"
rm -f "$work/snapshot.enc"
echo "$sha256  $work/snapshot.db" | sha256sum -c --quiet -
echo "fetched the snapshot through the API server: $(stat -c %%s "$work/snapshot.db") bytes"
`

func apiFetchScriptFor(kubeconfig, name string, count int, password, sum string) string {
	return script(fmt.Sprintf(apiFetchScript, opensslIterations), "kubeconfig", kubeconfig, "name", name, "count", strconv.Itoa(count), "password", password, "sha256", sum)
}
