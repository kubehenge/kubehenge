//go:build integration

package provisioner

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestIntegrationHARulesOfferedThroughTheChart holds the chart's accounts to the HA rules offered
// to cluster creators: the controller keeps the record, and the interface may read it, and nothing
// else of it, only while interface.offerHARules is on.
func TestIntegrationHARulesOfferedThroughTheChart(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, admin, _ := startAPIConfig(t)
	ctx := context.Background()
	as := func(account string) client.Client {
		t.Helper()
		impersonated := rest.CopyConfig(cfg)
		impersonated.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:" + classNamespace + ":" + account,
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + classNamespace, "system:authenticated"},
		}
		c, err := client.New(impersonated, client.Options{Scheme: admin.Scheme()})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	controller, ui := as("kubehenge-controller"), as("kubehenge-interface")
	key := client.ObjectKey{Namespace: classNamespace, Name: HARulesName}

	record := &corev1.ConfigMap{Namespace: classNamespace, Name: HARulesName, Data: map[string]string{HARulesKey: `[]`}}
	if err := controller.Create(ctx, record); err != nil {
		t.Fatalf("the controller cannot record the rules: %v", err)
	}
	record.Data[HARulesKey] = `[{"rule":"fast","nodes":["pve1"]}]`
	if err := controller.Update(ctx, record); err != nil {
		t.Fatalf("the controller cannot change the record: %v", err)
	}
	if err := ui.Get(ctx, key, &corev1.ConfigMap{}); !apierrors.IsForbidden(err) {
		t.Fatalf("the interface reads the record without the setting: %v", err)
	}

	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace,
		"testdata/integration-values.yaml", "testdata/offer-ha-rules-values.yaml") {
		if o.GetKind() == "Role" && o.GetName() == "kubehenge-interface" && o.GetNamespace() == classNamespace {
			if err := admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), client.FieldOwner("helm"), client.ForceOwnership); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := &corev1.ConfigMap{}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err := ui.Get(ctx, key, read)
		if err == nil {
			break
		}
		if !apierrors.IsForbidden(err) || time.Now().After(deadline) {
			t.Fatalf("the interface cannot read the record with the setting on: %v", err)
		}
	}
	if offered, err := ParseOfferedRules(read); err != nil || len(offered) != 1 || offered[0].Rule != "fast" {
		t.Fatalf("read %v: %v", offered, err)
	}
	read.Data[HARulesKey] = `[]`
	if err := ui.Update(ctx, read); !apierrors.IsForbidden(err) {
		t.Errorf("the interface changes the record: %v", err)
	}
	if err := ui.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: ReleaseChecksumsName}, &corev1.ConfigMap{}); !apierrors.IsForbidden(err) {
		t.Errorf("the interface reads another ConfigMap: %v", err)
	}
	if err := controller.Delete(ctx, record); err != nil {
		t.Errorf("the controller cannot withdraw the record: %v", err)
	}
}
