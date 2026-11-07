package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	psapi "k8s.io/pod-security-admission/api"
	"k8s.io/pod-security-admission/policy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// podSecurityExemptKey is where the chart lists, in the interface's ConfigMap, the namespaces Pod
// Security Admission leaves alone in every cluster, as a JSON list.
const podSecurityExemptKey = "podSecurityExempt"

// refusedPodsLimit bounds the pods a check lists.
const refusedPodsLimit = 500

// podSecurityCheck is what the restricted Pod Security level would refuse of the pods a cluster
// runs now.
type podSecurityCheck struct {
	// Checked counts the pods looked at, those of exempt namespaces and finished ones left out.
	Checked int `json:"checked"`
	// Truncated says the cluster has more pods than were read, or more refused than are listed.
	Truncated bool         `json:"truncated"`
	Refused   []refusedPod `json:"refused"`
	Skipped   string       `json:"skipped,omitempty"`
}

type refusedPod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reasons   string `json:"reasons"`
}

// exemptNamespaces reads the namespaces the chart exempts from Pod Security Admission.
func (s *Server) exemptNamespaces(ctx context.Context, kube client.Client) ([]string, error) {
	cm := &corev1.ConfigMap{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: interfaceConfigMap}, cm)
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		return []string{metav1.NamespaceSystem}, nil
	case err != nil:
		return nil, fmt.Errorf("reading the namespaces Pod Security Admission exempts: %w", err)
	}
	exempt := []string{metav1.NamespaceSystem}
	if raw := cm.Data[podSecurityExemptKey]; raw != "" {
		var listed []string
		if err := json.Unmarshal([]byte(raw), &listed); err != nil {
			return nil, fmt.Errorf("ConfigMap %s/%s: %s: %w", s.ClassNamespace, interfaceConfigMap, podSecurityExemptKey, err)
		}
		exempt = append(exempt, listed...)
	}
	return exempt, nil
}

// checkPodSecurity answers which running pods the restricted Pod Security level would refuse, for
// the dialog that switches CIS hardening on. The pods are read as the cluster's health identity, and
// judged by Kubernetes' own checks.
func (s *Server) checkPodSecurity(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	exempt, err := s.exemptNamespaces(r.Context(), u.kube)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	reached, why, err := s.healthWorkload(r.Context(), u.kube, c, provisioner.IdentityHealth)
	switch {
	case err != nil:
		writeJSON(w, http.StatusOK, podSecurityCheck{Refused: []refusedPod{}, Skipped: "The cluster could not be reached: " + problem(err)})
		return
	case why != "":
		writeJSON(w, http.StatusOK, podSecurityCheck{Refused: []refusedPod{}, Skipped: why})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), healthDeadline)
	defer cancel()
	pods, err := reached.Kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{ResourceVersion: "0", Limit: healthListLimit})
	if err != nil {
		writeJSON(w, http.StatusOK, podSecurityCheck{Refused: []refusedPod{}, Skipped: "The cluster's pods could not be read: " + problem(err)})
		return
	}
	check, err := restrictedCheck(pods.Items, exempt)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	check.Truncated = check.Truncated || pods.Continue != ""
	writeJSON(w, http.StatusOK, check)
}

// restrictedCheck judges pods by the restricted level at the newest version, as the cluster's
// admission does with CIS hardening on. Pods in the exempt namespaces and finished pods are left
// out.
func restrictedCheck(pods []corev1.Pod, exempt []string) (podSecurityCheck, error) {
	evaluator, err := policy.NewEvaluator(policy.DefaultChecks(), nil)
	if err != nil {
		return podSecurityCheck{}, fmt.Errorf("setting up the Pod Security checks: %w", err)
	}
	level := psapi.LevelVersion{Level: psapi.LevelRestricted, Version: psapi.LatestVersion()}
	out := podSecurityCheck{Refused: []refusedPod{}}
	for i := range pods {
		p := &pods[i]
		if slices.Contains(exempt, p.Namespace) || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		out.Checked++
		result := policy.AggregateCheckResults(evaluator.EvaluatePod(level, &p.ObjectMeta, &p.Spec))
		if result.Allowed {
			continue
		}
		if len(out.Refused) == refusedPodsLimit {
			out.Truncated = true
			continue
		}
		out.Refused = append(out.Refused, refusedPod{Namespace: p.Namespace, Name: p.Name, Reasons: result.ForbiddenReason()})
	}
	return out, nil
}
