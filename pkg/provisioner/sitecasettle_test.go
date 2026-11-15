package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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

func TestSiteRollSettlingHoldsAClusterWhoseObjectsCannotBeReadForTheSettleTimeAlone(t *testing.T) {
	t.Parallel()
	c := siteCluster("web")
	c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: siteControlGVK.Group, Kind: siteControlGVK.Kind, Name: "web-cp"}
	c.Annotations = map[string]string{SiteRollAnnotation: `{"at":"2026-11-18T12:00:00Z","generations":{"ControlPlane":1}}`}
	// The kind is not mapped, so the control plane cannot be read.
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&c).Build()
	if got := SiteRollSettling(t.Context(), kube, []clusterv1.Cluster{c}, siteNow.Add(time.Minute)); !got[client.ObjectKeyFromObject(&c)] {
		t.Errorf("a minute in: settling = %v, want the cluster to keep its place", got)
	}
	if got := SiteRollSettling(t.Context(), kube, []clusterv1.Cluster{c}, siteNow.Add(siteRollSettle+time.Minute)); got[client.ObjectKeyFromObject(&c)] {
		t.Errorf("past the settle time: settling = %v, want an unreadable cluster to hold no place for good", got)
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

func TestClusterPausedByTheAnnotationHoldsNoPlaceAndTakesNoSiteCA(t *testing.T) {
	t.Parallel()
	paused := siteCluster("a-paused")
	paused.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
	rolling := paused.DeepCopy()
	rolling.Status.Conditions = append(rolling.Status.Conditions, metav1.Condition{Type: clusterv1.ClusterRollingOutCondition, Status: metav1.ConditionTrue})
	b := siteCluster("b")
	plan := PlanSiteRoll("new", SiteRollClusters([]clusterv1.Cluster{*rolling, b}, siteClassNamespace, nil), 1, siteNow)
	if !slices.Equal(plan.Now, keys("b")) || plan.Rolling != 0 {
		t.Errorf("paused mid-roll: plan = %+v, want b to roll, the paused cluster holding no place", plan)
	}
	plan = PlanSiteRoll("new", SiteRollClusters([]clusterv1.Cluster{paused}, siteClassNamespace, nil), 1, siteNow)
	if len(plan.Now) != 0 || !slices.Equal(plan.Waiting, keys("a-paused")) {
		t.Errorf("paused, not rolling: plan = %+v, want it to wait for its unpause", plan)
	}
}

func TestSiteCAThatCannotBeGivenStopsNoOtherStepOrCluster(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Status.Conditions = []metav1.Condition{{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue}}
	// The fake API server knows no control-plane kind, so the rollout cannot be read.
	c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: siteControlGVK.Group, Kind: siteControlGVK.Kind, Name: "web-cp"}
	r, kube := newReconciler(t, &fakeCloud{}, c)
	r.Reader, r.SiteCA, r.Now = kube, siteAuthority(t), func() time.Time { return siteNow }
	if err := reconcile(t, r, c); err == nil || !strings.Contains(err.Error(), "site CA") {
		t.Errorf("reconcile: %v, want the site CA's failure returned", err)
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: c.Namespace, Name: "web-proxmox-ccm"}, hcp); err != nil {
		t.Errorf("the add-ons were not applied after the site CA failed: %v", err)
	}
	var live clusterv1.Cluster
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(c), &live); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(live.Status.Conditions, SiteRollCondition)
	if cond == nil || cond.Reason != SiteCANotGivenReason {
		t.Fatalf("condition %+v, want %s", cond, SiteCANotGivenReason)
	}
	other := siteCluster("other")
	plan := PlanSiteRoll(r.SiteCA, SiteRollClusters([]clusterv1.Cluster{live, other}, r.ClassNamespace, nil), 1, siteNow)
	if !slices.Equal(plan.Now, keys("other")) || plan.Skipped[client.ObjectKeyFromObject(c)] == nil {
		t.Errorf("plan = %+v, want the failing cluster skipped and the other given the place", plan)
	}
}

func TestTheSiteCAsVariableIsHeldToTheControllerWithoutRefusingAnyone(t *testing.T) {
	t.Parallel()
	ca := siteAuthority(t)
	const controller = "system:serviceaccount:" + siteClassNamespace + ":kubehenge-controller"
	const topology = "system:serviceaccount:capi-system:capi-manager"
	const note = `{"at":"2026-11-18T12:00:00Z","generations":{"ControlPlane":1}}`
	given := siteCluster("web", jsonVar(SiteCAsVariable, `["stored"]`))
	given.Annotations = map[string]string{SiteRollAnnotation: note}
	request := func(user string, op admissionv1.Operation, old *clusterv1.Cluster) context.Context {
		var req admission.Request
		req.Operation, req.UserInfo = op, authenticationv1.UserInfo{Username: user}
		if old != nil {
			raw, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			req.OldObject.Raw = raw
		}
		return admission.NewContextWithRequest(t.Context(), req)
	}
	create := func(user string) context.Context { return request(user, admissionv1.Create, nil) }
	update := func(user string) context.Context { return request(user, admissionv1.Update, &given) }
	withNote := func(c clusterv1.Cluster) clusterv1.Cluster {
		c.Annotations = map[string]string{SiteRollAnnotation: note}
		return c
	}
	for name, tc := range map[string]struct {
		ctx     context.Context //nolint:containedctx // a table of requests
		in      clusterv1.Cluster
		now     time.Time
		want    string
		keepsNo bool
	}{
		"a new cluster without it gets the site CA":               {create("alice"), unbuilt("web"), siteNow, ca, false},
		"a recovery create with an old value gets the site CA":    {create("alice"), withNote(unbuilt("web", jsonVar(SiteCAsVariable, `["old"]`))), siteNow, ca, false},
		"a new cluster gets no site CA that has lapsed":           {create("alice"), unbuilt("web", jsonVar(SiteCAsVariable, `["old"]`)), siteNow.Add(2 * time.Hour), "", false},
		"a create read without a request gets the site CA":        {t.Context(), unbuilt("web"), siteNow, ca, false},
		"client-side apply without the field keeps the stored":    {update("alice"), siteCluster("web"), siteNow, "stored", true},
		"replace with a stale value keeps the stored":             {update("alice"), siteCluster("web", jsonVar(SiteCAsVariable, `["stale"]`)), siteNow, "stored", true},
		"another account of that name elsewhere keeps the stored": {update("system:serviceaccount:elsewhere:kubehenge-controller"), siteCluster("web"), siteNow, "stored", true},
		"the topology controller's update passes untouched":       {update(topology), withNote(siteCluster("web", jsonVar(SiteCAsVariable, `["stored"]`))), siteNow, "stored", true},
		"the controller's write passes as it is":                  {update(controller), siteCluster("web", jsonVar(SiteCAsVariable, `["new"]`)), siteNow, "new", false},
		"the controller's removal passes as it is":                {update(controller), siteCluster("web"), siteNow, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := &ClusterDefaulter{Hooks: &Hooks{ClassNamespace: siteClassNamespace, SiteCA: func() string { return ca }, Now: func() time.Time { return tc.now }}}
			c := tc.in.DeepCopy()
			c.Spec.ClusterNetwork.Pods.CIDRBlocks, c.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"10.42.0.0/16"}, []string{"10.43.0.0/16"}
			if err := d.Default(tc.ctx, c); err != nil {
				t.Fatal(err)
			}
			if got := appliedSiteCA(c); got != tc.want {
				t.Errorf("siteCAs = %q, want %q", got, tc.want)
			}
			if tc.keepsNo && c.Annotations[SiteRollAnnotation] != note {
				t.Errorf("rollout note = %q, want the stored one kept", c.Annotations[SiteRollAnnotation])
			}
			if !tc.keepsNo && tc.in.Annotations[SiteRollAnnotation] == "" && c.Annotations[SiteRollAnnotation] != "" {
				t.Errorf("rollout note %q appeared", c.Annotations[SiteRollAnnotation])
			}
		})
	}
	// The topology controller's own update that leaves the variable as stored is not changed at all.
	c := given.DeepCopy()
	c.Spec.ClusterNetwork.Pods.CIDRBlocks, c.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"10.42.0.0/16"}, []string{"10.43.0.0/16"}
	before := c.DeepCopy()
	d := &ClusterDefaulter{Hooks: &Hooks{ClassNamespace: siteClassNamespace, SiteCA: func() string { return ca }, Now: func() time.Time { return siteNow }}}
	if err := d.Default(update(topology), c); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(before, c) {
		t.Errorf("the topology controller's update was changed:\n%+v\nwant\n%+v", c.Spec.Topology.Variables, before.Spec.Topology.Variables)
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

func TestADamagedBoundaryOutsideThePEMBlocksIsRefusedNamingItsLine(t *testing.T) {
	t.Parallel()
	good := siteAuthority(t) + "\n"
	lines := strings.Count(good, "\n")
	noBegin := good[strings.Index(good, "\n")+1:]
	for name, tc := range map[string]struct {
		bundle string
		line   int
	}{
		"first BEGIN line lost a dash":      {strings.Replace(good, "-----BEGIN", "----BEGIN", 1) + good, 1},
		"first BEGIN line lost a letter":    {strings.Replace(good, "BEGIN", "BEGN", 1) + good, 1},
		"first BEGIN line lost its dashes":  {strings.Replace(good, "-----BEGIN CERTIFICATE-----", "BEGIN CERTIFICATE", 1) + good, 1},
		"first BEGIN line missing":          {noBegin + good, 1},
		"second BEGIN line lost a dash":     {good + strings.Replace(good, "-----BEGIN", "----BEGIN", 1), lines + 1},
		"second BEGIN line lost a letter":   {good + strings.Replace(good, "BEGIN", "BEGN", 1), lines + 1},
		"second BEGIN line missing":         {good + "subject=CN=site\n" + noBegin, lines + 2},
		"after openssl's attributes, a gap": {"Bag Attributes\n    friendlyName: site\n" + noBegin, 3},
		"text after the last block":         {good + "\nextra\n", lines + 2},
	} {
		err := checkCertificates(tc.bundle, siteNow)
		if want := fmt.Sprintf("on line %d", tc.line); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want one naming %q", name, err, want)
		}
	}
	text := "Certificate:\n    Data:\n        Version: 3 (0x2)\n        Serial Number: 1 (0x1)\n" +
		"        Signature Algorithm: ecdsa-with-SHA256\n            30:45:02:21:00:d3:5c:9b:7e:61:aa:10:42:8f:2e:91:0c:3b:77:52:\n"
	for name, bundle := range map[string]string{
		"CRLF":                     strings.ReplaceAll(good+good, "\n", "\r\n"),
		"blank lines":              "\n\n" + good + "\n  \n" + good + "\n",
		"openssl pkcs12 -nokeys":   "Bag Attributes\n    friendlyName: site\n    localKeyID: 01 02 03\nsubject=CN=site\nissuer=CN=site\n" + good,
		"openssl x509 -text":       text + good,
		"attributes of each block": "subject=CN=site\nissuer=CN=site\n" + good + "subject=CN=site\nissuer=CN=site\n" + good,
	} {
		if err := checkCertificates(bundle, siteNow); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAdmissionLeavesTheSiteCAsVariableToTheDefaulterAndHoldsTheWindowToAKnownZone(t *testing.T) {
	t.Parallel()
	old := storedCluster("team-a", "web", "ceph", variableOf("controlPlaneEndpoint", `"192.0.2.10"`))
	h, _ := hooksFor(t, old)
	validator := &ClusterValidator{Hooks: h}
	withCAs := changedVariable(old, SiteCAsVariable, `["x"]`)
	// The defaulter has put the stored value back by the time a user's change is validated, so
	// refusing it here would only refuse what ClusterDefaulter lets through.
	if _, err := validator.ValidateUpdate(t.Context(), old, withCAs); err != nil {
		t.Errorf("a change carrying %s: %v", SiteCAsVariable, err)
	}
	var req admission.Request
	req.UserInfo = authenticationv1.UserInfo{Username: "system:serviceaccount:" + h.ClassNamespace + ":" + controllerAccount}
	if _, err := validator.ValidateUpdate(admission.NewContextWithRequest(t.Context(), req), old, withCAs); err != nil {
		t.Errorf("the controller setting %s: %v", SiteCAsVariable, err)
	}
	window := changedVariable(old, upgradesVariable, `{"mode":"patch","window":{"timeZone":"Mars/Olympus"}}`)
	if _, err := validator.ValidateUpdate(t.Context(), old, window); err == nil || !strings.Contains(err.Error(), "timeZone") {
		t.Errorf("a window in Mars/Olympus: %v", err)
	}
}

func TestSiteRollConditionGoesOnceTheClusterHasNothingToSay(t *testing.T) {
	t.Parallel()
	stale := metav1.Condition{Type: SiteRollCondition, Status: metav1.ConditionFalse, Reason: UpgradeWindowInvalidReason}
	raw, err := json.Marshal([]string{siteCAPEM})
	if err != nil {
		t.Fatal(err)
	}
	applied := siteCluster("applied", jsonVar(SiteCAsVariable, string(raw)))
	waiting := siteCluster("waiting")
	for _, c := range []*clusterv1.Cluster{&applied, &waiting} {
		c.Status.Conditions = append(c.Status.Conditions, stale)
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&applied, &waiting).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	r := &Reconciler{Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, SiteCA: siteCAPEM, Now: func() time.Time { return siteNow }}
	for _, name := range []string{"applied", "waiting"} {
		var live clusterv1.Cluster
		if err := kube.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: name}, &live); err != nil {
			t.Fatal(err)
		}
		if _, err := r.rollSiteCA(t.Context(), &live); err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: name}, &live); err != nil {
			t.Fatal(err)
		}
		if meta.FindStatusCondition(live.Status.Conditions, SiteRollCondition) != nil {
			t.Errorf("%s keeps the condition of a window that was fixed", name)
		}
	}
}
