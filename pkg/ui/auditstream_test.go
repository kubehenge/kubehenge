package ui

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeLogLine struct {
	at   time.Time
	text string
}

// apiServerFake serves the API server pods and their logs over HTTP, as the API server and the
// kubelet do. A log is served with timestamps from SinceTime in whole seconds, followed or cut at
// LimitBytes. Only each log's current file is served: rotate drops the lines before it, but a
// stream that follows the log goes on into the new file. The previous container's log is served
// where the kubelet still has it.
type apiServerFake struct {
	mu        sync.Mutex
	pods      map[string]*corev1.Pod
	files     map[string][]fakeLogLine
	followers map[string][]*fakeFollower
	// previous is the previous container's log, by pod, where there is one.
	previous map[string][]fakeLogLine
	// opens and previousOpens are where each read of the current and the previous log started.
	opens, previousOpens map[string][]time.Time
	srv                  *httptest.Server
}

// fakeFollower is one stream that follows a log. A value on quit ends it, a nil one as the end of
// a container that stopped and an error as a connection lost.
type fakeFollower struct {
	lines chan string
	quit  chan error
}

func newAPIServerFake(t *testing.T, pods ...string) *apiServerFake {
	t.Helper()
	api := &apiServerFake{
		pods: map[string]*corev1.Pod{}, files: map[string][]fakeLogLine{},
		followers: map[string][]*fakeFollower{}, previous: map[string][]fakeLogLine{},
		opens: map[string][]time.Time{}, previousOpens: map[string][]time.Time{},
	}
	for _, name := range pods {
		api.pods[name] = &corev1.Pod{
			Name: name, Namespace: metav1.NamespaceSystem, UID: types.UID("uid-" + name), Labels: map[string]string{"k8s-app": "kube-apiserver"},
		}
	}
	api.srv = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.srv.Close)
	return api
}

// server is an interface that reaches the fake, with the audit log switched on and a clock an hour
// past the first line.
func (api *apiServerFake) server(t *testing.T, log io.Writer, clock func() time.Time) *Server {
	t.Helper()
	on := &corev1.ConfigMap{Data: map[string]string{apiAuditLogKey: "true"}}
	on.Namespace, on.Name = "kubehenge-system", interfaceConfigMap
	s := &Server{
		Kube: fake.NewClientBuilder().WithObjects(on).Build(), ClassNamespace: "kubehenge-system",
		Log: slog.New(slog.NewTextHandler(log, nil)),
		NewClientset: func() (kubernetes.Interface, error) {
			return kubernetes.NewForConfig(&rest.Config{Host: api.srv.URL})
		},
	}
	s.init()
	s.clock = clock
	return s
}

// line is a line n seconds after auditBase: a person's ConfigMap creation when id is not empty,
// and a read of about 250 bytes otherwise.
func (*apiServerFake) line(n int, id string) fakeLogLine {
	text := auditLine("filler", "get", "admin", "ResponseComplete",
		`,"objectRef":{"resource":"pods","namespace":"x","name":"y"},"annotations":{"pad":"`+strings.Repeat("p", 60)+`"}`)
	if id != "" {
		text = auditLine(id, "create", "admin", "ResponseComplete",
			`,"objectRef":{"resource":"configmaps","namespace":"default","name":"`+id+`"},"responseStatus":{"code":201}`)
	}
	at := auditBase.Add(time.Duration(n) * time.Second)
	return fakeLogLine{at: at, text: strings.Replace(text, "2026-10-13T08:30:00.123456Z", at.Format(time.RFC3339Nano), 1)}
}

func (l fakeLogLine) String() string { return l.at.Format(time.RFC3339Nano) + " " + l.text + "\n" }

// add writes a line to a pod's log, n seconds after auditBase.
func (api *apiServerFake) add(pod string, n int, id string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.write(pod, n, id)
}

func (api *apiServerFake) write(pod string, n int, id string) {
	line := api.line(n, id)
	api.files[pod] = append(api.files[pod], line)
	for _, f := range api.followers[pod] {
		f.lines <- line.String()
	}
}

// addThenRotate writes a line and has the kubelet rotate the log file at once, before any read in
// between.
func (api *apiServerFake) addThenRotate(pod string, n int, id string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.write(pod, n, id)
	api.files[pod] = nil
}

// rotate has the kubelet rotate a pod's log file.
func (api *apiServerFake) rotate(pod string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.files[pod] = nil
}

// end ends the streams that follow a pod's log: with err as a connection lost, with nil as the end
// of a container that stopped.
func (api *apiServerFake) end(pod string, err error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, f := range api.followers[pod] {
		f.quit <- err
	}
	api.followers[pod] = nil
}

func (api *apiServerFake) unfollow(pod string, f *fakeFollower) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.followers[pod] = slices.DeleteFunc(api.followers[pod], func(g *fakeFollower) bool { return g == f })
}

func (api *apiServerFake) pod(name string, change func(*corev1.Pod)) {
	api.mu.Lock()
	defer api.mu.Unlock()
	change(api.pods[name])
}

// openedFrom is where each stream on the first pod's log started.
func (api *apiServerFake) openedFrom() []time.Time {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.Clone(api.opens[apiServerA])
}

func (api *apiServerFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/namespaces/kube-system/pods" {
		api.mu.Lock()
		list := corev1.PodList{Kind: "PodList", APIVersion: "v1"}
		for _, pod := range api.pods {
			list.Items = append(list.Items, *pod.DeepCopy())
		}
		api.mu.Unlock()
		raw, err := json.Marshal(list)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return
	}
	if path.Base(r.URL.Path) != "log" {
		http.NotFound(w, r)
		return
	}
	pod := path.Base(path.Dir(r.URL.Path))
	var since time.Time
	if s := r.URL.Query().Get("sinceTime"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	if r.URL.Query().Get("previous") == "true" {
		api.servePrevious(w, pod, since)
		return
	}
	api.mu.Lock()
	api.opens[pod] = append(api.opens[pod], since)
	backlog := linesSince(api.files[pod], since)
	var f *fakeFollower
	if r.URL.Query().Get("follow") == "true" {
		f = &fakeFollower{lines: make(chan string, 1<<12), quit: make(chan error, 1)}
		api.followers[pod] = append(api.followers[pod], f)
	}
	api.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	if f == nil {
		out := backlog
		if limit, err := strconv.Atoi(r.URL.Query().Get("limitBytes")); err == nil && limit < len(out) {
			out = out[:limit]
		}
		_, _ = io.WriteString(w, out)
		return
	}
	defer api.unfollow(pod, f)
	api.follow(w, r, f, backlog)
}

func linesSince(lines []fakeLogLine, since time.Time) string {
	var b strings.Builder
	for _, line := range lines {
		if !line.at.Before(since) {
			b.WriteString(line.String())
		}
	}
	return b.String()
}

// servePrevious serves the previous container's log, or the kubelet's refusal when it has none.
func (api *apiServerFake) servePrevious(w http.ResponseWriter, pod string, since time.Time) {
	api.mu.Lock()
	api.previousOpens[pod] = append(api.previousOpens[pod], since)
	lines, ok := api.previous[pod]
	api.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,`+
			`"message":"previous terminated container \"kube-apiserver\" not found"}`)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, linesSince(lines, since))
}

// forgetPrevious has the kubelet lose the first pod's previous log.
func (api *apiServerFake) forgetPrevious() {
	api.mu.Lock()
	defer api.mu.Unlock()
	delete(api.previous, apiServerA)
}

// restart restarts the first pod's container: its log becomes the previous one, and the one before
// it is gone.
func (api *apiServerFake) restart() {
	api.mu.Lock()
	defer api.mu.Unlock()
	const pod = apiServerA
	api.previous[pod] = api.files[pod]
	api.files[pod] = nil
	for _, f := range api.followers[pod] {
		f.quit <- nil
	}
	api.followers[pod] = nil
	p := api.pods[pod]
	restarts := int32(0)
	if len(p.Status.ContainerStatuses) > 0 {
		restarts = p.Status.ContainerStatuses[0].RestartCount
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "kube-apiserver", RestartCount: restarts + 1}}
}

// previousFrom is where each read of the first pod's previous log started.
func (api *apiServerFake) previousFrom() []time.Time {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.Clone(api.previousOpens[apiServerA])
}

func (*apiServerFake) follow(w http.ResponseWriter, r *http.Request, f *fakeFollower, backlog string) {
	flush := func(s string) bool {
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return http.NewResponseController(w).Flush() == nil
	}
	if !flush(backlog) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case line := <-f.lines:
			if !flush(line) {
				return
			}
		case err := <-f.quit:
			if err != nil {
				// A connection lost mid-way: the client reads an unexpected end of the body.
				panic(http.ErrAbortHandler)
			}
			for {
				select {
				case line := <-f.lines:
					if !flush(line) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

// waitAuditKept waits until the activity log holds exactly the changes want, sorted. It wakes on
// each entry written, so `go test -timeout` is its only bound.
func waitAuditKept(t *testing.T, s *Server, want ...string) {
	t.Helper()
	var got []string
	for {
		// Taken before the read, so an entry written after it still wakes the wait.
		s.activityLog.mu.Lock()
		changed := s.activityLog.mu.next()
		s.activityLog.mu.release()
		kept, err := s.activityLog.since(auditBase.Add(-time.Hour), 1000, func(*activityEntry) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		got = got[:0]
		for i := range kept {
			got = append(got, kept[i].What)
		}
		slices.Sort(got)
		if slices.Equal(got, want) {
			return
		}
		select {
		case <-changed:
		case <-t.Context().Done():
			t.Fatalf("kept %v, want %v", got, want)
		}
	}
}

// A change made a few seconds before the kubelet rotates the API server's log file, and after the
// last read, is kept: the interface reads each line as it is written.
func TestAChangeIsKeptThoughTheKubeletRotatesItsLogAway(t *testing.T) {
	t.Parallel()
	api := newAPIServerFake(t, "kube-apiserver-a")
	var clock atomic.Int64
	clock.Store(auditBase.Add(time.Hour).UnixNano())
	out := &syncBuffer{}
	s := api.server(t, out, func() time.Time { return time.Unix(0, clock.Load()) })
	for n := range 10 {
		api.add("kube-apiserver-a", n, "")
	}
	api.add("kube-apiserver-a", 10, "before")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.followAuditLog(ctx, 10*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitAuditKept(t, s, "created configmaps before")
	api.addThenRotate("kube-apiserver-a", 11, "kubehenge-e2e-audit")
	api.add("kube-apiserver-a", 12, "")
	waitAuditKept(t, s, "created configmaps before", "created configmaps kubehenge-e2e-audit")
	if strings.Contains(out.String(), "rotated") {
		t.Errorf("a rotation the interface followed was warned about as lines lost: %s", out)
	}
}
