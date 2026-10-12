package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/afero"
)

// validBuild is a BUILD file that would build, were the command line right.
// addCode declares a layer "app" that adds the glob "code", so that building it checks the glob's files.
const addCode = `layer(name="app", base="busybox", steps=[add("code", "/app")])`

var validBuild = map[string]string{
	"BUILD": pullBusybox + `layer(name="app", base="busybox", command=["echo", "hello"])`,
}

func TestCommandLineMistakes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "push without target",
			args: []string{"push"},
			want: []string{"missing target and image name", "vamos push server ghcr.io/me/app:1.0"},
		},
		{
			name: "push without image",
			args: []string{"push", "app"},
			want: []string{`missing image name after target "app"`, "vamos push server ghcr.io/me/app:1.0"},
		},
		{
			name: "load without image",
			args: []string{"load", "app"},
			want: []string{`missing image name after target "app"`, "vamos load server app:dev"},
		},
		{
			name: "save without file",
			args: []string{"save", "app"},
			want: []string{`missing output file after target "app"`, "vamos save server app.tar"},
		},
		{
			name: "export without folder",
			args: []string{"export", "app"},
			want: []string{`missing folder after target "app"`, "vamos export codegen out"},
		},
		{
			name: "push with two images",
			args: []string{"push", "app", "app:1", "app:2"},
			want: []string{"expected a target and one image name, got 3 arguments: app app:1 app:2"},
		},
		{
			name: "save with two files",
			args: []string{"save", "app", "a.tar", "b.tar"},
			want: []string{"expected a target and one output file, got 3 arguments"},
		},
		{
			name: "unknown flag",
			args: []string{"load", "app", "app", "--bogus"},
			want: []string{"unknown flag: --bogus"},
		},
		{
			name: "unknown shorthand",
			args: []string{"push", "-x", "app", "app"},
			want: []string{"unknown shorthand flag: 'x'"},
		},
		{
			name: "tag without value",
			args: []string{"save", "app", "app.tar", "--tag"},
			want: []string{"flag needs an argument: --tag"},
		},
		{
			name: "tag on push",
			args: []string{"push", "app", "app", "--tag", "app:1"},
			want: []string{"unknown flag: --tag"},
		},
		{
			name: "dir without value",
			args: []string{"load", "app", "app", "-C"},
			want: []string{"flag needs an argument: 'C'"},
		},
		{
			name: "tls-verify not a bool",
			args: []string{"push", "app", "app", "--tls-verify=maybe"},
			want: []string{`invalid argument "maybe"`, "--tls-verify"},
		},
		{
			name: "push-from not a side",
			args: []string{"push", "app", "app", "--push-from=remote"},
			want: []string{`invalid argument "remote" for "--push-from" flag: want "executor" or "client"`},
		},
		{
			name: "replace on save",
			args: []string{"save", "app", "app.tar", "--replace"},
			want: []string{"unknown flag: --replace"},
		},
		{
			name: "build without targets",
			args: []string{"build"},
			want: []string{"missing target name", "vamos build packages"},
		},
		{
			name: "rebuild on push",
			args: []string{"push", "app", "app", "--rebuild"},
			want: []string{"unknown flag: --rebuild"},
		},
		{
			name: "prune with argument",
			args: []string{"prune", "all"},
			want: []string{"vamos prune takes no arguments, got: all"},
		},
		{
			name: "timestamp not a time",
			args: []string{"save", "app", "app.tar", "--timestamp", "yesterday"},
			want: []string{`invalid argument "yesterday"`, "--timestamp", "want Unix seconds"},
		},
		{
			name: "timestamp before 1970",
			args: []string{"build", "app", "--timestamp=-1"},
			want: []string{`invalid argument "-1"`, "must not be before 1970"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vamos(t, project(t, validBuild), tt.args...).failsWith(t, true, tt.want...)
		})
	}
}

// TestBuildTargetMistakes checks that naming targets `vamos build` can't build is reported before
// anything is built.
func TestBuildTargetMistakes(t *testing.T) {
	build := pullBusybox + `
glob(name="code", include=["app.sh"])
layer(name="packages", base="busybox", steps=[add("code", "/app")])
`
	tests := []struct {
		name    string
		targets []string
		want    []string
	}{
		{
			name:    "misspelled target",
			targets: []string{"pakcages"},
			want:    []string{`/project/BUILD has no target "pakcages"; did you mean "packages"?`},
		},
		{
			name:    "unknown target",
			targets: []string{"packages", "server"},
			want:    []string{`/project/BUILD has no target "server"; its targets are "busybox", "code", "packages"`},
		},
		{
			name:    "label without its leading slashes",
			targets: []string{"backend:test"},
			want: []string{
				`/project/BUILD has no target "backend:test"; a label starts with "//", as in "//backend:test"`,
			},
		},
		{
			name:    "glob",
			targets: []string{"code"},
			want:    []string{`"code" is a glob(), whose files are read by the targets that add them`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{"BUILD": build, "app.sh": ""})
			args := append([]string{"build", "-C", projectDir}, tt.targets...)
			vamos(t, fsys, args...).failsWith(t, false, tt.want...)
		})
	}
}

func TestUnknownCommands(t *testing.T) {
	vamos(t, project(t, validBuild), "bulid").
		failsWith(t, true, `unknown command "bulid"`)
	vamos(t, project(t, validBuild), "pusj", "app").
		failsWith(t, true, `unknown command "pusj"; did you mean "push"?`)
}

func TestNoCommandShowsHelp(t *testing.T) {
	r := vamos(t, project(t, validBuild))
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, want := range []string{"build", "push", "load", "save", "export", "prune"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help = %q, want it to list %q", r.stdout, want)
		}
	}
}

func TestInvalidImageNames(t *testing.T) {
	out := filepath.Join(t.TempDir(), "app.tar")
	digest := "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "uppercase push",
			args: []string{"push", "app", "MyApp"},
			want: []string{`invalid image name "MyApp"`, "must be lowercase"},
		},
		{
			name: "uppercase load",
			args: []string{"load", "app", "MyApp"},
			want: []string{`invalid image name "MyApp"`, "must be lowercase"},
		},
		{
			name: "space",
			args: []string{"load", "app", "my app"},
			want: []string{`invalid image name "my app"`},
		},
		{
			name: "empty tag",
			args: []string{"load", "app", "app:"},
			want: []string{`invalid image name "app:"`},
		},
		{
			name: "digest",
			args: []string{"push", "app", "app" + digest},
			want: []string{"must not include a digest"},
		},
		{
			name: "invalid save tag",
			args: []string{"save", "app", out, "-t", "Bad Tag"},
			want: []string{`invalid image name "Bad Tag"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(tt.args, "-C", projectDir)
			vamos(t, project(t, validBuild), args...).failsWith(t, false, tt.want...)
		})
	}
	assertNoFiles(t, filepath.Dir(out))
}

func TestProjectDirectoryMistakes(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	t.Run("missing directory", func(t *testing.T) {
		vamos(t, project(t, validBuild), "load", "app", "app", "-C", "/nope").
			failsWith(t, false, "directory /nope does not exist")
	})
	t.Run("directory is a file", func(t *testing.T) {
		vamos(t, project(t, validBuild), "load", "app", "app", "-C", "/project/BUILD").
			failsWith(t, false, "/project/BUILD is not a directory")
	})
	t.Run("no BUILD file", func(t *testing.T) {
		vamos(t, project(t, map[string]string{"script.sh": "echo hi"}), "load", "app", "app", "-C", projectDir).
			failsWith(t, false, "no BUILD file in /project")
	})
	t.Run("relative directory", func(t *testing.T) {
		// -C resolves against the working directory, as other tools do.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		vamos(t, afero.NewMemMapFs(), "load", "app", "app", "-C", "sub").
			failsWith(t, false, "directory "+filepath.Join(wd, "sub")+" does not exist")
	})
}

func TestBuildFileMistakes(t *testing.T) {
	// Targets follow pullBusybox, so they start on line 2, or line 3 after withGlob.
	const withGlob = `glob(name="code", include=["app.sh"])` + "\n"
	tests := []struct {
		name  string
		build string
		files map[string]string
		want  []string
	}{
		{
			name:  "syntax error",
			build: pullBusybox + "layer(\n    name=\"app\",\n    base=\"busybox\",\n    command=[\"echo\", \"hi\"\n)\n",
			want:  []string{"/project/BUILD:6:", "got ')', want ']'"},
		},
		{
			name:  "misspelled function",
			build: pullBusybox + `bulid(name="app", base="busybox")`,
			want:  []string{"/project/BUILD:2:1:", "undefined: bulid"},
		},
		{
			name:  "misspelled argument",
			build: pullBusybox + `layer(name="app", base="busybox", comand=["echo"])`,
			want:  []string{"/project/BUILD:2:6:", `unexpected keyword argument "comand"`},
		},
		{
			name:  "missing name",
			build: pullBusybox + `layer(base="busybox")`,
			want:  []string{"/project/BUILD:2:6:", "missing argument for name"},
		},
		{
			name:  "empty name",
			build: pullBusybox + `layer(name="", base="busybox")`,
			want:  []string{"/project/BUILD:2:6:", "layer: name must not be empty"},
		},
		{
			name:  "missing base",
			build: pullBusybox + `layer(name="app", command=["echo"])`,
			want:  []string{"/project/BUILD:2:6:", "missing argument for base"},
		},
		{
			name:  "unknown base",
			build: pullBusybox + `layer(name="app", base="busybx")`,
			want: []string{
				"/project/BUILD:2:6:",
				`layer: base "busybx" is not "scratch" or an earlier pull() or layer(); earlier targets are "busybox"`,
			},
		},
		{
			name:  "base names an image instead of a target",
			build: `layer(name="app", base="docker.io/library/busybox:1.36")`,
			want: []string{
				"/project/BUILD:1:6:",
				`base "docker.io/library/busybox:1.36" is not "scratch" or an earlier pull() or layer(); ` +
					"there are none before this",
			},
		},
		{
			name:  "base declared later",
			build: pullBusybox + "layer(name=\"app\", base=\"deps\")\nlayer(name=\"deps\", base=\"busybox\")\n",
			want:  []string{"/project/BUILD:2:6:", `base "deps" is not "scratch" or an earlier pull() or layer()`},
		},
		{
			name:  "name used twice",
			build: pullBusybox + `layer(name="busybox", base="busybox")`,
			want:  []string{"/project/BUILD:2:6:", `layer: name "busybox" is already used at /project/BUILD:1:5`},
		},
		{
			name:  "target named scratch",
			build: `pull(name="scratch", image="busybox")`,
			want:  []string{"/project/BUILD:1:5:", `pull: name "scratch" is reserved for the empty base image`},
		},
		{
			name:  "pull without image",
			build: `pull(name="busybox")`,
			want:  []string{"/project/BUILD:1:5:", "missing argument for image"},
		},
		{
			name:  "empty image",
			build: `pull(name="busybox", image="")`,
			want:  []string{"/project/BUILD:1:5:", "pull: image must not be empty"},
		},
		{
			name:  "invalid image",
			build: `pull(name="busybox", image="Busy Box")`,
			want:  []string{"/project/BUILD:1:5:", `pull: invalid image "Busy Box"`},
		},
		{
			name:  "tag in image",
			build: `pull(name="busybox", image="busybox:1.36")`,
			want:  []string{"/project/BUILD:1:5:", `image "busybox:1.36" must not include a tag; use tag="1.36"`},
		},
		{
			name:  "digest in image",
			build: `pull(name="busybox", image="busybox@sha256:` + strings.Repeat("0", 64) + `")`,
			want:  []string{"/project/BUILD:1:5:", "must not include a digest"},
		},
		{
			name:  "invalid tag",
			build: `pull(name="busybox", image="busybox", tag="1 36")`,
			want:  []string{"/project/BUILD:1:5:", `pull: invalid tag "1 36"`},
		},
		{
			name:  "empty pull name",
			build: `pull(name="", image="busybox")`,
			want:  []string{"/project/BUILD:1:5:", "pull: name must not be empty"},
		},
		{
			name:  "old arguments",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", sources=["code"], destination="/app")`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:6:", `unexpected keyword argument "sources"`},
		},
		{
			name:  "steps not a list",
			build: pullBusybox + `layer(name="app", base="busybox", steps=run(["true"]))`,
			want:  []string{"/project/BUILD:2:6:", "steps", "got step, want list"},
		},
		{
			name:  "step not a step",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[run(["true"]), "make"])`,
			want: []string{
				"/project/BUILD:2:6:",
				"layer: steps[1] is string, want a step, such as add() or run()",
			},
		},
		{
			name:  "add source not a name",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[add(1, "/app")])`,
			want:  []string{"/project/BUILD:2:45:", "add: source is int, want a label or dep()"},
		},
		{
			name: "add sources as a list",
			build: pullBusybox + withGlob +
				`layer(name="app", base="busybox", steps=[add(["code"], "/app")])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:45:", "add: source is list, want a label or dep()"},
		},
		{
			name:  "source names a file",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[add("app.sh", "/app")])`,
			files: map[string]string{"app.sh": "echo hi"},
			want: []string{
				"/project/BUILD:2:45:",
				`add: source "app.sh" is not an earlier glob() or task(); there are none before this`,
			},
		},
		{
			name:  "unknown source",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", steps=[add("cod", "/app")])`,
			files: map[string]string{"app.sh": "echo hi"},
			want: []string{
				"/project/BUILD:3:45:",
				`add: source "cod" is not an earlier glob() or task(); earlier globs and tasks are "code"`,
			},
		},
		{
			name:  "source names a pull",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[add("busybox", "/app")])`,
			want:  []string{"/project/BUILD:2:45:", `add: source "busybox" is a pull(), not a glob() or task()`},
		},
		{
			name:  "base names a glob",
			build: pullBusybox + withGlob + `layer(name="app", base="code")`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:6:", `layer: base "code" is a glob(), not a pull() or layer()`},
		},
		{
			name:  "dest not a string",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", steps=[add("code", ["/app"])])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:45:", "dest", "got list, want string"},
		},
		{
			name:  "command item not a string",
			build: pullBusybox + `layer(name="app", base="busybox", command=["sleep", 5])`,
			want:  []string{"/project/BUILD:2:6:", "command[1] is int, want string"},
		},
		{
			name:  "add without dest",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", steps=[add("code")])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:45:", "missing argument for dest"},
		},
		{
			name:  "relative dest",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", steps=[add("code", "app")])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:45:", `add: dest "app" must be an absolute path`},
		},
		{
			name:  "run not a list",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[run("make")])`,
			want:  []string{"/project/BUILD:2:45:", "run: for parameter argv: got string, want list"},
		},
		{
			name:  "run item not a string",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[run(["sleep", 5])])`,
			want:  []string{"/project/BUILD:2:45:", "run: argv[1] is int, want string"},
		},
		{
			name:  "empty run",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[run([])])`,
			want:  []string{"/project/BUILD:2:45:", "run: argv must not be empty"},
		},
		{
			name:  "env without names",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[env({"LANG": "C"})])`,
			want: []string{
				"/project/BUILD:2:45:",
				`env: give variables by name, as NAME="value", or a dict of them as **vars`,
			},
		},
		{
			name:  "env without variables",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[env()])`,
			want:  []string{"/project/BUILD:2:45:", "env: no variables given"},
		},
		{
			name:  "env value not a string",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[env(PORT=8080)])`,
			want:  []string{"/project/BUILD:2:45:", "env: PORT is int, want string"},
		},
		{
			name:  "env empty name",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[env(**{"": "x"})])`,
			want:  []string{"/project/BUILD:2:45:", "env: a variable's name is empty"},
		},
		{
			name:  "build_env name with =",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[build_env(**{"A=B": "x"})])`,
			want: []string{
				"/project/BUILD:2:51:",
				`build_env: variable name "A=B" must not contain "=" or a NUL byte`,
			},
		},
		{
			name:  "env name with NUL",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[env(**{"A\x00B": "x"})])`,
			want: []string{
				"/project/BUILD:2:45:",
				`env: variable name "A\x00B" must not contain "=" or a NUL byte`,
			},
		},
		{
			name:  "env value with NUL",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[env(A="x\x00y")])`,
			want:  []string{"/project/BUILD:2:45:", "env: A's value must not contain a NUL byte"},
		},
		{
			name:  "env value with a lone $",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[env(PRICE="$5")])`,
			want: []string{
				"/project/BUILD:2:45:",
				`env: PRICE="$5": "$" must be followed by a variable's name or "{"; write "$$" for "$"`,
			},
		},
		{
			name:  "build_env value with a default",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[build_env(A="${B:-c}")])`,
			want: []string{
				"/project/BUILD:2:51:",
				`build_env: A="${B:-c}": "${" must be followed by a variable's name and "}"; write "$$" for "$"`,
			},
		},
		{
			name: "env name given twice",
			build: pullBusybox +
				`layer(name="app", base="busybox", steps=[env(LANG="C", **{"LANG": "C.UTF-8"})])`,
			want: []string{"/project/BUILD:2:45:", `env: variable "LANG" is given twice`},
		},
		{
			name:  "relative workdir",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[workdir("app")])`,
			want:  []string{"/project/BUILD:2:49:", `workdir: path "app" must be an absolute path`},
		},
		{
			name:  "workdir relative before a variable",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[workdir("app/$DIR")])`,
			want:  []string{"/project/BUILD:2:49:", `workdir: path "app/$DIR" must be an absolute path`},
		},
		{
			name: "dest with a default",
			build: pullBusybox + withGlob +
				`layer(name="app", base="busybox", steps=[add("code", "${D:-/app}")])`,
			files: map[string]string{"app.sh": "echo hi"},
			want: []string{
				"/project/BUILD:3:45:",
				`add: dest "${D:-/app}": "${" must be followed by a variable's name and "}"; write "$$" for "$"`,
			},
		},
		{
			name:  "glob without include",
			build: `glob(name="code")`,
			want:  []string{"/project/BUILD:1:5:", "missing argument for include"},
		},
		{
			name:  "empty include",
			build: `glob(name="code", include=[])`,
			want:  []string{"/project/BUILD:1:5:", "glob: include must not be empty"},
		},
		{
			name:  "empty include path",
			build: `glob(name="code", include=["app.sh", ""])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:1:5:", `glob: include[1] is empty; use "." for the whole directory`},
		},
		{
			name:  "empty task include path",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=[""])`,
			want:  []string{"/project/BUILD:2:5:", `task: include[0] is empty; use "." for the whole directory`},
		},
		{
			name:  "include item not a string",
			build: `glob(name="code", include=[1])`,
			want:  []string{"/project/BUILD:1:5:", "glob: include[0] is int, want string"},
		},
		{
			name:  "bad include pattern",
			build: `glob(name="code", include=["src/[a"])`,
			want:  []string{"/project/BUILD:1:5:", `glob: include "src/[a": syntax error in pattern`},
		},
		{
			name:  "double star within a name",
			build: `glob(name="code", include=["src/**.ts"])`,
			want: []string{
				"/project/BUILD:1:5:",
				`glob: include "src/**.ts": "**" must be a whole part of the path, as in "src/**/*.ts"`,
			},
		},
		{
			name:  "exclude outside project",
			build: `glob(name="code", include=["."], exclude=["../secret"])`,
			want:  []string{"/project/BUILD:1:5:", `glob: exclude "../secret" is outside /project`},
		},
		{
			name:  "empty exclude path",
			build: `glob(name="code", include=["."], exclude=[""])`,
			want:  []string{"/project/BUILD:1:5:", `glob: exclude[0] is empty`},
		},
		{
			name:  "mode mask out of range",
			build: `glob(name="code", include=["."], mode_mask=0o10000)`,
			want:  []string{"/project/BUILD:1:5:", "glob: mode_mask 0o10000 must be between 0 and 0o7777"},
		},
		{
			name:  "mode mask negative",
			build: `glob(name="code", include=["."], mode_mask=-1)`,
			want:  []string{"/project/BUILD:1:5:", "glob: mode_mask -0o1 must be between 0 and 0o7777"},
		},
		{
			name:  "mode mask not an int",
			build: `glob(name="code", include=["."], mode_mask="755")`,
			want:  []string{"/project/BUILD:1:5:", `glob: for parameter "mode_mask": got string, want int`},
		},
		{
			name:  "limit not positive",
			build: `glob(name="code", include=["."], max_file_count=0)`,
			want:  []string{"/project/BUILD:1:5:", "glob: max_file_count must be positive, not 0"},
		},
		{
			name:  "file size limit not positive",
			build: `glob(name="code", include=["."], max_file_size=-1)`,
			want:  []string{"/project/BUILD:1:5:", "glob: max_file_size must be positive, not -1"},
		},
		{
			name:  "total size limit not positive",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["."], max_total_size=0)`,
			want:  []string{"/project/BUILD:2:5:", "task: max_total_size must be positive, not 0"},
		},
		{
			name:  "missing pattern prefix",
			build: `glob(name="code", include=["scr/**"])` + "\n" + pullBusybox + addCode,
			want:  []string{"/project/BUILD:1:5:", `glob: include "scr/**": "scr" not found in /project`},
		},
		{
			name: "owner with a group",
			build: pullBusybox + withGlob +
				`layer(name="app", base="busybox", steps=[add("code", "/app", owner="app:app")])`,
			want: []string{
				"/project/BUILD:3:45:",
				`add: owner "app:app" must not contain ":"; give owner and group apart`,
			},
		},
		{
			name:  "missing file",
			build: withGlob + pullBusybox + addCode,
			want:  []string{"/project/BUILD:1:5:", `glob: include "app.sh" not found in /project`},
		},
		{
			name: "missing file in a glob the target needs",
			build: `glob(name="docs", include=["README"])` + "\n" + withGlob + pullBusybox + addCode + "\n" +
				`layer(name="docs-layer", base="busybox", steps=[add("docs", "/docs")])`,
			want: []string{"/project/BUILD:2:5:", `glob: include "app.sh" not found in /project`},
		},
		{
			name:  "absolute include",
			build: `glob(name="code", include=["/etc/passwd"])`,
			want:  []string{`include "/etc/passwd" must be a path relative to /project`},
		},
		{
			name:  "include outside project",
			build: `glob(name="code", include=["../secret"])`,
			want:  []string{`include "../secret" is outside /project`},
		},
		{
			name:  "empty glob name",
			build: `glob(name="", include=["app.sh"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:1:5:", "glob: name must not be empty"},
		},
		{
			name:  "glob name used twice",
			build: pullBusybox + `glob(name="busybox", include=["app.sh"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:2:5:", `glob: name "busybox" is already used at /project/BUILD:1:5`},
		},
		{
			name:  "task include without source",
			build: pullBusybox + `task(name="make", base="busybox", include=["app"])`,
			want:  []string{"/project/BUILD:2:5:", "task: include needs source, the directory it is relative to"},
		},
		{
			name:  "task source without include",
			build: pullBusybox + `task(name="make", base="busybox", source="/out")`,
			want:  []string{"/project/BUILD:2:5:", "task: source needs include, the files to collect from it"},
		},
		{
			name: "add a task without output",
			build: pullBusybox + `task(name="test", base="busybox", steps=[run(["true"])])` + "\n" +
				`layer(name="app", base="busybox", steps=[add("test", "/app")])`,
			want: []string{
				"/project/BUILD:3:45:",
				`add: source "test" is a task() without output; give it source and include`,
			},
		},
		{
			name:  "relative task source",
			build: pullBusybox + `task(name="make", base="busybox", source="out", include=["app"])`,
			want:  []string{"/project/BUILD:2:5:", `task: source "out" must be an absolute path`},
		},
		{
			name:  "task include outside source",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["../etc"])`,
			want:  []string{"/project/BUILD:2:5:", `task: include "../etc" is outside /out`},
		},
		{
			name:  "task exclude without source",
			build: pullBusybox + `task(name="make", base="busybox", exclude=["*.map"])`,
			want:  []string{"/project/BUILD:2:5:", "task: exclude needs source and include, the files to collect"},
		},
		{
			name:  "task limit without source",
			build: pullBusybox + `task(name="make", base="busybox", max_file_size=1)`,
			want:  []string{"/project/BUILD:2:5:", "task: max_file_size needs source and include"},
		},
		{
			name:  "bad task exclude pattern",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["."], exclude=["[x"])`,
			want:  []string{"/project/BUILD:2:5:", `task: exclude "[x": syntax error in pattern`},
		},
		{
			name:  "task command",
			build: pullBusybox + `task(name="make", base="busybox", command=["make"], source="/out", include=["app"])`,
			want:  []string{"/project/BUILD:2:5:", `unexpected keyword argument "command"`},
		},
		{
			name: "base names a task",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["app"])` + "\n" +
				`layer(name="app", base="make")`,
			want: []string{"/project/BUILD:3:6:", `layer: base "make" is a task(), not a pull() or layer()`},
		},
		{
			name: "run outside steps",
			build: pullBusybox + `run(["true"])` + "\n" +
				`layer(name="app", base="busybox")`,
			want: []string{
				`/project/BUILD:2:4: run: run(["true"]) is not in the steps of any layer() or task()`,
			},
		},
		{
			name: "run outside steps, beside steps given by a function",
			build: pullBusybox + `layer(name="lib", base="busybox", steps=[run(["true"])], command=lambda ctx: [])` +
				"\n" + `run(["false"])` + "\n" +
				`layer(name="app", base="busybox")`,
			want: []string{
				`/project/BUILD:3:4: run: run(["false"]) is not in the steps of any layer() or task()`,
			},
		},
		{
			name: "add outside steps",
			build: pullBusybox + withGlob + `add("code", "/app")` + "\n" +
				`layer(name="app", base="busybox")`,
			files: map[string]string{"app.sh": "echo hi"},
			want: []string{
				`/project/BUILD:3:4: add: add("code", "/app") is not in the steps of any layer() or task()`,
			},
		},
		{
			name: "env outside steps",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[run(["true"])])` + "\n" +
				`setup = [env(**{"A": "1", "a.b": "2"})]`,
			want: []string{
				`/project/BUILD:3:13: env: env(A="1", **{"a.b": "2"}) is not in the steps of any ` +
					`layer() or task()`,
			},
		},
		{
			name:  "empty BUILD",
			build: "",
			want:  []string{`/project/BUILD has no target "app"; it declares none`},
		},
		// A NUL byte can't reach a build's graph, so each is reported where the BUILD file gives it.
		{
			name:  "NUL in a name",
			build: pullBusybox + `layer(name="a\x00b", base="busybox")`,
			want:  []string{"/project/BUILD:2:6:", `layer: name "a\x00b" must not contain a NUL byte`},
		},
		{
			name:  "NUL in argv",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[run(["echo", "a\x00"])])`,
			want:  []string{"/project/BUILD:2:45:", "run: argv[1] must not contain a NUL byte"},
		},
		{
			name:  "NUL in a command",
			build: pullBusybox + `layer(name="app", base="busybox", command=["a\x00"])`,
			want:  []string{"/project/BUILD:2:6:", "layer: command[0] must not contain a NUL byte"},
		},
		{
			name:  "NUL in a path",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[workdir("/a\x00")])`,
			want:  []string{"/project/BUILD:2:49:", `workdir: path "/a\x00" must not contain a NUL byte`},
		},
		{
			name: "NUL in an owner",
			build: pullBusybox + withGlob +
				`layer(name="app", base="busybox", steps=[add("code", "/app", owner="a\x00")])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:45:", `add: owner "a\x00" must not contain a NUL byte`},
		},
		{
			name:  "NUL in a task's source",
			build: pullBusybox + `task(name="app", base="busybox", source="/a\x00", include=["."])`,
			want:  []string{"/project/BUILD:2:5:", `task: source "/a\x00" must not contain a NUL byte`},
		},
		{
			name:  "fail in BUILD",
			build: `fail("unsupported platform")`,
			want:  []string{"/project/BUILD:1:5:", "unsupported platform"},
		},
		{
			name: "error in a helper function",
			build: pullBusybox + `
def app(src):
    glob(name="code", include=[src])

app("main.sh")
layer(name="app", base="busybox", steps=[add("code", "/app")])
`,
			want: []string{"/project/BUILD:4:9:", `include "main.sh" not found`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"BUILD": tt.build}
			for name, content := range tt.files {
				files[name] = content
			}
			out := t.TempDir()
			vamos(t, project(t, files), "save", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
				failsWith(t, false, tt.want...)
			assertNoFiles(t, out)
		})
	}
}

func TestWriteDestinationMistakes(t *testing.T) {
	dir := t.TempDir()
	t.Run("missing directory", func(t *testing.T) {
		path := filepath.Join(dir, "nope", "app.tar")
		vamos(t, project(t, validBuild), "save", "app", path, "-C", projectDir).
			failsWith(t, false, "cannot write "+path+": directory "+filepath.Join(dir, "nope")+" does not exist")
	})
	t.Run("path is a directory", func(t *testing.T) {
		vamos(t, project(t, validBuild), "save", "app", dir, "-C", projectDir).
			failsWith(t, false, "cannot write "+dir+": it is a directory")
	})
	t.Run("parent is a file", func(t *testing.T) {
		file := filepath.Join(dir, "file")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		vamos(t, project(t, validBuild), "save", "app", filepath.Join(file, "app.tar"), "-C", projectDir).
			failsWith(t, false, file+" is not a directory")
	})
}

func TestMissingAuthFile(t *testing.T) {
	t.Setenv("REGISTRY_AUTH_FILE", "/nonexistent/auth.json")

	vamos(t, project(t, validBuild), "push", "app", "app", "-C", projectDir).
		failsWith(t, false, "REGISTRY_AUTH_FILE", "/nonexistent/auth.json", "no such file")
}

// TestSymlinkSource rejects an include that is itself a link.
func TestSymlinkSource(t *testing.T) {
	fsys, _ := diskProject(t, map[string]string{
		"BUILD": `glob(name="code", include=["lib"])` + "\n" + pullBusybox + addCode,
	}, map[string]string{"lib": "../shared"})
	out := t.TempDir()

	vamos(t, fsys, "save", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, "/project/BUILD:1:5:", `include "lib" is a symbolic link`)
	assertNoFiles(t, out)
}

// TestSymlinkSourceParent rejects links at every depth of an included path.
func TestSymlinkSourceParent(t *testing.T) {
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "secret"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"external", "nested/external"} {
		t.Run(parent, func(t *testing.T) {
			src := parent + "/secret"
			fsys, _ := diskProject(t, map[string]string{
				"BUILD": `glob(name="code", include=["` + src + `"])` + "\n" + pullBusybox + addCode,
			}, map[string]string{parent: external})
			out := t.TempDir()

			vamos(t, fsys, "save", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
				failsWith(t, false, "/project/BUILD:1:5:",
					`include "`+src+`" has symbolic link parent "`+filepath.Join(projectDir, parent)+`"`)
			assertNoFiles(t, out)
		})
	}
}

// assertNoFiles fails the test if dir holds anything, such as a partly written image.
func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("unexpected file left in %s: %s", dir, e.Name())
	}
}
