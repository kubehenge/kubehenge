package ui

import (
	"context"
	"io"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// demoMetrics is what a demo cluster's API server reports of the deprecated APIs clients requested.
const demoMetrics = `# TYPE apiserver_requested_deprecated_apis gauge
apiserver_requested_deprecated_apis{group="orders.example.com",removed_release="1.37",resource="invoices",subresource="",version="v1beta1"} 1
`

// demoDeprecatedAPIs opens the demo's metrics.
func demoDeprecatedAPIs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(demoMetrics)), nil
}

// demoWebhook names the demo's webhook configuration and the Service behind it.
const demoWebhook = "order-policy"

// demoThinWebhook is a webhook that fails closed with a single endpoint behind it, which an
// upgrade's drain can take.
func demoThinWebhook() []runtime.Object {
	return []runtime.Object{
		&admissionregistrationv1.ValidatingWebhookConfiguration{
			Name: demoWebhook,
			Webhooks: []admissionregistrationv1.ValidatingWebhook{{
				Name: demoWebhook + ".example.com", FailurePolicy: new(admissionregistrationv1.Fail),
				ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: "billing", Name: demoWebhook}},
			}},
		},
		&discoveryv1.EndpointSlice{
			Namespace: "billing", Name: demoWebhook + "-1", Labels: map[string]string{discoveryv1.LabelServiceName: demoWebhook},
			Endpoints: []discoveryv1.Endpoint{{
				NodeName: new("worker"), Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
			}},
		},
	}
}
