package features

import (
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/testenv"
)

// newMetricsWorld starts a scenario of the operational metrics feature, from its background: a project that pulls
// busybox, which builds. A coordinator exports metrics of its capacity, speed, cache and what degrades quietly, over
// OpenTelemetry and Prometheus, as the standard OTEL_* variables configure it.
func newMetricsWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// aTaskAndAppAddingIt declares the task "out", which writes a file, with nonce, and the layer "app" on busybox, which
// adds it.
func aTaskAndAppAddingIt(nonce string) string {
	return `
		task(name="out", base="busybox", steps=[run(["sh", "-c", "mkdir /out && echo ` + nonce + ` > /out/f"])],
		     source="/out", include=["f"])
		layer(name="app", base="busybox", steps=[add("out", "/app")])
	`
}

// anAppAtTheBarrier declares the layer "app" on busybox, which waits at the scenario's barrier, with nonce.
func anAppAtTheBarrier(nonce string) string {
	return `layer(name="app", base="busybox", steps=[env(NONCE="` + nonce + `"),
		run(["wget", "-q", "-O", "/dev/null", "<barrier>"])])`
}

// TestMetrics_ConfiguredByTheStandardVariables specifies, as the coordinator reference says, that a coordinator reads
// its metric export configuration from the standard OTEL_* variables when it starts: OTEL_METRICS_EXPORTER takes otlp,
// prometheus, console or none, or several, and unset is otlp when an OTLP endpoint is set and none otherwise. The
// scenario that finds nothing at Prometheus's default port runs alone, since it reads a port the machine shares.
func TestMetrics_ConfiguredByTheStandardVariables(t *testing.T) {
	t.Run("A coordinator with nothing configured serves no metrics", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenNothingListensOnThePrometheusDefaultPort()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.ThenNothingAnswersOnThePrometheusDefaultPort()
	})

	t.Run("OTLP and Prometheus at once", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenTheCoordinatorEnvironmentSets("OTEL_METRICS_EXPORTER", "otlp")
		w.GivenTheCoordinatorExportsMetricsEvery(200 * time.Millisecond)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCollectorReceivedTheMetric("vamos.jobs.duration")
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_jobs_duration_seconds_count", 1, "kind", "build")
	})

	t.Run("An unknown metrics exporter is a mistake", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorEnvironmentSets("OTEL_METRICS_EXPORTER", "statsd")
		w.WhenIStartACoordinatorUntilItListens()
		w.ThenItFailsWith("OTEL_METRICS_EXPORTER", "statsd", "otlp", "prometheus", "console", "none")
	})
}

// TestMetrics_PrometheusIsServedApartFromTheAPI specifies that a coordinator serves Prometheus metrics at /metrics on
// an address of their own, and none on the API's, and fails to start on an address it can't listen on.
func TestMetrics_PrometheusIsServedApartFromTheAPI(t *testing.T) {
	parallelRule(t)
	t.Run("Metrics are scraped on their own port", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_workers", 1)
		w.ThenTheAPIServesNoMetrics()
	})

	t.Run("A metrics port in use is a mistake", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorEnvironmentSets("OTEL_METRICS_EXPORTER", "prometheus")
		w.GivenAPortThatAnotherProcessListensOn()
		w.WhenIStartACoordinatorUntilItListens()
		w.ThenItFailsWith("serving Prometheus metrics on 127.0.0.1:")
	})
}

// TestMetrics_LabeledCoarsely specifies that no metric is labeled with a target, a workspace, a job, an attempt, a
// worker's ID or a file, so that builds add no series.
func TestMetrics_LabeledCoarsely(t *testing.T) {
	parallelRule(t)
	t.Run("A workspace's builds add no series", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		nonce := testenv.NewNonce()
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.storefront")`})
		w.GivenTheBuildFileDeclares(anAppEchoing(nonce) + "\n" +
			`layer(name="api", base="busybox", steps=[env(NONCE="` + nonce + `"), run(["true"])])`)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIBuild("api", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetrics()
		w.ThenNoSeriesNames("com.acme.storefront", "app", "api")
	})
}

// TestMetrics_Capacity specifies the metrics of whether the fleet has enough workers: attempts queued and running,
// workers and their slots by platform, builds waiting for a platform, open jobs, and jobs refused by why. The platform
// no worker runs is whichever of linux/amd64 and linux/arm64 this machine isn't.
func TestMetrics_Capacity(t *testing.T) {
	parallelRule(t)
	t.Run("A build waiting for a platform is counted", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker("--offer", thisPlatform(), "--offer", anotherPlatform())
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.GivenAnotherClientBuildsOnTheCoordinator("app", onTheCoordinator("<coordinator>"),
			forPlatform(anotherPlatform()), onTheExecPlatform(anotherPlatform()))
		w.WhenIScrapeTheMetricsOnceItShows("vamos_builds_waiting", 1, "platform", anotherPlatform())
		w.ThenTheMetricIsAtLeast("vamos_attempts", 1, "state", "queued")
	})

	t.Run("A refused build is counted by reason", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker("--max-builds", "1")
		w.GivenABarrier()
		w.GivenTheBuildFileDeclares(anAppAtTheBarrier(testenv.NewNonce()))
		w.GivenAnotherClientBuildsOnTheCoordinator("app", onTheCoordinator("<coordinator>"))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItFailsWith("busy")
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_jobs_refused_total", 1, "reason", "busy")
	})

	t.Run("Workers and their slots are counted by platform", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_workers", 1, "platform", thisPlatform())
		w.ThenTheMetricIs("vamos_worker_slots", 2, "state", "free")
	})
}

// TestMetrics_Speed specifies the metrics of whether builds are fast: jobs and attempts timed, and the API's requests,
// by route, never by path.
func TestMetrics_Speed(t *testing.T) {
	parallelRule(t)
	t.Run("A build's job and attempt are timed", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_jobs_duration_seconds_count", 1, "kind", "build", "outcome", "succeeded")
		w.ThenTheMetricIs("vamos_attempts_duration_seconds_count", 1, "kind", "layer", "outcome", "succeeded")
	})

	t.Run("Requests are labeled by route, not by path", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricsHaveTheLabel("http_server_request_duration_seconds_count", "http_route",
			"/v1/jobs/{id}/events")
		w.ThenTheMetricsHaveTheLabel("http_server_request_duration_seconds_count", "http_request_method", "GET")
		w.ThenNoSeriesNames()
	})
}

// TestMetrics_Cache specifies the metrics of whether the cache earns its keep: hits, misses by cause, joins, what hits
// saved, results lost by why, and what the stores hold.
func TestMetrics_Cache(t *testing.T) {
	parallelRule(t)
	t.Run("A hit and a miss are counted, with what the hit saved", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[env(NONCE="` + testenv.NewNonce() +
			`"), run(["sleep", "2"])])`)
		w.GivenTheCoordinatorHasBuilt("app")
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_cache_misses_total", 1, "kind", "layer", "cause", "new")
		w.ThenTheMetricIs("vamos_cache_hits_total", 1, "kind", "layer")
		w.ThenTheMetricIsAtLeast("vamos_cache_saved_seconds_total", 1, "kind", "layer")
	})

	t.Run("A result lost to a missing blob is counted", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(aTaskAndAppAddingIt(testenv.NewNonce()))
		w.GivenTheCoordinatorHasBuilt("app")
		w.GivenTheBlobsOfTheCoordinatorsBuildCacheAreRemoved()
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_cache_lost_total", 1, "kind", "task", "reason", "missing")
		w.ThenTheMetricIs("vamos_cache_misses_total", 1, "cause", "lost")
	})

	t.Run("The store's size follows what it holds", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(aTaskAndAppAddingIt(testenv.NewNonce()))
		w.GivenTheCoordinatorHasBuilt("app")
		w.GivenTheCoordinatorHasSettled()
		w.GivenIScrapedTheMetrics()
		w.WhenIPrune(onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIsLessThanBefore("vamos_store_size_bytes", "store", "content")
	})
}

// TestMetrics_WhatDegradesQuietly specifies the counters of what goes wrong without failing a build: retries, expired
// leases, kills, rejected reports, journal failures and gaps in event streams, each exported from zero.
func TestMetrics_WhatDegradesQuietly(t *testing.T) {
	parallelRule(t)
	t.Run("A lost worker's attempt is counted as retried", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithTwoWorkers()
		w.GivenABarrier()
		w.GivenTheBuildFileDeclares(anAppAtTheBarrier(testenv.NewNonce()))
		w.GivenTheWorkerRunningAStepIsStoppedOnceItReachesTheBarrier()
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_leases_expired_total", 1)
		w.ThenTheMetricIs("vamos_attempts_retries_total", 1)
	})

	t.Run("A quiet coordinator exports its counters at zero", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_leases_expired_total", 0)
		w.ThenTheMetricIs("vamos_reports_rejected_total", 0)
		w.ThenTheMetricIs("vamos_journal_failures_total", 0)
	})
}

// TestMetrics_HistogramsCarryExemplars specifies that a histogram measured from a span of a sampled exported trace
// carries the span as its exemplar, and one of a trace that isn't kept, none.
func TestMetrics_HistogramsCarryExemplars(t *testing.T) {
	parallelRule(t)
	t.Run("A slow attempt leads to its trace", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetricsAsOpenMetrics()
		w.ThenTheMetricHasAnExemplarOfTheExportedSpan("vamos_attempts_duration_seconds_bucket", "try")
	})

	t.Run("A sampled-out span leaves no exemplar", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenTheCoordinatorEnvironmentSets("OTEL_TRACES_SAMPLER", "always_off")
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetricsAsOpenMetrics()
		w.ThenTheMetricIs("vamos_attempts_duration_seconds_count", 1, "kind", "layer")
		w.ThenTheMetricHasNoExemplar("vamos_attempts_duration_seconds_bucket")
	})
}

// TestMetrics_AttemptsReportTheirResources specifies that a worker reports what an attempt's processes used, read from
// its cgroup, which the attempt's try span carries and the coordinator measures: its CPU time, and its peak memory
// where the worker's cgroups count it, as this machine's may not.
func TestMetrics_AttemptsReportTheirResources(t *testing.T) {
	parallelRule(t)
	t.Run("A busy step's CPU time is reported", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[env(NONCE="` + testenv.NewNonce() +
			`"), run(["sh", "-c", "timeout 2 sh -c 'while :; do :; done' || true"])])`)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"), traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpansSayAtLeast("executor", "try", traceArgs{"kind": "pull"},
			traceArgs{"kind": "layer", "cpu": atLeast(0.1)})
		w.WhenIScrapeTheMetrics()
		w.ThenTheMetricIs("vamos_attempts_cpu_seconds_count", 1, "kind", "layer")
	})
}

// TestMetrics_WorkersExportOverOTLP specifies that a worker exports its own counters over OTLP, as the standard
// variables say, and refuses Prometheus, which it can't serve.
func TestMetrics_WorkersExportOverOTLP(t *testing.T) {
	parallelRule(t)
	t.Run("A worker's counters reach the collector", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenTheWorkerExportsMetricsToTheCollector(200 * time.Millisecond)
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.ThenTheCollectorReceivedTheMetricAt("vamos.worker.fenced", 0)
	})

	t.Run("A worker refuses Prometheus", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheWorkerEnvironmentSets("OTEL_METRICS_EXPORTER", "prometheus")
		w.WhenIStartAWorker()
		w.ThenItFailsWith("OTEL_METRICS_EXPORTER=prometheus", "workers export metrics over OTLP")
	})
}

// TestMetrics_TheHelmChartExposesTelemetry specifies that the chart's defaults set no OTEL_* variable and expose no
// metrics port, and that with Prometheus on it serves the coordinator's metrics on every address, behind its Service's
// port named metrics.
func TestMetrics_TheHelmChartExposesTelemetry(t *testing.T) {
	parallelRule(t)
	t.Run("The chart's defaults export nothing", func(t *testing.T) {
		w := newWorld(t)
		w.WhenIRenderTheChart()
		w.ThenTheChartSetsNoOTELVariable()
		w.ThenTheCoordinatorServiceExposesTheMetricsPort(false)
	})

	t.Run("Prometheus on in the chart", func(t *testing.T) {
		w := newWorld(t)
		w.WhenIRenderTheChart("telemetry.prometheus.enabled=true")
		w.ThenTheCoordinatorSets("OTEL_METRICS_EXPORTER", "prometheus")
		w.ThenTheCoordinatorSets("OTEL_EXPORTER_PROMETHEUS_HOST", "::")
		w.ThenTheCoordinatorServiceExposesTheMetricsPort(true)
	})
}

// TestMetrics_FlushedOnShutdown specifies that a coordinator that shuts down exports what it holds before it exits.
func TestMetrics_FlushedOnShutdown(t *testing.T) {
	parallelRule(t)
	t.Run("A coordinator flushes on interrupt", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenTheCoordinatorEnvironmentSets("OTEL_BSP_SCHEDULE_DELAY", "3600000")
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIInterruptTheCoordinator()
		w.ThenTheCollectorAlreadyReceivedTheTraceOf("build")
	})
}

// TestMetrics_TheReferenceListsEveryMetric checks that the table of metrics in docs/design/observability.md lists
// exactly the Vamos and HTTP metrics a coordinator's Prometheus scrape shows once it has run a build, each exported
// from zero, or recorded by the build, but for the peak memory of attempts, which a worker reports only where its
// cgroup has the memory controller, as this machine's may not.
func TestMetrics_TheReferenceListsEveryMetric(t *testing.T) {
	parallelRule(t)
	t.Run("The reference lists what a scrape shows", func(t *testing.T) {
		w := newMetricsWorld(t)
		w.GivenTheCoordinatorServesPrometheusMetrics()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(aTaskAndAppAddingIt(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIScrapeTheMetrics()
		w.ThenTheReferenceListsTheScrapedMetrics("../../docs/design/observability.md",
			"vamos_attempts_memory_peak_bytes")
	})
}
