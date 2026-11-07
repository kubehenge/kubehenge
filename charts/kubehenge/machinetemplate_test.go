package kubehenge_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const decodeBuffer = 4096

// decodeObjects reads every object of a stream of YAML documents.
func decodeObjects(t *testing.T, data []byte) []*unstructured.Unstructured {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), decodeBuffer)
	var out []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		if len(u.Object) > 0 {
			out = append(out, u)
		}
	}
}

// machineTemplateValidators are the validators of the Proxmox VE provider's CRDs, by kind and
// version, from the release `make capmox-crds` downloads.
func machineTemplateValidators(t *testing.T) map[string]validation.SchemaValidator {
	t.Helper()
	files, err := filepath.Glob("../../test/crds/capmox-*.yaml")
	if err != nil || len(files) != 1 {
		t.Fatalf("the Proxmox VE provider's CRDs are missing (%v, %v): run `make capmox-crds`", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	validators := map[string]validation.SchemaValidator{}
	for _, o := range decodeObjects(t, raw) {
		if o.GetKind() != "CustomResourceDefinition" {
			continue
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, crd); err != nil {
			t.Fatal(err)
		}
		if crd.Spec.Names.Kind != "ProxmoxMachineTemplate" {
			continue
		}
		for _, v := range crd.Spec.Versions {
			var schema apiextensions.JSONSchemaProps
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v.Schema.OpenAPIV3Schema, &schema, nil); err != nil {
				t.Fatal(err)
			}
			validator, _, err := validation.NewSchemaValidator(&schema)
			if err != nil {
				t.Fatal(err)
			}
			validators[crd.Spec.Group+"/"+v.Name] = validator
		}
	}
	if len(validators) == 0 {
		t.Fatalf("%s holds no ProxmoxMachineTemplate CRD", files[0])
	}
	return validators
}

// A value the CRD does not accept, such as a matchPolicy outside its enum, is refused by the API
// server for every cluster class, so the chart's rendered machine templates must pass the schema.
func TestMachineTemplatesFollowTheProviderSchema(t *testing.T) {
	t.Parallel()
	validators := machineTemplateValidators(t)
	helm := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system", "-f", "../../pkg/provisioner/testdata/integration-values.yaml") //nolint:gosec // the test's own chart and values
	var stderr bytes.Buffer
	helm.Stderr = &stderr
	out, err := helm.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	var checked int
	for _, o := range decodeObjects(t, out) {
		if o.GetKind() != "ProxmoxMachineTemplate" {
			continue
		}
		checked++
		validator, ok := validators[o.GroupVersionKind().Group+"/"+o.GroupVersionKind().Version]
		if !ok {
			t.Fatalf("%s: no CRD version %s", o.GetName(), o.GetAPIVersion())
		}
		if errs := validation.ValidateCustomResource(nil, o.Object, validator); len(errs) > 0 {
			t.Errorf("%s: %v", o.GetName(), errs.ToAggregate())
		}
		// The check has to be able to fail.
		bad := o.DeepCopy()
		if err := unstructured.SetNestedField(bad.Object, "subset", "spec", "template", "spec", "templateSelector", "matchPolicy"); err != nil {
			t.Fatal(err)
		}
		if errs := validation.ValidateCustomResource(nil, bad.Object, validator); len(errs) == 0 {
			t.Errorf("%s: a matchPolicy outside the CRD's enum passed", o.GetName())
		}
	}
	if checked == 0 {
		t.Fatal("the chart renders no ProxmoxMachineTemplate")
	}
}
