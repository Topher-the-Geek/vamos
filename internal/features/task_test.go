package features

import (
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newTaskWorld starts a scenario of the task feature, from its background.
// A task takes images and files in and gives files out.
// It runs its steps in a container made from its base,
// keeps only the files it selects beneath its source, and discards the container.
// A task without output is a check: it succeeds or fails.
// Its project pulls busybox.
func newTaskWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newTaskMistakeWorld starts a scenario of the task feature whose task is declared wrongly,
// which loading catches before anything runs, so that it needs no buildah.
// Its project pulls busybox.
func newTaskMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// TestTask_OutputIsSelectedBeneathSource specifies that a task's output is the files it selects beneath its source.
func TestTask_OutputIsSelectedBeneathSource(t *testing.T) {
	t.Run("Only selected files are output, at their paths relative to the source", func(t *testing.T) {
		w := newTaskWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="input", include=["input.txt"])
			task(
			    name="shout",
			    base="busybox",
			    steps=[
			        add("input", "/work"),
			        run(["sh", "-c", """
			        mkdir -p /work/out/sub
			        tr a-z A-Z < /work/input.txt > /work/out/upper.txt
			        echo nested > /work/out/sub/nested.txt
			        echo left out > /work/out/skipped.txt
			        echo shouting
			        """]),
			    ],
			    source="/work/out",
			    include=["upper.txt", "sub"],
			)
		`)
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIExport("shout")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes("shouting")
		w.ThenTheExportHolds(files{
			"upper.txt":      "HELLO\n",
			"sub/nested.txt": "nested\n",
		})
	})

	t.Run("Output keeps its modes, within its mode mask", func(t *testing.T) {
		w := newTaskWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(
			    name="make",
			    base="busybox",
			    steps=[run(["sh", "-c", """
			        mkdir -p /out/shared
			        echo tool > /out/tool
			        echo map > /out/tool.map
			        echo data > /out/shared/data
			        chmod 4750 /out/tool
			        chmod 1777 /out/shared
			    """])],
			    source="/out",
			    include=["**"],
			    exclude=["*.map"],
			    mode_mask=0o7777,
			)
		`)
		w.WhenIExport("make")
		w.ThenItSucceeds()
		w.ThenTheExportHolds(files{
			"tool":        "tool\n",
			"shared/data": "data\n",
		})
		w.ThenTheExportModesAre(modes{
			"tool":        0o4750,
			"shared":      0o1777,
			"shared/data": 0o644,
		})
	})

	t.Run("The default mode mask retains only the permission bits of 0o755", func(t *testing.T) {
		w := newTaskWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(
			    name="make",
			    base="busybox",
			    steps=[run(["sh", "-c", "mkdir /out && echo lib > /out/lib.so && chmod 4777 /out/lib.so"])],
			    source="/out",
			    include=["*.so"],
			)
		`)
		w.WhenIExport("make")
		w.ThenItSucceeds()
		w.ThenTheExportHolds(files{"lib.so": "lib\n"})
		w.ThenTheExportModesAre(modes{"lib.so": 0o755})
	})

	t.Run("Allowing no files, a task may output none", func(t *testing.T) {
		w := newTaskWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(
			    name="make",
			    base="busybox",
			    steps=[run(["mkdir", "/out"])],
			    source="/out",
			    include=["*.so"],
			    allow_no_files=True,
			)
		`)
		w.WhenIExport("make")
		w.ThenItSucceeds()
		w.ThenTheExportHolds(files{})
	})

	t.Run("A source that is a link is resolved within the container", func(t *testing.T) {
		w := newTaskWorld(t)
		marker := "/vamos-test-" + testenv.NewNonce() // which only the container has
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			task(
			    name="make",
			    base="busybox",
			    steps=[run(["sh", "-c", "mkdir -p %[1]s && echo inside > %[1]s/file.txt && ln -s %[1]s /out"])],
			    source="/out",
			    include=["file.txt"],
			)
			layer(name="app", base="busybox", steps=[add("make", "/app")])
		`, marker))
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{"/app/file.txt": "inside\n"})
	})
}

// TestTask_ContainerIsDiscarded specifies that a task's container is discarded.
func TestTask_ContainerIsDiscarded(t *testing.T) {
	t.Run("A layer that adds a task's output gets that output alone", func(t *testing.T) {
		w := newTaskWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(
			    name="make",
			    base="busybox",
			    steps=[run(["sh", "-c", """
			        mkdir -p /work/out
			        echo made > /work/out/made.txt
			        touch /work/scratch
			    """])],
			    source="/work/out",
			    include=["made.txt"],
			)
			layer(name="app", base="busybox", steps=[add("make", "/app")])
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{"/app/made.txt": "made\n"})
		w.ThenTheImageHoldsNothing(under("/work"))
		w.ThenNoImageIsLeftBehind()
	})
}

// TestTask_TakesImagesAndFiles specifies that a task takes images and files in.
func TestTask_TakesImagesAndFiles(t *testing.T) {
	t.Run("A task runs with its base's working directory and environment", func(t *testing.T) {
		w := newTaskWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="tools", base="busybox", steps=[env(GREETING="hello"), workdir("/srv")])
			task(
			    name="greet",
			    base="tools",
			    steps=[run(["sh", "-c", "mkdir out && echo $GREETING from $(pwd) > out/greeting.txt"])],
			    source="/srv/out",
			    include=["greeting.txt"],
			)
		`)
		w.WhenIExport("greet")
		w.ThenItSucceeds()
		w.ThenTheExportHolds(files{"greeting.txt": "hello from /srv\n"})
	})

	t.Run("A task can add project files and another task's output", func(t *testing.T) {
		w := newTaskWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="input", include=["input.txt"])
			task(
			    name="shout",
			    base="busybox",
			    steps=[
			        add("input", "/in"),
			        run(["sh", "-c", "mkdir /out && tr a-z A-Z < /in/input.txt > /out/upper.txt"]),
			    ],
			    source="/out",
			    include=["upper.txt"],
			)
			task(
			    name="count",
			    base="busybox",
			    steps=[add("shout", "/count"), run(["sh", "-c", "wc -c < /count/upper.txt > /count/size.txt"])],
			    source="/count",
			    include=["size.txt"],
			)
		`)
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIExport("count")
		w.ThenItSucceeds()
		w.ThenTheExportHolds(files{"size.txt": "6\n"})
	})
}

// TestTask_WithoutOutputIsACheck specifies that a task without output is a check.
func TestTask_WithoutOutputIsACheck(t *testing.T) {
	t.Run("A check runs, and is reused while its inputs are unchanged", func(t *testing.T) {
		w := newTaskWorld(t)
		tested := "tested " + testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="busybox")
			task(name="test", base="app", steps=[run(["echo", %q])])
		`, tested))
		w.WhenIBuild("test")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("test"): ran})
		w.ThenTheOutputIncludes(tested)
		w.WhenIBuild("test")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("test"): reused})
		w.ThenTheOutputIncludes(tested)
		w.ThenNoImageIsLeftBehind()
	})

	t.Run("A failing check fails the build", func(t *testing.T) {
		w := newTaskWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox")
			task(name="broken", base="app", steps=[run(["false"])])
		`)
		w.WhenIBuild("broken")
		w.ThenItFailsWith(`task "broken": run(["false"]) failed`)
		w.ThenNoImageIsLeftBehind()
	})
}

// TestTask_UnselectableOutputFails specifies that a task whose output can't be selected fails, leaving nothing behind.
func TestTask_UnselectableOutputFails(t *testing.T) {
	for _, example := range []struct {
		name    string
		command string // the task's one run(), as Starlark
		source  string
		include string
		options string // more selection arguments, if any, as Starlark
		errors  []string
	}{
		{
			name:    "The command fails",
			command: `["false"]`,
			source:  "/",
			include: "bin",
			errors:  []string{`task "make": run(["false"]) failed`, "exit status 1"},
		},
		{
			name:    "The source is missing",
			command: `["true"]`,
			source:  "/nope",
			include: "app",
			errors:  []string{`task "make": source "/nope" not found`},
		},
		{
			name:    "The source is a file",
			command: `["true"]`,
			source:  "/bin/sh",
			include: "app",
			errors:  []string{`task "make": source "/bin/sh" is not a directory`},
		},
		{
			name:    "An include is missing",
			command: `["mkdir", "/out"]`,
			source:  "/out",
			include: "app.js",
			errors:  []string{`task "make": include "app.js" not found in /out`},
		},
		{
			name:    "An include is a link",
			command: `["sh", "-c", "mkdir /out && ln -s /etc/passwd /out/passwd"]`,
			source:  "/out",
			include: "passwd",
			errors:  []string{`task "make": include "passwd" is a symbolic link`},
		},
		{
			name:    "An include's parent is a link",
			command: `["sh", "-c", "mkdir /out && ln -s /etc /out/etc"]`,
			source:  "/out",
			include: "etc/passwd",
			errors:  []string{`task "make": include "etc/passwd" has symbolic link parent "/out/etc"`},
		},
		{
			name:    "No output matches",
			command: `["mkdir", "/out"]`,
			source:  "/out",
			include: "*",
			errors:  []string{`task "make": no files in /out match include "*"; pass allow_no_files=True`},
		},
		{
			name:    "More output matches than max_file_count",
			command: `["sh", "-c", "mkdir /out && touch /out/a /out/b"]`,
			source:  "/out",
			include: ".",
			options: ", max_file_count=1",
			errors:  []string{`task "make": more than max_file_count=1 files in /out match`},
		},
	} {
		build := fmt.Sprintf(`task(name="make", base="busybox", steps=[run(%s)], source=%q, include=[%q]%s)`,
			example.command, example.source, example.include, example.options)

		t.Run(example.name+", exporting it", func(t *testing.T) {
			w := newTaskWorld(t)
			w.GivenTheBuildFileDeclares(build)
			w.WhenIExport("make")
			w.ThenItFailsWith(example.errors...)
			w.ThenNothingIsExported()
			w.ThenNoImageIsLeftBehind()
		})

		t.Run(example.name+", adding it to a layer", func(t *testing.T) {
			w := newTaskWorld(t)
			w.GivenTheBuildFileDeclares(build)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("make", "/app")])`)
			w.WhenISave("app")
			w.ThenItFailsWith(example.errors...)
			w.ThenNothingIsSaved()
			w.ThenNoImageIsLeftBehind()
		})
	}
}

// TestTask_SourceAndIncludeGoTogether specifies that a task gives source and include together, or omits both
// for a task that only runs checks, and that selection options require output to be declared.
func TestTask_SourceAndIncludeGoTogether(t *testing.T) {
	for _, example := range []struct {
		name  string
		task  string // its arguments after name and base
		error string // after the task's position
	}{
		{
			name:  "Include without source fails",
			task:  `include=["app"]`,
			error: "include needs source, the directory it is relative to",
		},
		{
			name:  "Source without include fails",
			task:  `source="/out"`,
			error: "source needs include, the files to collect from it",
		},
		{
			name:  "Exclude without output fails",
			task:  `exclude=["*.map"]`,
			error: "exclude needs source and include, the files to collect",
		},
		{
			name:  "A limit without output fails",
			task:  `max_file_size=1`,
			error: "max_file_size needs source and include",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTaskMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`task(name="make", base="busybox", ` + example.task + `)`)
			w.WhenIExport("make")
			w.ThenItFailsWith("/project/BUILD:2:5: task: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsExported()
		})
	}
}

// TestTask_OutputPatternsStayBeneathAnAbsoluteSource specifies that a task's source is an absolute container
// directory, and that its patterns, relative to source, cannot be empty or resolve outside it.
func TestTask_OutputPatternsStayBeneathAnAbsoluteSource(t *testing.T) {
	for _, example := range []struct {
		name  string
		task  string // its arguments after name and base
		error string // after the task's position
	}{
		{
			name:  "A relative source fails",
			task:  `source="out", include=["app"]`,
			error: `source "out" must be an absolute path`,
		},
		{
			name:  "An empty include fails, saying to use . for the whole directory",
			task:  `source="/out", include=[""]`,
			error: `include[0] is empty; use "." for the whole directory`,
		},
		{
			name:  "An include outside the source fails",
			task:  `source="/out", include=["../etc"]`,
			error: `include "../etc" is outside /out`,
		},
		{
			name:  "An exclude that doesn't parse fails",
			task:  `source="/out", include=["."], exclude=["[x"]`,
			error: `exclude "[x": syntax error in pattern`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTaskMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`task(name="make", base="busybox", ` + example.task + `)`)
			w.WhenIExport("make")
			w.ThenItFailsWith("/project/BUILD:2:5: task: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsExported()
		})
	}
}

// TestTask_OutputLimitsArePositive specifies that the limits of a task's output selection must be positive.
func TestTask_OutputLimitsArePositive(t *testing.T) {
	for _, limit := range []string{"max_file_count", "max_file_size", "max_total_size"} {
		t.Run("A "+limit+" of zero fails", func(t *testing.T) {
			w := newTaskMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`task(name="make", base="busybox", source="/out", include=["."], ` +
				limit + `=0)`)
			w.WhenIExport("make")
			w.ThenItFailsWith("/project/BUILD:2:5: task: " + limit + " must be positive, not 0")
			w.ThenNoTargetRan()
			w.ThenNothingIsExported()
		})
	}
}
