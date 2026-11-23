package coord

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/sched"
	"github.com/vamos-build/vamos/internal/telemetry"
	"github.com/vamos-build/vamos/internal/tracing"
)

// metered returns cfg measuring into the reader it returns.
func metered(t *testing.T, cfg Config) (Config, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	cfg.Telemetry = func(string) (*telemetry.Telemetry, error) { return &telemetry.Telemetry{Meter: provider}, nil }
	return cfg, reader
}

// measured returns the value of the counter or gauge name, in what reader collects now, at the point whose
// attributes are exactly attrs, and whether there is one.
func measured(t *testing.T, reader *sdkmetric.ManualReader, name string, attrs ...attribute.KeyValue) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var points []metricdata.DataPoint[int64]
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				points = data.DataPoints
			case metricdata.Gauge[int64]:
				points = data.DataPoints
			}
			for _, p := range points {
				if p.Attributes.Equals(&want) {
					return p.Value, true
				}
			}
		}
	}
	return 0, false
}

// checkMeasured checks that name, at attrs, is want.
func checkMeasured(t *testing.T, reader *sdkmetric.ManualReader, want int64, name string,
	attrs ...attribute.KeyValue) {
	t.Helper()
	if got, ok := measured(t, reader, name, attrs...); !ok || got != want {
		t.Errorf("%s %v = %d (exported: %t); want %d", name, attrs, got, ok, want)
	}
}

func reason(r string) attribute.KeyValue { return attribute.String("reason", r) }

// TestMetricsFromZero checks that a coordinator that has run nothing exports its counters of what degrades quietly,
// each at zero.
func TestMetricsFromZero(t *testing.T) {
	cfg, reader := metered(t, Config{})
	newFixture(t, cfg)
	for _, name := range []string{"vamos.leases.expired", "vamos.journal.failures", "vamos.events.gaps",
		"vamos.attempts.retries"} {
		checkMeasured(t, reader, 0, name)
	}
}

// TestMetricsRefused checks that a job refused is counted by why: a graph of another version, for a platform not
// offered, while as many jobs are open as allowed, and captured against a pruned generation.
func TestMetricsRefused(t *testing.T) {
	cfg, reader := metered(t, Config{MaxBuilds: 1})
	f := newFixture(t, cfg)
	startWorker(t, f, amd64, 1, fakeRun(f, nil))

	g := testGraph(t, f, "refused")
	stale := g
	stale.Generation = 99 // the cache is at its first
	if outcome, _ := runJob(t, f.c, stale); outcome.Failure == nil {
		t.Fatal("a build of a pruned generation succeeded")
	}
	checkMeasured(t, reader, 1, "vamos.jobs.refused", reason(refusedGeneration))

	old := g
	old.Version = g.Version + 1
	if _, err := f.c.Submit(t.Context(), api.Request{Graph: &old}); err == nil {
		t.Fatal("a graph of another version was accepted")
	}
	checkMeasured(t, reader, 1, "vamos.jobs.refused", reason(refusedVersion))

	unoffered := g
	unoffered.Platform = imgspecv1.Platform{OS: "linux", Architecture: "s390x"}
	if _, err := f.c.Submit(t.Context(), api.Request{Graph: &unoffered}); err == nil {
		t.Fatal("a graph for a platform not offered was accepted")
	}
	checkMeasured(t, reader, 1, "vamos.jobs.refused", reason(refusedPlatform))

	held, err := f.c.Submit(t.Context(), api.Request{Graph: &g})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.c.Cancel(context.Background(), held) })
	if _, err := f.c.Submit(t.Context(), api.Request{Graph: &g}); !errors.Is(err, sched.ErrBusy) {
		t.Fatalf("a second build while one is open = %v; want %v", err, sched.ErrBusy)
	}
	checkMeasured(t, reader, 1, "vamos.jobs.refused", reason(refusedBusy))
}

// TestMetricsPool checks the pool's counters, where each is counted: a running attempt withdrawn, a pending one no
// build demands, and a heartbeat for an attempt that isn't live, each a kill by its reason; reports of a killed
// attempt and of one from an earlier generation, each rejected by its reason; and a lease that expired.
func TestMetricsPool(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := newMetrics(provider.Meter(meterName))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	generation := uint64(1)
	p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return generation, nil })
	p.now, p.metrics = func() time.Time { return now }, metrics
	w := &worker{id: "w", info: remote.WorkerInfo{Platform: amd64, Capacity: 4}, running: 3}
	p.workers[w.id] = w
	running := func(id string) *attempt {
		at := &attempt{id: id, worker: w, lease: now.Add(p.cfg.Lease), done: make(chan struct{}), generation: 1}
		p.attempts[at.id] = at
		return at
	}
	withdrawn, old, lost := running("withdrawn"), running("old"), running("lost")
	pending := &attempt{id: "pending", trace: t.Context(), done: make(chan struct{})}
	p.pending = []*attempt{pending}

	p.withdraw(withdrawn, killWithdrawn)
	p.withdraw(withdrawn, killWithdrawn) // counted once
	p.withdraw(pending, killDemand)
	unknown := newID() // as an earlier instance's
	for range 2 {      // its worker missed the first answer, and heartbeats again: one kill
		if answer, _ := p.Heartbeat(t.Context(), unknown, remote.Heartbeat{}); !answer.Kill {
			t.Error("the heartbeat of an unknown attempt wasn't told to kill")
		}
	}
	for range 2 { // the first beat answers the kill, and the second meets the attempt settling: neither is stale
		if answer, _ := p.Heartbeat(t.Context(), withdrawn.id, remote.Heartbeat{}); !answer.Kill {
			t.Error("the heartbeat of a withdrawn attempt wasn't told to kill")
		}
	}
	checkMeasured(t, reader, 1, "vamos.attempts.killed", reason(killWithdrawn))
	checkMeasured(t, reader, 1, "vamos.attempts.killed", reason(killDemand))
	checkMeasured(t, reader, 1, "vamos.attempts.killed", reason(killStale))

	_ = p.Report(t.Context(), withdrawn.id, remote.AttemptReport{}) // a report of a killed one, settling
	generation = 2                                                  // a prune ran
	_ = p.Report(t.Context(), old.id, remote.AttemptReport{})
	checkMeasured(t, reader, 1, "vamos.reports.rejected", reason(rejectedStale))
	checkMeasured(t, reader, 1, "vamos.reports.rejected", reason(rejectedGeneration))

	now = lost.lease.Add(time.Millisecond)
	p.expire()
	checkMeasured(t, reader, 1, "vamos.leases.expired")
	// The worker of the withdrawn attempt heartbeats late, once it has settled: counted as withdrawn, not as stale.
	// That of the lost one does too: killed now, as stale.
	for _, id := range []string{withdrawn.id, lost.id} {
		if answer, _ := p.Heartbeat(t.Context(), id, remote.Heartbeat{}); !answer.Kill {
			t.Errorf("the late heartbeat of %s wasn't told to kill", id)
		}
	}
	checkMeasured(t, reader, 2, "vamos.attempts.killed", reason(killStale))
	checkMeasured(t, reader, 1, "vamos.attempts.killed", reason(killWithdrawn))
}

// TestMetricsGauges checks the gauges as they are collected, with one build's tasks running and another's waiting for
// a platform: attempts queued, by the platform they run on, and
// running, by their worker's; workers and their slots by platform; builds waiting for a platform no worker serves;
// and jobs running, by kind.
func TestMetricsGauges(t *testing.T) {
	cfg, reader := metered(t, Config{Platforms: []imgspecv1.Platform{amd64, arm64}})
	f := newFixture(t, cfg)
	release := make(chan struct{})
	startWorker(t, f, amd64, 3, fakeRun(f, func(a *remote.Assignment, _ func() bool) error {
		if a.Request != nil && a.Request.RunsCommands() {
			<-release // the tasks run until the test ends; the pulls, which any worker takes, end at once
		}
		return nil
	}))
	waiting := testGraphFor(t, f, "waits", arm64) // no worker serves arm64
	waiting.Platform = arm64
	id, err := f.c.Submit(t.Context(), api.Request{Graph: &waiting})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.c.Cancel(context.Background(), id) })
	running, err := f.c.Submit(t.Context(), api.Request{Graph: new(testGraph(t, f, "runs"))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.c.Cancel(context.Background(), running) })
	t.Cleanup(func() { close(release) }) // first, so that the cancelled tasks end

	platform := func(p string) attribute.KeyValue { return attribute.String("platform", p) }
	state := func(s string) attribute.KeyValue { return attribute.String("state", s) }
	waitFor(t, func() bool {
		n, _ := measured(t, reader, "vamos.attempts", state("running"), platform("linux/amd64"))
		q, _ := measured(t, reader, "vamos.attempts", state("queued"), platform("linux/arm64"))
		return n == 2 && q == 2 // each build's two tasks
	})
	checkMeasured(t, reader, 1, "vamos.workers", platform("linux/amd64"))
	checkMeasured(t, reader, 2, "vamos.worker.slots", state("busy"), platform("linux/amd64"))
	checkMeasured(t, reader, 1, "vamos.worker.slots", state("free"), platform("linux/amd64"))
	checkMeasured(t, reader, 1, "vamos.builds.waiting", platform("linux/arm64"))
	checkMeasured(t, reader, 2, "vamos.jobs.open", attribute.String("kind", "build"))
}

// TestMetricsJournalAndGaps checks that a journal write that fails is counted, and a watch that meets a gap in a
// job's events.
func TestMetricsJournalAndGaps(t *testing.T) {
	state := t.TempDir()
	cfg, reader := metered(t, Config{State: state})
	f := newFixture(t, cfg)
	startWorker(t, f, amd64, 1, fakeRun(f, nil))
	f.c.jobs.RetainEvents(1)
	g := testGraph(t, f, "gap")
	id, err := f.c.Submit(t.Context(), api.Request{Graph: &g})
	if err != nil {
		t.Fatal(err)
	}
	// Watched from the start until a watch meets the gap, as one does once the job has ended, when only its last event
	// is retained; each watch meets one gap at most.
	for gaps := int64(0); gaps == 0; gaps, _ = measured(t, reader, "vamos.events.gaps") {
		if err := f.c.Watch(t.Context(), id, 0, func(api.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	checkMeasured(t, reader, 1, "vamos.events.gaps")

	if err := os.RemoveAll(filepath.Join(state, "tmp")); err != nil {
		t.Fatal(err)
	}
	g = testGraph(t, f, "unrecorded")
	if _, err := f.c.Submit(t.Context(), api.Request{Graph: &g}); err == nil {
		t.Fatal("a job whose record couldn't be written was accepted")
	}
	checkMeasured(t, reader, 1, "vamos.journal.failures")
}

// TestMetricsStoreSize checks that a coordinator over a directory observes what its stores hold, which a prune
// empties.
func TestMetricsStoreSize(t *testing.T) {
	d, err := cache.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.PutBytes(t.Context(), []byte("held")); err != nil {
		t.Fatal(err)
	}
	cfg, reader := metered(t, Config{})
	c, err := New(d, d, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	content := attribute.String("store", "content")
	waitFor(t, func() bool { n, _ := measured(t, reader, "vamos.store.size", content); return n == 4 })
	if _, err := d.ClearContent(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkMeasured(t, reader, 0, "vamos.store.size", content)
}

// TestWithdrawnTryEndsKilled checks that the try of a running attempt that is withdrawn ends at once, killed, so that
// the trace the job keeps as it returns holds it, rather than once the worker hears of the kill.
func TestWithdrawnTryEndsKilled(t *testing.T) {
	p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return 0, nil })
	w := &worker{id: "w", info: remote.WorkerInfo{Platform: amd64, Capacity: 1}, running: 1}
	p.workers[w.id] = w
	recorder := tracing.NewRecorder("executor", nil)
	ctx := tracing.WithRecorder(t.Context(), recorder, "")
	at := &attempt{id: "a", worker: w, lease: time.Now().Add(time.Hour), done: make(chan struct{})}
	_, at.try = tracing.Start(ctx, "try", tracing.Role, tracing.RoleTry)
	p.attempts[at.id] = at
	p.withdraw(at, killWithdrawn)
	spans := recorder.Spans()
	if len(spans) != 1 || spans[0].Args["outcome"] != "killed" {
		t.Errorf("once withdrawn, the attempt's trace holds %+v; want its try, ended killed", spans)
	}
}

// sizedMemory is a memory store that tells whether its size was asked for, as a cache.Directory's count starts then.
type sizedMemory struct {
	*cache.Memory
	asked atomic.Bool
}

func (s *sizedMemory) Size() (cache.DirectorySize, bool) {
	s.asked.Store(true)
	return cache.DirectorySize{}, true
}

// TestStoresCountedOnlyForExportedMetrics checks that a coordinator asks its stores for their size, which starts a
// directory's count, only if it exports metrics: not without telemetry, nor with none returned, nor with a meter that
// exports nothing.
func TestStoresCountedOnlyForExportedMetrics(t *testing.T) {
	exporting, _ := metered(t, Config{})
	for name, c := range map[string]struct {
		telemetry func(string) (*telemetry.Telemetry, error)
		asked     bool
	}{
		"no telemetry":  {},
		"none returned": {telemetry: func(string) (*telemetry.Telemetry, error) { return nil, nil }},
		"no-op meter": {telemetry: func(string) (*telemetry.Telemetry, error) {
			return &telemetry.Telemetry{Meter: noop.NewMeterProvider()}, nil
		}},
		"exported meter": {telemetry: exporting.Telemetry, asked: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &sizedMemory{Memory: cache.NewMemory()}
			coordinator, err := New(s, s, Config{Telemetry: c.telemetry})
			if err != nil {
				t.Fatal(err)
			}
			coordinator.Close()
			if s.asked.Load() != c.asked {
				t.Errorf("the coordinator asked its store for its size: %t; want %t", s.asked.Load(), c.asked)
			}
		})
	}
}

// TestTryOutcomes checks how a try's span says it ended: a report with a result succeeded, one with nothing or a
// failure, or a tagging that failed a destination, failed, one lost or killed as such, and one the coordinator's
// shutdown ended killed, not failed.
func TestTryOutcomes(t *testing.T) {
	for _, c := range []struct {
		name   string
		at     attempt
		report *remote.AttemptReport
		err    error
		want   string
	}{
		{"result", attempt{}, &remote.AttemptReport{Report: &sched.Report{}}, nil, "succeeded"},
		{"nothing", attempt{}, &remote.AttemptReport{}, nil, "failed"},
		{"a build's report of a push", attempt{}, &remote.AttemptReport{Pushed: &api.Pushed{}}, nil, "failed"},
		{"push", attempt{push: &remote.PushWork{}}, &remote.AttemptReport{Pushed: &api.Pushed{}}, nil, "succeeded"},
		{"a push's report of a build", attempt{push: &remote.PushWork{}},
			&remote.AttemptReport{Report: &sched.Report{}}, nil, "failed"},
		{"upload", attempt{push: &remote.PushWork{Mode: remote.ModeUpload}},
			&remote.AttemptReport{Uploaded: &remote.Uploaded{}}, nil, "succeeded"},
		{"tagging", attempt{push: &remote.PushWork{Mode: remote.ModeTag}},
			&remote.AttemptReport{Tagged: &api.Outcome{}}, nil, "succeeded"},
		{"failure", attempt{}, &remote.AttemptReport{Failure: &sched.AttemptFailure{}}, nil, "failed"},
		{"tagging failed", attempt{push: &remote.PushWork{Mode: remote.ModeTag}},
			&remote.AttemptReport{Tagged: &api.Outcome{Failure: &api.Failure{}}}, nil, "failed"},
		{"lost", attempt{lost: true}, nil, errWorkerLost, "lost"},
		{"killed", attempt{killed: true}, nil, context.Canceled, "killed"},
		{"shutdown", attempt{}, nil, errPoolClosed, "killed"},
		{"error", attempt{}, nil, errors.New("reading the build cache's generation"), "failed"},
	} {
		if got := c.at.tryOutcome(c.report, c.err); got != c.want {
			t.Errorf("%s: the try's outcome = %q; want %q", c.name, got, c.want)
		}
	}
}

// TestShutdownWithdrawsUncounted checks that what the coordinator's shutdown takes back, as its cancelled jobs
// withdraw their attempts, isn't counted as withdrawn by their clients, nor as stale when its worker heartbeats late.
func TestShutdownWithdrawsUncounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := newMetrics(provider.Meter(meterName))
	if err != nil {
		t.Fatal(err)
	}
	p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return 0, nil })
	p.metrics = metrics
	w := &worker{id: "w", info: remote.WorkerInfo{Platform: amd64, Capacity: 1}, running: 1}
	p.workers[w.id] = w
	at := &attempt{id: newID(), worker: w, lease: time.Now().Add(time.Hour), done: make(chan struct{})}
	p.attempts[at.id] = at
	p.closing()
	p.withdraw(at, killWithdrawn)
	p.close()
	if answer, _ := p.Heartbeat(t.Context(), at.id, remote.Heartbeat{}); !answer.Kill { // its worker, late
		t.Error("the late heartbeat of an attempt the shutdown ended wasn't told to kill")
	}
	checkMeasured(t, reader, 0, "vamos.attempts.killed", reason(killWithdrawn))
	checkMeasured(t, reader, 0, "vamos.attempts.killed", reason(killStale))
}
