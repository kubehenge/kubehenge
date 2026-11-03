package provisioner

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func operatorClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range providerKinds {
		scheme.AddKnownTypeWithName(operatorAPI.WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(operatorAPI.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	metav1.AddToGroupVersion(scheme, operatorAPI)
	return interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), *funcs)
}

type condition struct {
	kind       string
	status     metav1.ConditionStatus
	generation int64
	message    string
}

// appliedHash is the hash the providers of these tests record as applied.
const appliedHash = "2eb6f85a"

func provider(kind, ns, name string, generation int64, conditions ...condition) *unstructured.Unstructured {
	p := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"version": "v1.14.3"}}}
	p.SetGroupVersionKind(operatorAPI.WithKind(kind))
	p.SetNamespace(ns)
	p.SetName(name)
	p.SetGeneration(generation)
	p.SetAnnotations(map[string]string{appliedSpecHashAnnotation: appliedHash})
	list := make([]any, 0, len(conditions))
	for _, c := range conditions {
		list = append(list, map[string]any{
			"type": c.kind, "status": string(c.status), "observedGeneration": c.generation,
			"reason": "Test", "message": c.message, "lastTransitionTime": "2026-01-01T00:00:00Z",
		})
	}
	p.Object["status"] = map[string]any{"conditions": list}
	return p
}

// providerCache is the Secret the operator caches p's manifests in. It records hash as applied.
func providerCache(p *unstructured.Unstructured, hash string) *corev1.Secret {
	version, _, _ := unstructured.NestedString(p.Object, "spec", "version")
	return &corev1.Secret{
		Namespace: p.GetNamespace(), Name: providerCacheName(p.GetKind(), p.GetName(), version),
		Annotations: map[string]string{appliedSpecHashAnnotation: hash},
	}
}

// installedCore is the core provider as the operator leaves it once installed, with its cache and
// its Deployment, which change may alter first.
func installedCore(change func(*appsv1.Deployment)) []client.Object {
	p := provider("CoreProvider", "capi-system", "cluster-api", 1, condition{"ProviderInstalled", metav1.ConditionTrue, 1, ""})
	dep := providerDeployment("capi-system", "capi-controller-manager")
	dep.UID = "first"
	if change != nil {
		change(dep)
	}
	return []client.Object{p, providerCache(p, appliedHash), dep}
}

func TestProviderCacheNameIsTheOperators(t *testing.T) {
	t.Parallel()
	// The operator's ProviderCacheName joins its GetType, the provider's name and its version.
	operatorTypes := map[string]string{
		"CoreProvider": "core", "BootstrapProvider": "bootstrap", "ControlPlaneProvider": "control-plane",
		"InfrastructureProvider": "infrastructure", "IPAMProvider": "ipam", "AddonProvider": "addon",
	}
	for _, kind := range providerKinds {
		if got, want := providerCacheName(kind, "x", "v1.14.3"), operatorTypes[kind]+"-x-v1.14.3-cache"; got != want {
			t.Errorf("%s: %q, want %q", kind, got, want)
		}
	}
}

func TestProvidersPending(t *testing.T) {
	t.Parallel()
	installed := func(g int64) condition { return condition{"ProviderInstalled", metav1.ConditionTrue, g, ""} }
	upgraded := func(g int64) condition { return condition{"ProviderUpgraded", metav1.ConditionTrue, g, ""} }
	rolling := providerDeployment("capi-system", "capi-controller-manager")
	rolling.Generation, rolling.Status.ObservedGeneration = 2, 1
	scaledDown := providerDeployment("capi-system", "capi-controller-manager")
	scaledDown.Spec.Replicas = new(int32(0))
	scaledDown.Status = appsv1.DeploymentStatus{}
	unhashed := provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1))
	unhashed.SetAnnotations(nil)
	for _, tc := range []struct {
		name     string
		provider *unstructured.Unstructured
		// cached is the hash on the provider's cache Secret, with none when empty.
		cached string
		dep    *appsv1.Deployment
		want   []string
	}{
		{"installed", provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1)), appliedHash, nil, nil},
		// The operator leaves ProviderInstalled at the generation it installed when the version changes.
		{"upgraded", provider("CoreProvider", "capi-system", "cluster-api", 2, installed(1), upgraded(2)), appliedHash, nil, nil},
		{
			"changed, not yet applied", provider("CoreProvider", "capi-system", "cluster-api", 3, installed(1), upgraded(2)), appliedHash, nil,
			[]string{"CoreProvider capi-system/cluster-api: generation 3 not applied yet"},
		},
		{
			"failing", provider("CoreProvider", "capi-system", "cluster-api", 2, installed(1),
				condition{"ProviderUpgraded", metav1.ConditionFalse, 2, "no route to host"}), appliedHash, nil,
			[]string{"CoreProvider capi-system/cluster-api: ProviderUpgraded: no route to host"},
		},
		// The operator has patched its cache Secret and not yet the provider.
		{
			"hash differs from the cache", provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1)), "9c1d07aa", nil,
			[]string{"CoreProvider capi-system/cluster-api: still being applied"},
		},
		{
			"no hash recorded", unhashed, appliedHash, nil,
			[]string{"CoreProvider capi-system/cluster-api: no applied spec recorded yet"},
		},
		{
			"no cache of its version", provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1)), "", nil,
			[]string{"CoreProvider capi-system/cluster-api: its manifests for v1.14.3 not cached yet"},
		},
		{
			"rolling out", provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1)), appliedHash, rolling,
			[]string{"Deployment capi-system/capi-controller-manager: rolling out"},
		},
		{"scaled to none", provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1)), appliedHash, scaledDown, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dep := tc.dep
			if dep == nil {
				dep = providerDeployment("capi-system", "capi-controller-manager")
			}
			objs := []client.Object{
				tc.provider, dep,
				// Another namespace's Deployment does not stand in for the provider's.
				providerDeployment("kubeadm-bootstrap-system", "capi-kubeadm-bootstrap-controller-manager"),
			}
			if tc.cached != "" {
				objs = append(objs, providerCache(tc.provider, tc.cached))
			}
			got, _, err := providersPending(context.Background(), operatorClient(t, &interceptor.Funcs{}, objs...))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("pending = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProvidersPendingWithoutADeployment(t *testing.T) {
	t.Parallel()
	p := provider("InfrastructureProvider", "proxmox-infrastructure-system", "proxmox", 1, condition{"ProviderInstalled", metav1.ConditionTrue, 1, ""})
	c := operatorClient(t, &interceptor.Funcs{}, p, providerCache(p, appliedHash))
	got, _, err := providersPending(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"namespace proxmox-infrastructure-system: no provider Deployment"}; !slices.Equal(got, want) {
		t.Errorf("pending = %q, want %q", got, want)
	}
}

// deploymentsAfter has the provider Deployments listed as change makes them once the bubble's clock
// has passed after, so that a test can replace or roll one out partway through a wait.
func deploymentsAfter(after time.Duration, change func(*appsv1.DeploymentList)) *interceptor.Funcs {
	start := time.Now()
	return &interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if deps, ok := list.(*appsv1.DeploymentList); ok && time.Since(start) >= after {
				change(deps)
			}
			return nil
		},
	}
}

// waitTook runs WaitForProviders on synctest's clock and gives how long it took and what it wrote.
func waitTook(t *testing.T, c client.Reader) (time.Duration, string) {
	t.Helper()
	var progress strings.Builder
	start := time.Now()
	if err := WaitForProviders(context.Background(), c, 2*time.Second, ProvidersSettle, &progress); err != nil {
		t.Fatal(err)
	}
	return time.Since(start), progress.String()
}

func TestWaitForProvidersWaitsOutTheSettle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		took, progress := waitTook(t, operatorClient(t, &interceptor.Funcs{}, installedCore(nil)...))
		if took < ProvidersSettle || took > ProvidersSettle+2*time.Second {
			t.Errorf("the wait took %s, want the settle of %s", took, ProvidersSettle)
		}
		if !strings.Contains(progress, "the providers to stay unchanged for 30s") {
			t.Errorf("progress = %q, want the settle named", progress)
		}
	})
}

// The operator's second install of a provider deletes its Deployments and creates them again.
func TestWaitForProvidersStartsOverOnAReplacedDeployment(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		replaced := &interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				deps, ok := list.(*appsv1.DeploymentList)
				switch elapsed := time.Since(start); {
				case !ok || elapsed < 10*time.Second:
				case elapsed < 16*time.Second:
					deps.Items = nil
				default:
					for i := range deps.Items {
						deps.Items[i].UID = types.UID("second")
					}
				}
				return nil
			},
		}
		took, progress := waitTook(t, operatorClient(t, replaced, installedCore(nil)...))
		if took < 16*time.Second+ProvidersSettle {
			t.Errorf("the wait took %s, want the settle after the Deployment came back at 16s", took)
		}
		if !strings.Contains(progress, "namespace capi-system: no provider Deployment") {
			t.Errorf("progress = %q, want the missing Deployment", progress)
		}
	})
}

func TestWaitForProvidersStartsOverOnANewGeneration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := operatorClient(t, deploymentsAfter(20*time.Second, func(deps *appsv1.DeploymentList) {
			for i := range deps.Items {
				deps.Items[i].Generation, deps.Items[i].Status.ObservedGeneration = 2, 2
			}
		}), installedCore(nil)...)
		if took, _ := waitTook(t, c); took < 20*time.Second+ProvidersSettle {
			t.Errorf("the wait took %s, want the settle after the change at 20s", took)
		}
	})
}

func TestWaitForProvidersWaitsForTheRollout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		objs := installedCore(func(dep *appsv1.Deployment) {
			dep.Generation, dep.Status.ObservedGeneration = 2, 1
		})
		c := operatorClient(t, deploymentsAfter(20*time.Second, func(deps *appsv1.DeploymentList) {
			for i := range deps.Items {
				deps.Items[i].Status.ObservedGeneration = 2
			}
		}), objs...)
		took, progress := waitTook(t, c)
		if took < 20*time.Second+ProvidersSettle {
			t.Errorf("the wait took %s, want the settle after the rollout at 20s", took)
		}
		if !strings.Contains(progress, "Deployment capi-system/capi-controller-manager: rolling out") {
			t.Errorf("progress = %q, want the rollout", progress)
		}
	})
}

func TestWaitForProvidersWaitsForTheCache(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		objs := installedCore(nil)
		objs[1].SetAnnotations(map[string]string{appliedSpecHashAnnotation: "9c1d07aa"})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		err := WaitForProviders(ctx, operatorClient(t, &interceptor.Funcs{}, objs...), 2*time.Second, ProvidersSettle, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "CoreProvider capi-system/cluster-api: still being applied") {
			t.Errorf("err = %v, want the provider still being applied", err)
		}
	})
}

func TestWaitForProvidersRidesOutTheAPIServer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lists := 0
		c := operatorClient(t, &interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				lists++
				if lists <= 2 {
					return errors.New("connection refused")
				}
				return c.List(ctx, list, opts...)
			},
		}, installedCore(nil)...)
		_, progress := waitTook(t, c)
		if strings.Count(progress, "connection refused") != 1 {
			t.Errorf("progress = %q, want the error once", progress)
		}
	})
}

func TestWaitForProvidersSaysWhatItWaitedFor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := provider("CoreProvider", "capi-system", "cluster-api", 2, condition{"ProviderInstalled", metav1.ConditionTrue, 1, ""})
		c := operatorClient(t, &interceptor.Funcs{}, p, providerCache(p, appliedHash), providerDeployment("capi-system", "capi-controller-manager"))
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := WaitForProviders(ctx, c, 2*time.Second, ProvidersSettle, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "CoreProvider capi-system/cluster-api: generation 2 not applied yet") {
			t.Errorf("err = %v, want it to name the provider", err)
		}
	})
}

func TestWaitForOperatorRetriesADryRun(t *testing.T) {
	t.Parallel()
	var creates int
	c := operatorClient(t, &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			o := &client.CreateOptions{}
			o.ApplyOptions(opts)
			if !slices.Equal(o.DryRun, []string{metav1.DryRunAll}) {
				t.Errorf("create without a dry run: %v", o.DryRun)
			}
			creates++
			if creates <= 2 {
				return errors.New(`failed calling webhook "vaddonprovider.kb.io": no route to host`)
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	if err := WaitForOperator(context.Background(), c, "capi-operator-system", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if creates != 3 {
		t.Errorf("%d attempts, want 3", creates)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(operatorAPI.WithKind("AddonProviderList"))
	if err := c.List(context.Background(), list); err != nil || len(list.Items) != 0 {
		t.Errorf("the probe was kept: %v %v", list.Items, err)
	}
}

func TestWaitForOperatorGivesTheLastRefusal(t *testing.T) {
	t.Parallel()
	c := operatorClient(t, &interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errors.New("no route to host")
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := WaitForOperator(ctx, c, "capi-operator-system", time.Millisecond); err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("err = %v, want the webhook's refusal", err)
	}
}
