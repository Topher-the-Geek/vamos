package features

import (
	"testing"
)

// newBuildFileWorld starts a scenario of the BUILD file feature, from its background.
// A BUILD file is a Starlark program that declares named targets, which vamos loads before anything runs.
// Every scenario's file holds a mistake, which loading catches, so it needs no buildah,
// and saves its layer, to see that the mistake leaves nothing where it would have saved.
// Its project pulls busybox.
func newBuildFileWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// theCodeGlob declares the glob "code" of app.sh, which theCode holds, for a scenario whose layer adds it.
const theCodeGlob = `glob(name="code", include=["app.sh"])`

// theCode is the project file that theCodeGlob selects.
var theCode = files{"app.sh": "echo hi"}

// TestBuildFile_MistakesFailAsItLoads specifies that a mistake in how a BUILD file declares its targets,
// such as a misspelled argument or a value of the wrong type, fails the command as the file loads,
// before anything runs,
// with an error that names the file, line and column of the call that made it.
func TestBuildFile_MistakesFailAsItLoads(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox and theCodeGlob, on line 3
		error string
	}{
		{
			name:  "A syntax error is reported where the parser finds it",
			build: "layer(\n    name=\"app\",\n    base=\"busybox\",\n    command=[\"echo\", \"hi\"\n)",
			error: "/project/BUILD:7:2: got ')', want ']'",
		},
		{
			name:  "A misspelled function is undefined",
			build: `bulid(name="app", base="busybox")`,
			error: "/project/BUILD:3:1: undefined: bulid",
		},
		{
			name:  "A misspelled argument is unexpected",
			build: `layer(name="app", base="busybox", comand=["echo"])`,
			error: `/project/BUILD:3:6: layer: unexpected keyword argument "comand"`,
		},
		{
			name:  "An argument that layer() no longer takes is unexpected",
			build: `layer(name="app", base="busybox", sources=["code"], destination="/app")`,
			error: `/project/BUILD:3:6: layer: unexpected keyword argument "sources"`,
		},
		{
			name:  "A task takes no command",
			build: `task(name="app", base="busybox", command=["make"], source="/out", include=["app"])`,
			error: `/project/BUILD:3:5: task: unexpected keyword argument "command"`,
		},
		{
			name:  "A target without a name fails",
			build: `layer(base="busybox")`,
			error: "/project/BUILD:3:6: layer: missing argument for name",
		},
		{
			name:  "A layer without a base fails",
			build: `layer(name="app", command=["echo"])`,
			error: "/project/BUILD:3:6: layer: missing argument for base",
		},
		{
			name:  "An add() without a destination fails",
			build: `layer(name="app", base="busybox", steps=[add("code")])`,
			error: "/project/BUILD:3:45: add: missing argument for dest",
		},
		{
			name:  "Steps that aren't a list fail",
			build: `layer(name="app", base="busybox", steps=run(["true"]))`,
			error: `/project/BUILD:3:6: layer: for parameter "steps": got step, want list`,
		},
		{
			name:  "A step that isn't a step fails",
			build: `layer(name="app", base="busybox", steps=[run(["true"]), "make"])`,
			error: "/project/BUILD:3:6: layer: steps[1] is string, want a step, such as add() or run()",
		},
		{
			name:  "A command that isn't a list fails",
			build: `layer(name="app", base="busybox", command="echo")`,
			error: `/project/BUILD:3:6: layer: for parameter "command": got string, want list`,
		},
		{
			name:  "A command whose argument isn't a string fails",
			build: `layer(name="app", base="busybox", command=["sleep", 5])`,
			error: "/project/BUILD:3:6: layer: command[1] is int, want string",
		},
		{
			name:  "An entrypoint whose argument isn't a string fails",
			build: `layer(name="app", base="busybox", entrypoint=["java", 1])`,
			error: "/project/BUILD:3:6: layer: entrypoint[1] is int, want string",
		},
		{
			name:  "An add() source that isn't a name fails",
			build: `layer(name="app", base="busybox", steps=[add(1, "/app")])`,
			error: "/project/BUILD:3:45: add: source is int, want a label or dep()",
		},
		{
			name:  "An add() source that is a list fails",
			build: `layer(name="app", base="busybox", steps=[add(["code"], "/app")])`,
			error: "/project/BUILD:3:45: add: source is list, want a label or dep()",
		},
		{
			name:  "An add() destination that isn't a string fails",
			build: `layer(name="app", base="busybox", steps=[add("code", ["/app"])])`,
			error: "/project/BUILD:3:45: add: for parameter dest: got list, want string",
		},
		{
			name:  "A run() that isn't given a list fails",
			build: `layer(name="app", base="busybox", steps=[run("make")])`,
			error: "/project/BUILD:3:45: run: for parameter argv: got string, want list",
		},
		{
			name:  "A run() argument that isn't a string fails",
			build: `layer(name="app", base="busybox", steps=[run(["sleep", 5])])`,
			error: "/project/BUILD:3:45: run: argv[1] is int, want string",
		},
		{
			name:  "Secrets that aren't secrets fail",
			build: `layer(name="app", base="busybox", steps=[run(["true"], secrets=["t"])])`,
			error: "/project/BUILD:3:45: run: secrets[0] is string, " +
				`want a secret, such as secret("token", env="TOKEN")`,
		},
		{
			name:  "An include that isn't a string fails",
			build: `glob(name="other", include=[1])`,
			error: "/project/BUILD:3:5: glob: include[0] is int, want string",
		},
		{
			name:  "A mode_mask that isn't an int fails",
			build: `glob(name="other", include=["."], mode_mask="755")`,
			error: `/project/BUILD:3:5: glob: for parameter "mode_mask": got string, want int`,
		},
		{
			name:  "A limit that isn't an int fails",
			build: `glob(name="other", include=["."], max_file_count=True)`,
			error: `/project/BUILD:3:5: glob: for parameter "max_file_count": got bool, want int`,
		},
		{
			name:  "fail() fails the command with its message",
			build: `fail("unsupported platform")`,
			error: "/project/BUILD:3:5: fail: unsupported platform",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(theCodeGlob)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(theCode)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}

	t.Run("A mistake in a function that the file calls is reported where the function makes it", func(t *testing.T) {
		w := newBuildFileWorld(t)
		w.GivenTheBuildFileDeclares(`
			def app(src):
			    glob(name="code", include=[src])

			app("main.sh")
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD:3:9: glob: include "main.sh" not found in /project`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestBuildFile_StringsHoldNoNULByte specifies that no string that a BUILD file gives a target or step,
// such as a name, an argument, a path, an owner or a variable, may hold a NUL byte, which can't reach a container.
func TestBuildFile_StringsHoldNoNULByte(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox and theCodeGlob, on line 3
		error string
	}{
		{
			name:  "In a target's name",
			build: `layer(name="a\x00b", base="busybox")`,
			error: `/project/BUILD:3:6: layer: name "a\x00b" must not contain a NUL byte`,
		},
		{
			name:  "In a run() argument",
			build: `layer(name="app", base="busybox", steps=[run(["echo", "a\x00"])])`,
			error: "/project/BUILD:3:45: run: argv[1] must not contain a NUL byte",
		},
		{
			name:  "In a command",
			build: `layer(name="app", base="busybox", command=["a\x00"])`,
			error: "/project/BUILD:3:6: layer: command[0] must not contain a NUL byte",
		},
		{
			name:  "In a workdir() path",
			build: `layer(name="app", base="busybox", steps=[workdir("/a\x00")])`,
			error: `/project/BUILD:3:49: workdir: path "/a\x00" must not contain a NUL byte`,
		},
		{
			name:  "In an owner",
			build: `layer(name="app", base="busybox", steps=[add("code", "/app", owner="a\x00")])`,
			error: `/project/BUILD:3:45: add: owner "a\x00" must not contain a NUL byte`,
		},
		{
			name:  "In a task's source",
			build: `task(name="app", base="busybox", source="/a\x00", include=["."])`,
			error: `/project/BUILD:3:5: task: source "/a\x00" must not contain a NUL byte`,
		},
		{
			name:  "In a variable's name",
			build: `layer(name="app", base="busybox", steps=[env(**{"A\x00B": "x"})])`,
			error: `/project/BUILD:3:45: env: variable name "A\x00B" must not contain "=" or a NUL byte`,
		},
		{
			name:  "In a variable's value",
			build: `layer(name="app", base="busybox", steps=[env(A="x\x00y")])`,
			error: "/project/BUILD:3:45: env: A's value must not contain a NUL byte",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(theCodeGlob)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(theCode)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestBuildFile_StepsAreMadeInAStepsList specifies that steps are made in the steps list of a task or layer,
// and that a step made anywhere else is an error.
func TestBuildFile_StepsAreMadeInAStepsList(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // after the pull of busybox and theCodeGlob, on line 3
		error string
	}{
		{
			name:  "A run() outside any steps fails",
			build: "run([\"true\"])\nlayer(name=\"app\", base=\"busybox\")",
			error: `/project/BUILD:3:4: run: run(["true"]) is not in the steps of any layer() or task()`,
		},
		{
			name: "A run() outside any steps fails beside steps that a function gives",
			build: `layer(name="lib", base="busybox", steps=[run(["true"])], command=lambda ctx: [])` + "\n" +
				"run([\"false\"])\nlayer(name=\"app\", base=\"busybox\")",
			error: `/project/BUILD:4:4: run: run(["false"]) is not in the steps of any layer() or task()`,
		},
		{
			name:  "An add() outside any steps fails",
			build: "add(\"code\", \"/app\")\nlayer(name=\"app\", base=\"busybox\")",
			error: `/project/BUILD:3:4: add: add("code", "/app") is not in the steps of any layer() or task()`,
		},
		{
			name: "An env() kept in a variable but never used fails",
			build: `layer(name="app", base="busybox", steps=[run(["true"])])` + "\n" +
				`setup = [env(**{"A": "1", "a.b": "2"})]`,
			error: `/project/BUILD:4:13: env: env(A="1", **{"a.b": "2"}) is not in the steps of any layer() or task()`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildFileWorld(t)
			w.GivenTheBuildFileDeclares(theCodeGlob)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(theCode)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}
