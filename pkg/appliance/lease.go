package appliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// The lease keeps an appliance from applying the management cluster on a state older than what
// last changed it: after a snapshot rollback, the restore of an older backup, a storage replica
// that took over after a crash, or with a second appliance cloned or restored beside the first.
// Each apply counts a generation up before it changes anything, beside the state and in a
// ConfigMap in the management cluster. An apply whose state holds a lower generation than the
// cluster's is refused.

const (
	leaseConfigMap = "kubehenge-appliance-lease"
	// leaseTimeout bounds each request for the lease, which an apply waits on before it starts.
	leaseTimeout = 30 * time.Second
)

// StaleStateError is an apply or removal refused because an apply this appliance's state does not
// know changed the management cluster since.
type StaleStateError struct {
	Local   int `json:"local"`
	Cluster int `json:"cluster"`
}

func (e *StaleStateError) Error() string {
	return fmt.Sprintf("this appliance's state is from apply %d, and the management cluster is at apply %d, as after a snapshot rollback, an older backup or a second appliance. "+
		"Take it over on the Administration page to go ahead with this state", e.Local, e.Cluster)
}

// ErrLeaseTaken is another appliance claiming the management cluster at the same moment.
var ErrLeaseTaken = errors.New("another appliance started applying the management cluster at the same time")

type leaseFile struct {
	Generation int `json:"generation"`
}

func (a *Appliance) leasePath() string { return filepath.Join(a.workdir(), "lease.json") }

// localGeneration is the generation of the last apply this appliance's state knows, 0 for none.
func (a *Appliance) localGeneration() int {
	raw, err := os.ReadFile(a.leasePath())
	if err != nil {
		return 0
	}
	var l leaseFile
	if json.Unmarshal(raw, &l) != nil {
		return 0
	}
	return l.Generation
}

func (a *Appliance) setLocalGeneration(g int) error {
	raw, err := json.Marshal(leaseFile{Generation: g})
	if err == nil {
		err = os.MkdirAll(a.workdir(), privateDir)
	}
	if err == nil {
		err = writeFileAtomic(a.leasePath(), raw)
	}
	if err != nil {
		return fmt.Errorf("recording the apply on the appliance: %w", err)
	}
	return nil
}

// managementClient reaches the management cluster with its admin kubeconfig.
func (a *Appliance) managementClient(admin []byte) (kubernetes.Interface, error) { //nolint:ireturn // a fake stands in for it in tests
	if a.ManagementClient != nil {
		return a.ManagementClient(admin)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(admin)
	if err != nil {
		return nil, fmt.Errorf("the admin kubeconfig: %w", err)
	}
	cfg.Timeout = leaseTimeout
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("a client for the management cluster: %w", err)
	}
	return kube, nil
}

// clusterLease reads the management cluster's lease: its generation, 0 without one, and the
// ConfigMap to update, nil without one.
func clusterLease(ctx context.Context, kube kubernetes.Interface) (int, *corev1.ConfigMap, error) {
	cm, err := kube.CoreV1().ConfigMaps(interfaceNamespace).Get(ctx, leaseConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("reading the management cluster's lease: %w", err)
	}
	g, err := strconv.Atoi(cm.Data["generation"])
	if err != nil {
		return 0, nil, fmt.Errorf("the management cluster's lease holds no generation: %w", err)
	}
	return g, cm, nil
}

// writeLease records generation g in the management cluster, creating the ConfigMap where there
// is none and otherwise updating cm, which fails with a conflict when anyone changed it since.
func writeLease(ctx context.Context, kube kubernetes.Interface, cm *corev1.ConfigMap, g int) error {
	data := map[string]string{"generation": strconv.Itoa(g)}
	var err error
	if cm == nil {
		_, err = kube.CoreV1().ConfigMaps(interfaceNamespace).Create(ctx, &corev1.ConfigMap{
			Name: leaseConfigMap, Namespace: interfaceNamespace,
			Data: data,
		}, metav1.CreateOptions{})
	} else {
		cm = cm.DeepCopy()
		cm.Data = data
		_, err = kube.CoreV1().ConfigMaps(interfaceNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("recording the apply in the management cluster: %w", err)
	}
	return nil
}

// clusterOf reaches the management cluster the state in env records, or returns nil while the
// state holds none yet.
func (a *Appliance) clusterOf(ctx context.Context, env []string) (kubernetes.Interface, error) { //nolint:ireturn // the client managementClient makes
	if !a.HasState() {
		return nil, nil //nolint:nilnil // no management cluster yet, so nothing to reach and no error
	}
	admin, err := a.output(ctx, env, "kubeconfig")
	if err != nil || admin == "" {
		return nil, nil //nolint:nilnil,nilerr // a state without the cluster's kubeconfig, as after a first run that failed early, has no cluster yet
	}
	return a.managementClient([]byte(admin))
}

// claimLease counts the generation up before an apply changes anything, on the appliance and then
// in the management cluster, or refuses the apply with a StaleStateError. A management cluster
// that does not answer refuses an automatic apply. An administrator's goes ahead, as it may be the
// repair the cluster needs, and its log says so.
func (a *Appliance) claimLease(ctx context.Context, j *Job, env []string) error {
	kube, err := a.clusterOf(ctx, env)
	if err == nil && kube == nil {
		return nil
	}
	local, cluster := a.localGeneration(), 0
	var cm *corev1.ConfigMap
	if err == nil {
		cluster, cm, err = clusterLease(ctx, kube)
	}
	switch {
	case err != nil && j.Automatic:
		return fmt.Errorf("%w, so it is not known whether a newer state changed it", err)
	case err != nil:
		j.Add("The management cluster's lease could not be read, so this apply goes ahead unchecked: " + err.Error())
		return nil //nolint:nilerr // an administrator's apply may be the repair the cluster needs
	case cluster > local:
		return &StaleStateError{Local: local, Cluster: cluster}
	}
	if err = a.setLocalGeneration(local + 1); err != nil {
		return err
	}
	err = writeLease(ctx, kube, cm, local+1)
	switch {
	case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
		if undo := a.setLocalGeneration(local); undo != nil {
			return fmt.Errorf("%w, and %w", ErrLeaseTaken, undo)
		}
		return ErrLeaseTaken
	case err != nil:
		// The appliance is ahead now, which the next apply records in the cluster.
		j.Add("The management cluster did not record this apply, which the next one does: " + err.Error())
	}
	return nil
}

// checkLease refuses a removal by a state older than the management cluster's. A cluster that does
// not answer is no reason to keep it.
func (a *Appliance) checkLease(ctx context.Context, env []string) error {
	kube, err := a.clusterOf(ctx, env)
	if err != nil || kube == nil {
		return nil //nolint:nilerr // a cluster out of reach is removed all the same
	}
	cluster, _, err := clusterLease(ctx, kube)
	if local := a.localGeneration(); err == nil && cluster > local {
		return &StaleStateError{Local: local, Cluster: cluster}
	}
	return nil
}

// TakeOver makes this appliance's state the one the management cluster follows, after an apply was
// refused with a StaleStateError. The next apply then undoes what the newer state made that this
// one does not know.
func (a *Appliance) TakeOver(ctx context.Context, passphrase string) error {
	admin, err := a.stateOutput(ctx, passphrase, "kubeconfig")
	if err != nil {
		return err
	}
	kube, err := a.managementClient([]byte(admin))
	if err != nil {
		return err
	}
	cluster, _, err := clusterLease(ctx, kube)
	if err != nil {
		return err
	}
	if err = a.setLocalGeneration(max(cluster, a.localGeneration())); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if j := a.job; j != nil && !j.running() {
		j.mu.Lock()
		j.Stale = nil
		j.mu.Unlock()
		a.saveLog(&a.job, j.view())
	}
	a.Log.Info("management cluster taken over", "generation", cluster)
	return nil
}
