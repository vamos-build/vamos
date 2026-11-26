package worker

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// meterName names the meter of the worker's instruments, as the telemetry package names its own.
const meterName = "github.com/vamos-build/vamos"

// metrics are the worker's counters of what degrades quietly, each created at start so that it is exported from zero.
type metrics struct {
	fenced, dropped, stuck metric.Int64Counter
}

// fence counts an attempt the worker killed as its lease couldn't be renewed.
func (m *metrics) fence() { m.count(func(m *metrics) metric.Int64Counter { return m.fenced }) }

// drop counts a batch of an attempt's events the coordinator didn't take.
func (m *metrics) drop() { m.count(func(m *metrics) metric.Int64Counter { return m.dropped }) }

// stick counts an attempt's cgroup that a kill couldn't empty in time.
func (m *metrics) stick() { m.count(func(m *metrics) metric.Int64Counter { return m.stuck }) }

// newMetrics returns the instruments of the meter provider, those of a no-op one if it is nil.
func newMetrics(provider metric.MeterProvider) (*metrics, error) {
	if provider == nil {
		provider = noop.NewMeterProvider()
	}
	meter := provider.Meter(meterName)
	var errs []error
	counter := func(name, unit, description string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(description))
		errs = append(errs, err)
		return c
	}
	m := &metrics{
		fenced: counter("vamos.worker.fenced", "{attempt}",
			"Running attempts the worker killed as their lease couldn't be renewed."),
		dropped: counter("vamos.worker.events.dropped", "{batch}",
			"Batches of an attempt's events the coordinator didn't take."),
		stuck: counter("vamos.worker.cgroups.stuck", "{cgroup}",
			"Attempts' cgroups that a kill couldn't empty in time."),
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	for _, c := range []metric.Int64Counter{m.fenced, m.dropped, m.stuck} {
		c.Add(context.Background(), 0) // so that each is exported from zero
	}
	return m, nil
}

// count adds one to the counter of m that which returns, if m counts anything, as a Worker that New didn't make
// doesn't.
func (m *metrics) count(which func(*metrics) metric.Int64Counter) {
	if m != nil {
		which(m).Add(context.Background(), 1)
	}
}
