package features

import (
	"testing"
)

// newTargetMistakeWorld starts a scenario of the targets feature whose BUILD file names or refers to a target wrongly,
// which loading catches before anything runs, so that it needs no buildah.
// Every target has a unique name within its BUILD file, and a target refers to another by its name.
// Its project pulls busybox.
func newTargetMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newEmptyTargetMistakeWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newEmptyTargetMistakeWorld starts a scenario of the targets feature as newTargetMistakeWorld does,
// for a BUILD file that declares no other target before the scenario's: its project holds nothing.
func newEmptyTargetMistakeWorld(t *testing.T) *world {
	t.Helper()
	return newWorld(t)
}

// TestTargets_NamesAreUniqueAndNonempty specifies that every target has a unique, nonempty name within its BUILD file.
func TestTargets_NamesAreUniqueAndNonempty(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name:  "A layer with an empty name fails",
			build: `layer(name="", base="busybox")`,
			error: "/project/BUILD:2:6: layer: name must not be empty",
		},
		{
			name:  "A pull with an empty name fails",
			build: `pull(name="", image="busybox")`,
			error: "/project/BUILD:2:5: pull: name must not be empty",
		},
		{
			name:  "A glob with an empty name fails",
			build: `glob(name="", include=["app.sh"])`,
			error: "/project/BUILD:2:5: glob: name must not be empty",
		},
		{
			name:  "A layer with a name already used fails, naming where it was",
			build: `layer(name="busybox", base="busybox")`,
			error: `/project/BUILD:2:6: layer: name "busybox" is already used at /project/BUILD:1:5`,
		},
		{
			name:  "A glob with a name already used fails, naming where it was",
			build: `glob(name="busybox", include=["app.sh"])`,
			error: `/project/BUILD:2:5: glob: name "busybox" is already used at /project/BUILD:1:5`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestTargets_NamesAreNotLabelsOrScratch specifies that names cannot contain : or /,
// and that scratch is reserved for the empty image.
func TestTargets_NamesAreNotLabelsOrScratch(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name:  "A name with a colon fails",
			build: `layer(name="a:b", base="busybox")`,
			error: `/project/BUILD:2:6: layer: name "a:b" must not contain ":" or "/", which labels use`,
		},
		{
			name:  "A name with a slash fails",
			build: `layer(name="a/b", base="busybox")`,
			error: `/project/BUILD:2:6: layer: name "a/b" must not contain ":" or "/", which labels use`,
		},
		{
			name:  "A target named scratch fails",
			build: `pull(name="scratch", image="busybox")`,
			error: `/project/BUILD:2:5: pull: name "scratch" is reserved for the empty base image`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestTargets_LocalDependenciesAreDeclaredFirst specifies that local dependencies are declared before the targets
// that use them: a reference to any other name fails, naming the earlier targets it could be.
func TestTargets_LocalDependenciesAreDeclaredFirst(t *testing.T) {
	for _, example := range []struct {
		name    string
		build   string // after the pull of busybox, on line 2
		project files  // the project's other files
		error   string
	}{
		{
			name:  "A base declared later fails",
			build: "layer(name=\"app\", base=\"deps\")\nlayer(name=\"deps\", base=\"busybox\")",
			error: `/project/BUILD:2:6: layer: base "deps" is not "scratch" or an earlier pull() or layer(); ` +
				`earlier targets are "busybox"`,
		},
		{
			name:  "A misspelled base fails, naming the earlier targets",
			build: `layer(name="app", base="busybx")`,
			error: `/project/BUILD:2:6: layer: base "busybx" is not "scratch" or an earlier pull() or layer(); ` +
				`earlier targets are "busybox"`,
		},
		{
			name:  "A base that names an image rather than a target fails",
			build: `layer(name="app", base="docker.io/library/busybox:1.36")`,
			error: `/project/BUILD:2:6: layer: base "docker.io/library/busybox:1.36" is not "scratch" ` +
				`or an earlier pull() or layer(); earlier targets are "busybox"`,
		},
		{
			name: "A misspelled add() source fails, naming the earlier globs and tasks",
			build: "glob(name=\"code\", include=[\"app.sh\"])\n" +
				`layer(name="app", base="busybox", steps=[add("cod", "/app")])`,
			error: `/project/BUILD:3:45: add: source "cod" is not an earlier glob() or task(); ` +
				`earlier globs and tasks are "code"`,
		},
		{
			name:    "An add() source that names a file rather than a target fails",
			build:   `layer(name="app", base="busybox", steps=[add("app.sh", "/app")])`,
			project: files{"app.sh": "echo hi"},
			error: `/project/BUILD:2:45: add: source "app.sh" is not an earlier glob() or task(); ` +
				"there are none before this",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(example.project)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}

	t.Run("A base in a file with no earlier targets fails, saying there are none", func(t *testing.T) {
		w := newEmptyTargetMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`layer(name="app", base="docker.io/library/busybox:1.36")`)
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD:1:6: layer: base "docker.io/library/busybox:1.36" is not "scratch" ` +
			"or an earlier pull() or layer(); there are none before this")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestTargets_KindsHaveTheirUses specifies what each kind of target can be used as:
// only a pull or a layer can be a base, and only a glob or a task with output can be an add() source.
func TestTargets_KindsHaveTheirUses(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox, on line 2
		error string
	}{
		{
			name:  "A glob as a base fails",
			build: "glob(name=\"code\", include=[\"app.sh\"])\n" + `layer(name="app", base="code")`,
			error: `/project/BUILD:3:6: layer: base "code" is a glob(), not a pull() or layer()`,
		},
		{
			name: "A task as a base fails",
			build: `task(name="make", base="busybox", source="/out", include=["app"])` + "\n" +
				`layer(name="app", base="make")`,
			error: `/project/BUILD:3:6: layer: base "make" is a task(), not a pull() or layer()`,
		},
		{
			name:  "A pull as an add() source fails",
			build: `layer(name="app", base="busybox", steps=[add("busybox", "/app")])`,
			error: `/project/BUILD:2:45: add: source "busybox" is a pull(), not a glob() or task()`,
		},
		{
			name: "A task without output as an add() source fails",
			build: `task(name="test", base="busybox", steps=[run(["true"])])` + "\n" +
				`layer(name="app", base="busybox", steps=[add("test", "/app")])`,
			error: `/project/BUILD:3:45: add: source "test" is a task() without output; give it source and include`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestTargets_CommandsNameTheBuildFilesTargets specifies that the targets a command names are names in the current
// directory's BUILD file, or workspace labels: any other name fails, before anything runs,
// suggesting the name that was meant where one is close, and otherwise naming the targets there are.
func TestTargets_CommandsNameTheBuildFilesTargets(t *testing.T) {
	for _, example := range []struct {
		name    string
		targets []string // the first, and those after it
		error   string
	}{
		{
			name:    "A misspelled target fails, naming the one meant",
			targets: []string{"pakcages"},
			error:   `/project/BUILD has no target "pakcages"; did you mean "packages"?`,
		},
		{
			name:    "An unknown target fails, naming the targets there are",
			targets: []string{"packages", "server"},
			error:   `/project/BUILD has no target "server"; its targets are "busybox", "code", "packages"`,
		},
		{
			name:    "A label without its leading slashes fails, saying how a label starts",
			targets: []string{"backend:test"},
			error:   `/project/BUILD has no target "backend:test"; a label starts with "//", as in "//backend:test"`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newTargetMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`
				glob(name="code", include=["app.sh"])
				layer(name="packages", base="busybox", steps=[add("code", "/app")])
			`)
			w.WhenIBuild(example.targets[0], alongWith(example.targets[1:]...))
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
		})
	}

	t.Run("A target of a BUILD file that declares none fails, saying so", func(t *testing.T) {
		w := newEmptyTargetMistakeWorld(t)
		w.GivenTheBuildFileDeclares("")
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD has no target "app"; it declares none`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestTargets_GlobsAreNotNamedByCommands specifies that globs provide input filesets to add(),
// and cannot be named directly by a command.
func TestTargets_GlobsAreNotNamedByCommands(t *testing.T) {
	t.Run("Building a glob fails, saying its files are read by the targets that add them", func(t *testing.T) {
		w := newTargetMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`glob(name="code", include=["app.sh"])`)
		w.WhenIBuild("code")
		w.ThenItFailsWith(`"code" is a glob(), whose files are read by the targets that add them`)
		w.ThenNoTargetRan()
	})
}
