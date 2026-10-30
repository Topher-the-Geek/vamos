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

// newParametersMistakeWorld starts a scenario of the parameters feature whose parameters, or the arguments given
// them, are wrong, which loading catches before anything runs, so that it needs no buildah.
// Its project pulls busybox.
func newParametersMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// libsTakingParameters declares, on lines 2 to 8, after the pull of busybox, the layer "lib", whose parameters are of
// each kind, the layer "server", whose parameter "shards" every use gives, and the layer "plain", which takes none.
const libsTakingParameters = `
	layer(
	    name="lib",
	    params={"mode": param(default="release", values=["debug", "release"]), "languages": ["en"], "jobs": 1},
	    base="busybox",
	)
	layer(name="server", params={"shards": param(type="int")}, base="busybox")
	layer(name="plain", base="busybox")
`

// appTakingParameters declares the layer "app", whose parameters are of each kind, the layer "tests",
// which takes a mode, and the layer "plain", which takes none, for scenarios of --set.
const appTakingParameters = `
	layer(
	    name="app",
	    params={
	        "mode": param(default="release", values=["debug", "release"]),
	        "languages": ["en"],
	        "verbose": False,
	        "jobs": 4,
	        "regions": param(default=["eu"], values=["eu", "us"]),
	    },
	    base="busybox",
	)
	layer(name="tests", params={"mode": "release"}, base="busybox")
	layer(name="plain", base="busybox")
`

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
// by param(), must be supplied on every use: a use that doesn't give it fails, saying how to give it.
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

	for _, example := range []struct {
		name  string
		build string // on line 9, after libsTakingParameters
		error string
	}{
		{
			name:  "dep() without the value fails, saying how to give it",
			build: `layer(name="app", base=dep("server"))`,
			error: `/project/BUILD:9:27: dep: "server" needs a value for its parameter "shards", ` +
				`which has no default; give it with dep("server", shards = ...)`,
		},
		{
			name:  "A plain label fails, since it gives no values",
			build: `layer(name="app", base="server")`,
			error: `/project/BUILD:9:6: layer: base: "server" needs a value for its parameter "shards", ` +
				`which has no default; give it with dep("server", shards = ...)`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(libsTakingParameters)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}

	t.Run("dep() of another package's target, on another platform, without the value fails, saying how to give it",
		func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheProjectHolds(files{"WORKSPACE": ""})
			w.GivenThePackageDeclares("other",
				`layer(name="server", params={"shards": param(type="int")}, base="scratch")`)
			w.GivenTheBuildFileDeclares(`layer(name="app", base=dep("//other:server", platform=EXEC_PLATFORM))`)
			w.WhenISave("app")
			w.ThenItFailsWith(`/project/BUILD:2:27: dep: "//other:server" needs a value for its parameter "shards", ` +
				`which has no default; give it with dep("//other:server", shards = ...)`)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})

	t.Run("A command that names the target without the value fails, saying how to give it", func(t *testing.T) {
		w := newParametersMistakeWorld(t)
		w.GivenTheBuildFileDeclares(libsTakingParameters)
		w.WhenISave("server")
		w.ThenItFailsWith(`"server" needs a value for its parameter "shards", which has no default; ` +
			"give it with --set shards=...")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
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

// TestParameters_DeclaredInAPlainDictionary specifies that targets other than pulls accept a params dictionary,
// from each parameter's name to its default or param(), which can't be a function of ctx.
func TestParameters_DeclaredInAPlainDictionary(t *testing.T) {
	for _, example := range []struct {
		name   string
		params string
		error  string // after the layer's position
	}{
		{
			name:   "Parameters given by a function fail",
			params: `lambda ctx: {}`,
			error:  "layer: params is function, want a dict; params can't depend on ctx",
		},
		{
			name:   "Parameters given as a list fail",
			params: `["mode"]`,
			error:  "layer: params is list, want a dict from each parameter's name to its default",
		},
		{
			name:   "A parameter whose name isn't a string fails",
			params: `{1: "x"}`,
			error:  "layer: params has a key of type int, want a parameter's name",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", params=` + example.params + `, base="busybox")`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:6: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestParameters_NamesAreReadAsFieldsOfCtx specifies that user parameters are read as ctx.<name>,
// so a name is an identifier, and ctx's own fields, platform, os, arch, variant and exec_platform, are reserved.
func TestParameters_NamesAreReadAsFieldsOfCtx(t *testing.T) {
	for _, example := range []struct {
		name   string
		params string
		error  string // after the layer's position
	}{
		{
			name:   "A name that isn't an identifier fails",
			params: `{"build-mode": "release"}`,
			error:  `layer: params: name "build-mode" must be an identifier, since functions read it as ctx.<name>`,
		},
		{
			name:   "The name platform is built in, and fails",
			params: `{"platform": "linux/amd64"}`,
			error:  `layer: params: "platform" is built in, as ctx.platform, and can't be declared`,
		},
		{
			name:   "The name os is built in, and fails",
			params: `{"os": "linux"}`,
			error:  `layer: params: "os" is built in, as ctx.os, and can't be declared`,
		},
		{
			name:   "The name arch is built in, and fails",
			params: `{"arch": "arm64"}`,
			error:  `layer: params: "arch" is built in, as ctx.arch, and can't be declared`,
		},
		{
			name:   "The name variant is built in, and fails",
			params: `{"variant": "v7"}`,
			error:  `layer: params: "variant" is built in, as ctx.variant, and can't be declared`,
		},
		{
			name:   "The name exec_platform is built in, and fails",
			params: `{"exec_platform": "linux/amd64"}`,
			error:  `layer: params: "exec_platform" is built in, as ctx.exec_platform, and can't be declared`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", params=` + example.params + `, base="busybox")`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:6: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestParameters_TypesAreStringsBooleansIntegersAndLists specifies that the supported types are strings, booleans,
// integers, and lists of strings.
func TestParameters_TypesAreStringsBooleansIntegersAndLists(t *testing.T) {
	for _, example := range []struct {
		name   string
		params string
		error  string // after the layer's position
	}{
		{
			name:   "A default of another type fails",
			params: `{"jobs": 4.5}`,
			error:  "layer: params: jobs: default is float, want a string, a bool, an int or a list of strings",
		},
		{
			name:   "A list default holding something other than strings fails",
			params: `{"languages": ["en", 1]}`,
			error:  "layer: params: languages: default[1] is int, want string",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", params=` + example.params + `, base="busybox")`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:6: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestParameters_ParamGivesADefaultOrATypeThatAgree specifies that param() is given a default or a type,
// one of "string", "bool", "int" or "list", and that if both are given, they must agree.
func TestParameters_ParamGivesADefaultOrATypeThatAgree(t *testing.T) {
	for _, example := range []struct {
		name  string
		param string
		error string // after param()'s position
	}{
		{
			name:  "A param() with neither a default nor a type fails",
			param: `param(values=["a"])`,
			error: "param: give a default, or else the type of a value that every use must give",
		},
		{
			name:  "A type that vamos doesn't have fails, naming those it has",
			param: `param(type="str")`,
			error: `param: type "str" is not one of "string", "bool", "int", "list"`,
		},
		{
			name:  "A default that isn't of the type fails",
			param: `param(default="a", type="list")`,
			error: `param: default is string, but type is "list"`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", params={"mode": ` + example.param + `}, base="busybox")`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:40: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestParameters_ValuesConstrainStringsAndLists specifies that values is a nonempty list of allowed strings,
// for a string or list parameter, that constrains each item of a list.
func TestParameters_ValuesConstrainStringsAndLists(t *testing.T) {
	for _, example := range []struct {
		name  string
		param string
		error string // after param()'s position
	}{
		{
			name:  "A default that isn't one of the values fails",
			param: `param(default="fast", values=["debug", "release"])`,
			error: `param: default "fast" is not one of its values, "debug", "release"`,
		},
		{
			name:  "A list default with an item that isn't one of the values fails",
			param: `param(default=["en", "xx"], values=["en", "es"])`,
			error: `param: default has "xx", which is not one of its values, "en", "es"`,
		},
		{
			name:  "Values of a bool parameter fail",
			param: `param(default=False, values=["yes"])`,
			error: "param: values is only for a string or list parameter",
		},
		{
			name:  "Values of an int parameter fail",
			param: `param(default=1, values=["1"])`,
			error: "param: values is only for a string or list parameter",
		},
		{
			name:  "Empty values fail",
			param: `param(default="a", values=[])`,
			error: "param: values must not be empty",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", params={"mode": ` + example.param + `}, base="busybox")`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:40: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestParameters_SetNamesAParameterOfANamedTarget specifies that --set name=value names a parameter of a target the
// command names: a flag that matches none of the named targets is an error, as is one that gives no value.
func TestParameters_SetNamesAParameterOfANamedTarget(t *testing.T) {
	for _, example := range []struct {
		name    string
		targets []string // the first, and those after it
		sets    []string
		error   string
	}{
		{
			name:    "A misspelled parameter fails, naming the one meant",
			targets: []string{"app"},
			sets:    []string{"mod=debug"},
			error:   `--set mod=debug: "app" has no parameter "mod"; did you mean "mode"?`,
		},
		{
			name:    "An unknown parameter fails, naming the parameters there are",
			targets: []string{"app"},
			sets:    []string{"colour=red"},
			error: `--set colour=red: "app" has no parameter "colour"; its parameters are "mode", "languages", ` +
				`"verbose", "jobs", "regions"`,
		},
		{
			name:    "A parameter of a target that declares none fails, saying so",
			targets: []string{"plain"},
			sets:    []string{"mode=debug"},
			error:   `--set mode=debug: "plain" has no parameter "mode"; it declares none`,
		},
		{
			name:    "A parameter that none of several targets has fails",
			targets: []string{"tests", "plain"},
			sets:    []string{"verbose=true"},
			error:   `--set verbose=true: none of the named targets has a parameter "verbose"`,
		},
		{
			name:    "The platform fails, saying to give it with --platform",
			targets: []string{"app"},
			sets:    []string{"platform=linux/arm64"},
			error:   "--set platform=linux/arm64: give the platform with --platform",
		},
		{
			name:    "A --set without a value fails, saying how to give one",
			targets: []string{"app"},
			sets:    []string{"mode"},
			error:   `--set "mode": want name=value, as in --set mode=debug`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(appTakingParameters)
			w.WhenIBuild(example.targets[0], alongWith(example.targets[1:]...), setting(example.sets...))
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
		})
	}
}

// TestParameters_SetGivesAValueTheParameterCanHave specifies that --set gives a parameter once, a value it can have:
// one of its values, if it has them, an integer for an int, and lowercase true or false for a boolean.
func TestParameters_SetGivesAValueTheParameterCanHave(t *testing.T) {
	for _, example := range []struct {
		name  string
		sets  []string
		error string
	}{
		{
			name:  "A value that isn't one of the parameter's values fails, naming them",
			sets:  []string{"mode=fast"},
			error: `--set mode=fast: mode "fast" is not one of its values, "debug", "release"`,
		},
		{
			name:  "An empty value that isn't one of the parameter's values fails",
			sets:  []string{"mode="},
			error: `--set mode=: mode "" is not one of its values, "debug", "release"`,
		},
		{
			name:  "A list with an item that isn't one of the parameter's values fails, naming them",
			sets:  []string{"regions=eu,xx"},
			error: `--set regions=eu,xx: regions has "xx", which is not one of its values, "eu", "us"`,
		},
		{
			name:  "A boolean that isn't true or false fails",
			sets:  []string{"verbose=yes"},
			error: `--set verbose=yes: verbose is "yes", want true or false`,
		},
		{
			name:  "A boolean that isn't lowercase fails",
			sets:  []string{"verbose=True"},
			error: `--set verbose=True: verbose is "True", want true or false`,
		},
		{
			name:  "An integer that isn't one fails",
			sets:  []string{"jobs=many"},
			error: `--set jobs=many: jobs is "many", want an integer`,
		},
		{
			name:  "An empty integer fails",
			sets:  []string{"jobs="},
			error: `--set jobs=: jobs is "", want an integer`,
		},
		{
			name:  "A parameter given twice fails",
			sets:  []string{"mode=debug", "mode=release"},
			error: "--set mode=release: mode is given more than once",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(appTakingParameters)
			w.WhenISave("app", setting(example.sets...))
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestParameters_ArgumentsAreParametersOfTheTarget specifies that the target that dep() names must declare each
// argument's parameter and take its value, and that the using target must declare the parameter that arg() forwards.
func TestParameters_ArgumentsAreParametersOfTheTarget(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // on line 10, after libsTakingParameters and the glob "code"
		sets  []string
		error string
	}{
		{
			name:  "An argument for a parameter the target doesn't have fails, naming the one meant",
			build: `layer(name="app", base=dep("lib", langauges=["en"]))`,
			error: `/project/BUILD:10:27: dep: "lib" has no parameter "langauges"; did you mean "languages"?`,
		},
		{
			name:  "An argument for a target that declares no parameters fails, saying so",
			build: `layer(name="app", base=dep("plain", mode="debug"))`,
			error: `/project/BUILD:10:27: dep: "plain" has no parameter "mode"; it declares none`,
		},
		{
			name:  "An argument for scratch fails, since it takes none",
			build: `layer(name="app", base=dep("scratch", mode="debug"))`,
			error: `/project/BUILD:10:27: dep: "scratch" is the empty image, which takes no arguments`,
		},
		{
			name:  "A value the parameter can't have fails",
			build: `layer(name="app", base=dep("lib", mode="fast"))`,
			error: `/project/BUILD:10:27: dep: "lib": mode "fast" is not one of its values, "debug", "release"`,
		},
		{
			name:  "A value of another type fails",
			build: `layer(name="app", base=dep("lib", jobs="2"))`,
			error: `/project/BUILD:10:27: dep: "lib": jobs is string, want int`,
		},
		{
			name: "An arg() of a parameter the using target doesn't have fails, naming the one meant",
			build: `layer(name="app", params={"languages": ["en"]}, ` +
				`base=dep("lib", languages=arg("langauges")))`,
			error: `/project/BUILD:10:78: arg: "app" has no parameter "langauges" to pass on; ` +
				`did you mean "languages"?`,
		},
		{
			name:  "An arg() in a step, of a parameter the using target doesn't have, fails",
			build: `layer(name="app", base="busybox", steps=[add(dep("code", mode=arg("mode")), "/")])`,
			error: `/project/BUILD:10:66: arg: "app" has no parameter "mode" to pass on; it declares none`,
		},
		{
			name:  "An arg() given by a function, of a parameter the using target doesn't have, fails",
			build: `layer(name="app", base=lambda ctx: dep("lib", mode=arg("mode")))`,
			error: `/project/BUILD:10:55: arg: "app" has no parameter "mode" to pass on; it declares none ` +
				"(configuring //:app)",
		},
		{
			name:  "An arg() whose value is of another type fails",
			build: `layer(name="app", params={"jobs": "1"}, base=dep("lib", jobs=arg("jobs")))`,
			error: `/project/BUILD:10:65: arg: "lib": jobs is string, want int (configuring //:app)`,
		},
		{
			name:  "An arg() whose value the parameter can't have fails, naming the configuration",
			build: `layer(name="app", params={"mode": "release"}, base=dep("lib", mode=arg("mode")))`,
			sets:  []string{"mode=fast"},
			error: `/project/BUILD:10:71: arg: "lib": mode "fast" is not one of its values, "debug", "release" ` +
				`(configuring //:app with mode="fast")`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(libsTakingParameters)
			w.GivenTheBuildFileDeclares(`glob(name="code", params={"mode": "release"}, include=["app.sh"])`)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
			w.WhenISave("app", setting(example.sets...))
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestParameters_FunctionsOfCtxGiveWholeAttributes specifies that a function of ctx reads user parameters as
// ctx.<name> and returns the whole attribute, such as a complete steps or include list, whose entries can't be
// functions themselves, and that its mistakes name the configuration it was given.
func TestParameters_FunctionsOfCtxGiveWholeAttributes(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // on line 2, after the pull of busybox
		sets  []string
		error string
	}{
		{
			name:  "Reading a parameter the target doesn't have fails, naming the one meant",
			build: `layer(name="app", params={"mode": "release"}, base="busybox", command=lambda ctx: [ctx.mod])`,
			error: `/project/BUILD:2:87: ctx has no parameter "mod"; the target's parameters are "mode" ` +
				"(did you mean .mode?) (configuring //:app)",
		},
		{
			name:  "A function that gives a value of the wrong type fails, naming the configuration",
			build: `layer(name="app", params={"mode": "release"}, base="busybox", command=lambda ctx: ctx.mode)`,
			sets:  []string{"mode=debug"},
			error: `/project/BUILD:2:6: layer: for parameter "command": got string, want list ` +
				`(configuring //:app with mode="debug")`,
		},
		{
			name:  "A function that gives a function fails",
			build: `layer(name="app", base="busybox", command=lambda ctx: lambda: [])`,
			error: "/project/BUILD:2:6: layer: lambda gave a function, not the value of command (configuring //:app)",
		},
		{
			name:  "A function as an entry of a list fails, saying to give the whole list",
			build: `layer(name="app", base="busybox", command=["echo", lambda ctx: ctx.platform])`,
			error: "/project/BUILD:2:6: layer: command[1] is function, want string; " +
				"a function of ctx must give the whole list",
		},
		{
			name: "A function that gives a list holding itself fails",
			build: `
				def command(ctx):
				    xs = []
				    xs.append(xs)
				    return xs

				layer(name="app", base="busybox", command=command)
			`,
			error: "/project/BUILD:7:6: layer: command[0] is list, want string (configuring //:app)",
		},
		{
			name: "A function that gives a dict holding itself as a base fails",
			build: `
				def base(ctx):
				    d = {}
				    d["k"] = d
				    return d

				layer(name="app", base=base)
			`,
			error: "/project/BUILD:7:6: layer: base is dict, want a label or dep() (configuring //:app)",
		},
		{
			name:  "A function that names a target there isn't fails, naming the configuration",
			build: `layer(name="app", params={"mode": "release"}, base=lambda ctx: "base-" + ctx.mode)`,
			error: `/project/BUILD:2:6: layer: base "base-release": /project/BUILD has no target "base-release"; ` +
				`its targets are "busybox", "app" (configuring //:app)`,
		},
		{
			name: "A glob whose function selects a folder there isn't fails, naming the configuration",
			build: `
				glob(
				    name="messages",
				    params={"languages": ["en"]},
				    include=lambda ctx: ["locales/%s/*.json" % l for l in ctx.languages],
				)
				layer(
				    name="app",
				    params={"languages": ["en"]},
				    base="busybox",
				    steps=[add(dep("messages", languages=arg("languages")), "/")],
				)
			`,
			sets: []string{"languages=en,xx"},
			error: `/project/BUILD:2:5: glob: include "locales/xx/*.json": "locales/xx" not found in /project ` +
				`(configuring //:messages with languages=["en", "xx"])`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.GivenTheProjectHolds(files{"locales/en/hi.json": "hello"})
			w.WhenISave("app", setting(example.sets...))
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestParameters_FunctionsOfCtxOnlyConfigure specifies that functions of ctx configure existing targets:
// they can't declare new targets, and a target's name and params can't be functions of ctx.
func TestParameters_FunctionsOfCtxOnlyConfigure(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // on line 2, after the pull of busybox
		error string
	}{
		{
			name:  "A function that declares a target fails, saying to declare it as the file runs",
			build: `layer(name="app", base="busybox", steps=lambda ctx: [add(glob(name="g", include=["x"]), "/")])`,
			error: "/project/BUILD:2:62: glob: a function of ctx can't declare a target; " +
				"declare it as the BUILD file runs (configuring //:app)",
		},
		{
			name:  "A name given by a function fails",
			build: `layer(name=lambda ctx: "app", base="busybox", command=lambda ctx: [])`,
			error: "/project/BUILD:2:6: layer: name is function, want string; a target's name can't depend on ctx",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newParametersMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}
