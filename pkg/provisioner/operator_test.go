package provisioner

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func operatorClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
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

func provider(kind, ns, name string, generation int64, conditions ...condition) *unstructured.Unstructured {
	p := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	p.SetGroupVersionKind(operatorAPI.WithKind(kind))
	p.SetNamespace(ns)
	p.SetName(name)
	p.SetGeneration(generation)
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

func TestProvidersPending(t *testing.T) {
	t.Parallel()
	installed := func(g int64) condition { return condition{"ProviderInstalled", metav1.ConditionTrue, g, ""} }
	upgraded := func(g int64) condition { return condition{"ProviderUpgraded", metav1.ConditionTrue, g, ""} }
	rolling := providerDeployment("capi-system", "capi-controller-manager")
	rolling.Generation, rolling.Status.ObservedGeneration = 2, 1
	scaledDown := providerDeployment("capi-system", "capi-controller-manager")
	scaledDown.Spec.Replicas = new(int32(0))
	scaledDown.Status = appsv1.DeploymentStatus{}
	for _, tc := range []struct {
		name     string
		provider *unstructured.Unstructured
		dep      *appsv1.Deployment
		want     []string
	}{
		{"installed", provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1)), nil, nil},
		// The operator leaves ProviderInstalled at the generation it installed when the version changes.
		{"upgraded", provider("CoreProvider", "capi-system", "cluster-api", 2, installed(1), upgraded(2)), nil, nil},
		{
			"changed, not yet applied", provider("CoreProvider", "capi-system", "cluster-api", 3, installed(1), upgraded(2)), nil,
			[]string{"CoreProvider capi-system/cluster-api: generation 3 not applied yet"},
		},
		{
			"failing", provider("CoreProvider", "capi-system", "cluster-api", 2, installed(1),
				condition{"ProviderUpgraded", metav1.ConditionFalse, 2, "no route to host"}), nil,
			[]string{"CoreProvider capi-system/cluster-api: ProviderUpgraded: no route to host"},
		},
		{
			"rolling out", provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1)), rolling,
			[]string{"Deployment capi-system/capi-controller-manager: rolling out"},
		},
		{"scaled to none", provider("CoreProvider", "capi-system", "cluster-api", 1, installed(1)), scaledDown, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dep := tc.dep
			if dep == nil {
				dep = providerDeployment("capi-system", "capi-controller-manager")
			}
			c := operatorClient(t, &interceptor.Funcs{}, tc.provider, dep,
				// Another namespace's Deployment does not stand in for the provider's.
				providerDeployment("kubeadm-bootstrap-system", "capi-kubeadm-bootstrap-controller-manager"))
			got, err := ProvidersPending(context.Background(), c)
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
	c := operatorClient(t, &interceptor.Funcs{},
		provider("InfrastructureProvider", "proxmox-infrastructure-system", "proxmox", 1, condition{"ProviderInstalled", metav1.ConditionTrue, 1, ""}))
	got, err := ProvidersPending(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"namespace proxmox-infrastructure-system: no provider Deployment"}; !slices.Equal(got, want) {
		t.Errorf("pending = %q, want %q", got, want)
	}
}

func TestWaitForProvidersRidesOutTheAPIServer(t *testing.T) {
	t.Parallel()
	lists := 0
	c := operatorClient(t, &interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists++
			if lists <= 2 {
				return errors.New("connection refused")
			}
			return c.List(ctx, list, opts...)
		},
	}, provider("CoreProvider", "capi-system", "cluster-api", 1, condition{"ProviderInstalled", metav1.ConditionTrue, 1, ""}),
		providerDeployment("capi-system", "capi-controller-manager"))
	var progress strings.Builder
	if err := WaitForProviders(context.Background(), c, time.Millisecond, &progress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "connection refused") || strings.Count(progress.String(), "waiting for") != 1 {
		t.Errorf("progress = %q, want the error once", progress.String())
	}
}

func TestWaitForProvidersSaysWhatItWaitedFor(t *testing.T) {
	t.Parallel()
	c := operatorClient(t, &interceptor.Funcs{},
		provider("CoreProvider", "capi-system", "cluster-api", 2, condition{"ProviderInstalled", metav1.ConditionTrue, 1, ""}),
		providerDeployment("capi-system", "capi-controller-manager"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := WaitForProviders(ctx, c, time.Millisecond, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "CoreProvider capi-system/cluster-api: generation 2 not applied yet") {
		t.Errorf("err = %v, want it to name the provider", err)
	}
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
