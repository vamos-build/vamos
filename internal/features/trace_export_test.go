package features

import (
	"fmt"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/testenv"
)

// newTraceExportWorld starts a scenario of the trace export feature, from its background: a project that pulls
// busybox, which builds. A coordinator exports its jobs' traces over OpenTelemetry, as the standard OTEL_* variables
// configure it; the client's own spans aren't exported.
func newTraceExportWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// The outer trace of the scenarios that join one, as a CI pipeline's TRACEPARENT names it.
const (
	outerTrace   = "4bf92f3577b34da6a3ce929d0e0e4736"
	outerSpan    = "00f067aa0ba902b7"
	outerSampled = "00-" + outerTrace + "-" + outerSpan + "-01"
)

// anAppEchoing declares the layer "app" on busybox, which runs echo hello, with nonce, so that it must run.
func anAppEchoing(nonce string) string {
	return `layer(name="app", base="busybox", steps=[env(NONCE="` + nonce + `"), run(["echo", "hello"])])`
}

// echoHello is the span of anAppEchoing's step.
const echoHello = `run(["echo", "hello"])`

// anAppSleeping declares the layer "app" on busybox, which sleeps a second, with nonce, so that it must run.
func anAppSleeping(nonce string) string {
	return `layer(name="app", base="busybox", steps=[env(NONCE="` + nonce + `"), run(["sleep", "1"])])`
}

// anAppOnItsBase declares the layer "base" on busybox and the layer "app" on it, both with nonce, so that both run.
func anAppOnItsBase(nonce string) string {
	return `
		layer(name="base", base="busybox", steps=[env(NONCE="` + nonce + `"), run(["touch", "/base"])])
		layer(name="app", base="base", steps=[run(["touch", "/app"])])
	`
}

// anAppAddingASource declares the glob "src" of a.txt, and the layer "app" on busybox, which adds it, with nonce.
func anAppAddingASource(nonce string) string {
	return `
		glob(name="src", include=["a.txt"])
		layer(name="app", base="busybox", steps=[env(NONCE="` + nonce + `"), add("src", "/src")])
	`
}

// TestTraceExport_ConfiguredByTheStandardVariables specifies, as the coordinator reference says, that a coordinator
// reads its trace export configuration from the standard OTEL_* variables when it starts: OTEL_TRACES_EXPORTER chooses
// otlp, console or none, unset it is otlp when an OTLP endpoint is set and none otherwise, and OTEL_SDK_DISABLED=true
// turns off every export.
func TestTraceExport_ConfiguredByTheStandardVariables(t *testing.T) {
	parallelRule(t)
	t.Run("An OTLP endpoint alone turns on trace export", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTraceHoldsASpanNamed("build", `layer "app"`)
		w.ThenTheTraceHoldsASpanNamed("build", echoHello)
	})

	t.Run("OTEL_SDK_DISABLED turns export off", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenTheCoordinatorEnvironmentSets("OTEL_SDK_DISABLED", "true")
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCollectorReceivedNoSpan()
	})

	t.Run("An unknown exporter is a mistake", func(t *testing.T) {
		w := newCoordinatorWorld(t)
		w.GivenTheCoordinatorEnvironmentSets("OTEL_TRACES_EXPORTER", "zipkin")
		w.WhenIStartACoordinatorUntilItListens()
		w.ThenItFailsWith(`OTEL_TRACES_EXPORTER=zipkin: "zipkin" isn't an exporter; it takes otlp, console or none`)
	})
}

// TestTraceExport_NothingConfiguredExportsNothing specifies that a coordinator started with no OTEL_* variable
// exports nothing, though a collector listens at the default endpoint. It runs alone, as that endpoint is the
// machine's.
func TestTraceExport_NothingConfiguredExportsNothing(t *testing.T) {
	t.Run("A coordinator with nothing configured exports nothing", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollectorAtTheDefaultEndpoint()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCollectorReceivedNoSpan()
	})
}

// TestTraceExport_NeverChangesABuild specifies that a build ends as it would without export, whether the exporter's
// endpoint is reachable or not, and that no client is told of an export that fails.
func TestTraceExport_NeverChangesABuild(t *testing.T) {
	parallelRule(t)
	t.Run("An unreachable collector fails nothing", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenTheCoordinatorEnvironmentSets("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenItWarnsNothing()
	})
}

// TestTraceExport_EachJobIsOneTrace specifies that each job a coordinator runs is exported as one trace, under one
// span for the job, holding the coordinator's spans of the job and those of the attempts it started, its workers'
// among them; the client's own spans aren't exported.
func TestTraceExport_EachJobIsOneTrace(t *testing.T) {
	parallelRule(t)
	t.Run("A build's trace holds its target and its worker's steps", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCollectorReceivedTheTracesOf("build")
		w.ThenTheExportedSpanIsWithin(echoHello, `layer "app"`)
	})

	t.Run("A push is a build's trace and a push's", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIPush("app", "<registry>/team/app:v1", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCollectorReceivedTheTracesOf("build", "push")
	})
}

// TestTraceExport_KeepsTimingAndNesting specifies that each exported span starts and ends when the vamos span did, on
// the coordinator's clock, and has as its parent the exported span of the vamos span it was part of.
func TestTraceExport_KeepsTimingAndNesting(t *testing.T) {
	parallelRule(t)
	t.Run("A step's exported span lies within its target's", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppSleeping(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheExportedSpanLastsAtLeast(`run(["sleep", "1"])`, time.Second)
		w.ThenTheExportedSpanIsWithin(`run(["sleep", "1"])`, `layer "app"`)
	})
}

// TestTraceExport_TheTraceFileNamesTheExportedTrace specifies that the --trace file names, on each job's top-level
// spans, the trace ID the job was exported under, and whether it was sampled.
func TestTraceExport_TheTraceFileNamesTheExportedTrace(t *testing.T) {
	parallelRule(t)
	t.Run("The trace file names the exported trace", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"), traced())
		w.ThenItSucceeds()
		w.ThenTheTraceFileNamesTheExportedTrace()
	})

	t.Run("A job that wasn't exported names no trace ID", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"), traced())
		w.ThenItSucceeds()
		w.ThenTheTraceFileNamesNoExportedTrace()
	})
}

// TestTraceExport_ASharedAttemptIsExportedOnce specifies that an attempt's spans are exported once, in the trace of
// the job that started it, and that each job that joined it links its target's span to the attempt's span.
func TestTraceExport_ASharedAttemptIsExportedOnce(t *testing.T) {
	parallelRule(t)
	t.Run("A joined attempt is linked, not repeated", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenABarrier()
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aSlowTask(nonce))
		w.GivenAnotherClientBuildsOnTheCoordinator("slow", onTheCoordinator("<coordinator>"))
		w.GivenTheOtherClientIsBuildingIt()
		w.GivenTheBarrierOpensOnceTheOtherClientIsJoined()
		w.WhenIBuild("slow", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheOtherClientSucceeds()
		w.ThenTheCollectorReceivedOneSpanNamed(slowTaskSteps(nonce)[1])
		w.ThenATargetSpanLinksToAnAttemptInAnotherTrace(`task "slow"`)
	})
}

// TestTraceExport_AJoinerKeepsTheCacheWriteOfAnAttemptNotExported specifies that a sampled job that joined an
// attempt whose starter's trace isn't kept holds the attempt's cache write. It runs alone, as it sets TRACEPARENT.
func TestTraceExport_AJoinerKeepsTheCacheWriteOfAnAttemptNotExported(t *testing.T) {
	t.Run("A joiner of an attempt whose starter's trace isn't kept keeps its cache write", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenABarrier()
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aSlowTask(nonce))
		w.GivenTRACEPARENT("00-" + outerTrace + "-" + outerSpan + "-00") // which the other client sends
		w.GivenAnotherClientBuildsOnTheCoordinator("slow", onTheCoordinator("<coordinator>"))
		w.GivenTheOtherClientIsBuildingIt()
		w.GivenTRACEPARENT("") // unset, for the build that joins
		w.GivenTheBarrierOpensOnceTheOtherClientIsJoined()
		w.WhenIBuild("slow", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheOtherClientSucceeds()
		w.ThenTheTraceHoldsASpanNamed("build", "cache write")
		w.ThenTheJobSpanIsTheRootOfItsOwnTrace("build", outerTrace)
	})
}

// TestTraceExport_ArrowsBecomeSpanLinks specifies that where a stored trace draws an arrow from the span that made
// something to a span that used it, the exported span that used it links to the exported span that made it.
func TestTraceExport_ArrowsBecomeSpanLinks(t *testing.T) {
	parallelRule(t)
	t.Run("A layer's start links to the commit of its base", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppOnItsBase(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheSpanWithinLinksToTheSpanWithin("start", `layer "app"`, "commit", `layer "base"`)
	})

	t.Run("An arrow from the client has no link", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppAddingASource(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"a.txt": "a"})
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheExportedSpanLinksToNothing(`add("src", "/src")`)
	})
}

// TestTraceExport_SamplingKeepsOrDropsATraceWhole specifies that a coordinator samples as OTEL_TRACES_SAMPLER and
// OTEL_TRACES_SAMPLER_ARG say, deciding once for each job, and keeping or dropping every span of its trace.
func TestTraceExport_SamplingKeepsOrDropsATraceWhole(t *testing.T) {
	parallelRule(t)
	t.Run("A ratio of zero drops every trace", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenTheCoordinatorEnvironmentSets("OTEL_TRACES_SAMPLER", "traceidratio")
		w.GivenTheCoordinatorEnvironmentSets("OTEL_TRACES_SAMPLER_ARG", "0")
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCollectorReceivedNoSpan()
	})

	t.Run("A sampled-out trace is dropped whole", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenTheCoordinatorEnvironmentSets("OTEL_TRACES_SAMPLER", "traceidratio")
		w.GivenTheCoordinatorEnvironmentSets("OTEL_TRACES_SAMPLER_ARG", "0.5")
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		nonce := testenv.NewNonce()
		for i := range 20 {
			w.GivenTheBuildFileDeclares(fmt.Sprintf(
				`layer(name="app%d", base="busybox", steps=[env(NONCE="%s"), run(["echo", "%d"])])`, i, nonce, i))
		}
		for i := range 20 { // one build each, as each is a job, whose trace is sampled alone
			w.WhenIBuild(fmt.Sprintf("app%d", i), onTheCoordinator("<coordinator>"))
			w.ThenItSucceeds()
		}
		w.ThenEachTraceIsWholeOrAbsent(20, `layer "app*`, `run(["echo", *`)
	})

	t.Run("An unknown sampler is a mistake", func(t *testing.T) {
		w := newCoordinatorWorld(t)
		w.GivenTheCoordinatorEnvironmentSets("OTEL_TRACES_SAMPLER", "sometimes")
		w.WhenIStartACoordinatorUntilItListens()
		w.ThenItFailsWith("OTEL_TRACES_SAMPLER=sometimes: not a sampler; it takes always_on, always_off, " +
			"traceidratio, parentbased_always_on, parentbased_always_off or parentbased_traceidratio")
	})
}

// TestTraceExport_ABuildJoinsAnOuterTrace specifies, as the CLI reference says, that a client building on a
// coordinator with a W3C traceparent in TRACEPARENT sends it with each job of the command, whose span is then a child
// of that span, in its trace, sampled as its flags say. It runs alone, as it sets TRACEPARENT.
func TestTraceExport_ABuildJoinsAnOuterTrace(t *testing.T) {
	t.Run("A build in CI is part of the pipeline's trace", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTRACEPARENT(outerSampled)
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheJobSpanIsUnder("build", outerTrace, outerSpan)
	})

	t.Run("Each job of a command joins the outer trace", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTRACEPARENT(outerSampled)
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIPush("app", "<registry>/team/app:v1", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheJobSpanIsUnder("build", outerTrace, outerSpan)
		w.ThenTheJobSpanIsUnder("push", outerTrace, outerSpan)
	})

	t.Run("An outer trace that isn't sampled isn't exported", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTRACEPARENT("00-" + outerTrace + "-" + outerSpan + "-00")
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"), traced())
		w.ThenItSucceeds()
		w.ThenTheCollectorReceivedNoSpan()
		w.ThenTheTraceFileNamesTrace(outerTrace, false)
	})

	t.Run("TRACEPARENT without a coordinator is unused", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenTRACEPARENT(outerSampled)
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenItWarnsNothing()
	})
}

// TestTraceExport_AMalformedTraceparentIsIgnored specifies that a client building on a coordinator whose TRACEPARENT
// isn't a W3C traceparent warns once, naming the variable and its value, and builds as if it were unset, and that a
// TRACESTATE that isn't a W3C tracestate is warned about likewise and dropped. It runs alone, as it sets TRACEPARENT.
func TestTraceExport_AMalformedTraceparentIsIgnored(t *testing.T) {
	t.Run("A malformed TRACEPARENT starts a trace of the build's own", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTRACEPARENT("00-not-a-trace")
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCommandSays("vamos: warning: " +
			`TRACEPARENT "00-not-a-trace" is not a W3C traceparent and is ignored`)
		w.ThenTheJobSpanIsTheRootOfItsOwnTrace("build", outerTrace)
	})

	t.Run("A malformed TRACESTATE is dropped", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTRACEPARENT(outerSampled)
		w.GivenTheEnvironmentSets("TRACESTATE", "not a state")
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCommandSays("vamos: warning: " + `TRACESTATE "not a state" is not a W3C tracestate and is ignored`)
		w.ThenTheJobSpanIsUnder("build", outerTrace, outerSpan)
		w.ThenTheJobSpanHasTraceState("build", "")
	})

	t.Run("An all-zero trace ID is malformed", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTRACEPARENT("00-00000000000000000000000000000000-" + outerSpan + "-01")
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCommandSays("vamos: warning: " + `TRACEPARENT "00-00000000000000000000000000000000-` + outerSpan +
			`-01" is not a W3C traceparent and is ignored`)
	})
}

// TestTraceExport_SpansSayWhatWasBuiltAndWhy specifies that exported spans carry vamos.job on the job's span,
// vamos.workspace on every span of a named workspace's job, vamos.target on a target's, and, on a target that missed,
// vamos.miss.cause and, for an own-inputs miss, the files added, removed and changed.
func TestTraceExport_SpansSayWhatWasBuiltAndWhy(t *testing.T) {
	parallelRule(t)
	t.Run("A miss's cause and files are exported", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenStoresOfTheScenariosOwn()
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheProjectHolds(files{"WORKSPACE": `workspace(name = "com.acme.storefront")`, "a.txt": "one"})
		w.GivenTheBuildFileDeclares(anAppAddingASource(testenv.NewNonce()))
		w.GivenTheCoordinatorHasBuilt("app")
		w.GivenTheCollectorForgetsTheLastCommandsSpans()
		w.GivenTheProjectHolds(files{"a.txt": "two"})
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheExportedSpanSaysAtLeast(`layer "app"`, map[string]string{"vamos.workspace": "com.acme.storefront",
			"vamos.target": "//:app platform=" + thisPlatform(), "vamos.miss.cause": "own-inputs",
			"vamos.miss.changed": "a.txt"})
	})

	t.Run("An unnamed workspace's spans carry no workspace", func(t *testing.T) {
		w := newTraceExportWorld(t)
		w.GivenAnOTLPCollector()
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(anAppEchoing(testenv.NewNonce()))
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTraceHoldsASpanNamed("build", `layer "app"`)
		w.ThenNoExportedSpanHas("vamos.workspace")
	})
}
