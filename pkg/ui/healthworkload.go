package ui

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Workload is a workload cluster as one of Kubehenge's health identities reaches it.
type Workload struct {
	Kube kubernetes.Interface
	// Readyz answers the API server's verbose readiness check, with any failed checks.
	Readyz func(ctx context.Context) (string, error)
	// Close lets go of its idle connections; may be nil.
	Close func()
}

func (w *Workload) close() {
	if w.Close != nil {
		w.Close()
	}
}

// connectWorkload reaches a workload cluster's API server with a bearer token, trusting its CA alone.
func connectWorkload(server string, ca []byte, token string) (*Workload, error) {
	cfg := &rest.Config{
		Host: server, BearerToken: token, CAData: ca,
		Timeout: healthDeadline, UserAgent: "kubehenge-interface",
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("the workload cluster's client: %w", err)
	}
	kube, err := kubernetes.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, fmt.Errorf("the workload cluster's client: %w", err)
	}
	return &Workload{
		Kube: kube,
		Readyz: func(ctx context.Context) (string, error) {
			// A failing check answers 500, with the checks in the body all the same.
			body, err := kube.CoreV1().RESTClient().Get().AbsPath("/readyz").Param("verbose", "true").DoRaw(ctx)
			if err != nil && len(body) == 0 {
				return "", fmt.Errorf("the API server's readiness: %w", err)
			}
			return string(body), nil
		},
		Close: hc.CloseIdleConnections,
	}, nil
}

func (s *Server) connect(server string, ca []byte, token string) (*Workload, error) {
	if s.ConnectWorkload != nil {
		return s.ConnectWorkload(server, ca, token)
	}
	return connectWorkload(server, ca, token)
}

// workloadToken is one of the Secrets Kubehenge's controller keeps a health identity's token in.
type workloadToken struct {
	server string
	ca     []byte
	token  string
}

// notSetUp is why a cluster whose health identity is not there yet shows nothing from inside.
const notSetUp = "Kubehenge's controller has not set up its read-only access to this cluster yet."

// readToken reads one of a cluster's health Secrets, and says why it cannot be used when it cannot.
func (s *Server) readToken(ctx context.Context, kube client.Client, c *clusterv1.Cluster, name string) (workloadToken, string, error) {
	secret := &corev1.Secret{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, secret)
	if apierrors.IsNotFound(err) {
		return workloadToken{}, notSetUp, nil
	}
	if err != nil {
		return workloadToken{}, "", fmt.Errorf("reading Secret %s: %w", name, err)
	}
	t := workloadToken{
		server: string(secret.Data[provisioner.HealthServerKey]), ca: secret.Data[provisioner.HealthCAKey],
		token: string(secret.Data[provisioner.HealthTokenKey]),
	}
	if t.server == "" || t.token == "" {
		return workloadToken{}, notSetUp, nil
	}
	if expires, err := time.Parse(time.RFC3339, secret.Annotations[provisioner.HealthExpiresAnnotation]); err == nil && !s.now().Before(expires) {
		return workloadToken{}, fmt.Sprintf("Kubehenge's access to the cluster ran out at %s UTC: its controller has not reached the cluster to renew it since.",
			expires.UTC().Format(time.DateTime)), nil
	}
	return t, "", nil
}

// workloadIdle is how long a workload cluster's connection is kept unused.
const workloadIdle = 10 * time.Minute

type workloadConns struct {
	mu    sync.Mutex
	byKey map[string]*workloadConn
}

type workloadConn struct {
	token string
	w     *Workload
	used  time.Time
}

// healthWorkload reaches a cluster as its health identity, with the connection kept from the last
// read while the token is the same.
func (s *Server) healthWorkload(ctx context.Context, kube client.Client, c *clusterv1.Cluster) (*Workload, string, error) {
	t, why, err := s.readToken(ctx, kube, c, provisioner.HealthSecretName(c.Name))
	if why != "" || err != nil {
		return nil, why, err
	}
	key := c.Namespace + "/" + c.Name + "/" + string(c.UID)
	now := s.now()
	wc := &s.workloads
	wc.mu.Lock()
	defer wc.mu.Unlock()
	for k, kept := range wc.byKey {
		if now.Sub(kept.used) > workloadIdle {
			kept.w.close()
			delete(wc.byKey, k)
		}
	}
	if kept, ok := wc.byKey[key]; ok && kept.token == t.token {
		kept.used = now
		return kept.w, "", nil
	}
	w, err := s.connect(t.server, t.ca, t.token)
	if err != nil {
		return nil, "", err
	}
	if kept, ok := wc.byKey[key]; ok {
		kept.w.close()
	}
	if wc.byKey == nil {
		wc.byKey = map[string]*workloadConn{}
	}
	wc.byKey[key] = &workloadConn{token: t.token, w: w, used: now}
	return w, "", nil
}

// workloadRead is what the health identity read inside a cluster.
type workloadRead struct {
	// skipped says why nothing was read.
	skipped       string
	connectErr    error
	nodes         []corev1.Node
	nodesErr      error
	pods          []corev1.Pod
	podsTruncated bool
	podsErr       error
	claims        []corev1.PersistentVolumeClaim
	claimsErr     error
	events        []corev1.Event
	eventsErr     error
	readyz        string
}

// unreachable tells whether the cluster was to be read and did not answer.
func (wl *workloadRead) unreachable() bool {
	return wl.skipped == "" && (wl.connectErr != nil || wl.nodesErr != nil)
}

// why says why nothing shows from inside the cluster, when nothing does.
func (wl *workloadRead) why() string {
	switch {
	case wl.skipped != "":
		return wl.skipped
	case wl.connectErr != nil:
		return "Kubehenge could not reach the cluster: " + problem(wl.connectErr)
	case wl.nodesErr != nil:
		return "The cluster's API server did not answer: " + problem(wl.nodesErr)
	default:
		return ""
	}
}

// readWorkload reads the nodes, pods, volume claims, warnings and readiness of a cluster in the
// interface's namespace, as its health identity, each within healthDeadline. Elsewhere the
// interface reads no Secret, so it has no way in.
func (s *Server) readWorkload(ctx context.Context, kube client.Client, c *clusterv1.Cluster) workloadRead {
	if c.Namespace != s.Namespace {
		return workloadRead{skipped: "Kubehenge reads inside the clusters of its own namespace, " + s.Namespace + ", only; this one is managed from elsewhere."}
	}
	if !meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return workloadRead{skipped: "The cluster's API server is not up yet."}
	}
	tctx, cancel := context.WithTimeout(ctx, healthDeadline)
	w, why, err := s.healthWorkload(tctx, kube, c)
	cancel()
	if why != "" || err != nil {
		return workloadRead{skipped: why, connectErr: err}
	}
	var wl workloadRead
	var wg sync.WaitGroup
	read := func(into *error, f func(context.Context) error) {
		wg.Go(func() {
			rctx, cancel := context.WithTimeout(ctx, healthDeadline)
			defer cancel()
			*into = f(rctx)
		})
	}
	// From the API server's cache, which is what a page read at human pace needs.
	list := metav1.ListOptions{ResourceVersion: "0", Limit: healthListLimit}
	core := w.Kube.CoreV1()
	read(&wl.nodesErr, func(ctx context.Context) error {
		l, err := core.Nodes().List(ctx, list)
		if err == nil {
			wl.nodes = l.Items
		}
		return err //nolint:wrapcheck // problem() reads it, and says it as the API server did
	})
	read(&wl.podsErr, func(ctx context.Context) error {
		l, err := core.Pods(metav1.NamespaceAll).List(ctx, list)
		if err == nil {
			wl.pods, wl.podsTruncated = l.Items, l.Continue != ""
		}
		return err //nolint:wrapcheck // as above
	})
	read(&wl.claimsErr, func(ctx context.Context) error {
		l, err := core.PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, list)
		if err == nil {
			wl.claims = l.Items
		}
		return err //nolint:wrapcheck // as above
	})
	read(&wl.eventsErr, func(ctx context.Context) error {
		warnings := list
		warnings.FieldSelector = "type=" + corev1.EventTypeWarning
		l, err := core.Events(metav1.NamespaceAll).List(ctx, warnings)
		if err == nil {
			wl.events = l.Items
		}
		return err //nolint:wrapcheck // as above
	})
	if w.Readyz != nil {
		var readyzErr error
		read(&readyzErr, func(ctx context.Context) error {
			var err error
			wl.readyz, err = w.Readyz(ctx)
			return err
		})
	}
	wg.Wait()
	return wl
}

// readinessAttention lists the API server's failing readiness checks, such as etcd's.
func readinessAttention(wl *workloadRead, a *attention) {
	for line := range strings.Lines(wl.readyz) {
		check, ok := strings.CutPrefix(strings.TrimSpace(line), "[-]")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(check, " ")
		a.add(severityError, sectionControlPlane, "API server check "+name+" failing", strings.TrimSpace(check), time.Time{})
	}
}

// failingWaits are why a container waits that keep it from ever running as it is.
var failingWaits = map[string]bool{
	"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true,
}

// startupGrace is how long a pod may take to pull its images and start before it is a problem.
const startupGrace = 5 * time.Minute

// oomKilled is the reason a container stopped for running out of memory.
const oomKilled = "OOMKilled"

// podsSection lists the pods that are not well, worst first.
func podsSection(wl *workloadRead, now time.Time, a *attention) section[podView] {
	s := section[podView]{Items: []podView{}, Truncated: wl.podsTruncated}
	if wl.podsErr != nil && !wl.unreachable() {
		s.Error = problem(wl.podsErr)
	}
	for i := range wl.pods {
		if v, ok := podProblem(&wl.pods[i], now); ok {
			s.Items = append(s.Items, v)
		}
	}
	slices.SortFunc(s.Items, func(x, y podView) int {
		return cmp.Or(cmp.Compare(severityRank(x.Severity), severityRank(y.Severity)), strings.Compare(x.Namespace, y.Namespace), strings.Compare(x.Name, y.Name))
	})
	listed := 0
	for i := range s.Items {
		v := &s.Items[i]
		// A pod that failed long ago and stayed is a leftover, listed below but no news.
		if !v.Since.IsZero() && now.Sub(v.Since) > eventWindow && v.Severity != severityError {
			continue
		}
		if listed == podsNeedingAttention {
			a.add(v.Severity, sectionPods, "More pods not well", "", time.Time{})
			break
		}
		what := "Pod " + v.Namespace + "/" + v.Name + ": " + v.Reason
		if v.Restarts > 0 {
			what += fmt.Sprintf(", %d restarts", v.Restarts)
		}
		*a = append(*a, attentionView{
			Severity: v.Severity, Section: sectionPods, What: what, Message: v.Message, Since: v.Since,
			Pod: &podRef{Namespace: v.Namespace, Name: v.Name},
		})
		listed++
	}
	return s
}

// podProblem says what is wrong with a pod, if anything.
func podProblem(p *corev1.Pod, now time.Time) (podView, bool) {
	v := podView{Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, Since: p.CreationTimestamp.Time}
	statuses := slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses)
	unwell := ""
	for i := range statuses {
		cs := &statuses[i]
		v.Restarts += cs.RestartCount
		if unwell == "" && containerProblem(cs, &v, now) {
			unwell = cs.Name
		}
	}
	if unwell == "" && !phaseProblem(p, statuses, &v, now) {
		return podView{}, false
	}
	// Only a container that has run has a log, so one still waiting for its image or a node offers none.
	ran := map[string]bool{}
	for i := range statuses {
		cs := &statuses[i]
		ran[cs.Name] = cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil
	}
	v.Containers = []string{}
	if ran[unwell] {
		v.Containers = append(v.Containers, unwell)
	}
	for _, containers := range [][]corev1.Container{p.Spec.InitContainers, p.Spec.Containers} {
		for i := range containers {
			if name := containers[i].Name; name != unwell && ran[name] {
				v.Containers = append(v.Containers, name)
			}
		}
	}
	return v, true
}

// containerProblem fills in what keeps a container from running, or what stopped it lately.
func containerProblem(cs *corev1.ContainerStatus, v *podView, now time.Time) bool {
	last := cs.LastTerminationState.Terminated
	if w := cs.State.Waiting; w != nil && failingWaits[w.Reason] {
		v.Severity, v.Reason, v.Message = severityError, w.Reason, w.Message
		if last != nil {
			v.Since = last.FinishedAt.Time
			stopped := fmt.Sprintf("It last stopped with exit code %d (%s).", last.ExitCode, last.Reason)
			if last.Reason == oomKilled {
				stopped = "It last stopped for running out of memory (OOMKilled)."
			}
			v.Message = strings.TrimSpace(stopped + " " + w.Message)
		}
		return true
	}
	for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, last} {
		if t != nil && t.Reason == oomKilled && now.Sub(t.FinishedAt.Time) < eventWindow {
			v.Severity, v.Reason, v.Since = severityWarning, oomKilled, t.FinishedAt.Time
			v.Message = "Container " + cs.Name + " ran out of memory and was stopped."
			return true
		}
	}
	return false
}

// phaseProblem fills in what is wrong with a pod by its phase, once it has had time to start.
func phaseProblem(p *corev1.Pod, statuses []corev1.ContainerStatus, v *podView, now time.Time) bool {
	switch p.Status.Phase {
	case corev1.PodPending:
		if sched := podCondition(p, corev1.PodScheduled); sched != nil && sched.Status == corev1.ConditionFalse {
			v.Severity, v.Reason, v.Message, v.Since = severityWarning, cmp.Or(sched.Reason, "Unschedulable"), sched.Message, sched.LastTransitionTime.Time
			return now.Sub(v.Since) >= settling
		}
		v.Severity, v.Reason = severityWarning, "Pending"
		for i := range statuses {
			if w := statuses[i].State.Waiting; w != nil {
				v.Reason, v.Message = w.Reason, w.Message
				break
			}
		}
		return now.Sub(v.Since) >= startupGrace
	case corev1.PodFailed:
		v.Severity, v.Reason, v.Message = severityWarning, cmp.Or(p.Status.Reason, "Failed"), p.Status.Message
		for i := range statuses {
			if t := statuses[i].State.Terminated; t != nil && t.FinishedAt.After(v.Since) {
				v.Since = t.FinishedAt.Time
			}
		}
		return true
	case corev1.PodRunning:
		ready := podCondition(p, corev1.PodReady)
		if ready == nil || ready.Status == corev1.ConditionTrue || now.Sub(ready.LastTransitionTime.Time) < settling {
			return false
		}
		v.Severity, v.Reason, v.Since = severityWarning, "NotReady", ready.LastTransitionTime.Time
		v.Message = cmp.Or(ready.Message, "Its containers are not ready.")
		return true
	case corev1.PodUnknown:
		v.Severity, v.Reason, v.Message = severityWarning, "Unknown", "Its node does not report on it."
		return true
	case corev1.PodSucceeded:
		return false
	}
	return false
}

func podCondition(p *corev1.Pod, kind corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == kind {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// volumesSection lists the volume claims still waiting for a volume.
func volumesSection(wl *workloadRead, now time.Time, a *attention) section[volumeView] {
	s := section[volumeView]{Items: []volumeView{}}
	if wl.claimsErr != nil && !wl.unreachable() {
		s.Error = problem(wl.claimsErr)
	}
	for i := range wl.claims {
		pvc := &wl.claims[i]
		since := pvc.CreationTimestamp.Time
		if pvc.Status.Phase != corev1.ClaimPending || now.Sub(since) < settling {
			continue
		}
		v := volumeView{Namespace: pvc.Namespace, Name: pvc.Name, Since: since}
		if pvc.Spec.StorageClassName != nil {
			v.StorageClass = *pvc.Spec.StorageClassName
		}
		a.add(severityWarning, sectionVolumes, "Volume claim "+v.Namespace+"/"+v.Name+" waiting for a volume", "", since)
		s.Items = append(s.Items, v)
	}
	return s
}

// The last lines of a container's log shown, and the most of it read.
const (
	podLogLines = 60
	podLogBytes = 256 << 10
	// podLogsDeadline covers the two reads behind a log: the pod, then its container's log.
	podLogsDeadline = 2 * healthDeadline
)

type podLogView struct {
	Container string `json:"container"`
	// Previous tells the lines are from the container's last run, which has stopped, as a
	// crash-looping one has no running one of its own.
	Previous bool     `json:"previous"`
	Lines    []string `json:"lines"`
}

// podLogs hands out the last lines of a pod's container's log. Logs carry secrets often enough,
// so this needs what the admin kubeconfig does, Sys.Modify on the cluster's pool, and is read with
// an identity that reads logs and nothing else; every read is logged with who made it.
func (s *Server) podLogs(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), podLogsDeadline)
	defer cancel()
	health, why, err := s.healthWorkload(ctx, u.kube, c)
	if why == "" && err == nil {
		var logs workloadToken
		if logs, why, err = s.readToken(ctx, u.kube, c, provisioner.HealthLogsSecretName(c.Name)); why == "" && err == nil {
			s.servePodLogs(ctx, w, r, u, c, health, logs)
			return
		}
	}
	if why != "" {
		writeError(w, http.StatusConflict, why)
		return
	}
	s.Log.Error("reaching a cluster for pod logs", "cluster", c.Namespace+"/"+c.Name, "error", err)
	writeError(w, http.StatusBadGateway, "the cluster did not answer")
}

func (s *Server) servePodLogs(ctx context.Context, w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, health *Workload, logs workloadToken) {
	ns, name, container := r.PathValue("podNamespace"), r.PathValue("pod"), r.URL.Query().Get("container")
	pod, err := health.Kube.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "no such pod")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "the cluster did not answer: "+problem(err))
		return
	}
	statuses := slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)
	i := slices.IndexFunc(statuses, func(cs corev1.ContainerStatus) bool { return cs.Name == container })
	if i < 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("pod %s/%s has no container %q that has run", ns, name, container))
		return
	}
	cs := statuses[i]
	previous := cs.State.Running == nil && (cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil)
	reader, err := s.connect(logs.server, logs.ca, logs.token)
	if err != nil {
		s.Log.Error("reaching a cluster for pod logs", "cluster", c.Namespace+"/"+c.Name, "error", err)
		writeError(w, http.StatusBadGateway, "the cluster did not answer")
		return
	}
	defer reader.close()
	// Never Previous: the kubelet hands out the newest run without it, the running one or else the
	// last to stop, and keeps that run. Previous names the run before by the status the kubelet last
	// wrote, which lags behind its removal of all but the newest exited container, so a crash-looping
	// container's read would fail with "unable to retrieve container logs" now and then.
	stream, err := reader.Kube.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{
		Container: container, TailLines: new(int64(podLogLines)), LimitBytes: new(int64(podLogBytes)),
	}).Stream(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "the cluster did not hand out the log: "+problem(err))
		return
	}
	defer func() { _ = stream.Close() }()
	lines, err := readLines(io.LimitReader(stream, podLogBytes))
	if err != nil && !errors.Is(err, context.Canceled) {
		writeError(w, http.StatusBadGateway, "reading the log: "+problem(err))
		return
	}
	s.Log.Info("read pod logs", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "pod", ns+"/"+name, "container", container)
	writeJSON(w, http.StatusOK, podLogView{Container: container, Previous: previous, Lines: lines})
}

// readLines reads a log's lines, the last podLogLines of them, however long each is.
func readLines(r io.Reader) ([]string, error) {
	lines := []string{}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), podLogBytes)
	for scan.Scan() {
		lines = append(lines, scan.Text())
	}
	if len(lines) > podLogLines {
		lines = lines[len(lines)-podLogLines:]
	}
	if err := scan.Err(); err != nil {
		return lines, fmt.Errorf("the log: %w", err)
	}
	return lines, nil
}
