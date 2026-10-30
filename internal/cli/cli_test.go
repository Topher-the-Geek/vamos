package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/afero"
)

// validBuild is a BUILD file that would build, were the command line right.
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
		{
			name: "secret without a source",
			args: []string{"build", "app", "--secret", "id=token"},
			want: []string{`invalid argument "id=token"`, `secret "token" needs env=VARIABLE or src=FILE`},
		},
		{
			name: "secret without an ID",
			args: []string{"build", "app", "--secret", "token,env=TOKEN"},
			want: []string{`"token" isn't key=value; want id=ID,env=VARIABLE or id=ID,src=FILE`},
		},
		{
			name: "secret with an unknown key",
			args: []string{"build", "app", "--secret", "id=token,file=/t"},
			want: []string{`unknown key "file"; want id=ID,env=VARIABLE or id=ID,src=FILE`},
		},
		{
			name: "secret given twice",
			args: []string{"build", "app", "--secret", "id=token,env=A", "--secret", "id=token,src=t"},
			want: []string{`secret "token" is given twice`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vamos(t, project(t, validBuild), tt.args...).failsWith(t, true, tt.want...)
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
			failsWith(t, false, "no BUILD or BUILD.vms file in /project")
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

func TestMissingAuthFile(t *testing.T) {
	t.Setenv("REGISTRY_AUTH_FILE", "/nonexistent/auth.json")

	vamos(t, project(t, validBuild), "push", "app", "app", "-C", projectDir).
		failsWith(t, false, "REGISTRY_AUTH_FILE", "/nonexistent/auth.json", "no such file")
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
