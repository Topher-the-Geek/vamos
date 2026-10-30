package features

import (
	"maps"
	"slices"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newTraceWorld starts a scenario of the trace feature, from its background: a project, which builds.
// --trace writes a trace of the command, which Perfetto shows: BUILD loading and glob capture, each pull and target,
// execution steps, cache activity and image delivery.
func newTraceWorld(t *testing.T) *world {
	t.Helper()
	return newBuildingWorld(t)
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
				traceArgs{"outcome": "building", "image ID": "<image>"})
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
				traceArgs{"outcome": "cached", "image ID": "<image>"})
			w.ThenTheTraceSpanHolds("executor", `layer "app"`, "cache lookup")
			w.ThenTheTraceArrowsAre()
		})
}

// TestTrace_ShowsDelivery specifies that the trace shows image delivery: a push's copy to the registry, from the
// executor or from the client, with a span for each blob, which a push again skips, as the registry has it; a save's
// copy to its archive; and an export, saying the files it wrote.
func TestTrace_ShowsDelivery(t *testing.T) {
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

// TestTrace_ShowsPulls specifies that the trace of a pull of an image that the store lacks shows resolving its tag,
// and copying the image, a blob at a time.
func TestTrace_ShowsPulls(t *testing.T) {
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
