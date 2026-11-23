package coord

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/containerd/platforms"

	"github.com/vamos-build/vamos/internal/cache"
)

// meterName names the meter of the coordinator's instruments, as the telemetry package names its own.
const meterName = "github.com/vamos-build/vamos"

// The reasons a job is refused, as vamos.jobs.refused labels them.
const (
	refusedBusy       = "busy"       // as many jobs are open as the coordinator accepts
	refusedPlatform   = "platform"   // the graph's platform isn't offered
	refusedVersion    = "version"    // the graph's version isn't the coordinator's
	refusedGeneration = "generation" // the build cache was pruned while the build's sources were captured
)

// The reasons an attempt is killed, as vamos.attempts.killed labels them.
const (
	killWithdrawn = "withdrawn" // every job that wanted it ended
	killDemand    = "demand"    // no build demands it any more, or permits its emulation
	killStale     = "stale"     // its worker heartbeat for an attempt the coordinator no longer counts live
)

// The reasons a report is rejected, as vamos.reports.rejected labels them.
const (
	rejectedStale      = "stale"      // the attempt is no longer live: lost, killed or replaced
	rejectedGeneration = "generation" // the build cache was pruned while it ran
)

// Metrics are the coordinator's counters of what happens with no span of its own, counted where it happens, each
// created at start so that it is exported from zero. A coordinator always has one, a no-op meter's if it exports no
// metrics; its gauges, which read state as they are collected, are registered by register.
type Metrics struct {
	refused, killed, rejected               metric.Int64Counter // by reason
	expired, journalFailures, gaps, retries metric.Int64Counter
	attempts, workers, slots, waiting, jobs metric.Int64ObservableGauge
	store                                   metric.Int64ObservableGauge // bytes, by store
	meter                                   metric.Meter
	exported                                bool // meter exports what it measures, which a nil one didn't
	// registration is the gauges' callback's, which stays registered until the meter's provider shuts down, after
	// Close, so that its last export holds the gauges, unless New fails, which unregisters it.
	registration metric.Registration
}

// newMetrics returns the instruments of meter, nil for a no-op one's.
func newMetrics(meter metric.Meter) (*Metrics, error) {
	exported := meter != nil
	if meter == nil {
		meter = noop.NewMeterProvider().Meter(meterName)
	}
	m := &Metrics{meter: meter, exported: exported}
	var errs []error
	counter := func(name, unit, description string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(description))
		errs = append(errs, err)
		return c
	}
	gauge := func(name, unit, description string) metric.Int64ObservableGauge {
		g, err := meter.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(description))
		errs = append(errs, err)
		return g
	}
	m.refused = counter("vamos.jobs.refused", "{job}", "Jobs refused, by reason.")
	m.killed = counter("vamos.attempts.killed", "{attempt}", "Attempts taken back, queued or running, by reason.")
	m.rejected = counter("vamos.reports.rejected", "{report}", "Workers' reports refused, by reason.")
	m.expired = counter("vamos.leases.expired", "{lease}", "Attempts lost as their worker's lease expired.")
	m.journalFailures = counter("vamos.journal.failures", "{write}", "Writes to the job journal that failed.")
	m.gaps = counter("vamos.events.gaps", "{gap}", "Gaps in a job's events that watches were served.")
	m.retries = counter("vamos.attempts.retries", "{attempt}", "Attempts tried again after their worker was lost.")
	m.attempts = gauge("vamos.attempts", "{attempt}", "Attempts queued for a worker or running, by platform.")
	m.workers = gauge("vamos.workers", "{worker}", "Registered workers, by platform.")
	m.slots = gauge("vamos.worker.slots", "{slot}", "Registered workers' slots, busy or free, by platform.")
	m.waiting = gauge("vamos.builds.waiting", "{build}", "Builds waiting for a worker of a platform none serves.")
	m.jobs = gauge("vamos.jobs.open", "{job}", "Jobs running, by kind.")
	m.store = gauge("vamos.store.size", "By", "What the build cache holds, by store: content or records.")
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("making the coordinator's metrics: %w", err)
	}
	ctx := context.Background()
	for _, c := range []metric.Int64Counter{m.expired, m.journalFailures, m.gaps, m.retries} {
		c.Add(ctx, 0) // so that each is exported from zero
	}
	for c, reasons := range map[metric.Int64Counter][]string{
		m.refused:  {refusedBusy, refusedPlatform, refusedVersion, refusedGeneration},
		m.killed:   {killWithdrawn, killDemand, killStale},
		m.rejected: {rejectedStale, rejectedGeneration},
	} {
		for _, r := range reasons {
			c.Add(ctx, 0, metric.WithAttributes(attribute.String("reason", r)))
		}
	}
	return m, nil
}

// jobKinds are the kinds of job, which vamos.jobs.open observes each of, at zero when none runs.
var jobKinds = []string{"build", "push", "publish"}

// sizesRead is what one collection read of the stores' counts.
type sizesRead struct {
	content, records         cache.DirectorySize
	contentDone, recordsDone bool
}

// sizer is a store that counts what it holds, as cache.Directory does.
type sizer interface {
	Size() (cache.DirectorySize, bool)
}

// platformLabel returns the platform at runs on, as its metrics label it: "any" for work that runs no command, such
// as a push or a resolve, which any worker takes.
func (at *attempt) platformLabel() string {
	if at.req == nil || !at.req.RunsCommands() {
		return "any"
	}
	return at.runsOn()
}

// tryPlatform returns the platform at's try on w is labelled with: w's, or "any" for work that runs no command, as
// the gauges label it while it waits.
func (at *attempt) tryPlatform(w *worker) string {
	if at.platformLabel() == "any" {
		return "any"
	}
	return platforms.Format(w.info.Platform)
}

// unregister stops the gauges' callback, for a coordinator that failed to start.
func (m *Metrics) unregister() {
	if m.registration != nil {
		_ = m.registration.Unregister() // fails only if it was never registered
	}
}

// noMetrics returns the instruments of a no-op meter.
func noMetrics() *Metrics {
	m, _ := newMetrics(nil) // a no-op meter's instruments never fail
	return m
}

// register registers the callback of the gauges, which reads c's pool and jobs as each collection asks.
func (m *Metrics) register(c *Coordinator) error {
	// The stores' counts start now, only if they are exported, so that they are done by the first collection, or
	// soon after.
	if m.exported {
		for _, store := range []any{c.content, c.records} {
			if s, ok := store.(sizer); ok {
				s.Size()
			}
		}
	}
	r, err := m.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c.pool.observe(m, o)
		open := c.openJobs()
		for _, kind := range jobKinds {
			o.ObserveInt64(m.jobs, int64(open[kind]), metric.WithAttributes(attribute.String("kind", kind)))
		}
		// Only a store that counts what it holds is observed, each by its own count, once it has counted what it held
		// as it opened; one store that is both is read once, so that a scrape shows both or neither.
		content, _ := c.content.(sizer)
		records, _ := c.records.(sizer)
		var size sizesRead
		if content != nil {
			size.content, size.contentDone = content.Size()
		}
		// Compared as directories alone, since interfaces of a type that isn't comparable panic when compared.
		d, _ := c.content.(*cache.Directory)
		if same, _ := c.records.(*cache.Directory); d != nil && same == d {
			size.records, size.recordsDone = size.content, size.contentDone
		} else if records != nil {
			size.records, size.recordsDone = records.Size()
		}
		if size.contentDone {
			o.ObserveInt64(m.store, size.content.Content, metric.WithAttributes(attribute.String("store", "content")))
		}
		if size.recordsDone {
			o.ObserveInt64(m.store, size.records.Records, metric.WithAttributes(attribute.String("store", "records")))
		}
		return nil
	}, m.attempts, m.workers, m.slots, m.waiting, m.jobs, m.store)
	if err != nil {
		return fmt.Errorf("observing the coordinator's state: %w", err)
	}
	m.registration = r
	return nil
}

// refuse counts a job refused for reason.
func (m *Metrics) refuse(reason string) {
	m.refused.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// kill counts an attempt taken back for reason, queued or running.
func (m *Metrics) kill(reason string) {
	m.killed.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// reject counts a report rejected for reason.
func (m *Metrics) reject(reason string) {
	m.rejected.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// count adds one to c.
func (m *Metrics) count(c metric.Int64Counter) {
	c.Add(context.Background(), 1)
}

// observe observes the pool's gauges: attempts queued, by the platform they run on, and running, by their worker's;
// workers and their slots, busy and free, by platform; and builds with an attempt that waits for a platform no
// registered worker serves, by that platform.
func (p *pool) observe(m *Metrics, o metric.Observer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	type key struct{ state, platform string }
	attempts := map[key]int64{}
	waiting := map[string]map[string]bool{} // the builds waiting, by platform
	// Every platform offered or served is observed, at zero when nothing is there.
	for _, platform := range p.cfg.Platforms {
		name := platforms.Format(platforms.Normalize(platform))
		attempts[key{"queued", name}], attempts[key{"running", name}] = 0, 0
		waiting[name] = map[string]bool{}
	}
	for _, at := range p.pending {
		attempts[key{"queued", at.platformLabel()}]++
	}
	workers, slots := map[string]int64{}, map[key]int64{}
	for _, w := range p.workers {
		platform := platforms.Format(w.info.Platform)
		workers[platform]++
		attempts[key{"queued", platform}] += 0
		attempts[key{"running", platform}] += 0
		if waiting[platform] == nil {
			waiting[platform] = map[string]bool{}
		}
		slots[key{"busy", platform}] += int64(w.running)
		slots[key{"free", platform}] += int64(max(w.info.Capacity-w.running, 0))
	}
	for _, at := range p.attempts {
		if at.worker != nil && !at.ended() {
			attempts[key{"running", at.tryPlatform(at.worker)}]++ // as its try's duration is labelled
		}
	}
	for _, at := range p.pending {
		if at.req == nil || !at.req.RunsCommands() {
			continue // it takes any worker, or one that publishes, whatever its platform
		}
		served := false
		for _, w := range p.workers {
			served = served || p.suits(w, at)
		}
		if !served {
			platform := at.runsOn()
			if waiting[platform] == nil {
				waiting[platform] = map[string]bool{}
			}
			waiting[platform][string(at.build.job)] = true
		}
	}
	for k, n := range attempts {
		o.ObserveInt64(m.attempts, n, metric.WithAttributes(attribute.String("state", k.state),
			attribute.String("platform", k.platform)))
	}
	for platform, n := range workers {
		o.ObserveInt64(m.workers, n, metric.WithAttributes(attribute.String("platform", platform)))
	}
	for k, n := range slots {
		o.ObserveInt64(m.slots, n, metric.WithAttributes(attribute.String("state", k.state),
			attribute.String("platform", k.platform)))
	}
	for platform, builds := range waiting {
		o.ObserveInt64(m.waiting, int64(len(builds)), metric.WithAttributes(attribute.String("platform", platform)))
	}
}
