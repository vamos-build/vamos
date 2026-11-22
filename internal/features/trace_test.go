package features

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/testenv"
)

// newTraceWorld starts a scenario of the trace feature, from its background: a project, which builds.
// --trace writes a trace of the command, which Perfetto shows: BUILD loading and glob capture, each pull and target,
// execution steps, cache activity and image delivery.
func newTraceWorld(t *testing.T) *world {
	t.Helper()
	return newBuildingWorld(t)
}

// slowTaskSteps returns the names of the steps of aSlowTask(nonce)'s task: setting the nonce, then waiting at the
// scenario's barrier, which GivenABarrier starts, until a step opens it, as once another build has joined the task.
func slowTaskSteps(nonce string) []string {
	return []string{`env(NONCE="` + nonce + `")`, `run(["wget", "-q", "-O", "/dev/null", "<barrier>"])`}
}

// aSlowTask declares the task "slow", on busybox, whose steps are slowTaskSteps(nonce).
func aSlowTask(nonce string) string {
	return `task(name="slow", base="busybox", steps=[` + strings.Join(slowTaskSteps(nonce), ", ") + `])`
}

// aTaskAndALayer declares the task "gen", which joins the files of the glob "src", and the layer "app", which adds
// the task's file and prints it, both setting NONCE to nonce, so that they are built rather than reused.
func aTaskAndALayer(nonce string) string {
	return `
		glob(name="src", include=["src"])
		task(
		    name="gen",
		    base="busybox",
		    steps=[
		        env(NONCE="` + nonce + `"),
		        add("src", "/in"),
		        run(["sh", "-c", "mkdir /out && cat /in/src/* > /out/all.txt"]),
		    ],
		    source="/out",
		    include=["all.txt"],
		)
		layer(
		    name="app",
		    base="busybox",
		    steps=[add("gen", "/data"), env(NONCE="` + nonce + `"), workdir("/data"), run(["cat", "all.txt"])],
		    command=["cat", "/data/all.txt"],
		)
	`
}

// theSources are the files that the glob "src" of aTaskAndALayer selects: two, of five bytes.
var theSources = files{"src/a.txt": "a\n", "src/b.txt": "bb\n"}

// TestTrace_ShowsTheBuild specifies that the trace shows BUILD loading and glob capture, each pull and target, each
// on its own track, with its execution steps, as the BUILD file wrote them, and cache activity; that arrows connect
// the results of globs, pulls and targets to the steps that consume them; and that spans include details such as
// file counts, byte sizes, digests and image IDs.
func TestTrace_ShowsTheBuild(t *testing.T) {
	parallelRule(t)
	t.Run("A save's trace shows its load, its globs, and each target's steps, with arrows to what they use",
		func(t *testing.T) {
			w := newTraceWorld(t)
			w.GivenAProjectThatPullsBusybox()
			w.GivenTheBuildFileDeclares(aTaskAndALayer(testenv.NewNonce()))
			w.GivenTheProjectHolds(theSources)
			w.WhenIBuild("busybox") // so that the traced build finds it pulled, even when the scenario runs alone
			w.ThenItSucceeds()
			w.WhenIShowTheGraph("app")
			w.ThenItSucceeds()
			w.WhenISave("app", traced())
			w.ThenItSucceeds()
			w.ThenTheTraceSpanHolds("vamos", "vamos save app *", "load", "store graph", "run", "writing *")
			w.ThenTheTraceSpanHolds("vamos", "load", `glob "src"`)
			w.ThenTheTraceSpanSaysAtLeast("vamos", `glob "src"`, traceArgs{"files": 2, "bytes": 5})
			w.ThenTheTraceGlobHasTheTreeTheGraphShows("src")
			w.ThenTheTraceSpanIsWithin("executor", "run", "vamos", "run")
			w.ThenTheTraceSpanIsOnTrack("executor", `pull "busybox"`, `pull "busybox"`)
			w.ThenTheTraceSpanSaysAtLeast("executor", `pull "busybox"`, traceArgs{"cached": true})
			w.ThenTheTraceCopiesNoBlobWithin("executor", `pull "busybox"`)
			w.ThenTheTraceSpanHolds("executor", `task "gen"`, "cache lookup", "start", `env(NONCE="*`,
				`add("src", "/in")`, `run(["sh", "-c", "mkdir /out && cat /in/src/* > /out/all.txt"])`,
				"collect output", "cache write")
			w.ThenTheTraceSpanSaysAtLeast("executor", "collect output", traceArgs{"files": 1, "bytes": 5})
			w.ThenTheTraceSpanIsOnTrack("executor", `layer "app"`, `layer "app"`)
			w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
				traceArgs{"status": "building", "image ID": "<image>"})
			w.ThenTheTraceSpanHolds("executor", `layer "app"`, "cache lookup", "start", `add("gen", "/data")`,
				`env(NONCE="*`, `workdir("/data")`, `run(["cat", "all.txt"])`, "commit", "cache write")
			w.ThenTheTraceSpanHolds("executor", `add("gen", "/data")`, "checkout", "copy")
			w.ThenTheTraceSpanSaysAtLeast("executor", `add("gen", "/data")`, traceArgs{"files": 1, "dest": "/data"})
			w.ThenTheTraceSpanSaysAtLeast("executor", `run(["cat", "all.txt"])`, traceArgs{"stdout bytes": 5})
			w.ThenTheTraceSpanSaysAtLeast("executor", "commit", traceArgs{"image ID": "<image>"})
			w.ThenTheTraceArrowsAre(
				`glob "src" -> add("src", "/in")`,
				`pull "busybox" -> start`, // the task's
				`pull "busybox" -> start`, // the layer's
				`collect output -> add("gen", "/data")`,
			)
		})

	t.Run("A build's trace holds its load, the graph it stores and its run, and copies nothing already pulled",
		func(t *testing.T) {
			w := newTraceWorld(t)
			w.GivenAProjectThatPullsBusybox()
			w.GivenTheBuildFileDeclares(aTaskAndALayer(testenv.NewNonce()))
			w.GivenTheProjectHolds(theSources)
			w.WhenIBuild("busybox")
			w.ThenItSucceeds()
			w.WhenIBuild("app", traced())
			w.ThenItSucceeds()
			w.ThenTheTraceSpanHolds("vamos", "vamos build app", "load", "store graph", "run")
			w.ThenTheTraceHasNoSpan("executor", "blob *")
		})

	t.Run("A build again shows each target reused, with no arrows, since nothing used what they made",
		func(t *testing.T) {
			w := newTraceWorld(t)
			w.GivenAProjectThatPullsBusybox()
			w.GivenTheBuildFileDeclares(aTaskAndALayer(testenv.NewNonce()))
			w.GivenTheProjectHolds(theSources)
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.WhenISave("app", traced())
			w.ThenItSucceeds()
			w.ThenTheImageIsTheOneSavedFirst()
			w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`,
				traceArgs{"status": "cached", "image ID": "<image>"})
			w.ThenTheTraceSpanHolds("executor", `layer "app"`, "cache lookup")
			w.ThenTheTraceArrowsAre()
		})
}

// TestTrace_ShowsDelivery specifies that the trace shows image delivery: a push's copy to the registry, from the
// executor or from the client, with a span for each blob, which a push again skips, as the registry has it; a save's
// copy to its archive; and an export, saying the files it wrote.
func TestTrace_ShowsDelivery(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name    string
		options []option
		job     string // the executor's job within the client's span for sending the image
		track   string // the job's
		copier  string // the process that copies to the registry
	}{
		{
			name:   "A push from the executor shows its copy to the registry, a blob at a time",
			job:    "push to *",
			track:  "push",
			copier: "executor",
		},
		{
			name:    "A push from the client shows the client's copy to the registry, a blob at a time",
			options: []option{fromTheClient(), withoutTLSVerification()},
			job:     "export to *",
			track:   "export",
			copier:  "vamos",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTraceWorld(t)
			w.GivenAProjectThatPullsBusybox()
			w.GivenARegistry()
			w.GivenTheBuildFileDeclares(aTaskAndALayer(testenv.NewNonce()))
			w.GivenTheProjectHolds(theSources)
			w.WhenIPush("app", "<registry>/team/app:v1", append(example.options, traced())...)
			w.ThenItSucceeds()
			w.ThenTheTraceSpanIsWithin("executor", example.job, "vamos", "pushing to <registry>/team/app:v1")
			w.ThenTheTraceSpanIsOnTrack("executor", example.job, example.track)
			w.ThenTheTraceSpanIsWithin(example.copier, "copy to docker://*", "vamos",
				"pushing to <registry>/team/app:v1")
			w.ThenTheTraceCopiesTheImagePushed(example.copier)
			w.WhenIPush("app", "<registry>/team/app:v1", append(example.options, traced())...)
			w.ThenItSucceeds()
			w.ThenTheTraceSkipsTheLayersPushed(example.copier)
		})
	}

	t.Run("A save shows the executor's export within the client's writing of the archive", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(aTaskAndALayer(testenv.NewNonce()))
		w.GivenTheProjectHolds(theSources)
		w.WhenISave("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanIsWithin("executor", "export to the content store", "vamos", "writing <destination>/app.tar")
		w.ThenTheTraceSpanIsWithin("vamos", "copy to docker-archive:*", "vamos", "writing <destination>/app.tar")
	})

	t.Run("An export shows the files it wrote", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(aTaskAndALayer(testenv.NewNonce()))
		w.GivenTheProjectHolds(theSources)
		w.WhenIExport("gen", traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("vamos", "exporting to <destination>/gen", traceArgs{"files": "sha256:*"})
		w.ThenTheExportHolds(files{"all.txt": "a\nbb\n"})
	})

	t.Run("Perfetto reads a push's trace as vamos means it", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenARegistry()
		w.GivenTheBuildFileDeclares(aTaskAndALayer(testenv.NewNonce()))
		w.GivenTheProjectHolds(theSources)
		w.WhenIPush("app", "<registry>/team/app:v1", traced())
		w.ThenItSucceeds()
		w.ThenPerfettoReadsTheTraceAsVamosMeansIt()
	})
}

// aPublicationOfTwoImages declares what aTaskAndALayer does, with nonce, the layer "worker" on scratch, which adds the
// file of the glob "src", and the publication "release", which publishes app to <registry>/team/app and worker to
// <registry>/team/worker.
func aPublicationOfTwoImages(nonce string) string {
	return aTaskAndALayer(nonce) + `
		layer(name="worker", base="scratch", steps=[add("src", "/src")])
		publication(name="release", images={"<registry>/team/app": "app", "<registry>/team/worker": "worker"})
	`
}

// aPublicationOnScratch declares the layer "app" on scratch, which adds the file of the glob "src", holding the
// scenario's nonce, so that a coordinator's worker builds it with no base to pull, and the publication "release",
// which publishes it to <registry>/team/app.
const aPublicationOnScratch = `
	glob(name="src", include=["src"])
	layer(name="app", base="scratch", steps=[add("src", "/src")])
	publication(name="release", images={"<registry>/team/app": "app"})
`

// TestTrace_ShowsPublishing specifies that the trace of a publication shows one publish job of the executor within
// the client's span of publishing, and within it a span for each upload and each tag written, every upload ending
// before the first tag starts, and each copy in the process that made it: the executor, the client given
// --push-from=client, or a coordinator's worker, never the coordinator itself.
func TestTrace_ShowsPublishing(t *testing.T) {
	parallelRule(t)
	t.Run("Uploads, then tags, within one publish job", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenARegistry()
		w.GivenTheBuildFileDeclares(aPublicationOfTwoImages(testenv.NewNonce()))
		w.GivenTheProjectHolds(theSources)
		w.WhenIPublish("release", taggedAs("v1", "latest"), traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSpanIsWithin("executor", "publish", "vamos", "publish")
		w.ThenTheTraceSpanIsOnTrack("executor", "publish", "publish")
		w.ThenTheTraceSpanHolds("executor", "publish",
			"upload to <registry>/team/app", "upload to <registry>/team/worker",
			"tag <registry>/team/app:v1", "tag <registry>/team/app:latest",
			"tag <registry>/team/worker:v1", "tag <registry>/team/worker:latest")
		w.ThenEverySpanEndsBeforeAnyStarts("executor", "upload to *", "tag *")
	})

	for _, example := range []struct {
		name       string
		options    []option
		copier     string // the process that uploads and tags
		notCopying string // a process that copies nothing to the registry
	}{
		{
			name:       "A publication from the executor shows its uploads and tags in the executor's process",
			copier:     "executor",
			notCopying: "vamos",
		},
		{
			name:       "A publication from the client shows its uploads and tags in the client's process",
			options:    []option{fromTheClient(), withoutTLSVerification()},
			copier:     "vamos",
			notCopying: "executor",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTraceWorld(t)
			w.GivenAProjectThatPullsBusybox()
			w.GivenARegistry()
			w.GivenTheBuildFileDeclares(aPublicationOnScratch)
			w.GivenTheProjectHolds(files{"src": testenv.NewNonce()})
			w.WhenIPublish("release", append([]option{taggedAs("v1"), traced()}, example.options...)...)
			w.ThenItSucceeds()
			w.ThenTheTraceSpanIsWithin(example.copier, "upload to <registry>/team/app", "vamos", "publish")
			w.ThenTheTraceSpanIsWithin(example.copier, "copy to docker://<registry>/team/app*", "vamos", "publish")
			w.ThenTheTraceSpanIsWithin(example.copier, "tag <registry>/team/app:v1", "vamos", "publish")
			w.ThenTheTraceHasNoSpan(example.notCopying, "upload to *")
			w.ThenTheTraceHasNoSpan(example.notCopying, "tag *")
		})
	}

	t.Run("A publication on a coordinator shows its uploads and tags in the worker's process, none in its own",
		func(t *testing.T) {
			w := newTraceWorld(t)
			w.GivenAProjectThatPullsBusybox()
			w.GivenARegistry()
			w.GivenACoordinatorWithAWorker()
			w.GivenTheBuildFileDeclares(aPublicationOnScratch)
			w.GivenTheProjectHolds(files{"src": testenv.NewNonce()})
			w.WhenIPublish("release", taggedAs("v1"), onTheCoordinator("<coordinator>"), traced())
			w.ThenItSucceeds()
			w.ThenTheTraceSpanIsWithin("worker", "upload to <registry>/team/app", "vamos", "publish")
			w.ThenTheTraceSpanIsWithin("worker", "copy to docker://<registry>/team/app*", "vamos", "publish")
			w.ThenTheTraceSpanIsWithin("worker", "tag <registry>/team/app:v1", "vamos", "publish")
			w.ThenTheTraceHasNoSpan("executor", "upload to *")
			w.ThenTheTraceHasNoSpan("executor", "tag *")
			w.ThenTheTraceHasNoSpan("executor", "copy to docker://*")
		})

	t.Run("Publishing again skips what is uploaded", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenARegistry()
		w.GivenTheBuildFileDeclares(aPublicationOnScratch)
		w.GivenTheProjectHolds(files{"src": testenv.NewNonce()})
		w.WhenIPublish("release", taggedAs("v1"))
		w.ThenItSucceeds()
		w.WhenIPublish("release", taggedAs("v2"), traced())
		w.ThenItSucceeds()
		w.ThenTheTraceSkipsTheLayersPushed("executor")
	})
}

// TestTrace_ShowsPulls specifies that the trace of a pull of an image that the store lacks shows resolving its tag,
// and copying the image, a blob at a time.
func TestTrace_ShowsPulls(t *testing.T) {
	parallelRule(t)
	t.Run("A pull resolves its tag and copies the image it names", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "base", thisPlatform()) // new, so not pulled before
		w.GivenTheBuildFileDeclares(`pull(name="base", image="<registry>/base", tag="v1")`)
		w.WhenIBuild("base", traced())
		w.ThenItSucceeds()
		w.ThenTheTracePullsTheImage(`pull "base"`, "base", thisPlatform())
	})
}

// TestTrace_IsWrittenEvenIfTheCommandFails specifies that once tracing starts, vamos writes the trace even if
// execution fails, ending at the failing step, with its error, and reports the command's error.
func TestTrace_IsWrittenEvenIfTheCommandFails(t *testing.T) {
	parallelRule(t)
	t.Run("A failing build's trace ends at the step that failed, with its error", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", steps=[run(["sh", "-c", "exit 3"]), env(NONCE="` +
			testenv.NewNonce() + `")])
		`)
		w.WhenIBuild("app", traced())
		w.ThenItFailsWith(`layer "app": run(["sh", "-c", "exit 3"]) failed`)
		w.ThenTheTraceSpanHolds("executor", `layer "app"`, "cache lookup", "start", `run(["sh", "-c", "exit 3"])`)
		w.ThenTheTraceSpanSaysAtLeast("executor", `run(["sh", "-c", "exit 3"])`, traceArgs{"error": "*exit status 3*"})
		w.ThenTheTraceSpanSaysAtLeast("vamos", "vamos build app", traceArgs{"error": "*failed: exit status 3*"})
	})
}

// newTraceMistakeWorld starts a scenario of the trace feature whose trace's path is a mistake, which is reported
// before anything is loaded or built, so that it needs no buildah: its BUILD file doesn't parse, so that an error
// about the trace shows it was checked first.
func newTraceMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenTheProjectHolds(files{"BUILD": "nope("})
	w.GivenAFileOutsideTheProjectHolds("kept.txt", "kept")
	return w
}

// TestTrace_PathCanBeWrittenAndIsNotTheOutput specifies that the trace's path must be a file that can be written,
// different from a save archive and outside an export destination folder, through any link, which is checked before
// anything is loaded or built, writing nothing.
func TestTrace_PathCanBeWrittenAndIsNotTheOutput(t *testing.T) {
	parallelRule(t)
	for _, example := range []struct {
		name  string
		trace string
		error string
	}{
		{
			name:  "A trace in a folder that isn't there fails",
			trace: "<outside>/missing/t.json",
			error: "--trace <outside>/missing/t.json: cannot write <outside>/missing/t.json: " +
				"directory <outside>/missing does not exist",
		},
		{
			name:  "A trace at a folder fails",
			trace: "<outside>",
			error: "--trace <outside>: cannot write <outside>: it is a directory",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTraceMistakeWorld(t)
			w.WhenIBuild("app", tracedTo(example.trace))
			w.ThenItFailsWith(example.error)
			w.ThenNothingElseIsOutsideTheProject()
		})
	}

	for _, example := range []struct {
		name  string
		links links // in the folder that save writes to
		trace string
		error string
	}{
		{
			name:  "A trace at save's archive fails",
			trace: "<destination>/app.tar",
			error: "--trace <destination>/app.tar: it is also the file that save writes",
		},
		{
			name:  "A trace at save's archive, through a link, fails",
			links: links{"self": "."},
			trace: "<destination>/self/app.tar",
			error: "--trace <destination>/self/app.tar: it is also the file that save writes",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTraceMistakeWorld(t)
			w.GivenTheDestinationLinks(example.links)
			w.WhenISave("app", tracedTo(example.trace))
			w.ThenItFailsWith(example.error)
			w.ThenTheDestinationPathsAre(slices.Collect(maps.Keys(example.links))...)
			w.ThenNothingElseIsOutsideTheProject()
		})
	}

	for _, example := range []struct {
		name    string
		links   links // in the folder that export writes into
		options []option
		trace   string
		paths   []string // in that folder, after
	}{
		{
			name:  "A trace at export's folder fails",
			trace: "<destination>/gen",
		},
		{
			name:  "A trace within export's folder fails",
			trace: "<destination>/gen/t.json",
		},
		{
			name:    "A trace within export's folder, through a link out of it, fails",
			links:   links{"gen/sub": "<outside>"},
			options: []option{replacing()},
			trace:   "<destination>/gen/sub/t.json",
			paths:   []string{"gen", "gen/sub"},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTraceMistakeWorld(t)
			w.GivenTheDestinationLinks(example.links)
			w.WhenIExport("gen", append(example.options, tracedTo(example.trace))...)
			w.ThenItFailsWith("--trace " + example.trace + ": it is within the folder that export replaces")
			w.ThenTheDestinationPathsAre(example.paths...)
			w.ThenTheDestinationLinksAtLeast(example.links)
			w.ThenNothingElseIsOutsideTheProject()
		})
	}
}

// aLayerThatRuns declares the layer "app", on busybox, whose step runs, given nonce, and takes two seconds, so that
// a later build that reuses it has time to save.
func aLayerThatRuns(nonce string) string {
	return `
		layer(name="app", base="busybox", steps=[env(NONCE="` + nonce + `"), run(["sh", "-c", "sleep 2; echo made"])])
	`
}

// TestTrace_EveryCommandThatBuildsIsTraced specifies, as the cache reference says, that "every command that builds is
// traced, whether or not you ask", and that "a command's trace holds all of its work: this process's, and each job's
// on its executor".
func TestTrace_EveryCommandThatBuildsIsTraced(t *testing.T) {
	parallelRule(t)
	t.Run("A build without --trace has a trace", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(aLayerThatRuns(testenv.NewNonce()))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenItPrintsItsTrace()
		w.WhenIReadTheTrace("<printed trace>")
		w.ThenItSucceeds()
		w.ThenTheTraceSpanIsWithin("executor", `run(["sh", "-c", "sleep 2; echo made"])`, "executor", `layer "app"`)
		w.ThenTheTraceSpanIsWithin("executor", `layer "app"`, "vamos", "vamos build app")
	})

	t.Run("A push's trace holds its build and its push", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenARegistry()
		w.GivenTheBuildFileDeclares(aLayerThatRuns(testenv.NewNonce()))
		w.WhenIPush("app", "<registry>/team/app:v1")
		w.ThenItSucceeds()
		w.ThenItPrintsItsTrace()
		w.WhenIReadTheTrace("<printed trace>")
		w.ThenItSucceeds()
		w.ThenTheTraceSpanHolds("vamos", "vamos push app *", "load", "store graph", "run", "pushing to *")
		w.ThenTheTraceSpanIsWithin("executor", `run(["sh", "-c", "sleep 2; echo made"])`, "executor", `layer "app"`)
		w.ThenTheTraceSpanIsWithin("executor", "push to *", "vamos", "pushing to <registry>/team/app:v1")
	})
}

// TestTrace_ACommandPrintsItsTrace specifies, as the CLI's output reference says, that "a command that builds prints
// its trace's digest on standard error once its jobs end, whether it succeeded or failed".
func TestTrace_ACommandPrintsItsTrace(t *testing.T) {
	parallelRule(t)
	t.Run("A failed build prints its trace", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(`
			task(name="check", base="busybox", steps=[run(["sh", "-c", "exit 3"]), env(NONCE="` +
			testenv.NewNonce() + `")])
		`)
		w.WhenIBuild("check")
		w.ThenItFailsWith(`task "check": run(["sh", "-c", "exit 3"]) failed`)
		w.ThenItPrintsItsTrace()
		w.WhenIReadTheTrace("<printed trace>")
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `run(["sh", "-c", "exit 3"])`, traceArgs{"error": "*exit status 3*"})
	})

	t.Run("A command that runs no job prints no trace", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheProjectHolds(files{"BUILD": "layer(name=\"app\", base=\"scratch\")\nnope("})
		w.WhenIBuild("app")
		w.ThenItFailsWith("/project/BUILD:2:6: got end of file, want ')'")
		w.ThenItPrintsNoTrace()
	})

	t.Run("--trace writes a command that ran no job", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheProjectHolds(files{"BUILD": "layer(name=\"app\", base=\"scratch\")\nnope("})
		w.WhenIBuild("app", traced())
		w.ThenItFailsWith("/project/BUILD:2:6: got end of file, want ')'")
		w.ThenItPrintsNoTrace()
		w.ThenTheTraceSpanHolds("vamos", "vamos build app", "load")
		w.ThenTheTraceSpanSaysAtLeast("vamos", "load", traceArgs{"error": "*got end of file*"})
	})

	t.Run("A trace that can't be stored is warned about", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenACoordinatorThatRefusesToStoreCommandTraces()
		w.GivenTheBuildFileDeclares(`layer(name="app", base="scratch", steps=[env(NONCE="` + testenv.NewNonce() +
			`")])`)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"), traced())
		w.ThenItSucceeds()
		w.ThenItPrintsNoTrace()
		w.ThenTheErrorOutputIncludes("vamos: warning: the command's trace couldn't be stored: ")
		// --trace writes the trace all the same, the executor's spans included.
		w.ThenTheTraceSpanIsWithin("executor", `env(NONCE="*`, "executor", `layer "app"`)
	})

	t.Run("An interrupted build prints its trace, which holds what ran", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenABarrier()
		w.GivenTheBarrierOpensOnceTheCommandIsInterrupted()
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", steps=[env(NONCE="` + testenv.NewNonce() + `"),
			    run(["sh", "-c", "echo started; wget -q -O /dev/null <barrier>"])])
		`)
		w.WhenIBuild("app", interruptedOnceItPrints("started"), traced())
		w.ThenItIsInterrupted()
		w.ThenTheBarrierOpened()
		w.ThenItPrintsItsTrace()
		w.ThenTheTraceSpanIsWithin("executor", `run(["sh", "-c", "echo started; wget *`, "executor",
			`layer "app"`)
		w.WhenIReadTheTrace("<printed trace>")
		w.ThenItSucceeds()
		w.ThenTheTraceSpanIsWithin("executor", `run(["sh", "-c", "echo started; wget *`, "executor",
			`layer "app"`)
	})
}

// TestTrace_VamosTraceWritesAStoredTrace specifies, as the CLI reference of vamos trace says, that it "writes a stored
// trace to file.json": "a digest reads the trace from the executor's build cache until a prune removes it", and "a job
// ID reads that job's trace from the coordinator that ran it", failing, writing nothing, for what it can't read.
func TestTrace_VamosTraceWritesAStoredTrace(t *testing.T) {
	parallelRule(t)
	t.Run("--trace and vamos trace write the same trace", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(aLayerThatRuns(testenv.NewNonce()))
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		written := w.theTrace().String()
		w.ThenItPrintsItsTrace()
		w.WhenIReadTheTrace("<printed trace>")
		w.ThenItSucceeds()
		if got := w.theTrace().String(); got != written {
			w.t.Errorf("vamos trace wrote\n%s\nwhere --trace wrote\n%s", got, written)
		}
	})

	t.Run("A coordinator's job's trace is read by its job ID", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenACoordinator()
		w.GivenTheBuildFileDeclares(`layer(name="app", base="scratch", steps=[env(NONCE="` + testenv.NewNonce() +
			`")])`)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.WhenIReadTheTrace("<job>", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTraceSpanHolds("executor", `layer "app"`, "cache lookup", "start", `env(NONCE="*`, "commit",
			"cache write")
	})

	t.Run("A job the coordinator no longer remembers", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenACoordinator()
		w.GivenTheBuildFileDeclares(`layer(name="app", base="scratch", steps=[env(NONCE="` + testenv.NewNonce() +
			`")])`)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenItPrintsItsTrace()
		w.GivenTheCoordinatorRestartsWithoutItsState()
		w.WhenIReadTheTrace("<job>", onTheCoordinator("<coordinator>"))
		w.ThenItFailsWith("the coordinator doesn't know the job <job> any more; " +
			"the trace digest that its command printed still reads its trace")
		w.ThenItWritesNoTrace()
		w.WhenIReadTheTrace("<printed trace>", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTraceSpanHolds("executor", `layer "app"`, "cache lookup", "start", `env(NONCE="*`, "commit",
			"cache write")
	})

	for _, example := range []struct {
		name, trace, error string
	}{
		{name: "A job ID without a coordinator is a mistake", trace: "0123456789abcdef",
			error: `"0123456789abcdef" is a job, which is read from the coordinator that holds it: give ` +
				`--coordinator, or the trace's digest, which the command printed`},
		{name: "A digest cut short is a mistake", trace: "sha256:abc123",
			error: `"sha256:abc123" is not a valid digest: invalid checksum digest length`},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newWorld(t)
			w.WhenIReadTheTrace(example.trace)
			w.ThenItFailsWith(example.error)
		})
	}

	t.Run("A digest that names no trace is a mistake", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(aLayerThatRuns(testenv.NewNonce()))
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		log := w.traceName(`<log of layer "app">`)
		w.WhenIReadTheTrace(log)
		w.ThenItFailsWith(log + " is not a trace this vamos can read: ")
	})

	t.Run("A pruned trace is gone", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenStoresOfTheScenariosOwn()
		w.GivenTheBuildFileDeclares(`layer(name="app", base="scratch", steps=[env(NONCE="` + testenv.NewNonce() +
			`")])`)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenItPrintsItsTrace()
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.WhenIReadTheTrace("<printed trace>")
		w.ThenItFailsWith(w.traceName("<printed trace>") + ": the executor no longer holds this trace; a prune of " +
			"the build cache removes traces")
	})
}

// TestTrace_AnAttemptsTraceIsKeptWithItsResult specifies, as the cache reference says, that "a build that waits for
// another build's run of the same target shows that run's steps, on a track named for the target with (shared)", and
// that a reused target's span says "when the result was made, how long making it took, and the time reusing it
// saved".
func TestTrace_AnAttemptsTraceIsKeptWithItsResult(t *testing.T) {
	parallelRule(t)
	t.Run("A build that joins another's run shows its steps on the target's shared track", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenAProjectThatPullsBusybox()
		w.GivenABarrier()
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aSlowTask(nonce))
		w.GivenAnotherClientBuildsOnTheCoordinator("slow", onTheCoordinator("<coordinator>"))
		w.GivenTheOtherClientIsBuildingIt()
		w.GivenTheBarrierOpensOnceTheOtherClientIsJoined()
		w.WhenIBuild("slow", onTheCoordinator("<coordinator>"), traced())
		w.ThenItSucceeds()
		w.ThenTheBarrierOpened()
		w.ThenTheTraceSpanIsOnTrack("worker", slowTaskSteps(nonce)[1], `task "slow" (shared)`)
	})

	t.Run("Two builds that share an attempt both show its steps", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenAProjectThatPullsBusybox()
		w.GivenABarrier()
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aSlowTask(nonce))
		steps := slowTaskSteps(nonce)
		w.GivenAnotherClientBuildsOnTheCoordinator("slow", onTheCoordinator("<coordinator>"))
		w.GivenTheOtherClientIsBuildingIt()
		w.GivenTheBarrierOpensOnceTheOtherClientIsJoined()
		w.WhenIBuild("slow", onTheCoordinator("<coordinator>"), traced())
		w.ThenItSucceeds()
		w.ThenTheBarrierOpened()
		for _, step := range steps {
			w.ThenTheTraceSpanIsOnTrack("worker", step, `task "slow" (shared)`)
		}
		w.ThenEverySpanEndsBeforeAnyStarts("worker", steps[0], steps[1])
		w.ThenTheOtherClientSucceeds()
		w.WhenIReadTheTrace("<other client's trace>", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		for _, step := range steps { // its own, off the shared track
			w.ThenTheTraceSpanIsOnTrack("worker", step, step)
		}
		w.ThenEverySpanEndsBeforeAnyStarts("worker", steps[0], steps[1])
		w.ThenTheTraceHasNoSpan("executor", "trace incomplete")
	})

	t.Run("A build that left a shared attempt says so", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenAProjectThatPullsBusybox()
		w.GivenABarrier()
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aSlowTask(nonce))
		steps := slowTaskSteps(nonce)
		w.GivenAnotherClientBuildsOnTheCoordinator("slow", onTheCoordinator("<coordinator>"))
		w.GivenTheOtherClientIsBuildingIt()
		w.GivenTheOtherClientIsInterruptedOnceJoined()
		w.GivenTheBarrierOpensOnceTheOtherClientEnds()
		w.WhenIBuild("slow", onTheCoordinator("<coordinator>"), traced())
		w.ThenItSucceeds()
		w.ThenTheBarrierOpened()
		w.ThenTheOtherClientIsInterrupted()
		for _, step := range steps {
			w.ThenTheTraceSpanIsOnTrack("worker", step, `task "slow" (shared)`)
		}
		w.ThenEverySpanEndsBeforeAnyStarts("worker", steps[0], steps[1])
		w.WhenIReadTheTrace("<other client's trace>", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", "trace incomplete",
			traceArgs{"reason": "the job left the attempt running"})
	})

	t.Run("A reused target shows the time it saved", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(aLayerThatRuns(testenv.NewNonce()))
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIBuild("app", traced())
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.ThenTheTraceSpanSaysAtLeast("executor", `layer "app"`, traceArgs{"status": "cached", "made": "*",
			"took": "*s", "saved": "*s", "attempt trace": "sha256:*"})
		w.ThenTheHitTookAtLeast(`layer "app"`, 2*time.Second)
		w.WhenIReadTheTrace(`<attempt trace of layer "app">`)
		w.ThenItSucceeds()
		w.ThenTheTraceSpanIsOnTrack("executor", `run(["sh", "-c", "sleep 2; echo made"])`, `layer "app"`)
	})
}

// TestTrace_AMissingTraceNeverCostsAHit specifies, as the cache reference says, that "a result whose trace is gone is
// reused all the same; a trace that would show it says the trace is gone".
func TestTrace_AMissingTraceNeverCostsAHit(t *testing.T) {
	parallelRule(t)
	w := newTraceWorld(t)
	w.GivenStoresOfTheScenariosOwn()
	w.GivenTheBuildFileDeclares(`layer(name="app", base="scratch", steps=[env(NONCE="` + testenv.NewNonce() +
		`")])`)
	w.WhenIBuild("app", traced())
	w.ThenItSucceeds()
	w.ThenItPrintsItsTrace()
	w.GivenTheAttemptTraceOfIsRemoved(`layer "app"`)

	t.Run("A job whose attempt's trace is gone says so", func(t *testing.T) {
		w.WhenIReadTheTrace("<printed trace>")
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", "trace incomplete",
			traceArgs{"reason": "the attempt's trace is gone from the build cache"})
		w.ThenTheTraceSpanIsOnTrack("executor", "trace incomplete", `layer "app"`)
	})

	t.Run("A result whose trace was removed is reused", func(t *testing.T) {
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
	})
}

// aTaskOfManySteps declares the task "steps", on scratch, which sets NONCE to nonce, then STEP to each of 1 to 40 in
// turn: an attempt whose trace holds a span for each, which an attempt's trace of a few kilobytes can't keep.
func aTaskOfManySteps(nonce string) string {
	steps := []string{`env(NONCE="` + nonce + `")`}
	for i := 1; i <= 40; i++ {
		steps = append(steps, fmt.Sprintf(`env(STEP="%d")`, i))
	}
	return `task(name="steps", base="scratch", steps=[` + strings.Join(steps, ", ") + `])`
}

// TestTrace_ATraceIsCappedInSize specifies, as the cache reference says, that "each stored trace is kept whole within
// 16 MiB, as a log is: past that, the latest-started spans are dropped, and the trace says it was cut short and how
// many it dropped".
func TestTrace_ATraceIsCappedInSize(t *testing.T) {
	parallelRule(t)
	t.Run("An attempt that traces past the cap is cut short", func(t *testing.T) {
		w := newTraceWorld(t)
		w.GivenACoordinatorWhoseAttemptTracesKeep(4 << 10)
		w.GivenTheBuildFileDeclares(aTaskOfManySteps(testenv.NewNonce()))
		w.WhenIBuild("steps", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenItPrintsItsTrace()
		w.ThenItWarnsNothing()
		w.WhenIReadTheTrace("<printed trace>", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTraceSpanSaysAtLeast("executor", `task "steps"`, traceArgs{"status": "building"})
		w.ThenTheTraceSpanIsWithin("executor", `env(STEP="1")`, "executor", `task "steps"`)
		w.ThenTheTraceHasNoSpan("executor", `env(STEP="40")`)
		w.ThenTheTraceSpanSaysAtLeast("executor", "trace truncated", traceArgs{"dropped": aCount})
		w.ThenTheTraceSpanIsOnTrack("executor", "trace truncated", `task "steps"`)
	})
}

// TestTrace_SecretsNeverReachAStoredTrace specifies that a stored trace, a command's, a job's or an attempt's, holds
// no secret's value, as --trace's file holds none.
func TestTrace_SecretsNeverReachAStoredTrace(t *testing.T) {
	w := newTraceWorld(t)
	w.GivenStoresOfTheScenariosOwn()
	w.GivenTheBuildFileDeclares(`
		glob(name="src", include=["a.txt"])
		task(name="secret", base="scratch", steps=[env(NONCE="` + testenv.NewNonce() + `"),
		    add("src", "/src")])
	`)
	w.GivenTheProjectHolds(files{"a.txt": "a"})
	w.GivenTheEnvironmentSets("VAMOS_TEST_TOKEN", "stored-secret-value")
	w.WhenIBuild("secret", withSecrets("id=token,env=VAMOS_TEST_TOKEN"))
	w.ThenItSucceeds()
	w.ThenItPrintsItsTrace()
	w.ThenTheStoredTracesDoNotContain("stored-secret-value")
	w.WhenIReadTheTrace("<printed trace>")
	w.ThenItSucceeds()
	w.ThenTheTraceDoesNotContain("stored-secret-value")
}
