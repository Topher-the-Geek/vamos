package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestParamMistakes checks the errors for parameters that a target can't declare.
func TestParamMistakes(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   string
	}{
		{
			name:   "name not an identifier",
			params: `{"build-mode": "release"}`,
			want:   `layer: params: name "build-mode" must be an identifier, since functions read it as ctx.<name>`,
		},
		{
			name:   "name reserved",
			params: `{"platform": "linux/amd64"}`,
			want:   `layer: params: "platform" is built in, as ctx.platform, and can't be declared`,
		},
		{
			name:   "default of another type",
			params: `{"jobs": 4.5}`,
			want:   "layer: params: jobs: default is float, want a string, a bool, an int or a list of strings",
		},
		{
			name:   "list default of another type",
			params: `{"languages": ["en", 1]}`,
			want:   "layer: params: languages: default[1] is int, want string",
		},
		{
			name:   "default not a value",
			params: `{"mode": param(default="fast", values=["debug", "release"])}`,
			want:   `/project/BUILD:1:40: param: default "fast" is not one of its values, "debug", "release"`,
		},
		{
			name:   "list default not values",
			params: `{"languages": param(default=["en", "xx"], values=["en", "es"])}`,
			want:   `param: default has "xx", which is not one of its values, "en", "es"`,
		},
		{
			name:   "values of a bool",
			params: `{"verbose": param(default=False, values=["yes"])}`,
			want:   "param: values is only for a string or list parameter",
		},
		{
			name:   "no values",
			params: `{"mode": param(default="a", values=[])}`,
			want:   "param: values must not be empty",
		},
		{
			name:   "neither default nor type",
			params: `{"mode": param(values=["a"])}`,
			want:   "param: give a default, or else the type of a value that every use must give",
		},
		{
			name:   "unknown type",
			params: `{"mode": param(type="str")}`,
			want:   `param: type "str" is not one of "string", "bool", "int", "list"`,
		},
		{
			name:   "default not of the type",
			params: `{"mode": param(default="a", type="list")}`,
			want:   `param: default is string, but type is "list"`,
		},
		{
			name:   "a function",
			params: `lambda ctx: {}`,
			want:   "/project/BUILD:1:6: layer: params is function, want a dict; params can't depend on ctx",
		},
		{
			name:   "not a dict",
			params: `["mode"]`,
			want:   "layer: params is list, want a dict from each parameter's name to its default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{
				"BUILD": `layer(name="app", params=` + tt.params + `, base="scratch")`,
			})
			vamos(t, fsys, "build", "app", "-C", projectDir).failsWith(t, false, tt.want)
		})
	}

	t.Run("pull", func(t *testing.T) {
		fsys := project(t, map[string]string{
			"BUILD": `pull(name="app", image="busybox", params={"mode": "release"})`,
		})
		vamos(t, fsys, "build", "app", "-C", projectDir).failsWith(t, false,
			"/project/BUILD:1:5: pull: a pull takes no parameters; only its platform configures it")
	})
}

// paramsBuild declares a target with parameters of each type, which the tests of --set name.
const paramsBuild = `
layer(name="lib", base="scratch")
layer(
    name="app",
    params={
        "mode": param(default="release", values=["debug", "release"]),
        "languages": ["en"],
        "verbose": False,
        "jobs": 4,
    },
    base="scratch",
)
layer(name="tests", params={"mode": "release"}, base="scratch")
layer(name="server", params={"shards": param(type="int")}, base="scratch")
`

// TestSetMistakes checks the errors for --set flags that don't give a parameter of the named target a value it
// can have.
func TestSetMistakes(t *testing.T) {
	tests := []struct {
		name    string
		targets []string
		args    []string
		want    string
	}{
		{
			name: "no value",
			args: []string{"--set", "mode"},
			want: `--set "mode": want name=value, as in --set mode=debug`,
		},
		{
			name: "unknown parameter",
			args: []string{"--set", "mod=debug"},
			want: `--set mod=debug: "app" has no parameter "mod"; did you mean "mode"?`,
		},
		{
			name: "unknown parameter, far from any",
			args: []string{"--set", "colour=red"},
			want: `--set colour=red: "app" has no parameter "colour"; its parameters are "mode", "languages", ` +
				`"verbose", "jobs"`,
		},
		{
			name:    "no parameters declared",
			targets: []string{"lib"},
			args:    []string{"--set", "mode=debug"},
			want:    `--set mode=debug: "lib" has no parameter "mode"; it declares none`,
		},
		{
			name:    "no target of several has it",
			targets: []string{"tests", "lib"},
			args:    []string{"--set", "verbose=true"},
			want:    `--set verbose=true: none of the named targets has a parameter "verbose"`,
		},
		{
			name: "platform",
			args: []string{"--set", "platform=linux/arm64"},
			want: "--set platform=linux/arm64: give the platform with --platform",
		},
		{
			name: "given twice",
			args: []string{"--set", "mode=debug", "--set", "mode=release"},
			want: "--set mode=release: mode is given more than once",
		},
		{
			name: "not one of the values",
			args: []string{"--set", "mode=fast"},
			want: `--set mode=fast: mode "fast" is not one of its values, "debug", "release"`,
		},
		{
			name: "not a bool",
			args: []string{"--set", "verbose=yes"},
			want: `--set verbose=yes: verbose is "yes", want true or false`,
		},
		{
			name: "not an int",
			args: []string{"--set", "jobs=many"},
			want: `--set jobs=many: jobs is "many", want an integer`,
		},
		{
			name:    "required parameter not given",
			targets: []string{"server"},
			want: `"server" needs a value for its parameter "shards", which has no default; ` +
				"give it with --set shards=...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{"BUILD": paramsBuild})
			targets := tt.targets
			if targets == nil {
				targets = []string{"app"}
			}
			args := append(append([]string{"build"}, targets...), "-C", projectDir)
			vamos(t, fsys, append(args, tt.args...)...).failsWith(t, false, tt.want)
		})
	}
}

// TestSetSeveralTargets checks that building several targets gives each --set flag to those that have the
// parameter, and leaves out the others.
func TestSetSeveralTargets(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": paramsBuild})
	r := vamos(t, fsys, "build", "tests", "app", "lib", "--set", "mode=debug", "-C", projectDir).ran(t)
	for _, target := range []string{`layer "tests" (mode="debug")`, `layer "app" (mode="debug")`, `layer "lib"`} {
		if !strings.Contains(r.stdout, target+": ") {
			t.Errorf("stdout = %q, want %s built or cached", r.stdout, target)
		}
	}
}

// TestDepMistakes checks the errors for targets named with arguments: from dep() and arg(), from functions of ctx,
// and from what those functions give.
func TestDepMistakes(t *testing.T) {
	// lib takes parameters of each kind, and server one that every use must give.
	const libs = `layer(
    name="lib",
    params={"mode": param(default="release", values=["debug", "release"]), "languages": ["en"], "jobs": 1},
    base="scratch",
)
layer(name="server", params={"shards": param(type="int")}, base="scratch")
glob(name="code", params={"mode": "release"}, include=["app.sh"])
`
	tests := []struct {
		name  string
		build string
		set   []string
		want  []string
	}{
		{
			name:  "unknown parameter",
			build: `layer(name="app", base=dep("lib", langauges=["en"]))`,
			want: []string{
				"/project/BUILD:9:27:",
				`dep: "lib" has no parameter "langauges"; did you mean "languages"?`,
			},
		},
		{
			name:  "parameter of a target that declares none",
			build: `layer(name="app", base=dep("base", mode="debug"))`,
			want:  []string{"/project/BUILD:9:27:", `dep: "base" has no parameter "mode"; it declares none`},
		},
		{
			name:  "value it can't have",
			build: `layer(name="app", base=dep("lib", mode="fast"))`,
			want:  []string{"/project/BUILD:9:27:", `dep: "lib": mode "fast" is not one of its values`},
		},
		{
			name:  "value of another type",
			build: `layer(name="app", base=dep("lib", jobs="2"))`,
			want:  []string{"/project/BUILD:9:27:", `dep: "lib": jobs is string, want int`},
		},
		{
			name:  "required parameter not given",
			build: `layer(name="app", base=dep("server"))`,
			want: []string{
				"/project/BUILD:9:27:",
				`dep: "server" needs a value for its parameter "shards", which has no default; ` +
					`give it with dep("server", shards = ...)`,
			},
		},
		{
			name:  "required parameter of a plain label",
			build: `layer(name="app", base="server")`,
			want: []string{
				"/project/BUILD:9:6:",
				`layer: base: "server" needs a value for its parameter "shards", which has no default`,
			},
		},
		{
			name:  "required parameter of another package's target",
			build: `layer(name="app", base=dep("//other:server", platform=EXEC_PLATFORM))`,
			want:  []string{"/project/BUILD:9:27:", `dep: "//other:server" needs a value for its parameter "shards"`},
		},
		{
			name:  "scratch",
			build: `layer(name="app", base=dep("scratch", mode="debug"))`,
			want:  []string{"/project/BUILD:9:27:", `dep: "scratch" is the empty image, which takes no arguments`},
		},
		{
			name:  "a later target",
			build: `layer(name="app", base=dep("later"))` + "\n" + `layer(name="later", base="scratch")`,
			want: []string{
				"/project/BUILD:9:6:",
				`layer: base "later" is not "scratch" or an earlier pull() or layer()`,
			},
		},
		{
			name: "a later target beside a function",
			build: `layer(name="app", base="later", command=lambda ctx: [])` + "\n" +
				`layer(name="later", base="scratch")`,
			want: []string{
				"/project/BUILD:9:6:",
				`layer: base "later" is not "scratch" or an earlier pull() or layer()`,
			},
		},
		{
			name:  "a glob as a base",
			build: `layer(name="app", base=dep("code"))`,
			want:  []string{"/project/BUILD:9:6:", `layer: base "code" is a glob(), not a pull() or layer()`},
		},
		{
			name:  "platform that doesn't parse",
			build: `layer(name="app", base=dep("lib", platform="x86"))`,
			want:  []string{"/project/BUILD:9:27:", `dep: platform "x86": want os/arch`},
		},
		{
			name:  "platform of another type",
			build: `layer(name="app", base=dep("lib", platform=64))`,
			want: []string{
				"/project/BUILD:9:27:",
				`dep: platform is int, want a platform, such as "linux/arm64", or EXEC_PLATFORM`,
			},
		},
		{
			name: "arg the target doesn't declare",
			build: `layer(
    name="app",
    params={"languages": ["en"]},
    base=dep("lib", languages=arg("langauges")),
)`,
			want: []string{
				"/project/BUILD:12:34:",
				`arg: "app" has no parameter "langauges" to pass on; did you mean "languages"?`,
			},
		},
		{
			name:  "arg in a step the target doesn't declare",
			build: `layer(name="app", base="scratch", steps=[add(dep("code", mode=arg("mode")), "/")])`,
			want: []string{
				"/project/BUILD:9:66:",
				`arg: "app" has no parameter "mode" to pass on; it declares none`,
			},
		},
		{
			name:  "arg given by a function the target doesn't declare",
			build: `layer(name="app", base=lambda ctx: dep("lib", mode=arg("mode")))`,
			want: []string{
				"/project/BUILD:9:55:",
				`arg: "app" has no parameter "mode" to pass on; it declares none (configuring //:app)`,
			},
		},
		{
			name:  "arg of another type",
			build: `layer(name="app", params={"jobs": "1"}, base=dep("lib", jobs=arg("jobs")))`,
			want: []string{
				"/project/BUILD:9:65:",
				`arg: "lib": jobs is string, want int (configuring //:app)`,
			},
		},
		{
			name:  "arg with a value it can't have",
			build: `layer(name="app", params={"mode": "release"}, base=dep("lib", mode=arg("mode")))`,
			set:   []string{"mode=fast"},
			want: []string{
				"/project/BUILD:9:71:",
				`arg: "lib": mode "fast" is not one of its values, "debug", "release" ` +
					`(configuring //:app with mode="fast")`,
			},
		},
		{
			name:  "unknown parameter in a function",
			build: `layer(name="app", params={"mode": "release"}, base="scratch", command=lambda ctx: [ctx.mod])`,
			want: []string{
				"/project/BUILD:9:87:",
				`ctx has no parameter "mod"; the target's parameters are "mode" (did you mean .mode?) ` +
					"(configuring //:app)",
			},
		},
		{
			name:  "function gives the wrong type",
			build: `layer(name="app", params={"mode": "release"}, base="scratch", command=lambda ctx: ctx.mode)`,
			set:   []string{"mode=debug"},
			want: []string{
				"/project/BUILD:9:6:",
				`layer: for parameter "command": got string, want list (configuring //:app with mode="debug")`,
			},
		},
		{
			name:  "function gives a function",
			build: `layer(name="app", base="scratch", command=lambda ctx: lambda: [])`,
			want:  []string{"/project/BUILD:9:6:", "layer: lambda gave a function, not the value of command"},
		},
		{
			name:  "function in a list",
			build: `layer(name="app", base="scratch", command=["echo", lambda ctx: ctx.platform])`,
			want: []string{
				"/project/BUILD:9:6:",
				"layer: command[1] is function, want string; a function of ctx must give the whole list",
			},
		},
		{
			name:  "function gives a missing target",
			build: `layer(name="app", params={"mode": "release"}, base=lambda ctx: "base-" + ctx.mode)`,
			want: []string{
				"/project/BUILD:9:6:",
				`layer: base "base-release": /project/BUILD has no target "base-release"; its targets are "lib", ` +
					`"server", "code", "base", "app" (configuring //:app)`,
			},
		},
		{
			name: "function gives a list holding itself",
			build: `def command(ctx):
    xs = []
    xs.append(xs)
    return xs

layer(name="app", base="scratch", command=command)`,
			want: []string{"/project/BUILD:14:6:", "layer: command[0] is list, want string"},
		},
		{
			name: "function gives a dict holding itself",
			build: `def base(ctx):
    d = {}
    d["k"] = d
    return d

layer(name="app", base=base)`,
			want: []string{"/project/BUILD:14:6:", "layer: base is dict, want a label or dep()"},
		},
		{
			name:  "name given by a function",
			build: `layer(name=lambda ctx: "app", base="scratch", command=lambda ctx: [])`,
			want: []string{
				"/project/BUILD:9:6:",
				"layer: name is function, want string; a target's name can't depend on ctx",
			},
		},
		{
			name:  "unknown parameter beside a function",
			build: `layer(name="app", base="scratch", comand=lambda ctx: [])`,
			want:  []string{"/project/BUILD:9:6:", `layer: unexpected keyword argument "comand"`},
		},
		{
			name:  "function declares a target",
			build: `layer(name="app", base="scratch", steps=lambda ctx: [add(glob(name="g", include=["x"]), "/")])`,
			want: []string{
				"/project/BUILD:9:62:",
				"glob: a function of ctx can't declare a target; declare it as the BUILD file runs",
				"(configuring //:app)",
			},
		},
		{
			name: "cycle through a function",
			build: `layer(name="a", base=lambda ctx: "b")
layer(name="b", base="a")
layer(name="app", base="b")`,
			want: []string{"targets use each other in a cycle: b -> a -> b"},
		},
		{
			name: "cycle through another configuration",
			build: `layer(name="a", params={"mode": "release"}, base=lambda ctx: dep("b", mode="debug"))
layer(name="b", params={"mode": "release"}, base=dep("a", mode=arg("mode")))
layer(name="app", base="a")`,
			want: []string{"targets use each other in a cycle: a -> b -> a"},
		},
		{
			name: "glob given by a function finds no folder",
			build: `glob(
    name="messages",
    params={"languages": ["en"]},
    include=lambda ctx: ["locales/%s/*.json" % l for l in ctx.languages],
)
layer(
    name="app",
    params={"languages": ["en"]},
    base="scratch",
    steps=[add(dep("messages", languages=arg("languages")), "/")],
)`,
			set: []string{"languages=en,xx"},
			want: []string{
				"/project/BUILD:9:5: glob:",
				`(configuring //:messages with languages=["en", "xx"])`,
			},
		},
		{
			name: "task's source given by a function, without include",
			build: `task(name="make", params={"out": ""}, base="scratch", source=lambda ctx: ctx.out)
layer(name="app", base="scratch", steps=[add("make", "/app")])`,
			want: []string{
				"/project/BUILD:10:45:",
				`add: source "make" is a task() without output; give it source and include`,
			},
		},
		{
			name: "glob's platform",
			build: `glob(name="g", include=lambda ctx: [str(ctx.platform)])
layer(name="app", base="scratch", steps=[add("g", "/")])`,
			want: []string{
				"/project/BUILD:9:",
				"ctx has no platform in a glob, whose files are the same for every platform (configuring //:g)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{
				"WORKSPACE":               "",
				"BUILD":                   libs + "layer(name=\"base\", base=\"scratch\")\n" + tt.build,
				"other/BUILD":             libs,
				"app.sh":                  "",
				"locales/en/message.json": "{}",
			})
			args := []string{"build", "app", "-C", projectDir}
			for _, set := range tt.set {
				args = append(args, "--set", set)
			}
			vamos(t, fsys, args...).failsWith(t, false, tt.want...)
		})
	}
}

// TestLoadMistakes checks the errors for load() statements and the .star files they name.
func TestLoadMistakes(t *testing.T) {
	tests := []struct {
		name  string
		build string
		files map[string]string
		want  []string
	}{
		{
			name:  "relative label",
			build: `load(":defs.star", "x")`,
			want:  []string{"/project/BUILD:1:1:", `a .star file is named by its label, which starts with "//"`},
		},
		{
			name:  "not a .star file",
			build: `load("//:BUILD", "x")`,
			want:  []string{"/project/BUILD:1:1:", `such as "//:defs.star"`},
		},
		{
			name:  "missing file",
			build: `load("//:defs.star", "x")`,
			want:  []string{"/project/BUILD:1:1:", "cannot load //:defs.star: no file /project/defs.star"},
		},
		{
			name:  "missing name",
			build: `load("//:defs.star", "nope")`,
			files: map[string]string{"defs.star": "X = 1\n"},
			want:  []string{"/project/BUILD:1:1:", "load: name nope not found in module //:defs.star"},
		},
		{
			name:  "error in the file",
			build: `load("//:defs.star", "X")`,
			files: map[string]string{"defs.star": "X = 1\nY = X + \"a\"\n"},
			want:  []string{"cannot load //:defs.star: /project/defs.star:2:7: unknown binary op: int + string"},
		},
		{
			name:  "declaring as it is loaded",
			build: `load("//:defs.star", "X")`,
			files: map[string]string{"defs.star": `X = 1` + "\n" + `glob(name="code", include=["app.sh"])`},
			want: []string{
				"/project/defs.star:2:5:",
				"glob: a .star file can't declare a target as it is loaded;",
				"declare it in a function that a BUILD file calls",
			},
		},
		{
			name:  "relative label as it is loaded",
			build: `load("//:defs.star", "X")`,
			files: map[string]string{"defs.star": `X = dep("base")`},
			want: []string{
				"/project/defs.star:1:8:",
				`dep: label "base": as a .star file is loaded, labels must start with "//"`,
			},
		},
		{
			name: "argument checked once used",
			build: `load("//:defs.star", "BASE")
layer(name="base", params={"mode": "release"}, base="scratch")
layer(name="app", base=BASE)`,
			files: map[string]string{"defs.star": `BASE = dep("//:base", mod="debug")`},
			want: []string{
				"/project/BUILD:3:6:",
				`layer: base: "//:base" has no parameter "mod"; did you mean "mode"?`,
			},
		},
		{
			name:  "cycle of loads",
			build: `load("//:a.star", "A")`,
			files: map[string]string{
				"a.star": `load("//:b.star", "B")` + "\nA = 1\n",
				"b.star": `load("//:a.star", "A")` + "\nB = 1\n",
			},
			want: []string{"//:a.star loads itself, through the files it loads"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"BUILD": tt.build, "app.sh": ""}
			for name, content := range tt.files {
				files[name] = content
			}
			vamos(t, project(t, files), "build", "app", "-C", projectDir).failsWith(t, false, tt.want...)
		})
	}
}

// paramBuild has targets that take parameters: a glob reads languages, and a task mode, while another task
// takes none. app passes its own parameters on to them, and names "mode" with an argument of its own too. nonce
// makes the targets unique to a test run.
func paramBuild(nonce string) string {
	return pullBusybox + `
glob(
    name="messages",
    params={"languages": ["en"]},
    include=lambda ctx: ["locales/%s" % l for l in ctx.languages],
)

task(
    name="plain",
    base="busybox",
    steps=[run(["sh", "-c", "mkdir /out && echo ` + nonce + ` > /out/plain.txt"])],
    source="/out",
    include=["plain.txt"],
)

task(
    name="mode",
    params={"mode": param(default="release", values=["debug", "release"])},
    base="busybox",
    steps=lambda ctx: [run(["sh", "-c", "mkdir /out && echo ` + nonce + ` %s > /out/mode.txt" % ctx.mode])],
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
        add(dep("mode", mode="release"), "/release"),
        add(dep("messages", languages=arg("languages")), "/app"),
    ],
)
`
}

// TestParams builds a target with the arguments that the command line and dep() give: they reach the functions
// of ctx of the targets they're passed to, a target that takes none is built once, and one named with two
// arguments is built with each.
func TestParams(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD":               paramBuild(nonce),
		"locales/en/hi.json":  "hello",
		"locales/es/hi.json":  "hola",
		"locales/fr/hi.json":  "salut",
		"locales/README.txt":  "not a language",
		"locales/de/hi.json":  "hallo",
		"locales/jp/hi.json":  "konnichiwa",
		"locales/es/bye.json": "adios",
	})
	dir := t.TempDir()

	// By default, mode is release, so "mode" with app's mode is the same target as with "release".
	r := vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, r, map[string]string{
		`task "plain"`: "building",
		`task "mode"`:  "building",
		`layer "app"`:  "building",
	})
	if strings.Count(r.stdout, `task "mode"`) != 1 {
		t.Errorf("stdout = %q, want the task in release mode built once", r.stdout)
	}
	img := readImage(t, filepath.Join(dir, "1.tar"))
	for path, want := range map[string]string{
		"/app/plain.txt":          nonce + "\n",
		"/app/mode.txt":           nonce + " release\n",
		"/release/mode.txt":       nonce + " release\n",
		"/app/locales/en/hi.json": "hello",
	} {
		if got := imageFile(t, img, path); got != want {
			t.Errorf("1.tar: %s = %q, want %q", path, got, want)
		}
	}

	// In debug mode, the task that app passes mode to runs again, and the one named in release mode stays.
	// Lists keep their order, so en,es names other files in the same order.
	r = vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir,
		"--set", "mode=debug", "--set", "languages=es,en").succeeds(t)
	assertTargets(t, r, map[string]string{
		`task "plain"`:               "cached",
		`task "mode" (mode="debug")`: "building",
		`task "mode"`:                "cached",
		`layer "app" (mode="debug", languages=["es", "en"])`: "building",
	})
	img = readImage(t, filepath.Join(dir, "2.tar"))
	for path, want := range map[string]string{
		"/app/mode.txt":            nonce + " debug\n",
		"/release/mode.txt":        nonce + " release\n",
		"/app/locales/en/hi.json":  "hello",
		"/app/locales/es/bye.json": "adios",
	} {
		if got := imageFile(t, img, path); got != want {
			t.Errorf("2.tar: %s = %q, want %q", path, got, want)
		}
	}
	if got := imagePaths(t, img, "/app/locales"); len(got) != 5 {
		t.Errorf("files in /app/locales = %q, want en's and es's alone", got)
	}
}

// TestLoadStarFile builds a target whose BUILD file loads helpers from a .star file: a function that declares a
// target, and one that gives an attribute as a function of ctx. Relative labels in either name targets of
// the package of the BUILD file that calls them.
func TestLoadStarFile(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"WORKSPACE": "",
		"defs.star": `load("//:more.star", "MODES")

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
        steps = lambda ctx: [run(["sh", "-c", "mkdir /out && echo %s %s > /out/echo.txt" % (text, ctx.mode)])],
        source = "/out",
        include = ["echo.txt"],
    )
`,
		"more.star": `MODES = ["debug", "release"]`,
		"app/BUILD": `load("//:defs.star", "echo_task", "in_mode")
` + pullBusybox + `
echo_task("echo", "` + nonce + `")

layer(name="debug", params={"mode": "release"}, base="busybox")

layer(
    name="app",
    base=in_mode("debug", "debug"),
    steps=[add("echo", "/app")],
)
`,
	})
	out := filepath.Join(t.TempDir(), "app.tar")
	r := vamos(t, fsys, "save", "app", out, "-C", filepath.Join(projectDir, "app")).succeeds(t)
	// The layer has no steps, so its key is the same in every run.
	if !strings.Contains(r.stdout, `layer "debug" (mode="debug"): `) {
		t.Errorf("stdout = %q, want the base built in debug mode", r.stdout)
	}
	if got := imageFile(t, readImage(t, out), "/app/echo.txt"); got != nonce+" release\n" {
		t.Errorf("/app/echo.txt = %q, want the task with its default", got)
	}
}
