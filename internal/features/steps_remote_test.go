package features

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	gcrremote "github.com/google/go-containerregistry/pkg/v1/remote"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/cli"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/run"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"
	"github.com/vamos-build/vamos/internal/worker"
	"go.podman.io/storage"
)

// Steps about building on a coordinator, which --coordinator names: one in this process, at <coordinator>.

// GivenACoordinator starts a coordinator, at <coordinator>, that builds with a local executor on stores of its
// own, as `vamos coordinator --local` does, until the scenario ends: so that what a command sends, it reads from
// the coordinator.
func (w *world) GivenACoordinator() {
	w.t.Helper()
	w.coordinatorStores = testenv.PrivateStores(w.t).Options
	w.startLocalCoordinator()
}

// GivenTheCoordinatorRestartsWithoutItsState stops the coordinator of GivenACoordinator and starts another on its
// stores, at another address, which <coordinator> then names, remembering none of the first's jobs, as a coordinator
// restarted without its state does.
func (w *world) GivenTheCoordinatorRestartsWithoutItsState() {
	w.t.Helper()
	if w.coordinatorStores.GraphRoot == "" {
		w.t.Fatal("no GivenACoordinator step started a coordinator to restart")
	}
	w.coordinatorServer.Close()
	if err := w.coordinatorLocal.Close(); err != nil {
		w.t.Fatal(err)
	}
	w.coordinator = ""
	w.startLocalCoordinator()
}

// startLocalCoordinator starts a coordinator that runs builds in this process, on the coordinator's stores.
func (w *world) startLocalCoordinator() {
	w.t.Helper()
	local, err := run.NewLocal(w.coordinatorStores)
	if err != nil {
		w.t.Fatal(err)
	}
	w.coordinatorLocal = local
	w.t.Cleanup(func() { local.Close() })
	content, err := local.ContentStore()
	if err != nil {
		w.t.Fatal(err)
	}
	w.startCoordinator((&remote.Server{Executor: local, Content: content}).Handler())
}

// GivenACoordinatorThatRunsOnlyGraphVersions starts a coordinator, at <coordinator>, that says it runs only graphs
// of versions, and answers nothing else, not finding it.
func (w *world) GivenACoordinatorThatRunsOnlyGraphVersions(versions ...int) {
	w.t.Helper()
	w.startCoordinatorThatSays(map[string]any{"graph_versions": versions})
}

// startCoordinatorThatSays starts a coordinator, at <coordinator>, whose capabilities are capabilities, as JSON
// encodes them, and that answers nothing else, not finding it.
func (w *world) startCoordinatorThatSays(capabilities any) {
	w.t.Helper()
	answer, err := json.Marshal(capabilities)
	if err != nil {
		w.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		if _, err := rw.Write(answer); err != nil {
			w.t.Errorf("the coordinator answering its capabilities: %v", err)
		}
	})
	w.startCoordinator(mux)
}

// GivenACoordinatorThatOffers starts a coordinator, at <coordinator>, that says it runs commands on offered, and
// that clients run them on preferred, unless it is "", and answers nothing else, not finding it.
func (w *world) GivenACoordinatorThatOffers(offered []string, preferred string) {
	w.t.Helper()
	caps := api.Capabilities{GraphVersions: []int{graph.Version}}
	for _, p := range offered {
		caps.Platforms = append(caps.Platforms, w.platform(p))
	}
	if preferred != "" {
		p := w.platform(preferred)
		caps.ExecPlatform = &p
	}
	w.startCoordinatorThatSays(caps)
}

// GivenACoordinatorOfAnotherGraphVersionThatOffers starts a coordinator, at <coordinator>, that says it runs commands
// on offered, preferring the first, but runs only graphs of another version than this vamos's, as one of another
// release does, and answers nothing else, not finding it.
func (w *world) GivenACoordinatorOfAnotherGraphVersionThatOffers(offered ...string) {
	w.t.Helper()
	caps := api.Capabilities{GraphVersions: []int{graph.Version + 1000}}
	for _, p := range offered {
		caps.Platforms = append(caps.Platforms, w.platform(p))
	}
	p := w.platform(offered[0])
	caps.ExecPlatform = &p
	w.startCoordinatorThatSays(caps)
}

// platform returns the platform that text names, stopping the scenario if it names none.
func (w *world) platform(text string) imgspecv1.Platform {
	w.t.Helper()
	p, err := graph.ParsePlatform(text)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

// ThenTheGraphIsConfiguredFor checks that the graph that WhenIShowTheGraph showed is configured for run() steps to
// run on platform.
func (w *world) ThenTheGraphIsConfiguredFor(platform string) {
	w.t.Helper()
	if w.graph == nil {
		w.t.Fatal(w.noGraph())
	}
	if w.graph.ExecPlatform != platform {
		w.t.Errorf("the graph is configured for %s, want %s", w.graph.ExecPlatform, platform)
	}
}

// startCoordinator serves handler, at <coordinator>, until the scenario ends, counting the requests it is sent and
// the jobs it is given, and noting what each job tells it.
func (w *world) startCoordinator(handler http.Handler) {
	w.t.Helper()
	w.hasNoCoordinator()
	w.coordinatorServer = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.served.Add(1)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/jobs" { // remote's pathJobs
			w.jobs.Add(1)
			if err := w.noteJob(r); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
			// The job's ID, which only the coordinator's answer says, for steps that read a job's trace by it.
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			var accepted struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &accepted) == nil && accepted.ID != "" {
				w.jobsMu.Lock()
				w.jobIDs = append(w.jobIDs, accepted.ID)
				w.jobsMu.Unlock()
			}
			maps.Copy(rw.Header(), rec.Header())
			rw.WriteHeader(rec.Code)
			_, _ = rw.Write(rec.Body.Bytes())
			return
		}
		handler.ServeHTTP(rw, r)
	}))
	w.t.Cleanup(w.coordinatorServer.Close)
	w.coordinator = w.coordinatorServer.URL
}

// toldJob is what a job that the scenario's coordinator was given told it: its kind, "graph", "push", "export" or
// "publish", and the name of the workspace it came from, "" for none.
type toldJob struct {
	kind, workspace string
}

// noteJob notes what the job that r, a POST of /v1/jobs, gives the scenario's coordinator tells it,
// leaving r's body to be read again, unless the body can't be read, which it returns, for the coordinator to refuse.
func (w *world) noteJob(r *http.Request) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("reading the job: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req api.Request
	err = json.Unmarshal(body, &req)
	told := toldJob{kind: "a job that isn't JSON: " + fmt.Sprint(err)}
	switch {
	case err != nil:
	case req.Graph != nil:
		told = toldJob{"graph", req.Graph.Workspace}
	case req.Push != nil:
		told = toldJob{"push", req.Push.Workspace}
	case req.Export != nil:
		told = toldJob{"export", req.Export.Workspace}
	case req.Publish != nil:
		told = toldJob{"publish", req.Publish.Workspace}
	default:
		told.kind = "a job of no kind"
	}
	w.toldMu.Lock()
	defer w.toldMu.Unlock()
	w.told = append(w.told, told)
	return nil
}

// toldJobs returns what every job the scenario's coordinator has been given told it, in order.
func (w *world) toldJobs() []toldJob {
	w.toldMu.Lock()
	defer w.toldMu.Unlock()
	return slices.Clone(w.told)
}

// ThenTheCoordinatorWasToldTheWorkspaceIs checks that the jobs that the last command gave the scenario's coordinator
// were exactly those of kinds, in order, "graph", "push", "export" or "publish", and that each told it the workspace
// is named name, or, if name is "", that it has no name.
func (w *world) ThenTheCoordinatorWasToldTheWorkspaceIs(name string, kinds ...string) {
	w.t.Helper()
	if len(kinds) == 0 {
		w.t.Fatal("ThenTheCoordinatorWasToldTheWorkspaceIs needs the kinds of job to expect")
	}
	var want []toldJob
	for _, kind := range kinds {
		want = append(want, toldJob{kind, name})
	}
	if !slices.Equal(w.toldDuring, want) {
		w.t.Errorf("the coordinator was given jobs %+v, want %+v", w.toldDuring, want)
	}
}

// ThenTheCoordinatorWasGivenTheBuild checks that the last command gave the scenario's coordinator a job: that it
// built there, and not in this process.
func (w *world) ThenTheCoordinatorWasGivenTheBuild() {
	w.t.Helper()
	if w.jobsDuring == 0 {
		w.t.Error("the coordinator was given no job, so the command built elsewhere")
	}
}

// WhenIStartACoordinatorUntilItListens runs `vamos coordinator` with args, listening on a port of its choosing, with
// a cache folder of the scenario's own, and interrupts it once it says where it listens, or after ten seconds if it
// never does: so that one that starts ends cleanly, and one given a mistake fails before it listens.
func (w *world) WhenIStartACoordinatorUntilItListens(args ...string) {
	w.t.Helper()
	w.sendsNothing("WhenIStartACoordinatorUntilItListens")
	interruptedOnceItPrints("Coordinating builds at ").apply(w, &commandLine{})
	w.interruptBy = 10 * time.Second
	w.run(append([]string{"coordinator", "--listen", "127.0.0.1:0", "--cache-dir", w.t.TempDir()}, args...)...)
}

// GivenTheCoordinatorHasBuilt builds target on the scenario's coordinator, with --coordinator, failing the scenario
// unless the build succeeds.
func (w *world) GivenTheCoordinatorHasBuilt(target string) {
	w.t.Helper()
	w.WhenIBuild(target, onTheCoordinator("<coordinator>"))
	if w.err != nil {
		w.t.Fatalf("building %s on the coordinator: %v\n%s", target, w.err, w.outputs())
	}
	if w.jobsDuring == 0 {
		w.t.Fatalf("building %s with --coordinator gave the coordinator no job", target)
	}
}

// ThenTheCoordinatorWasSentNothing checks that the last command sent the scenario's coordinator no request.
func (w *world) ThenTheCoordinatorWasSentNothing() {
	w.t.Helper()
	if w.servedDuring != 0 {
		w.t.Errorf("the coordinator was sent %d requests, want none", w.servedDuring)
	}
}

// hasNoCoordinator stops the scenario if a Given step has started a coordinator already.
func (w *world) hasNoCoordinator() {
	w.t.Helper()
	if w.coordinator != "" {
		w.t.Fatal("the scenario has a coordinator already")
	}
}

// front makes <coordinator> a front for the coordinator that served started, once it says where it listens, so that
// steps count what is sent to it, as for every coordinator, and returns the URL it listens at itself.
// It tells watching, if it has room, once it has sent the coordinator a command's watch of a job: not as the watch
// is answered, since a job may send no event, and so no answer, until a keep-alive many seconds later.
func (w *world) front(served *server) string {
	w.t.Helper()
	address := "http://" + served.waitFor(w.t, "Coordinating builds at ")
	backend, err := url.Parse(address)
	if err != nil {
		w.t.Fatal(err)
	}
	w.watching = make(chan struct{}, 1)
	w.timeline = &jobTimeline{jobs: map[string]*jobTimes{}}
	proxy := &httputil.ReverseProxy{FlushInterval: -1} // so that events stream
	proxy.ModifyResponse = func(r *http.Response) error {
		// remote's pathEvents, /v1/jobs/{id}/events, whose IDs are hex, which no escaping changes
		if job, ok := strings.CutPrefix(r.Request.URL.Path, "/v1/jobs/"); ok && strings.HasSuffix(job, "/events") {
			job = strings.TrimSuffix(job, "/events")
			r.Body = &eventWatch{ReadCloser: r.Body, job: job, timeline: w.timeline}
		}
		return nil
	}
	proxy.Rewrite = func(pr *httputil.ProxyRequest) {
		pr.SetURL(backend)
		if r := pr.Out; r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events") {
			pr.Out = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
				WroteRequest: func(info httptrace.WroteRequestInfo) {
					if info.Err != nil { // the coordinator has no watch, and the command fails to reach it
						return
					}
					select {
					case w.watching <- struct{}{}:
					default:
					}
				},
			}))
		}
	}
	w.startCoordinator(proxy)
	return address
}

// atCoordinator returns s with each <coordinator> replaced by the URL of the scenario's coordinator.
func (w *world) atCoordinator(s string) string {
	w.t.Helper()
	if strings.Contains(s, "<coordinator>") && w.coordinator == "" {
		w.t.Fatalf("%q names <coordinator>, but no Given step started one", s)
	}
	return strings.ReplaceAll(s, "<coordinator>", w.coordinator)
}

// onTheCoordinator builds on the coordinator at url, with --coordinator, in which <coordinator> stands for the
// scenario's. Every command takes it, and graph and update, which ask the coordinator which platform to configure for,
// and trace and prune, which read and prune its build cache.
func onTheCoordinator(url string) option {
	return option{"onTheCoordinator", everyCommandGraphUpdateTraceAndPrune, func(w *world, c *commandLine) {
		w.t.Helper()
		c.flags = append(c.flags, "--coordinator", w.atCoordinator(url))
	}}
}

// GivenACoordinatorWithAWorker starts `vamos coordinator`, with args, and `vamos worker`, in this process, until the
// scenario ends: the coordinator, at <coordinator>, keeps the layers that its workers build in <registry>/vamos-cache,
// args being placeholders as atPlaceholders says, and the
// worker, which builds in stores of its own, registers with it. It waits until the worker has registered.
func (w *world) GivenACoordinatorWithAWorker(args ...string) {
	w.t.Helper()
	w.hasNoCoordinator()
	w.t.Setenv(worker.ChildEnv, "1") // the worker runs this test binary as its attempts' process
	workerStores := testenv.PrivateStores(w.t)
	ctx, cancel := context.WithCancel(context.Background())
	var coordinator, registered *server
	w.t.Cleanup(func() { // registered first, so that it runs however the step ends
		cancel()
		for _, s := range []*server{registered, coordinator} {
			if s != nil {
				_ = s.wait(w.t, time.Minute) // how it ended is for a step to check, if a scenario asks
			}
		}
	})
	coordinator = w.serve(ctx, &w.stores.Options, append([]string{"coordinator", "--listen", "127.0.0.1:0",
		"--cache-dir", w.t.TempDir(), "--cache-repository", w.atRegistry("<registry>/vamos-cache"), "--lease", "2s"},
		w.allAtPlaceholders(args)...)...)
	address := w.front(coordinator)
	// The worker reaches the coordinator itself, so that what it sends isn't counted as a command's.
	registered = w.serve(ctx, &workerStores.Options, "worker", "--coordinator", address, "--capacity", "2")
	// As the worker says, rather than as the coordinator's platforms do, which --offer gives before any registers.
	registered.waitFor(w.t, "worker: registered as ", coordinator)
}

// GivenAnotherClientBuildsOnTheCoordinator starts building target on the scenario's coordinator, which options must
// name, as another client would, until the scenario ends, when it is interrupted, and waits until the coordinator has
// accepted its job and the client watches it. What it prints is its own, which no step checks, but which the scenario
// logs once it ends. It takes no option that traces or interrupts, which are for the commands that When steps run.
// Its watch of its job tells the scenario's watch signal, as any command's does, so a later step that waits on that
// signal drains it first, as WhenIInterruptTheCoordinatorWhileIBuild does.
func (w *world) GivenAnotherClientBuildsOnTheCoordinator(target string, options ...option) {
	w.t.Helper()
	w.jobsMu.Lock()
	before := len(w.jobIDs)
	w.jobsMu.Unlock()
	w.otherBuild = w.startAnotherClient(w.project(), target, options)
	// A client watches a job only once the coordinator has accepted it.
	select {
	case <-w.watching:
	case <-w.otherBuild.done:
		w.t.Fatalf("the other client's build ended before it watched its job, with %v, printing:\n%s",
			w.otherBuild.err, w.otherBuild.output.String())
	case <-time.After(20 * time.Second):
		w.t.Fatal("the other client didn't watch a job on the coordinator")
	}
	w.jobsMu.Lock()
	defer w.jobsMu.Unlock()
	if len(w.jobIDs) != before+1 {
		w.t.Fatalf("the coordinator accepted %d jobs of the other client, want 1", len(w.jobIDs)-before)
	}
	w.otherJob = w.jobIDs[before]
}

// GivenTwoClientsBuiltAtOnceOnTheCoordinator builds target on the scenario's coordinator, which options must name,
// from two clients at once, the first with the project's files as they are and also those of first, the second with
// them and those of second, and waits until both have built it, failing the scenario if either fails.
func (w *world) GivenTwoClientsBuiltAtOnceOnTheCoordinator(target string, first, second files, options ...option) {
	w.t.Helper()
	deadline := time.After(5 * time.Minute) // for the whole step, so that a client that stalls fails it
	w.jobsMu.Lock()
	before := len(w.jobIDs)
	w.jobsMu.Unlock()
	var clients []*otherClient
	for _, overlay := range []files{first, second} {
		kept, root := maps.Clone(w.files), w.root
		maps.Copy(w.files, overlay)
		fsys := w.project()
		w.files, w.root = kept, root
		clients = append(clients, w.startAnotherClient(fsys, target, options))
	}
	for i, c := range clients {
		if err := c.wait(deadline); err != nil {
			w.t.Fatalf("client %d's build %v, printing:\n%s", i+1, err, c.output.String())
		}
	}
	// At once: each job's target was building, or queued to, before the other job was done, as their events said
	// when the coordinator's front read them. That the two history writes ran at the same moment would take a barrier
	// within both attempts, which the tasks of persistent-traces leave to a separate PR.
	w.jobsMu.Lock()
	jobs := slices.Clone(w.jobIDs[before:])
	w.jobsMu.Unlock()
	if len(jobs) != 2 {
		w.t.Fatalf("the coordinator accepted %d jobs of the two clients, want 2", len(jobs))
	}
	timeline := w.watchedJobs()
	one, two := timeline.times(jobs[0]), timeline.times(jobs[1])
	for i, j := range []jobTimes{one, two} {
		if j.building.IsZero() || j.done.IsZero() {
			w.t.Fatalf("job %s's events didn't say it built a target and was done", jobs[i])
		}
	}
	if !one.building.Before(two.done) || !two.building.Before(one.done) {
		w.t.Fatalf("the two jobs' targets didn't build at once: %v to %v, and %v to %v", one.building, one.done,
			two.building, two.done)
	}
}

// wait waits until the client's build has ended, or deadline passes, and returns why it didn't succeed, if it didn't.
func (c *otherClient) wait(deadline <-chan time.Time) error {
	select {
	case <-c.done:
		if c.err != nil {
			return fmt.Errorf("failed with %w", c.err)
		}
		return nil
	case <-deadline:
		return errors.New("didn't end in time")
	}
}

// otherClient is a client that a Given step started, apart from the scenario's commands.
type otherClient struct {
	cancel context.CancelFunc
	done   chan struct{} // closed once its command has ended, with err
	err    error
	output *syncBuffer
}

// startAnotherClient starts building target from the project fsys, as another client would, until the scenario
// ends, when it is interrupted. What it prints is its own, which only the other client's steps check, but which the
// scenario logs once it ends. It takes no option that traces or interrupts, which are for the commands that When
// steps run. Its watch of its job tells the scenario's watch signal, as any command's does, so a later step that
// waits on that signal drains it first, as WhenIInterruptTheCoordinatorWhileIBuild does.
func (w *world) startAnotherClient(fsys afero.Fs, target string, options []option) *otherClient {
	w.t.Helper()
	if w.coordinator == "" {
		w.t.Fatal("no Given step started a coordinator for another client to build on")
	}
	c := w.apply("build", options)
	if c.traceBy != "" {
		w.t.Fatalf("another client's build doesn't take %s()", c.traceBy)
	}
	if w.interruptOn != "" || w.interruptIn != 0 || w.interruptBy != 0 {
		w.t.Fatal("another client's build isn't interrupted by an option; the scenario's end interrupts it")
	}
	args := w.commandLine(fsys, append([]string{"build", target}, c.flags...))
	select {
	case <-w.watching: // told of a watch by an earlier command, not this build's
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	other := &otherClient{cancel: cancel, done: make(chan struct{}), output: &syncBuffer{}}
	go func() {
		defer close(other.done)
		cmd := cli.NewRootCommand(w.commandConfig(fsys))
		cmd.SetOut(other.output)
		cmd.SetErr(other.output)
		cmd.SetArgs(args)
		other.err = cmd.ExecuteContext(ctx)
	}()
	w.t.Cleanup(func() {
		cancel()
		select {
		case <-other.done:
			w.t.Logf("the other client's build ended with %v, printing:\n%s", other.err, other.output.String())
		case <-time.After(time.Minute):
			w.t.Error("the other client's build didn't end within a minute of being interrupted")
		}
	})
	return other
}

// GivenTheOtherClientIsInterruptedOnceJoined interrupts the build of GivenAnotherClientBuildsOnTheCoordinator, as
// Ctrl-C would, once another job on the coordinator is told that it shares another build's attempt, as the scenario's
// next command's will be, so that the other client leaves an attempt that a build still waits on.
func (w *world) GivenTheOtherClientIsInterruptedOnceJoined() {
	w.t.Helper()
	other, job, timeline := w.otherClient(), w.otherJob, w.watchedJobs()
	ended := make(chan struct{})
	w.t.Cleanup(func() { close(ended) })
	go func() {
		for {
			if timeline.joinedBy(job) {
				other.cancel()
				return
			}
			select {
			case <-other.done:
				return
			case <-ended:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
}

// GivenTheOtherClientIsBuildingIt waits until the job of GivenAnotherClientBuildsOnTheCoordinator builds a target,
// rather than having been merely accepted, so that the scenario's next build of it joins the attempt the other
// started, rather than starting one the other joins.
func (w *world) GivenTheOtherClientIsBuildingIt() {
	w.t.Helper()
	other, job, timeline := w.otherClient(), w.otherJob, w.watchedJobs()
	deadline := time.After(time.Minute)
	for timeline.times(job).building.IsZero() {
		select {
		case <-other.done:
			w.t.Fatalf("the other client's build ended, with %v, before building, printing:\n%s", other.err,
				other.output.String())
		case <-deadline:
			w.t.Fatal("the other client's build didn't start building within a minute")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// watchedJobs returns the timeline of the jobs whose events the scenario's coordinator streams, failing the scenario
// if its coordinator isn't one whose events are watched.
func (w *world) watchedJobs() *jobTimeline {
	w.t.Helper()
	if w.timeline == nil {
		w.t.Fatal("the scenario's coordinator streams events that no step watches")
	}
	return w.timeline
}

// jobTimeline is what the events of a coordinator's jobs said, and when, by job ID.
type jobTimeline struct {
	mu   sync.Mutex
	jobs map[string]*jobTimes
}

// jobTimes are when a job's events first said a target builds, and that the job was done, and whether they said a
// target shares another build's attempt.
type jobTimes struct {
	building, done time.Time
	joined         bool
}

// times returns what job's events said so far.
func (t *jobTimeline) times(job string) jobTimes {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j := t.jobs[job]; j != nil {
		return *j
	}
	return jobTimes{}
}

// joinedBy reports whether a job other than job was told that a target of it shares another build's attempt.
func (t *jobTimeline) joinedBy(job string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, j := range t.jobs {
		if id != job && j.joined {
			return true
		}
	}
	return false
}

// eventWatch passes a job's events through, noting in timeline when they say what jobTimes holds.
type eventWatch struct {
	io.ReadCloser
	job      string
	timeline *jobTimeline
	partial  []byte // the start of a line that a read ended within
}

func (e *eventWatch) Read(p []byte) (int, error) {
	n, err := e.ReadCloser.Read(p)
	e.partial = append(e.partial, p[:n]...)
	for {
		i := bytes.IndexByte(e.partial, '\n')
		if i < 0 {
			break
		}
		var line struct {
			Event *api.Event `json:"event"`
		}
		if json.Unmarshal(e.partial[:i], &line) == nil && line.Event != nil {
			e.note(*line.Event)
		}
		e.partial = e.partial[i+1:]
	}
	return n, err
}

// note notes what ev says of the job, as it arrives.
func (e *eventWatch) note(ev api.Event) {
	e.timeline.mu.Lock()
	defer e.timeline.mu.Unlock()
	j := e.timeline.jobs[e.job]
	if j == nil {
		j = &jobTimes{}
		e.timeline.jobs[e.job] = j
	}
	switch {
	case ev.Kind == api.EventStatus && (strings.HasPrefix(ev.Status, "building") ||
		strings.HasPrefix(ev.Status, "rebuilding")):
		if j.building.IsZero() {
			j.building = time.Now()
		}
		if strings.HasSuffix(ev.Status, "shared with another build") { // not with a target of its own job
			j.joined = true
		}
	case ev.Kind == api.EventDone && j.done.IsZero():
		j.done = time.Now()
	}
}

// ThenTheOtherClientIsInterrupted waits until the build of GivenAnotherClientBuildsOnTheCoordinator has ended, and
// checks that an interrupt ended it.
func (w *world) ThenTheOtherClientIsInterrupted() {
	w.t.Helper()
	w.otherClientEnded()
	if !errors.Is(w.otherBuild.err, context.Canceled) {
		w.t.Fatalf("the other client's build ended with %v, not interrupted, printing:\n%s", w.otherBuild.err,
			w.otherBuild.output.String())
	}
}

// ThenTheOtherClientSucceeds waits until the build of GivenAnotherClientBuildsOnTheCoordinator has ended, and checks
// that it succeeded.
func (w *world) ThenTheOtherClientSucceeds() {
	w.t.Helper()
	w.otherClientEnded()
	if w.otherBuild.err != nil {
		w.t.Fatalf("the other client's build failed with %v, printing:\n%s", w.otherBuild.err,
			w.otherBuild.output.String())
	}
}

// otherClient returns the client of GivenAnotherClientBuildsOnTheCoordinator.
func (w *world) otherClient() *otherClient {
	w.t.Helper()
	if w.otherBuild == nil {
		w.t.Fatal("no GivenAnotherClientBuildsOnTheCoordinator step started another client")
	}
	return w.otherBuild
}

// otherClientEnded waits until the other client's build has ended, within five minutes.
func (w *world) otherClientEnded() {
	w.t.Helper()
	select {
	case <-w.otherClient().done:
	case <-time.After(5 * time.Minute):
		w.t.Fatal("the other client's build didn't end within five minutes")
	}
}

// syncBuffer is a buffer that a command's output can be written to while a step reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// allAtPlaceholders returns args, each with placeholders replaced as atPlaceholders says.
func (w *world) allAtPlaceholders(args []string) []string {
	w.t.Helper()
	expanded := make([]string, len(args))
	for i, arg := range args {
		expanded[i] = w.atPlaceholders(arg)
	}
	return expanded
}

// ThenTheCacheRepositoryHoldsALayer checks that <registry>/vamos-cache, the coordinator's cache repository, has a
// tag: an image that a worker pushed there.
func (w *world) ThenTheCacheRepositoryHoldsALayer() {
	w.t.Helper()
	repo, err := name.NewRepository(w.atRegistry("<registry>/vamos-cache"), name.Insecure)
	if err != nil {
		w.t.Fatal(err)
	}
	tags, err := gcrremote.List(repo, w.readingTheRegistry()...)
	if err != nil || len(tags) == 0 {
		w.t.Errorf("the cache repository has tags %q, %v; want a layer that the worker pushed", tags, err)
	}
}

// server is a vamos command that serves, run by serve, until its context ends.
type server struct {
	name  string // the command, such as "vamos worker", for a failure's message
	mu    sync.Mutex
	out   bytes.Buffer
	ended chan struct{} // closed once it has ended, with err
	err   error
}

func (s *server) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.Write(p)
}

// wait waits, for at most d, for the server to end, and returns its error, failing the test if it doesn't end.
func (s *server) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case <-s.ended:
		return s.err
	case <-time.After(d):
		t.Fatalf("%s didn't end within %s\n%s", s.name, d, s.output())
		return nil
	}
}

// output returns what the server has printed so far.
func (s *server) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

// waitFor waits until the server prints a line that starts with prefix, and returns the rest of the line,
// failing the test if the server, or any of others that it needs, ends first, or it prints no such line within
// twenty seconds.
func (s *server) waitFor(t *testing.T, prefix string, others ...*server) string {
	t.Helper()
	needed := append([]*server{s}, others...)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		late := time.Now().After(deadline)
		ended := slices.IndexFunc(needed, func(n *server) bool {
			select {
			case <-n.ended:
				return true
			default:
				return false
			}
		})
		// Read after seeing an end, or the deadline, so that a line printed just before it counts.
		if rest, ok := s.printed(prefix); ok {
			return rest
		}
		if ended >= 0 {
			t.Fatalf("%s ended with %v before %s printed %q\n%s", needed[ended].name, needed[ended].err, s.name,
				prefix, outputsOf(needed))
		}
		if late {
			t.Fatalf("%s printed no %q\n%s", s.name, prefix, outputsOf(needed))
		}
	}
}

// printed returns the rest of the first whole line that the server has printed that starts with prefix, and whether
// it has printed one.
func (s *server) printed(prefix string) (string, bool) {
	lines := strings.Split(s.output(), "\n")
	for _, line := range lines[:len(lines)-1] { // whole lines only, as one may be written in parts
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// outputsOf shows what each of servers has printed, for a failure's message.
func outputsOf(servers []*server) string {
	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "%s printed:\n%s\n", s.name, s.output())
	}
	return b.String()
}

// serve runs the vamos command line args, which reads no project, on the stores that store gives, until ctx ends,
// stopping the scenario if args is empty.
func (w *world) serve(ctx context.Context, store *storage.StoreOptions, args ...string) *server {
	w.t.Helper()
	if len(args) == 0 {
		w.t.Fatal("serve runs a command that serves, but was given no command line")
	}
	s := &server{name: "vamos " + args[0], ended: make(chan struct{})}
	cmd := cli.NewRootCommand(cli.Config{Fs: afero.NewMemMapFs(), Store: store}) // which reads no project
	cmd.SetOut(s)
	cmd.SetErr(s)
	cmd.SetArgs(args)
	go func() {
		s.err = cmd.ExecuteContext(ctx)
		close(s.ended)
	}()
	return s
}

// GivenARegistryThatNeverAnswers starts a registry, at <registry>, that holds every request it is sent until the
// one who sent it gives up, or the scenario ends.
func (w *world) GivenARegistryThatNeverAnswers() {
	w.t.Helper()
	ended := make(chan struct{})
	w.startRegistry(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-ended:
		case <-r.Context().Done():
		}
	}), true)
	w.t.Cleanup(func() { close(ended) }) // after startRegistry's, so first, letting the server close
}

// GivenALocalCoordinator starts `vamos coordinator --local`, in this process, behind <coordinator>, which builds on
// the scenario's stores until WhenIInterruptTheCoordinatorWhileIBuild interrupts it, or the scenario ends.
func (w *world) GivenALocalCoordinator() {
	w.t.Helper()
	w.hasNoCoordinator()
	ctx, cancel := context.WithCancel(context.Background())
	w.t.Cleanup(func() {
		cancel()
		if w.local != nil {
			_ = w.local.wait(w.t, time.Minute) // which ThenTheCoordinatorEndsCleanlyWithin checks, if a scenario asks
		}
	})
	w.local = w.serve(ctx, &w.stores.Options, "coordinator", "--local", "--listen", "127.0.0.1:0")
	w.interruptLocal = cancel
	w.front(w.local)
}

// WhenIInterruptTheCoordinatorWhileIBuild builds target on the coordinator of GivenALocalCoordinator and, once the
// build watches its job there, interrupts the coordinator, as Ctrl-C does, and once it has ended, closes
// <coordinator>, so that whatever the build sends from then finds no coordinator, as when one has ended.
// The build is the last command, for Then steps to check. It stops the scenario if the build ended before the
// interrupt, or if the build or the coordinator takes a minute where the step waits for it.
func (w *world) WhenIInterruptTheCoordinatorWhileIBuild(target string) {
	w.t.Helper()
	if w.local == nil {
		w.t.Fatal("no Given step started a local coordinator")
	}
	// Alongside the build, which runs as any command does, and which this touches nothing of until it has ended.
	select {
	case <-w.watching: // told of a watch by an earlier command, not this build's
	default:
	}
	var failures []string
	built, interrupted := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(interrupted)
		select {
		case <-w.watching:
		case <-built: // which the interrupt then follows, as the check below sees
		case <-time.After(time.Minute):
			failures = append(failures, "the build didn't watch its job within a minute")
		}
		w.interruptedAt = time.Now()
		w.interruptLocal()
		select {
		case <-w.local.ended:
			w.localErr, w.localTook = w.local.err, time.Since(w.interruptedAt)
		case <-time.After(time.Minute):
			failures = append(failures, "the coordinator didn't end within a minute of its interrupt")
		}
		w.coordinatorServer.Close()
	}()
	func() {
		returned := false
		defer func() { // even if the build stops the scenario, so that nothing is left changing it
			close(built)
			<-interrupted
			if !returned && len(failures) > 0 { // what the build's failure would otherwise hide
				w.t.Errorf("%s\n%s\n%s", strings.Join(failures, "; "), w.outputs(), w.local.output())
			}
		}()
		w.WhenIBuild(target, onTheCoordinator("<coordinator>"))
		returned = true
	}()
	w.buildTook = w.endedAt.Sub(w.interruptedAt) // not counting the wait for the coordinator to end
	if w.buildTook < 0 {
		failures = append(failures, "the build ended before its coordinator was interrupted")
	}
	if len(failures) > 0 {
		w.t.Fatalf("%s\n%s\n%s", strings.Join(failures, "; "), w.outputs(), w.local.output())
	}
}

// ThenTheBuildEndsWithin checks that the build of WhenIInterruptTheCoordinatorWhileIBuild ended within d of the
// coordinator's interrupt.
func (w *world) ThenTheBuildEndsWithin(d time.Duration) {
	w.t.Helper()
	if w.buildTook > d {
		w.t.Errorf("the build ended %s after its coordinator was interrupted, want within %s", w.buildTook, d)
	}
}

// ThenTheCoordinatorEndsCleanlyWithin checks that the coordinator that WhenIInterruptTheCoordinatorWhileIBuild
// interrupted ended without an error, within d of the interrupt.
func (w *world) ThenTheCoordinatorEndsCleanlyWithin(d time.Duration) {
	w.t.Helper()
	if w.localErr != nil {
		w.t.Errorf("the coordinator, interrupted, ended with %v, want a clean end\n%s", w.localErr, w.local.output())
	}
	if w.localTook > d {
		w.t.Errorf("the coordinator took %s to end once interrupted, want within %s", w.localTook, d)
	}
}

// GivenACoordinatorThatRefusesToStoreCommandTraces starts a coordinator, at <coordinator>, as GivenACoordinator does,
// whose content store refuses to store a command's trace, as a store that fills up once a build has ended would.
func (w *world) GivenACoordinatorThatRefusesToStoreCommandTraces() {
	w.t.Helper()
	local, err := run.NewLocal(testenv.PrivateStores(w.t).Options)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { local.Close() })
	content, err := local.ContentStore()
	if err != nil {
		w.t.Fatal(err)
	}
	w.startCoordinator((&remote.Server{Executor: local, Content: refusingCommandTraces{content}}).Handler())
}

// refusingCommandTraces is a content store that refuses to store a command's trace.
type refusingCommandTraces struct{ cache.ContentStore }

func (r refusingCommandTraces) Put(ctx context.Context, rd io.Reader, limit int64) (cache.Blob, error) {
	data, err := io.ReadAll(io.LimitReader(rd, limit+1))
	if err != nil {
		return cache.Blob{}, err
	}
	if h, err := tracing.ReadHeader(data); err == nil && h.Kind == api.KindCommand {
		return cache.Blob{}, errors.New("the store is full")
	}
	return r.ContentStore.Put(ctx, bytes.NewReader(data), limit)
}
