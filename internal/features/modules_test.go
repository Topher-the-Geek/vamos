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
