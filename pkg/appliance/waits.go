package appliance

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// waitsTimeout bounds the look at the management cluster after a failed run, which may find its
// API server down.
const waitsTimeout = 30 * time.Second

// maxWaits is how many pods a failed run's log names, so that a cluster that does not start at all
// does not bury the run's own lines.
const maxWaits = 10

// maxWaitMessage is the most of a pod's message a line carries. Image pull errors run long.
const maxWaitMessage = 300

// explainWaits adds to the job's log what keeps the management cluster's pods from running, after a
// run that failed. Helm, waiting for a release, says only that its time ran out. The admin
// kubeconfig is in the state once the cluster exists, also after a failed run, and stays in memory.
func (a *Appliance) explainWaits(ctx context.Context, j *Job, env []string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), waitsTimeout)
	defer cancel()
	admin, err := a.output(ctx, env, "kubeconfig")
	if err != nil {
		return
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(admin))
	if err != nil {
		return
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return
	}
	waits := waitingPods(ctx, kube)
	if len(waits) == 0 {
		return
	}
	j.Add("What the management cluster's pods wait for:")
	for _, w := range waits {
		j.Add(w)
	}
}

// waitingPods says, one line per pod, why pods of the cluster do not run or are not ready.
func waitingPods(ctx context.Context, kube kubernetes.Interface) []string {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for i := range pods.Items {
		p := &pods.Items[i]
		why := podWait(p)
		if why == "" {
			continue
		}
		if len(out) == maxWaits {
			out = append(out, "and more")
			break
		}
		out = append(out, "Pod "+p.Namespace+"/"+p.Name+" "+why)
	}
	return out
}

// podWait says why a pod does not run or is not ready, or returns "" for one that is, or is done.
func podWait(p *corev1.Pod) string {
	if p.Status.Phase == corev1.PodSucceeded {
		return ""
	}
	if c := condition(p, corev1.PodScheduled); c != nil && c.Status == corev1.ConditionFalse {
		return "is not scheduled: " + short(c.Message)
	}
	for _, cs := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			if w.Message == "" {
				return fmt.Sprintf("waits: container %s is in %s", cs.Name, w.Reason)
			}
			return fmt.Sprintf("waits: container %s is in %s: %s", cs.Name, w.Reason, short(w.Message))
		}
	}
	if p.Status.Phase == corev1.PodFailed {
		return "failed: " + short(p.Status.Message)
	}
	if c := condition(p, corev1.PodReady); c != nil && c.Status == corev1.ConditionFalse {
		return "runs but is not ready"
	}
	return ""
}

func condition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

func short(s string) string {
	r := []rune(s)
	if len(r) <= maxWaitMessage {
		return s
	}
	return string(r[:maxWaitMessage]) + "…"
}
