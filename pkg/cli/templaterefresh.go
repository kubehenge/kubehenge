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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
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

It runs in the management cluster, as the template refresh CronJob's Job. Each Ubuntu template the
clusters' machines are cloned from, the cloud image RKE2 and k3s share and a kubeadm template per
Kubernetes version, is built again with the updates Ubuntu published since, as a new template tagged
with when it was made. Clusters move to a build inside their maintenance windows once it is --soak
old. Of each template, the --keep newest builds stay, and any older one a cluster or a machine
being created still clones from.

One run goes at a time, held by a Lease in --namespace. How the run went is written to the
ConfigMap kubehenge-template-refresh there, which the interface shows.

The API token is read from KUBEHENGE_PROXMOX_TOKEN.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd, conn)
		},
	}
	o.flags.add(cmd)
	f := cmd.Flags()
	f.StringVar(&o.namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace of the refresh's Lease and status (env POD_NAMESPACE)")
	f.StringVar(&o.classNamespace, "class-namespace", os.Getenv("POD_NAMESPACE"), "namespace of Kubehenge's ClusterClasses (env POD_NAMESPACE)")
	f.IntVar(&o.keep, "keep", provisioner.DefaultBuildsKept, "builds of each template kept beside those in use")
	f.DurationVar(&o.soak, "soak", provisioner.DefaultNodeImageSoak, "how old a build is before clusters move to it; the controller's --node-image-soak")
	f.StringArrayVar(&o.missing, "missing", nil, "a template to build if it has no build yet: cloud-image, or kubeadm- and a Kubernetes version (repeatable)")
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
		// The chart refreshes by default; without its Secret the run says so where the interface
		// shows it, rather than the Job's pod never starting.
		return status.refused(cmd.Context(), o.trigger, fmt.Errorf(
			"no API token: Secret %s with key token is missing in namespace %s; the module makes it, or make it as the install guide describes, or switch the refresh off",
			o.tokenSecret, o.namespace))
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
		end := map[string]string{
			provisioner.RefreshState:    "succeeded",
			provisioner.RefreshFinished: time.Now().UTC().Format(time.RFC3339),
		}
		if runErr != nil {
			end[provisioner.RefreshState] = "failed"
			end[provisioner.RefreshError] = runErr.Error()
		}
		// Written past the run's context, which ends with a lost lease: the outcome must be recorded.
		status.set(context.WithoutCancel(ctx), end)
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
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
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
	return runErr
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
	return fmt.Errorf("the template refresh cannot start: %w", why)
}
