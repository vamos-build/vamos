// Package telemetry exports a coordinator's spans and metrics, and a worker's metrics, over OpenTelemetry.
//
// # Invariants
//
// This package imports tracing and OpenTelemetry, and no other vamos package;
// sched, api and tracing never import it or any of OpenTelemetry,
// and remote's non-test code never imports OpenTelemetry's SDK.
// The spans a Bridge exports are the snapshots that sched stores, so the exported trace and the stored one show the
// same spans, truncation included.
// Exported spans are made through the SDK with IDs and times that this package chooses:
// a span's context carries the IDs that chosenIDs gives it once, and its start and end are vamos's.
// Every ID is derived from the coordinator's instance, the job, its run after the first, and vamos's span or attempt
// IDs, never drawn at random,
// so that a job resumed after a restart that kept the coordinator's state goes on in the same trace,
// and a joiner links to an attempt's span without asking its starter;
// a trace joined through a traceparent keeps the outer trace's ID.
// Whether a job's trace is kept is decided once, as it starts, and a dropped trace makes no span at all,
// neither the job's nor its attempts'.
// Nothing configured exports nothing: an exporter is on only if its variable names it,
// or an OTLP endpoint is set for its signal.
// A value Setup can't take fails it, naming the variable, its value, unless it may be secret, and what it takes.
// The bridge remembers at most maxRemembered produced tokens and attempts for links, so its memory is bounded.
// A Bridge converts snapshots on a goroutine of its own, in the order it is given them, off the build's path,
// and the SDK blocks it rather than drop a span, so a snapshot is handed over whole or not at all:
// what the bridge drops, when more than maxQueuedBytes wait, is whole snapshots, reported once for each burst,
// and the rest of the job that lost one, whose trace is then cut short.
// Every exporter of traces shares the one converter, so one whose queue stays full holds up the others.
// What an exported span holds is bounded by the bridge's budget, maxAttributesSize and the sizes beside it,
// so that what the exporters' queues hold is bounded too.
//
// # Lifecycle
//
// Setup reads the OTEL_* variables through Config.Getenv, builds the exporters, and listens for Prometheus's scrapes
// before it returns, so that an address in use fails it.
// A coordinator then gives each job's context to Bridge.StartJob as the job starts,
// which puts its record and its tracing.Exported in the context, which the attempts it starts inherit.
// The sched hooks call Attempt as an attempt's runner returns, and Job as the job ends, each with the snapshot it
// stores.
// Export happens in the background: a failure doesn't reach the job, but the process's error handler,
// which HandleErrors installs once, and which reports each distinct one at most once a minute.
// A Setup that fails stops what it had started.
// Telemetry.Shutdown, called once the API has stopped, converts what the bridge has queued, then stops every exporter
// at once, so that one that blocks strands no other, and returns within 10 seconds whether or not they have finished.
//
// # Changes
//
// config.go reads the settings and names each mistake; sampler.go decides a job's sampling.
// ids.go derives the IDs and gives them to the SDK through chosenIDs; TestChosenIDs proves that the pinned SDK keeps
// them.
// bridge.go converts spans, their attributes, as design.md's table names them, and their links;
// BenchmarkConvertAttempt measures it.
// setup.go builds the exporters and serves Prometheus; errors.go limits the error handler.
// outer.go holds OuterTrace, the one check of an outer trace that the CLI and the coordinator make.
package telemetry
