package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// refreshLogLines is how much of a run's log its status keeps for the interface.
	refreshLogLines = 200
	// refreshLeaseWait is how long a run waits for another to release the lease, or for the lease
	// of one that died to run out, before it gives up.
	refreshLeaseWait = 3 * time.Minute
)

type templateRefreshOptions struct {
	flags          builderFlags
	namespace      string
	classNamespace string
	keep           int
	soak           time.Duration
	missing        []string
	missingOnly    bool
	trigger        string
	tokenSecret    string
}

func newTemplateRefreshCommand(conn *pve.Config) *cobra.Command {
	o := &templateRefreshOptions{}
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Build the templates clusters run again, and delete the builds nothing needs",
		Long: `Build the templates clusters run again, and delete the builds nothing needs.

It runs in the management cluster, as the Job of the template refresh CronJob. It builds again
every node OS template the clusters' machines are cloned from, with the updates its node OS
published since. That is RKE2's and k3s's per node OS and kubeadm's per Kubernetes version and node
OS, plus RKE2's on the default node OS and every RKE2 and k3s template the pool holds. Each build is
a new template, tagged with when it was made. Clusters move to a build inside their maintenance
windows once it is --soak old. Of each template, the --keep newest builds stay, and so does any
older one that a cluster, or a machine being created, still clones from.

One run goes at a time, held by a Lease in --namespace. How the run went is written to the
ConfigMap kubehenge-template-refresh there, which the interface shows.

The API token is read from KUBEHENGE_PROXMOX_TOKEN.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd, conn)
		},
	}
	o.flags.add(cmd)
	f := cmd.Flags()
	f.StringVar(&o.namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace of the refresh's Lease and status (env POD_NAMESPACE)")
	f.StringVar(&o.classNamespace, "class-namespace", os.Getenv("POD_NAMESPACE"), "namespace of Kubehenge's ClusterClasses (env POD_NAMESPACE)")
	f.IntVar(&o.keep, "keep", provisioner.DefaultBuildsKept, "builds of each template kept beside those in use")
	f.DurationVar(&o.soak, "soak", provisioner.DefaultNodeImageSoak, "how old a build must be before clusters move to it, as the controller's --node-image-soak")
	f.StringArrayVar(&o.missing, "missing", nil, "a template to build if it has no build yet, such as rke2-almalinux-10 or kubeadm-v1.37.1-almalinux-10 (repeatable)")
	f.BoolVar(&o.missingOnly, "missing-only", false, "build only the --missing templates, and only those without a build")
	f.StringVar(&o.trigger, "trigger", "", "what started the run, shown with its status")
	f.StringVar(&o.tokenSecret, "token-secret", "", "the Secret KUBEHENGE_PROXMOX_TOKEN comes from, named in the run's status when it is missing")
	return cmd
}

func (o *templateRefreshOptions) run(cmd *cobra.Command, conn *pve.Config) error {
	if o.namespace == "" || o.classNamespace == "" {
		return errors.New("--namespace and --class-namespace are required outside a pod")
	}
	if o.keep < 1 {
		return errors.New("--keep must be at least 1: the newest build of a template always stays")
	}
	var missing []templates.Request
	for _, key := range o.missing {
		req, err := provisioner.ParseRequestLine(key)
		if err != nil {
			return fmt.Errorf("--missing: %w", err)
		}
		missing = append(missing, req)
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("the management cluster's API: %w", err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme} {
		if err = add(scheme); err != nil {
			return fmt.Errorf("the refresh's scheme: %w", err)
		}
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("the management cluster's API: %w", err)
	}
	status := &refreshStatus{kube: kube, namespace: o.namespace, out: cmd.ErrOrStderr()}
	if conn.Token == "" && o.tokenSecret != "" {
		// The chart refreshes by default. Without its Secret the run says so where the interface
		// shows it, rather than the Job's pod never starting.
		return status.refused(cmd.Context(), o.trigger, fmt.Errorf(
			"no API token: namespace %s has no Secret %s with the key token. The management-cluster module makes it. Otherwise make it as the install guide describes, or switch the refresh off",
			o.namespace, o.tokenSecret))
	}
	builder, err := o.flags.connect(cmd, conn)
	if err != nil {
		return status.refused(cmd.Context(), o.trigger, err)
	}
	builder.Log = status.logf
	refresh := &provisioner.TemplateRefresh{
		Kube:           kube,
		Pool:           builder,
		ClassNamespace: o.classNamespace,
		Keep:           o.keep,
		Soak:           o.soak,
		Now:            time.Now,
		Log:            status.logf,
		Missing:        missing,
		MissingOnly:    o.missingOnly,
		Planned: func(lines []templates.Template) {
			names := make([]string, len(lines))
			for i := range lines {
				names[i] = lines[i].LineName
			}
			status.set(cmd.Context(), map[string]string{provisioner.RefreshLines: strings.Join(names, ", ")})
		},
	}
	return withRefreshLease(cmd.Context(), cfg, o.namespace, func(ctx context.Context) error {
		status.set(ctx, map[string]string{
			provisioner.RefreshState:    "running",
			provisioner.RefreshStarted:  time.Now().UTC().Format(time.RFC3339),
			provisioner.RefreshFinished: "",
			provisioner.RefreshTrigger:  o.trigger,
			provisioner.RefreshLines:    "",
			provisioner.RefreshError:    "",
			provisioner.RefreshLog:      "",
		})
		runErr := refresh.Run(ctx)
		// Written past the run's context, which ends with a lost lease: the outcome must be recorded.
		status.finished(context.WithoutCancel(ctx), o.trigger, runErr)
		if runErr != nil {
			return fmt.Errorf("refreshing the templates: %w", runErr)
		}
		return nil
	})
}

// withRefreshLease runs fn while holding the refresh's Lease, so that a scheduled run and one an
// administrator or cluster creator asked for never build at once.
func withRefreshLease(ctx context.Context, cfg *rest.Config, namespace string, fn func(context.Context) error) error {
	clients, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("the management cluster's API: %w", err)
	}
	identity, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("naming the refresh's Lease holder: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		runErr  error
		started = make(chan struct{})
		done    = make(chan struct{})
	)
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Namespace: namespace, Name: provisioner.TemplateRefreshName},
			Client:     clients.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
		},
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDeadline,
		RetryPeriod:   retryPeriod,
		// Not the elector's release: it writes over the Lease version it last saw, and a renewal
		// cut off by fn's end may already have changed it, which leaves the Lease held until it
		// runs out. releaseRefreshLease reads it again instead.
		ReleaseOnCancel: false,
		Name:            provisioner.TemplateRefreshName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leading context.Context) {
				close(started)
				defer close(done)
				defer cancel()
				runErr = fn(leading)
			},
			OnStoppedLeading: func() {},
		},
	})
	if err != nil {
		return fmt.Errorf("the refresh's Lease: %w", err)
	}
	go func() {
		select {
		case <-started:
		case <-time.After(refreshLeaseWait):
			cancel()
		case <-ctx.Done():
		}
	}()
	elector.Run(ctx)
	select {
	case <-started:
	default:
		return fmt.Errorf("another template refresh holds Lease %s/%s", namespace, provisioner.TemplateRefreshName)
	}
	<-done
	leases := clients.CoordinationV1().Leases(namespace)
	releasing, stop := context.WithTimeout(context.WithoutCancel(ctx), renewDeadline)
	defer stop()
	if err := releaseRefreshLease(releasing, leases, identity); err != nil {
		// The next run waits for the Lease to run out instead.
		ctrl.LoggerFrom(ctx).Error(err, "releasing the refresh's Lease")
	}
	return runErr
}

// releaseRefreshLease gives up the refresh's Lease as the elector would, if identity still holds
// it. It retries on a version that moved on in between.
func releaseRefreshLease(ctx context.Context, leases coordinationv1client.LeaseInterface, identity string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lease, err := leases.Get(ctx, provisioner.TemplateRefreshName, metav1.GetOptions{})
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != identity {
			return nil
		}
		now := metav1.NewMicroTime(time.Now())
		lease.Spec.HolderIdentity, lease.Spec.LeaseDurationSeconds = new(""), new(int32(1))
		lease.Spec.AcquireTime, lease.Spec.RenewTime = &now, &now
		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return fmt.Errorf("releasing Lease %s: %w", provisioner.TemplateRefreshName, err)
	}
	return nil
}

// refreshStatus writes a run's state and the end of its log to the refresh's ConfigMap. A failed
// write is logged and the run goes on: the status is for showing, the builds are what count.
type refreshStatus struct {
	kube      client.Client
	namespace string
	out       io.Writer

	mu    sync.Mutex
	lines []string
}

func (s *refreshStatus) logf(format string, args ...any) {
	line := time.Now().UTC().Format(time.TimeOnly) + " " + fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintln(s.out, line)
	s.mu.Lock()
	s.lines = append(s.lines, line)
	if len(s.lines) > refreshLogLines {
		s.lines = s.lines[len(s.lines)-refreshLogLines:]
	}
	log := strings.Join(s.lines, "\n")
	s.mu.Unlock()
	s.set(context.Background(), map[string]string{provisioner.RefreshLog: log})
}

func (s *refreshStatus) set(ctx context.Context, data map[string]string) {
	patch, err := json.Marshal(map[string]any{"data": data})
	if err == nil {
		cm := &corev1.ConfigMap{Namespace: s.namespace, Name: provisioner.TemplateRefreshName}
		err = s.kube.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch))
	}
	if err != nil {
		_, _ = fmt.Fprintf(s.out, "writing the refresh's status: %v\n", err)
	}
}

// finished records how a run ended, and a failure as an event too.
func (s *refreshStatus) finished(ctx context.Context, trigger string, runErr error) {
	end := map[string]string{
		provisioner.RefreshState:    "succeeded",
		provisioner.RefreshFinished: time.Now().UTC().Format(time.RFC3339),
	}
	if runErr != nil {
		end[provisioner.RefreshState] = "failed"
		end[provisioner.RefreshError] = runErr.Error()
	}
	s.set(ctx, end)
	if runErr != nil {
		s.failed(ctx, trigger, runErr)
	}
}

// eventNoteMax is the events API's limit on a message.
const eventNoteMax = 1024

// failed records a Warning event on the refresh's ConfigMap, where kubectl get events shows that a
// run failed and why. The ConfigMap holds the whole error and the end of the log.
func (s *refreshStatus) failed(ctx context.Context, trigger string, why error) {
	now := metav1.Now()
	note := fmt.Sprintf("template build (%s) failed: %v", trigger, why)
	if len(note) > eventNoteMax {
		note = strings.ToValidUTF8(note[:eventNoteMax-3], "") + "..."
	}
	e := &corev1.Event{
		GenerateName: provisioner.TemplateRefreshName + "-", Namespace: s.namespace,
		InvolvedObject: corev1.ObjectReference{APIVersion: "v1", Kind: "ConfigMap", Namespace: s.namespace, Name: provisioner.TemplateRefreshName},
		Type:           corev1.EventTypeWarning, Reason: "TemplateBuildFailed", Message: note,
		Source:         corev1.EventSource{Component: provisioner.TemplateRefreshName},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if err := s.kube.Create(ctx, e); err != nil {
		_, _ = fmt.Fprintf(s.out, "recording the failure as an event: %v\n", err)
	}
}

// refused records a run that could not start, so that the interface shows why, and returns why.
func (s *refreshStatus) refused(ctx context.Context, trigger string, why error) error {
	now := time.Now().UTC().Format(time.RFC3339)
	s.set(ctx, map[string]string{
		provisioner.RefreshState:    "failed",
		provisioner.RefreshStarted:  now,
		provisioner.RefreshFinished: now,
		provisioner.RefreshTrigger:  trigger,
		provisioner.RefreshLines:    "",
		provisioner.RefreshError:    why.Error(),
		provisioner.RefreshLog:      "",
	})
	s.failed(ctx, trigger, why)
	return fmt.Errorf("the template refresh cannot start: %w", why)
}
