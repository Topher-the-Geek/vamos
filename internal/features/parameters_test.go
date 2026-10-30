package features

import (
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newParametersWorld starts a scenario of the parameters feature, from its background.
// Targets other than pulls accept a params dictionary, whose values reach their functions of ctx.
// Its project pulls busybox.
func newParametersWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// appConfiguringItsParts declares the layer "app", which adds the task "plain", which takes no parameters,
// the task "mode" with app's mode, by a plain label, and in release mode, and the glob "messages" of app's languages,
// from the project's locales. The tasks print nonce.
func appConfiguringItsParts(nonce string) string {
	return fmt.Sprintf(`
		glob(
		    name="messages",
		    params={"languages": ["en"]},
		    include=lambda ctx: ["locales/%%s" %% l for l in ctx.languages],
		)
		task(
		    name="plain",
		    base="busybox",
		    steps=[run(["sh", "-c", "mkdir /out && echo %[1]s > /out/plain.txt"])],
		    source="/out",
		    include=["plain.txt"],
		)
		task(
		    name="mode",
		    params={"mode": param(default="release", values=["debug", "release"])},
		    base="busybox",
		    steps=lambda ctx: [run(["sh", "-c", "mkdir /out && echo %[1]s %%s > /out/mode.txt" %% ctx.mode])],
		    source="/out",
		    include=["mode.txt"],
		)
		layer(
		    name="app",
		    params={"mode": "release", "languages": ["en"]},
		    base="busybox",
		    steps=[
		        add("plain", "/app"),
		        add(dep("mode", mode=arg("mode")), "/app"),
		        add("mode", "/default"),
		        add(dep("mode", mode="release"), "/release"),
		        add(dep("messages", languages=arg("languages")), "/app"),
		    ],
		)
	`, nonce)
}

// locales are the project's messages, by language, which appConfiguringItsParts adds.
var locales = files{
	"locales/en/hi.json":  "hello",
	"locales/es/hi.json":  "hola",
	"locales/es/bye.json": "adios",
	"locales/fr/hi.json":  "salut",
	"locales/de/hi.json":  "hallo",
	"locales/jp/hi.json":  "konnichiwa",
	"locales/README.txt":  "not a language",
}

// settings declares the layer "settings", whose parameters of each type set its image's variables:
// VERBOSE, WORKERS to workers plus one, and LANGUAGES to the list as Starlark writes it.
const settings = `
	layer(
	    name="settings",
	    params={"verbose": False, "workers": 4, "languages": ["en"]},
	    base="busybox",
	    steps=lambda ctx: [env(
	        VERBOSE=str(ctx.verbose),
	        WORKERS=str(ctx.workers + 1),
	        LANGUAGES=repr(ctx.languages),
	    )],
	)
`

// TestParameters_PlainValuesAreDefaults specifies that a plain value declares both a default and a type,
// and that a plain label uses all the referenced target's defaults.
func TestParameters_PlainValuesAreDefaults(t *testing.T) {
	t.Run("Without arguments, every target takes its defaults, and one configured alike is built once",
		func(t *testing.T) {
			w := newParametersWorld(t)
			nonce := testenv.NewNonce()
			w.GivenTheBuildFileDeclares(appConfiguringItsParts(nonce))
			w.GivenTheProjectHolds(locales)
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{
				task("plain"): ran,
				task("mode"):  ran, // named with app's mode, release, by a plain label and in release mode: one target
				layer("app"):  ran,
			})
			w.ThenTheTargetWasBuiltInOneConfiguration(task("mode"))
			w.ThenTheImageHolds(under("/app"), files{
				"/app/plain.txt":          nonce + "\n",
				"/app/mode.txt":           nonce + " release\n",
				"/app/locales/en/hi.json": "hello",
			})
			w.ThenTheImageHolds(under("/default"), files{"/default/mode.txt": nonce + " release\n"})
			w.ThenTheImageHolds(under("/release"), files{"/release/mode.txt": nonce + " release\n"})
		})

	t.Run("A default's type is the parameter's", func(t *testing.T) {
		w := newParametersWorld(t)
		w.GivenTheBuildFileDeclares(settings)
		w.WhenISave("settings")
		w.ThenItSucceeds()
		w.ThenTheImageEnvironmentSets(environment{
			"VERBOSE":   "False",
			"WORKERS":   "5", // 4, plus 1
			"LANGUAGES": `["en"]`,
		})
	})
}

// TestParameters_WithoutADefaultIsGivenOnEveryUse specifies that a parameter without a default, given its type
// by param(), must be supplied on every use.
func TestParameters_WithoutADefaultIsGivenOnEveryUse(t *testing.T) {
	t.Run("A parameter without a default is given its value on every use", func(t *testing.T) {
		w := newParametersWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="server", params={"shards": param(type="int")}, base="busybox",
			      steps=lambda ctx: [env(SHARDS=str(ctx.shards * 2))])
		`)
		w.WhenISave("server", setting("shards=3"))
		w.ThenItSucceeds()
		w.ThenTheImageEnvironmentSets(environment{"SHARDS": "6"})
	})
}

// TestParameters_SetOverridesParameters specifies that --set name=value overrides a parameter, that lists use
// comma-separated strings and preserve order, and that an empty value gives an empty list.
func TestParameters_SetOverridesParameters(t *testing.T) {
	for _, example := range []struct {
		name string
		sets []string
		env  environment
	}{
		{
			name: "Booleans and integers take their values from --set",
			sets: []string{"verbose=true", "workers=8"},
			env:  environment{"VERBOSE": "True", "WORKERS": "9", "LANGUAGES": `["en"]`},
		},
		{
			name: "A list keeps the order --set gives it",
			sets: []string{"languages=es,en"},
			env:  environment{"VERBOSE": "False", "WORKERS": "5", "LANGUAGES": `["es", "en"]`},
		},
		{
			name: "An empty value gives an empty list",
			sets: []string{"languages="},
			env:  environment{"VERBOSE": "False", "WORKERS": "5", "LANGUAGES": "[]"},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersWorld(t)
			w.GivenTheBuildFileDeclares(settings)
			w.WhenISave("settings", setting(example.sets...))
			w.ThenItSucceeds()
			w.ThenTheImageEnvironmentSets(example.env)
		})
	}
}

// TestParameters_DepAndArgPassArguments specifies that dep() names a target with explicit parameter arguments,
// and that arg() forwards the using target's parameter value as an argument of dep().
func TestParameters_DepAndArgPassArguments(t *testing.T) {
	t.Run("dep() names a target with the arguments it gives", func(t *testing.T) {
		w := newParametersWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(appConfiguringItsParts(nonce))
		w.GivenTheProjectHolds(locales)
		w.GivenTheBuildFileDeclares(`
			layer(name="debug", base="busybox", steps=[add(dep("mode", mode="debug"), "/debug")])
		`)
		w.WhenISave("debug")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("mode", `mode="debug"`): ran})
		w.ThenTheImageHolds(under("/debug"), files{"/debug/mode.txt": nonce + " debug\n"})
	})

	t.Run("A target runs again with the arguments it passes on, and the others are reused", func(t *testing.T) {
		w := newParametersWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(appConfiguringItsParts(nonce))
		w.GivenTheProjectHolds(locales)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.WhenISave("app", setting("mode=debug", "languages=es,en"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{
			task("plain"):                reused,
			task("mode", `mode="debug"`): ran,
			task("mode"):                 reused,
			layer("app", `mode="debug", languages=["es", "en"]`): ran,
		})
		w.ThenTheImageHolds(under("/app"), files{
			"/app/plain.txt":           nonce + "\n",
			"/app/mode.txt":            nonce + " debug\n",
			"/app/locales/en/hi.json":  "hello",
			"/app/locales/es/hi.json":  "hola",
			"/app/locales/es/bye.json": "adios",
		})
		w.ThenTheImageHolds(under("/default"), files{"/default/mode.txt": nonce + " release\n"})
		w.ThenTheImageHolds(under("/release"), files{"/release/mode.txt": nonce + " release\n"})
	})
}

// TestParameters_OnlyUsedParametersAffectReuse specifies that parameters affect reuse through the configured inputs
// and operations, and that changing an unused parameter does not by itself force a target to run again.
func TestParameters_OnlyUsedParametersAffectReuse(t *testing.T) {
	t.Run("A target whose used parameter changes runs again", func(t *testing.T) {
		w := newParametersWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", params={"mode": "a"}, base="busybox", steps=lambda ctx: [run(["echo", %q, ctx.mode])])
		`, testenv.NewNonce()))
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.WhenISave("app", setting("mode=b"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app", `mode="b"`): ran})
	})

	t.Run("A target whose parameter changes but isn't used is reused", func(t *testing.T) {
		w := newParametersWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", params={"unused": "a"}, base="busybox", steps=[run(["echo", %q])])
		`, testenv.NewNonce()))
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.WhenISave("app", setting("unused=b"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app", `unused="b"`): reused})
	})
}

// TestParameters_SetGoesToEveryNamedTargetDeclaringIt specifies that with several targets, build applies each flag
// to every named target declaring that parameter.
func TestParameters_SetGoesToEveryNamedTargetDeclaringIt(t *testing.T) {
	t.Run("Each named target with the parameter takes the flag, and the others are built as they are",
		func(t *testing.T) {
			w := newParametersWorld(t)
			w.GivenTheBuildFileDeclares(`
				layer(name="lib", base="scratch")
				layer(name="app", params={"mode": "release", "verbose": False}, base="scratch")
				layer(name="tests", params={"mode": "release"}, base="scratch")
			`)
			w.WhenIBuild("tests", alongWith("app", "lib"), setting("mode=debug"))
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{
				layer("tests", `mode="debug"`): ranOrReused,
				layer("app", `mode="debug"`):   ranOrReused,
				layer("lib"):                   ranOrReused,
			})
			w.ThenTheTargetWasBuiltInOneConfiguration(layer("tests"))
			w.ThenTheTargetWasBuiltInOneConfiguration(layer("app"))
			w.ThenTheTargetWasBuiltInOneConfiguration(layer("lib"))
		})
}
