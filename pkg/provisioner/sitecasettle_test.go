package provisioner

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var siteControlGVK = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Kind: "KubeadmControlPlane"}

// rolloutState is a control plane or MachineDeployment as the rollout of a site CA finds it.
type rolloutState struct {
	Generation, Observed int64
	UpToDate, Desired    int32
}

// workers is a cluster's MachineDeployment in state s.
func workers(c *clusterv1.Cluster, s rolloutState) *clusterv1.MachineDeployment {
	md := &clusterv1.MachineDeployment{}
	md.Namespace, md.Name, md.Generation = c.Namespace, c.Name+"-md-0", s.Generation
	md.Labels = map[string]string{clusterv1.ClusterNameLabel: c.Name}
	md.Spec.Replicas = &s.Desired
	md.Status.ObservedGeneration, md.Status.UpToDateReplicas = s.Observed, &s.UpToDate
	return md
}

// controlPlaneIn is a cluster's control plane object in state s.
func controlPlaneIn(c *clusterv1.Cluster, s rolloutState) *unstructured.Unstructured {
	cp := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"replicas": int64(s.Desired)},
		"status": map[string]any{"observedGeneration": s.Observed, "updatedReplicas": int64(s.UpToDate)},
	}}
	cp.SetGroupVersionKind(siteControlGVK)
	cp.SetNamespace(c.Namespace)
	cp.SetName(c.Name + "-cp")
	cp.SetGeneration(s.Generation)
	return cp
}

// settleWorld is a cluster given a site CA when its control plane and workers were at generation 1.
func settleWorld(t *testing.T, given time.Time, control, worker rolloutState) (client.Client, clusterv1.Cluster) {
	t.Helper()
	c := siteCluster("web")
	c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: siteControlGVK.Group, Kind: siteControlGVK.Kind, Name: "web-cp"}
	// The topology controller has reconciled the cluster's latest generation, which is what the
	// cluster's own conditions can show before the control plane and the workers show anything.
	markReconciled(&c)
	raw, err := json.Marshal(siteRollNote{At: given, Generations: map[string]int64{controlPlaneKey: 1, "MachineDeployment/web-md-0": 1}})
	if err != nil {
		t.Fatal(err)
	}
	c.Annotations = map[string]string{SiteRollAnnotation: string(raw)}
	scheme := testScheme(t)
	scheme.AddKnownTypeWithName(siteControlGVK, &unstructured.Unstructured{})
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{siteControlGVK.GroupVersion()})
	mapper.Add(siteControlGVK, meta.RESTScopeNamespace)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(&c, controlPlaneIn(&c, control), workers(&c, worker)).Build()
	return kube, c
}

func TestSiteCAPlaceStaysTakenUntilControlPlaneAndWorkersHaveTakenItUp(t *testing.T) {
	t.Parallel()
	stale := rolloutState{Generation: 1, Observed: 1, UpToDate: 3, Desired: 3}
	taken := rolloutState{Generation: 2, Observed: 2, UpToDate: 3, Desired: 3}
	changed := rolloutState{Generation: 2, Observed: 2, UpToDate: 1, Desired: 3}
	for name, tc := range map[string]struct {
		given            time.Time
		control, workers rolloutState
		settling         bool
	}{
		"topology reconciled, nothing changed yet": {siteNow, stale, stale, true},
		"workers not changed yet":                  {siteNow, taken, stale, true},
		"control plane not changed yet":            {siteNow, stale, taken, true},
		"workers changed, replicas not up to date": {siteNow, taken, changed, true},
		"control plane changed, replicas lag":      {siteNow, changed, taken, true},
		"both taken up":                            {siteNow, taken, taken, false},
		"change that altered nothing, waited out":  {siteNow.Add(-siteRollSettle - time.Minute), stale, stale, false},
		"replicas still lag after the wait":        {siteNow.Add(-siteRollSettle - time.Minute), stale, changed, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kube, c := settleWorld(t, tc.given, tc.control, tc.workers)
			got := SiteRollSettling(t.Context(), kube, []clusterv1.Cluster{c}, siteNow)
			if got[client.ObjectKeyFromObject(&c)] != tc.settling {
				t.Errorf("settling = %v, want %v", got, tc.settling)
			}
		})
	}
}

func TestSiteRollSettlingHoldsAClusterWhoseObjectsCannotBeRead(t *testing.T) {
	t.Parallel()
	c := siteCluster("web")
	c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: siteControlGVK.Group, Kind: siteControlGVK.Kind, Name: "web-cp"}
	c.Annotations = map[string]string{SiteRollAnnotation: `{"at":"2026-11-18T12:00:00Z","generations":{"ControlPlane":1}}`}
	// The kind is not mapped, so the control plane cannot be read.
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&c).Build()
	if got := SiteRollSettling(t.Context(), kube, []clusterv1.Cluster{c}, siteNow); !got[client.ObjectKeyFromObject(&c)] {
		t.Errorf("settling = %v, want the cluster to keep its place", got)
	}
}

func TestSiteCAGivenToAClusterNotesTheGenerationsItStartsFrom(t *testing.T) {
	t.Parallel()
	kube, c := settleWorld(t, siteNow, rolloutState{Generation: 4, Observed: 4, UpToDate: 1, Desired: 1}, rolloutState{Generation: 7, Observed: 7, UpToDate: 2, Desired: 2})
	delete(c.Annotations, SiteRollAnnotation)
	if err := kube.Update(t.Context(), &c); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, SiteCA: siteCAPEM, Now: func() time.Time { return siteNow }}
	if err := r.giveSiteCA(t.Context(), client.ObjectKeyFromObject(&c), siteCAPEM); err != nil {
		t.Fatal(err)
	}
	var live clusterv1.Cluster
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(&c), &live); err != nil {
		t.Fatal(err)
	}
	var note siteRollNote
	if err := json.Unmarshal([]byte(live.Annotations[SiteRollAnnotation]), &note); err != nil {
		t.Fatal(err)
	}
	if note.Generations[controlPlaneKey] != 4 || note.Generations["MachineDeployment/web-md-0"] != 7 || !note.At.Equal(siteNow) {
		t.Errorf("note = %+v, want the control plane at 4 and the workers at 7, noted at %v", note, siteNow)
	}
}

func TestSiteCARollSkipsAClusterWhoseWindowCannotBeReadAndReportsIt(t *testing.T) {
	t.Parallel()
	bad := siteCluster("a-bad", jsonVar(upgradesVariable, `{"mode":"patch","window":{"start":"02:00","timeZone":"Mars/Olympus"}}`))
	good := siteCluster("z-good")
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&bad, &good).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	recorder := events.NewFakeRecorder(10)
	r := &Reconciler{Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, SiteCA: siteCAPEM, Events: recorder, Now: func() time.Time { return siteNow }}
	ctx := t.Context()
	for _, c := range []*clusterv1.Cluster{&good, &bad} {
		var live clusterv1.Cluster
		if err := kube.Get(ctx, client.ObjectKeyFromObject(c), &live); err != nil {
			t.Fatal(err)
		}
		if _, err := r.rollSiteCA(ctx, &live); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
	}
	var live clusterv1.Cluster
	if err := kube.Get(ctx, client.ObjectKeyFromObject(&good), &live); err != nil {
		t.Fatal(err)
	}
	if appliedSiteCA(&live) == "" {
		t.Error("the bad cluster's window kept the site CA from the other cluster")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(&bad), &live); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(live.Status.Conditions, SiteRollCondition)
	if appliedSiteCA(&live) != "" || cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "Mars/Olympus") {
		t.Errorf("bad cluster: site CA %q, condition %+v, want none given and the window named", appliedSiteCA(&live), cond)
	}
	if len(recorder.Events) != 1 {
		t.Errorf("%d events, want one for the bad cluster", len(recorder.Events))
	}
}

func TestPlanSiteRollSkipsAClusterWhoseWindowCannotBeRead(t *testing.T) {
	t.Parallel()
	clusters := []clusterv1.Cluster{
		siteCluster("a-bad", jsonVar(upgradesVariable, `{"mode":"patch","window":{"timeZone":"Mars/Olympus"}}`)),
		siteCluster("z-good"),
	}
	plan := PlanSiteRoll("new", SiteRollClusters(clusters, siteClassNamespace, nil), 1, siteNow)
	if !slices.Equal(plan.Now, keys("z-good")) || plan.Skipped[keys("a-bad")[0]] == nil {
		t.Errorf("plan = %+v, want z-good to roll and a-bad skipped with a reason", plan)
	}
}

func TestPausedClusterMidRollHoldsNoPlaceAndTakesOneAgainWhenUnpaused(t *testing.T) {
	t.Parallel()
	paused := siteCluster("a-paused")
	paused.Spec.Paused = new(true)
	paused.Status.Conditions = append(paused.Status.Conditions, metav1.Condition{Type: clusterv1.ClusterRollingOutCondition, Status: metav1.ConditionTrue})
	waiting := siteCluster("b")
	plan := PlanSiteRoll("new", SiteRollClusters([]clusterv1.Cluster{paused, waiting}, siteClassNamespace, nil), 1, siteNow)
	if !slices.Equal(plan.Now, keys("b")) || plan.Rolling != 0 {
		t.Errorf("paused: plan = %+v, want b to roll, the paused cluster holding no place", plan)
	}
	paused.Spec.Paused = nil
	plan = PlanSiteRoll("new", SiteRollClusters([]clusterv1.Cluster{paused, waiting}, siteClassNamespace, nil), 1, siteNow)
	if len(plan.Now) != 0 || plan.Rolling != 1 || !slices.Contains(plan.Waiting, keys("b")[0]) {
		t.Errorf("unpaused: plan = %+v, want the cluster to hold its place again and b to wait", plan)
	}
}

func TestOnlyTheControllerMaySetTheSiteCAsVariable(t *testing.T) {
	t.Parallel()
	as := func(user string) context.Context {
		var req admission.Request
		req.UserInfo = authenticationv1.UserInfo{Username: user}
		return admission.NewContextWithRequest(t.Context(), req)
	}
	without := siteCluster("web")
	with := siteCluster("web", jsonVar(SiteCAsVariable, `["x"]`))
	other := siteCluster("web", jsonVar(SiteCAsVariable, `["y"]`))
	const controller = "system:serviceaccount:" + siteClassNamespace + ":kubehenge-controller"
	for name, tc := range map[string]struct {
		ctx      context.Context //nolint:containedctx // a table of requests
		old, now *clusterv1.Cluster
		refused  bool
	}{
		"a user adds it":                                   {as("alice"), &without, &with, true},
		"a user creates a cluster with it":                 {as("alice"), nil, &with, true},
		"a user changes it":                                {as("alice"), &with, &other, true},
		"a user removes it":                                {as("alice"), &with, &without, true},
		"a user leaves it as it is":                        {as("alice"), &with, &with, false},
		"a user creates one without it":                    {as("alice"), nil, &without, false},
		"the controller adds it":                           {as(controller), &without, &with, false},
		"another service account in a different namespace": {as("system:serviceaccount:elsewhere:kubehenge-controller"), &without, &with, true},
		"no request":                                       {t.Context(), &without, &with, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			errs := validateSiteCAsOwner(tc.ctx, tc.old, tc.now, siteClassNamespace)
			if (len(errs) > 0) != tc.refused {
				t.Errorf("refused = %v (%v), want %v", len(errs) > 0, errs, tc.refused)
			}
		})
	}
}

func TestAnUpgradeWindowTheControllerCannotReadIsRefusedAtAdmission(t *testing.T) {
	t.Parallel()
	window := func(raw string) *clusterv1.Cluster {
		c := siteCluster("web", jsonVar(upgradesVariable, `{"mode":"patch","window":`+raw+`}`))
		return &c
	}
	old := window(`{"start":"02:00"}`)
	for name, tc := range map[string]struct {
		now     *clusterv1.Cluster
		refused bool
	}{
		"an unknown zone": {window(`{"timeZone":"Mars/Olympus"}`), true},
		"the wrong case":  {window(`{"timeZone":"europe/zurich"}`), true},
		"Local":           {window(`{"timeZone":"Local"}`), true},
		"a bad start":     {window(`{"start":"25:99"}`), true},
		"a known zone":    {window(`{"timeZone":"Europe/Kyiv","start":"03:00"}`), false},
		"no zone":         {window(`{"start":"03:00"}`), false},
	} {
		if errs := validateUpgradeWindow(old, tc.now); (len(errs) > 0) != tc.refused {
			t.Errorf("%s: errors %v, want refused = %v", name, errs, tc.refused)
		}
		if errs := validateUpgradeWindow(nil, tc.now); (len(errs) > 0) != tc.refused {
			t.Errorf("%s on creation: errors %v, want refused = %v", name, errs, tc.refused)
		}
	}
	// A cluster that already holds a bad zone can still change something else.
	bad := window(`{"timeZone":"Mars/Olympus"}`)
	if errs := validateUpgradeWindow(bad, bad); len(errs) != 0 {
		t.Errorf("an unchanged window was refused: %v", errs)
	}
}

func TestSiteTimeZoneThatIsNoTimeZoneIsReportedOnEveryCluster(t *testing.T) {
	t.Parallel()
	c := siteCluster("web")
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&c).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	recorder := events.NewFakeRecorder(10)
	r := &Reconciler{Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, Events: recorder, Now: func() time.Time { return siteNow }, SiteTimeZoneProblem: `--time-zone: time zone "Mars/Olympus" is not an IANA time zone`}
	var live clusterv1.Cluster
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(&c), &live); err != nil {
		t.Fatal(err)
	}
	if _, err := r.rollSiteCA(t.Context(), &live); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(&c), &live); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(live.Status.Conditions, SiteTimeZoneCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "Mars/Olympus") || len(recorder.Events) != 1 {
		t.Errorf("condition %+v with %d events, want the zone named in both", cond, len(recorder.Events))
	}
}

func TestACertificateBundleWithABrokenBlockIsRefusedWholeNamingTheBlock(t *testing.T) {
	t.Parallel()
	good := siteAuthority(t)
	broken := strings.Replace(siteAuthority(t), "-----END CERTIFICATE-----", "", 1)
	for name, bundle := range map[string]string{"first": broken + good, "second": good + broken} {
		err := checkCertificates(bundle, siteNow)
		want := "block 1 "
		if name == "second" {
			want = "block 2 "
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("a broken %s block: error %v, want one naming %q", name, err, want)
		}
	}
	if err := checkCertificates(good+good, siteNow); err != nil {
		t.Errorf("two good blocks: %v", err)
	}
}
