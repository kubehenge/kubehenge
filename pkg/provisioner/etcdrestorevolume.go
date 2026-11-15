package provisioner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkloadClients reaches the workload clusters' API servers. found is false until Cluster API has
// made a cluster's kubeconfig.
type WorkloadClients interface {
	Client(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, bool, error)
	// Attach streams stdin to a container of a pod, through the cluster's API server.
	Attach(ctx context.Context, cluster *clusterv1.Cluster, namespace, pod, container string, stdin io.Reader) error
}

// A snapshot on the backup volume reaches a kubeadm, RKE2 or k3s node through the cluster's own API
// server, the way the controller pulls its snapshots: nothing on the management cluster opens to the
// workload network. The controller starts a pod of Kubehenge's image on the node, attaches to its
// stdin and streams the snapshot, encrypted for this restore, into a file the node's restore reads.
// etcd's images hold no shell, so the snapshot cannot go in through etcd's own pods.
const (
	receiverContainer = "receive"
	// receiverSpace is how many times the snapshot's size the node needs free: the received copy, the
	// decrypted one and the restored data directory, which share the node's disk.
	receiverSpace = 3
	// receiverStart bounds the wait for the pod to start, its image pull included.
	receiverStart = 5 * time.Minute
)

// receiverPoll is how often the controller looks at the receiving pod.
var receiverPoll = 2 * time.Second

// errVolumeUnreached refuses a kubeadm, RKE2 or k3s restore from the volume where the controller
// reaches no workload API server, or has no image to receive the snapshot with.
var errVolumeUnreached = errors.New("the controller reaches no workload API server, through which a snapshot on the backup volume gets to the node. Download it and restore by hand")

// ErrHandOverAPIDown says why a kubeadm, RKE2 or k3s restore from the volume cannot start: the
// cluster's API server, the snapshot's way to the node, does not answer.
var ErrHandOverAPIDown = errors.New("the cluster's API server does not answer, and a snapshot on the backup volume reaches the node only through it. Download the snapshot and restore by hand, or restore from a bucket target")

// handOver streams the snapshot, encrypted with password, to the first node, and has the node check
// and decrypt it. The pod and the file go afterwards, whatever happened.
func (run *restoreRun) handOver(ctx context.Context, password string) error {
	if run.r.Workloads == nil || run.r.ReceiverImage == "" {
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
	file := RestoreWorkDir + "/received-" + id + ".enc"
	pod := receiverPod(id, run.nodes[0].Node, run.r.ReceiverImage, file, receiverSpace*run.snapshot.Size)
	if err = workload.Create(ctx, pod); err != nil {
		return fmt.Errorf("%w: %w", ErrHandOverAPIDown, err)
	}
	defer func() {
		after := context.WithoutCancel(ctx)
		_ = workload.Delete(after, pod)
		_ = run.on(after, run.nodes[0], "removing the received snapshot", "rm -f -- "+shellQuote(file))
	}()
	sum, err := run.receive(ctx, workload, pod, password)
	if err != nil {
		return err
	}
	return run.on(ctx, run.nodes[0], "decrypting the snapshot", receivedScriptFor(file, password, sum))
}

// receive starts the stream once the pod runs, and waits for the pod to check it. It returns the
// SHA-256 the node checks the decrypted snapshot by.
func (run *restoreRun) receive(ctx context.Context, workload client.Client, pod *corev1.Pod, password string) (string, error) {
	started, err := waitForPod(ctx, workload, pod, func(p *corev1.Pod) bool {
		return p.Status.Phase != corev1.PodPending
	})
	if err != nil {
		return "", err
	}
	if started.Status.Phase != corev1.PodRunning {
		return "", receiverRefusal(started)
	}
	sealed, err := run.sealedForNode(ctx, password)
	if err != nil {
		return "", err
	}
	defer sealed.done()
	r, w := io.Pipe()
	go func() { w.CloseWithError(SendEtcdSnapshot(w, sealed.body)) }()
	attachErr := run.r.Workloads.Attach(ctx, run.cluster, pod.Namespace, pod.Name, receiverContainer, r)
	_ = r.Close()
	ended, err := waitForPod(ctx, workload, pod, func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
	})
	switch {
	case err != nil && attachErr != nil:
		return "", fmt.Errorf("streaming the snapshot to %s: %w", run.nodes[0].Machine, attachErr)
	case err != nil:
		return "", err
	case ended.Status.Phase != corev1.PodSucceeded:
		return "", receiverRefusal(ended)
	}
	return sealed.sum, nil
}

// receiverRefusal is why the receiving pod failed, in its own words where it left them.
func receiverRefusal(p *corev1.Pod) error {
	for i := range p.Status.ContainerStatuses {
		if t := p.Status.ContainerStatuses[i].State.Terminated; t != nil && t.Message != "" {
			return fmt.Errorf("receiving the snapshot on node %s: %s", p.Spec.NodeName, t.Message)
		}
	}
	return fmt.Errorf("receiving the snapshot on node %s: the pod ended %s", p.Spec.NodeName, p.Status.Phase)
}

// waitForPod reads the pod until done says so, for at most receiverStart.
func waitForPod(ctx context.Context, workload client.Client, pod *corev1.Pod, done func(*corev1.Pod) bool) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, receiverStart)
	defer cancel()
	for {
		p := &corev1.Pod{}
		if err := workload.Get(ctx, client.ObjectKeyFromObject(pod), p); err != nil {
			return nil, fmt.Errorf("reading the receiving pod: %w", err)
		}
		if done(p) {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the pod receiving the snapshot on node %s did not get going: %w", pod.Spec.NodeName, ctx.Err())
		case <-time.After(receiverPoll):
		}
	}
}

// receiverPod runs `kubehenge etcd-snapshot receive` on node, writing to file in RestoreWorkDir.
func receiverPod(id, node, image, file string, need int64) *corev1.Pod {
	return &corev1.Pod{
		Namespace: metav1.NamespaceSystem, Name: "kubehenge-restore-" + id,
		Labels: map[string]string{"app.kubernetes.io/name": "kubehenge-restore"},
		Spec: corev1.PodSpec{
			NodeName:                     node,
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: new(false),
			Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name: receiverContainer, Image: image,
				Args:                     []string{"etcd-snapshot", "receive", "--out", file, "--need", strconv.FormatInt(need, 10)},
				Stdin:                    true,
				StdinOnce:                true,
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)), ReadOnlyRootFilesystem: new(true), AllowPrivilegeEscalation: new(false),
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "restore", MountPath: RestoreWorkDir}},
			}},
			Volumes: []corev1.Volume{{Name: "restore", HostPath: &corev1.HostPathVolumeSource{
				Path: RestoreWorkDir, Type: new(corev1.HostPathDirectoryOrCreate),
			}}},
		},
	}
}

// receivedScript decrypts the received snapshot and checks it as fetchScript does.
const receivedScript = `KUBEHENGE_PASSWORD="$password" openssl enc -d -aes-256-ctr -pbkdf2 -iter %d -md sha256 \
  -pass env:KUBEHENGE_PASSWORD -in "$received" -out "$work/snapshot.db"
rm -f "$received"
echo "$sha256  $work/snapshot.db" | sha256sum -c --quiet -
echo "received the snapshot through the API server: $(stat -c %%s "$work/snapshot.db") bytes"
`

func receivedScriptFor(file, password, sum string) string {
	return script(fmt.Sprintf(receivedScript, opensslIterations), "received", file, "password", password, "sha256", sum)
}
