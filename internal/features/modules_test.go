package features

import (
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newModuleWorld starts a scenario of the modules feature, from its background:
// a workspace whose BUILD files load functions and constants from .star files with Starlark's load().
func newModuleWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenTheProjectHolds(files{"WORKSPACE": ""})
	return w
}

// newModuleMistakeWorld starts a scenario of the modules feature whose load() or .star file is wrong,
// which loading catches before anything runs, so that it needs no buildah.
// Its project is a workspace, with a WORKSPACE file.
func newModuleMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenTheProjectHolds(files{"WORKSPACE": ""})
	return w
}

// TestModules_FunctionsDeclareTargetsForTheBuildFileThatCalls specifies that a module's functions can declare
// targets when called by a BUILD file, that references inside those functions resolve relative to the calling BUILD
// file's package, and that references in functions of ctx resolve relative to the target being configured.
func TestModules_FunctionsDeclareTargetsForTheBuildFileThatCalls(t *testing.T) {
	t.Run("Loaded functions declare a task, and name a target, in each calling package", func(t *testing.T) {
		w := newModuleWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheProjectHolds(files{
			"more.star": dedent(`
				MODES = ["debug", "release"]

				# A function of ctx, named here but used by targets of other packages: "debug" is each one's own.
				DEBUG = lambda ctx: dep("debug", mode = "debug")
			`),
			"defs.star": dedent(`
				load("//:more.star", "MODES")

				def in_mode(label, mode):
				    """Names label in mode, which must be one of MODES."""
				    if mode not in MODES:
				        fail("unknown mode " + mode)
				    return lambda ctx: dep(label, mode = mode)

				def echo_task(name, text):
				    task(
				        name = name,
				        params = {"mode": "release"},
				        base = "busybox",
				        steps = lambda ctx: [run(["sh", "-c", "mkdir /out && echo %s %s > /out/echo.txt" % (
				            text, ctx.mode)])],
				        source = "/out",
				        include = ["echo.txt"],
				    )
			`),
		})
		// Two packages call the same functions, and each gets its own targets, named in its own package.
		for _, pkg := range []string{"app", "tools"} {
			w.GivenThePackageDeclares(pkg, `load("//:defs.star", "echo_task", "in_mode")`)
			w.GivenThePackageDeclares(pkg, `load("//:more.star", "DEBUG")`)
			w.GivenThePackageDeclares(pkg, testenv.PullBusybox)
			w.GivenThePackageDeclares(pkg, `
				echo_task("echo", "`+pkg+` `+nonce+`")
				layer(name="debug", params={"mode": "release"}, base="busybox", command=["`+pkg+`"])
				layer(name="app", base=in_mode("debug", "debug"), steps=[add("echo", "/app")])
				layer(name="shared", base=DEBUG)
			`)
		}
		for _, pkg := range []string{"app", "tools"} {
			w.GivenIWorkIn(pkg)
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{layer("debug", `mode="debug"`): ranOrReused})
			w.ThenTheImageCommandIs(pkg)
			w.ThenTheImageHolds(under("/app"), files{"/app/echo.txt": pkg + " " + nonce + " release\n"})
			w.WhenISave("shared")
			w.ThenItSucceeds()
			w.ThenTheImageCommandIs(pkg)
		}
	})
}

// TestModules_LabelsNameStarFilesInTheWorkspace specifies that a module's label starts with // and names a .star file
// in the workspace, from which load() takes the names it gives.
func TestModules_LabelsNameStarFilesInTheWorkspace(t *testing.T) {
	for _, example := range []struct {
		name    string
		load    string
		modules files // the project's .star files
		error   string
	}{
		{
			name: "A label relative to the package fails, saying a module's label starts with //",
			load: `load(":defs.star", "x")`,
			error: `/project/BUILD:1:1: cannot load :defs.star: a .star file is named by its label, ` +
				`which starts with "//" and ends with ".star", such as "//:defs.star"`,
		},
		{
			name:  "A label to a file that isn't a .star file fails, saying only a .star file can be loaded",
			load:  `load("//:BUILD", "x")`,
			error: `/project/BUILD:1:1: cannot load //:BUILD: only a .star file can be loaded, such as "//:defs.star"`,
		},
		{
			name: "A label without a colon fails, saying how a .star file's label names it",
			load: `load("//defs.star", "x")`,
			error: `/project/BUILD:1:1: cannot load //defs.star: a .star file's label names its directory, ` +
				`then ":" and the file, such as "//:defs.star" or "//tools:defs.star"`,
		},
		{
			name: "A label whose directory is after the colon fails, saying how a .star file's label names it",
			load: `load("//:tools/defs.star", "x")`,
			error: `/project/BUILD:1:1: cannot load //:tools/defs.star: a .star file's label names its directory, ` +
				`then ":" and the file, such as "//:defs.star" or "//tools:defs.star"`,
		},
		{
			name:  "A label to a file that isn't there fails",
			load:  `load("//:defs.star", "x")`,
			error: "/project/BUILD:1:1: cannot load //:defs.star: no file /project/defs.star",
		},
		{
			name:    "A name the module doesn't define fails",
			load:    `load("//:defs.star", "nope")`,
			modules: files{"defs.star": "X = 1\n"},
			error:   "/project/BUILD:1:1: load: name nope not found in module //:defs.star",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newModuleMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.load)
			w.GivenTheProjectHolds(example.modules)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestModules_MistakesAreReportedInTheModule specifies that a mistake in a .star file fails the BUILD file that loads
// it, reported where it is in the .star file, and that a module that loads itself, through others, is an error.
func TestModules_MistakesAreReportedInTheModule(t *testing.T) {
	for _, example := range []struct {
		name    string
		modules files
		error   string
	}{
		{
			name:    "A mistake in a module fails, reported where it is",
			modules: files{"defs.star": "X = 1\nY = X + \"a\"\n"},
			error: "/project/BUILD:1:1: cannot load //:defs.star: " +
				"/project/defs.star:2:7: unknown binary op: int + string",
		},
		{
			name: "Modules that load each other fail, naming the module",
			modules: files{
				"defs.star": `load("//:more.star", "Y")` + "\nX = 1\n",
				"more.star": `load("//:defs.star", "X")` + "\nY = 1\n",
			},
			error: "/project/BUILD:1:1: cannot load //:defs.star: " +
				"/project/defs.star:1:1: cannot load //:more.star: " +
				"/project/more.star:1:1: cannot load //:defs.star: " +
				"//:defs.star loads itself, through the files it loads",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newModuleMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`load("//:defs.star", "X")`)
			w.GivenTheProjectHolds(example.modules)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestModules_DeclareNothingWhileLoading specifies that a module cannot declare targets at its top level while
// loading, nor name a target by a label relative to a package, since it is loaded for every package alike.
func TestModules_DeclareNothingWhileLoading(t *testing.T) {
	for _, example := range []struct {
		name   string
		module string // defs.star, which defines X
		error  string
	}{
		{
			name:   "A module that declares a target as it loads fails, saying to declare it in a function",
			module: "X = 1\n" + `glob(name="code", include=["app.sh"])`,
			error: "/project/defs.star:2:5: glob: a .star file can't declare a target as it is loaded; " +
				"declare it in a function that a BUILD file calls",
		},
		{
			name:   "A module that names a target by a relative label as it loads fails",
			module: `X = dep("base")`,
			error:  `/project/defs.star:1:8: dep: label "base": as a .star file is loaded, labels must start with "//"`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newModuleMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`load("//:defs.star", "X")`)
			w.GivenTheProjectHolds(files{"defs.star": example.module})
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestModules_ArgumentsAreCheckedWhereTheyAreUsed specifies that the arguments that a module's dep() gives are checked
// where a BUILD file uses it, against the target it names there.
func TestModules_ArgumentsAreCheckedWhereTheyAreUsed(t *testing.T) {
	t.Run("A module's dep() of a parameter the target doesn't have fails where the BUILD file uses it",
		func(t *testing.T) {
			w := newModuleMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`
				load("//:defs.star", "BASE")
				layer(name="base", params={"mode": "release"}, base="scratch")
				layer(name="app", base=BASE)
			`)
			w.GivenTheProjectHolds(files{"defs.star": `BASE = dep("//:base", mod="debug")`})
			w.WhenISave("app")
			w.ThenItFailsWith(`/project/BUILD:3:6: layer: base: "//:base" has no parameter "mod"; did you mean "mode"?`)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
}
