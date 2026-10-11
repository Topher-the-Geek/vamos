package internal

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/afero"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
)

// helloBuild copies a source file into busybox and sets a command that echoes.
var helloBuild = map[string]string{
	"BUILD": pullBusybox + `
glob(name="hello", include=["hello.txt"])
layer(
    name="app",
    base="busybox",
    steps=[add("hello", "/data")],
    command=["echo", "hello from vamos"],
)
`,
	"hello.txt": "hello, file\n",
}

func TestWrite(t *testing.T) {
	requireBuildah(t)
	out := filepath.Join(t.TempDir(), "app.tar")

	r := vamos(t, project(t, helloBuild), "write", "app", out, "--tag", "app:test", "-C", projectDir).succeeds(t)
	if !strings.Contains(r.stdout, out) {
		t.Errorf("stdout = %q, want it to name %s", r.stdout, out)
	}

	tag, err := name.NewTag("app:test")
	if err != nil {
		t.Fatal(err)
	}
	img, err := tarball.ImageFromPath(out, &tag)
	if err != nil {
		t.Fatalf("reading written image tagged app:test: %v", err)
	}
	if got, want := imageCmd(t, img), []string{"echo", "hello from vamos"}; !slices.Equal(got, want) {
		t.Errorf("command = %q, want %q", got, want)
	}
	if got := imageFile(t, img, "/data/hello.txt"); got != "hello, file\n" {
		t.Errorf("/data/hello.txt = %q, want the source file", got)
	}
}

func TestWriteUntagged(t *testing.T) {
	requireBuildah(t)
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, project(t, helloBuild), "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatalf("reading written image: %v", err)
	}
	if got := imageFile(t, img, "/data/hello.txt"); got != "hello, file\n" {
		t.Errorf("/data/hello.txt = %q, want the source file", got)
	}
}

func TestWriteReplacesExistingFile(t *testing.T) {
	requireBuildah(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "app.tar")
	if err := os.WriteFile(out, []byte("an old image"), 0o644); err != nil {
		t.Fatal(err)
	}

	vamos(t, project(t, helloBuild), "write", "app", out, "-C", projectDir).succeeds(t)

	if _, err := tarball.ImageFromPath(out, nil); err != nil {
		t.Fatalf("reading written image: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("files in output directory = %v, want only app.tar", entries)
	}
}

func TestWriteToPathWithColon(t *testing.T) {
	requireBuildah(t)
	for _, tag := range []string{"", "app:test"} {
		t.Run("tag "+tag, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "builds:2026")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "app:1.tar")
			args := []string{"write", "app", out, "-C", projectDir}
			if tag != "" {
				args = append(args, "--tag", tag)
			}

			vamos(t, project(t, helloBuild), args...).succeeds(t)

			img, err := tarball.ImageFromPath(out, nil)
			if err != nil {
				t.Fatalf("reading written image: %v", err)
			}
			if got := imageFile(t, img, "/data/hello.txt"); got != "hello, file\n" {
				t.Errorf("/data/hello.txt = %q, want the source file", got)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Errorf("files in output directory = %v, want only app:1.tar", entries)
			}
		})
	}
}

func TestWriteFileMode(t *testing.T) {
	requireBuildah(t)
	umask := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(umask) })
	tests := []struct {
		name     string
		existing os.FileMode // mode of a file already at the output path, if not 0
		want     os.FileMode
	}{
		{
			name: "new file",
			want: 0o644,
		},
		{
			name:     "replaced file",
			existing: 0o640,
			want:     0o640,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "app.tar")
			if tt.existing != 0 {
				if err := os.WriteFile(out, []byte("an old image"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(out, tt.existing); err != nil {
					t.Fatal(err)
				}
			}

			vamos(t, project(t, helloBuild), "write", "app", out, "-C", projectDir).succeeds(t)

			info, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != tt.want {
				t.Errorf("mode of app.tar = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteInsideProject(t *testing.T) {
	requireBuildah(t)
	fsys, root := diskProject(t, map[string]string{
		"BUILD": pullBusybox + `glob(name="project", include=["."])` + "\n" +
			`layer(name="app", base="busybox", steps=[add("project", "/app")])`,
		"hello.txt": "hello, file\n",
	}, nil)
	out := filepath.Join(root, projectDir, "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatalf("reading written image: %v", err)
	}
	// The tarball being written must not be copied in as a source.
	if got, want := imagePaths(t, img, "/app"), []string{"/app/BUILD", "/app/hello.txt"}; !slices.Equal(got, want) {
		t.Errorf("files in /app = %q, want %q", got, want)
	}
}

func TestWriteKeepsExistingFileOnFailure(t *testing.T) {
	requireBuildah(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "app.tar")
	if err := os.WriteFile(out, []byte("an old image"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `layer(name="app", base="busybox", steps=[run(["false"])])`,
	})

	vamos(t, fsys, "write", "app", out, "-C", projectDir).failsWith(t, false, `run(["false"]) failed`)

	if b, _ := os.ReadFile(out); string(b) != "an old image" {
		t.Errorf("app.tar = %q, want the old file untouched", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("files in output directory = %v, want only app.tar", entries)
	}
}

func TestScratch(t *testing.T) {
	requireBuildah(t)
	// Nothing is pulled: the image holds only the layer's sources.
	fsys := project(t, map[string]string{
		"BUILD": `glob(name="hello", include=["hello.txt"])` + "\n" +
			`layer(name="app", base="scratch", steps=[add("hello", "/data")])`,
		"hello.txt": "hello, file\n",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := imagePaths(t, img, ""), []string{"/data", "/data/hello.txt"}; !slices.Equal(got, want) {
		t.Errorf("files in image = %q, want %q", got, want)
	}
}

func TestSourceDirectory(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `glob(name="site", include=["site"])` + "\n" +
			`layer(name="app", base="busybox", steps=[add("site", "/srv")])`,
		"site/index.html": "<h1>hi</h1>",
		"site/css/a.css":  "h1 {}",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A directory and everything in it keep their paths relative to the BUILD file.
	if got := imageFile(t, img, "/srv/site/index.html"); got != "<h1>hi</h1>" {
		t.Errorf("/srv/site/index.html = %q", got)
	}
	if got := imageFile(t, img, "/srv/site/css/a.css"); got != "h1 {}" {
		t.Errorf("/srv/site/css/a.css = %q", got)
	}
}

func TestSourcePaths(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
glob(name="code", include=["main.sh", "lib/util.sh", "lib"])
glob(name="docs", include=["./docs/README.md", "main.sh"])
layer(name="app", base="busybox", steps=[add("code", "/app"), add("docs", "/app")])
`,
		"main.sh":        "echo main",
		"lib/util.sh":    "echo util",
		"lib/more.sh":    "echo more",
		"docs/README.md": "# app",
		"docs/other.md":  "not included",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Each file keeps its path relative to the BUILD file, however many globs include it.
	want := []string{
		"/app/docs",
		"/app/docs/README.md",
		"/app/lib",
		"/app/lib/more.sh",
		"/app/lib/util.sh",
		"/app/main.sh",
	}
	if got := imagePaths(t, img, "/app"); !slices.Equal(got, want) {
		t.Errorf("files in /app = %q, want %q", got, want)
	}
}

// TestIncludeProject includes the whole project, through a link to it, as a linked $PWD gives.
func TestIncludeProject(t *testing.T) {
	requireBuildah(t)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	// "-vendor" sorts before ".", and its link would fail to copy twice.
	if err := os.MkdirAll(filepath.Join(real, "-vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	build := pullBusybox + `glob(name="all", include=["-vendor", "."])` + "\n" +
		`layer(name="app", base="busybox", steps=[add("all", "/app")])`
	if err := os.WriteFile(filepath.Join(real, "BUILD"), []byte(build), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../BUILD", filepath.Join(real, "-vendor", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, projectDir)); err != nil {
		t.Fatal(err)
	}
	fsys := afero.NewBasePathFs(afero.NewOsFs(), root)
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/app/-vendor", "/app/-vendor/link", "/app/BUILD"}
	if got := imagePaths(t, img, "/app"); !slices.Equal(got, want) {
		t.Errorf("files in /app = %q, want %q", got, want)
	}
}

func TestDestinationModes(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `glob(name="code", include=["app.sh"])` + "\n" +
			`glob(name="nested", include=["tmp/app.sh"])` + "\n" +
			`layer(name="tmp", base="busybox", steps=[add("code", "/tmp")])` + "\n" +
			`layer(name="root", base="tmp", steps=[add("nested", "/")])` + "\n" +
			`layer(name="app", base="root", steps=[add("code", "/app")])`,
		"app.sh":     "echo hi",
		"tmp/app.sh": "echo hi",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Sources are added from a private temporary folder, whose mode must not reach the image,
	// and the folders that hold them on the host must not replace the image's own.
	modes := map[string]int64{}
	walkImage(t, img, func(hdr *tar.Header, r io.Reader) bool {
		modes[imagePath(hdr)] = hdr.Mode & 0o7777
		return false
	})
	for path, want := range map[string]int64{"/tmp": 0o1777, "/app": 0o755} {
		if got := modes[path]; got != want {
			t.Errorf("mode of %s = %#o, want %#o", path, got, want)
		}
	}
}

func TestSourceSymlinks(t *testing.T) {
	requireBuildah(t)
	fsys, root := diskProject(t, map[string]string{
		"BUILD": pullBusybox + `glob(name="site", include=["site"])` + "\n" +
			`layer(name="app", base="busybox", steps=[add("site", "/srv")])`,
		"site/index.html": "<h1>hi</h1>",
	}, map[string]string{
		"site/home.html": "index.html",
		"site/secret":    "/etc/hostname",
		"site/lib":       "../shared",
	})
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Like Docker's COPY, links are copied as links, so nothing outside the project is read.
	for path, want := range map[string]string{
		"/srv/site/home.html": "index.html",
		"/srv/site/secret":    "/etc/hostname",
		"/srv/site/lib":       "../shared",
	} {
		if got := imageLink(t, img, path); got != want {
			t.Errorf("%s links to %q, want %q", path, got, want)
		}
	}
}

func TestTargets(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
glob(name="deps_files", include=["deps.txt"])
layer(
    name="deps",
    base="busybox",
    steps=[
        add("deps_files", "/deps"),
        run(["sh", "-c", "echo installed > /deps/installed.txt"]),
    ],
    command=["echo", "from deps"],
)

glob(name="app_files", include=["app.txt"])
layer(
    name="app",
    base="deps",
    steps=[
        add("app_files", "/app"),
        run(["cat", "/deps/installed.txt"]),
    ],
)
`,
		"deps.txt": "a dependency\n",
		"app.txt":  "the app\n",
	})
	out := filepath.Join(t.TempDir(), "app.tar")
	before := unnamedImages(t)

	r := vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	// Each layer is added to the image of the one before.
	if !strings.Contains(r.stdout, "installed") {
		t.Errorf("stdout = %q, want the app target to see the deps target's files", r.stdout)
	}
	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/deps/deps.txt":      "a dependency\n",
		"/deps/installed.txt": "installed\n",
		"/app/app.txt":        "the app\n",
	} {
		if got := imageFile(t, img, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	// Like Docker's FROM, a layer keeps its base's command unless it sets one.
	if got, want := imageCmd(t, img), []string{"echo", "from deps"}; !slices.Equal(got, want) {
		t.Errorf("command = %q, want %q", got, want)
	}
	assertNoNewImages(t, before)
}

func TestTargetFails(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
layer(name="deps", base="busybox", steps=[run(["touch", "/installed"])])
layer(name="app", base="deps", steps=[run(["false"])])
`,
	})
	out := t.TempDir()
	before := unnamedImages(t)

	vamos(t, fsys, "write", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, `layer "app": run(["false"]) failed`)
	assertNoFiles(t, out)
	assertNoNewImages(t, before)
}

// TestExpandedPathRelative checks that a path that is only relative once its variables are expanded
// fails the target, saying what it became.
func TestExpandedPathRelative(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
layer(name="app", base="busybox", steps=[build_env(DIR="srv"), workdir("$DIR/app")])
`,
	})
	out := t.TempDir()
	before := unnamedImages(t)

	vamos(t, fsys, "write", "app", filepath.Join(out, "app.tar"), "-C", projectDir).failsWith(t, false,
		`layer "app": workdir("$DIR/app") failed: path "$DIR/app" is "srv/app", which must be an absolute path`)
	assertNoFiles(t, out)
	assertNoNewImages(t, before)
}

// TestSteps checks that a layer applies its steps in order: that files added later replace
// those added before, that env() and workdir() reach later runs and the image, as in a Dockerfile, that
// build_env() reaches only the target's later runs, and that values, add()'s dest and workdir()'s path refer to
// variables set before them.
func TestSteps(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
glob(name="conf", include=["app.conf"])
task(
    name="generated",
    base="busybox",
    steps=[run(["sh", "-c", "mkdir /out && echo generated > /out/app.conf"])],
    source="/out",
    include=["app.conf"],
)
layer(
    name="base",
    base="busybox",
    steps=[
        run(["test", "!", "-e", "/etc/app/app.conf"]),
        build_env(CONF="/etc"),
        add("conf", "/etc/app"),
        add("generated", "${CONF}/app"),
        env(GREETING="hello", **{"MODE": "image"}),
        build_env(MODE="build", TOOL="cc", ROOT="/srv/"),
        workdir("$ROOT/app/"),
        run(["sh", "-c", "echo $GREETING $MODE $TOOL > during.txt"]),
        env(MODE="final"),
        run(["sh", "-c", "echo $MODE $TOOL > after.txt"]),
        # Each value sees the variables as they were before the call: MODE is still "final" in LABEL.
        env(PATH="/app/bin:$PATH", MODE="${MODE}2", LABEL="$MODE-$TOOL-$UNSET-$$5"),
        run(["sh", "-c", "echo $PATH $MODE $LABEL > expanded.txt"]),
    ],
)
layer(
    name="app",
    base="base",
    steps=[
        run(["sh", "-c", "echo $GREETING $MODE ${TOOL:-unset} $(pwd) > /inherited.txt"]),
        workdir("/data"),
    ],
)
`,
		"app.conf": "from the project\n",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/etc/app/app.conf":   "generated\n",
		"/srv/app/during.txt": "hello build cc\n",
		"/srv/app/after.txt":  "final cc\n",
		"/srv/app/expanded.txt": "/app/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin " +
			"final2 final-cc--$5\n",
		"/inherited.txt": "hello final2 unset /srv/app\n",
	} {
		if got := imageFile(t, img, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"GREETING=hello", "MODE=final2", "LABEL=final-cc--$5"} {
		if !slices.Contains(config.Config.Env, want) {
			t.Errorf("env = %q, want it to include %s", config.Config.Env, want)
		}
	}
	if slices.ContainsFunc(config.Config.Env, func(v string) bool { return strings.HasPrefix(v, "TOOL=") }) {
		t.Errorf("env = %q, want build_env()'s TOOL left out", config.Config.Env)
	}
	if got := config.Config.WorkingDir; got != "/data" {
		t.Errorf("working directory = %q, want /data", got)
	}
	// Like a Dockerfile's WORKDIR, workdir() makes the directory even if nothing runs in it.
	if !walkImage(t, img, func(hdr *tar.Header, _ io.Reader) bool {
		return imagePath(hdr) == "/data" && hdr.Typeflag == tar.TypeDir
	}) {
		t.Errorf("/data not found in image, want workdir() to make it")
	}
}

// TestWorkdirUser checks that, as in a Dockerfile, workdir() makes its directory for the image's user, so that
// later runs, which run as that user, can write to it.
func TestWorkdirUser(t *testing.T) {
	requireBuildah(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")

	// busybox runs as root, so push it to the registry with a user that isn't. A pull() can be sent as it is.
	base := host + "/nonroot:v1"
	fsys := project(t, map[string]string{"BUILD": pullBusybox})
	vamos(t, fsys, "push", "busybox", base, "--tls-verify=false", "-C", projectDir).succeeds(t)
	ref, err := name.ParseReference(base, name.Insecure)
	must(t, err)
	img, err := remote.Image(ref)
	must(t, err)
	config, err := img.ConfigFile()
	must(t, err)
	config.Config.User = "1000:1000"
	img, err = mutate.ConfigFile(img, config)
	must(t, err)
	must(t, remote.Write(ref, img))

	fsys = project(t, map[string]string{
		"BUILD": fmt.Sprintf(`
pull(name="base", image="%s/nonroot", tag="v1")
layer(name="app", base="base", steps=[workdir("/work/dir"), run(["sh", "-c", "echo %s > made.txt"])])
`, host, newNonce()),
	})
	out := filepath.Join(t.TempDir(), "app.tar")
	vamos(t, fsys, "write", "app", out, "--tls-verify=false", "-C", projectDir).succeeds(t)

	img, err = tarball.ImageFromPath(out, nil)
	must(t, err)
	if !walkImage(t, img, func(hdr *tar.Header, _ io.Reader) bool {
		return imagePath(hdr) == "/work/dir/made.txt" && hdr.Uid == 1000
	}) {
		t.Errorf("/work/dir/made.txt not found in image, owned by 1000")
	}
}

func TestTask(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
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

# A task can use another's output.
task(
    name="count",
    base="busybox",
    steps=[
        add("shout", "/count"),
        run(["sh", "-c", "wc -c < /count/upper.txt > /count/size.txt"]),
    ],
    source="/count",
    include=["size.txt"],
)

layer(
    name="app",
    base="busybox",
    steps=[add("shout", "/app"), add("count", "/app"), add("input", "/app")],
)
`,
		"input.txt": "hello\n",
	})
	out := filepath.Join(t.TempDir(), "app.tar")
	before := unnamedImages(t)

	r := vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	if !strings.Contains(r.stdout, "shouting") {
		t.Errorf("stdout = %q, want the task's output", r.stdout)
	}
	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Only the included files reach the layer, at their paths relative to the task's source.
	want := []string{"/app/input.txt", "/app/size.txt", "/app/sub", "/app/sub/nested.txt", "/app/upper.txt"}
	if got := imagePaths(t, img, "/app"); !slices.Equal(got, want) {
		t.Errorf("files in /app = %q, want %q", got, want)
	}
	for path, want := range map[string]string{
		"/app/upper.txt":      "HELLO\n",
		"/app/sub/nested.txt": "nested\n",
		"/app/size.txt":       "6\n",
	} {
		if got := imageFile(t, img, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	// A task's image is discarded.
	if got := imagePaths(t, img, "/work"); len(got) > 0 {
		t.Errorf("files in /work = %q, want none from the task", got)
	}
	assertNoNewImages(t, before)
}

// TestTaskWithoutOutput checks that a task without source and include, such as one that runs tests, runs
// and is cached like any other, and fails the build if a step fails.
func TestTaskWithoutOutput(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + fmt.Sprintf(`
layer(name="app", base="busybox")
task(name="test", base="app", steps=[run(["echo", "tested %s"])])
task(name="broken", base="app", steps=[run(["false"])])
`, nonce),
	})
	before := unnamedImages(t)

	r := vamos(t, fsys, "build", "test", "-C", projectDir).ran(t)
	if !strings.Contains(r.stdout, `task "test": building`) || !strings.Contains(r.stdout, "tested "+nonce) {
		t.Errorf("stdout = %q, want the task run", r.stdout)
	}
	r = vamos(t, fsys, "build", "test", "-C", projectDir).ran(t)
	if !strings.Contains(r.stdout, `task "test": cached`) || !strings.Contains(r.stdout, "tested "+nonce) {
		t.Errorf("stdout = %q, want the task reused, and its output replayed", r.stdout)
	}
	vamos(t, fsys, "build", "broken", "-C", projectDir).failsWith(t, false, `task "broken": run(["false"]) failed`)
	assertNoNewImages(t, before)
}

// TestTaskOutputModes checks that files a task makes keep their modes, special bits included.
func TestTaskOutputModes(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
task(
    name="make",
    base="busybox",
    steps=[run(["sh", "-c", """
        mkdir -p /out/shared
        echo tool > /out/tool
        chmod 4750 /out/tool
        chmod 1777 /out/shared
    """])],
    source="/out",
    include=["tool", "shared"],
)
layer(name="app", base="busybox", steps=[add("make", "/app")])
`,
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]int64{}
	walkImage(t, img, func(hdr *tar.Header, r io.Reader) bool {
		modes[imagePath(hdr)] = hdr.Mode & 0o7777
		return false
	})
	for path, want := range map[string]int64{"/app/tool": 0o4750, "/app/shared": 0o1777} {
		if got := modes[path]; got != want {
			t.Errorf("mode of %s = %#o, want %#o", path, got, want)
		}
	}
}

// TestTaskOutputLinks checks that links made by a task resolve within its container.
func TestTaskOutputLinks(t *testing.T) {
	requireBuildah(t)
	marker := "/vamos-test-" + strings.ToLower(t.Name()) // exists only in the container
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
task(
    name="make",
    base="busybox",
    steps=[run(["sh", "-c", """
        mkdir -p ` + marker + `
        echo inside > ` + marker + `/file.txt
        ln -s ` + marker + ` /out
    """])],
    source="/out",
    include=["file.txt"],
)
layer(name="app", base="busybox", steps=[add("make", "/app")])
`,
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := imageFile(t, img, "/app/file.txt"); got != "inside\n" {
		t.Errorf("/app/file.txt = %q, want the file from the container", got)
	}
}

func TestTaskFailures(t *testing.T) {
	requireBuildah(t)
	tests := []struct {
		name    string
		command string
		source  string
		include string
		want    []string
	}{
		{
			name:    "command fails",
			command: `["false"]`,
			source:  "/",
			include: "bin",
			want:    []string{`task "make": run(["false"]) failed`, "exit status 1"},
		},
		{
			name:    "source not found",
			command: `["true"]`,
			source:  "/nope",
			include: "app",
			want:    []string{`task "make": source "/nope" not found`},
		},
		{
			name:    "source is a file",
			command: `["true"]`,
			source:  "/bin/sh",
			include: "app",
			want:    []string{`task "make": source "/bin/sh" is not a directory`},
		},
		{
			name:    "include not found",
			command: `["mkdir", "/out"]`,
			source:  "/out",
			include: "app.js",
			want:    []string{`task "make": include "app.js" not found in /out`},
		},
		{
			// Followed on the host, the link would read the host's /etc/passwd.
			name:    "include is a link",
			command: `["sh", "-c", "mkdir /out && ln -s /etc/passwd /out/passwd"]`,
			source:  "/out",
			include: "passwd",
			want:    []string{`task "make": include "passwd" is a symbolic link`},
		},
		{
			name:    "include has a link parent",
			command: `["sh", "-c", "mkdir /out && ln -s /etc /out/etc"]`,
			source:  "/out",
			include: "etc/passwd",
			want:    []string{`task "make": include "etc/passwd" has symbolic link parent "/out/etc"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{
				"BUILD": pullBusybox +
					`task(name="make", base="busybox", steps=[run(` + tt.command +
					`)], source="` + tt.source + `", include=["` + tt.include + `"])` + "\n" +
					`layer(name="app", base="busybox", steps=[add("make", "/app")])`,
			})
			out := t.TempDir()
			before := unnamedImages(t)

			vamos(t, fsys, "write", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
				failsWith(t, false, tt.want...)
			assertNoFiles(t, out)
			assertNoNewImages(t, before)
		})
	}
}

func TestPush(t *testing.T) {
	requireBuildah(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	image := host + "/team/app:v1"

	r := vamos(t, project(t, helloBuild), "push", "app", image, "--tls-verify=false", "-C", projectDir).succeeds(t)
	if !strings.Contains(r.stdout, image) {
		t.Errorf("stdout = %q, want it to name %s", r.stdout, image)
	}

	ref, err := name.ParseReference(image, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("pulling pushed image: %v", err)
	}
	if got, want := imageCmd(t, img), []string{"echo", "hello from vamos"}; !slices.Equal(got, want) {
		t.Errorf("command = %q, want %q", got, want)
	}
	if got := imageFile(t, img, "/data/hello.txt"); got != "hello, file\n" {
		t.Errorf("/data/hello.txt = %q, want the source file", got)
	}
}

func TestPushToRegistryMarkedInsecure(t *testing.T) {
	requireBuildah(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	conf := filepath.Join(t.TempDir(), "registries.conf")
	content := "[[registry]]\nlocation = \"" + host + "\"\ninsecure = true\n"
	if err := os.WriteFile(conf, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_REGISTRIES_CONF", conf)
	// containers/image caches registries.conf without noticing the variable change.
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)

	// registries.conf, not --tls-verify, allows plain HTTP here.
	vamos(t, project(t, helloBuild), "push", "app", host+"/app", "-C", projectDir).succeeds(t)
}

func TestPushWithAuthFile(t *testing.T) {
	requireBuildah(t)
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "vamos" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"log in"}]}`, http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	t.Setenv("REGISTRY_AUTH_FILE", filepath.Join(t.TempDir(), "auth.json"))
	auth := base64.StdEncoding.EncodeToString([]byte("vamos:secret"))
	content := `{"auths": {"` + host + `": {"auth": "` + auth + `"}}}`
	if err := os.WriteFile(os.Getenv("REGISTRY_AUTH_FILE"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	vamos(t, project(t, helloBuild), "push", "app", host+"/app", "--tls-verify=false", "-C", projectDir).succeeds(t)
}

func TestPushFailures(t *testing.T) {
	requireBuildah(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")

	t.Run("registry over HTTP without --tls-verify=false", func(t *testing.T) {
		vamos(t, project(t, helloBuild), "push", "app", host+"/app", "-C", projectDir).
			failsWith(t, false, "pushing to "+host+"/app:latest", "HTTP response to HTTPS client")
	})
	t.Run("registry not running", func(t *testing.T) {
		vamos(t, project(t, helloBuild), "push", "app", "127.0.0.1:1/app", "--tls-verify=false", "-C", projectDir).
			failsWith(t, false, "pushing to 127.0.0.1:1/app:latest", "connection refused")
	})
}

func TestLoad(t *testing.T) {
	requireBuildah(t)
	// Over TCP, like Docker, vamos uses TLS only when $DOCKER_CERT_PATH is set.
	for _, network := range []string{"unix", "tcp"} {
		t.Run(network, func(t *testing.T) {
			loaded := fakeDocker(t, network, "")

			r := vamos(t, project(t, helloBuild), "load", "app", "app:dev", "-C", projectDir).succeeds(t)
			if !strings.Contains(r.stdout, "app:dev") {
				t.Errorf("stdout = %q, want it to name app:dev", r.stdout)
			}

			images := loaded()
			if len(images) != 1 {
				t.Fatalf("Docker loaded %d images, want 1", len(images))
			}
			tag, err := name.NewTag("app:dev")
			if err != nil {
				t.Fatal(err)
			}
			img, err := tarball.Image(func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(images[0])), nil
			}, &tag)
			if err != nil {
				t.Fatalf("reading image sent to Docker as app:dev: %v", err)
			}
			if got, want := imageCmd(t, img), []string{"echo", "hello from vamos"}; !slices.Equal(got, want) {
				t.Errorf("command = %q, want %q", got, want)
			}
		})
	}
}

func TestLoadRejected(t *testing.T) {
	requireBuildah(t)
	fakeDocker(t, "unix", `{"errorDetail":{"message":"no space left on device"},"error":"no space left on device"}`)

	vamos(t, project(t, helloBuild), "load", "app", "app:dev", "-C", projectDir).
		failsWith(t, false, "loading into Docker: no space left on device")
}

func TestLoadWithoutDocker(t *testing.T) {
	requireBuildah(t)
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")

	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `layer(name="app", base="busybox", steps=[run(["echo", "build completed"])])`,
	})
	r := vamos(t, fsys, "load", "app", "app:dev", "-C", projectDir).
		failsWith(t, false, "loading into Docker", "/nonexistent/docker.sock")
	if !strings.Contains(r.stdout, "build completed") {
		t.Errorf("stdout = %q, want the build to run before connecting to Docker", r.stdout)
	}
}

// TestLoadIntoDocker checks the fake Docker daemon's assumptions against a real one.
func TestLoadIntoDocker(t *testing.T) {
	requireBuildah(t)
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("needs a running Docker daemon")
	}
	image := "vamos-test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + ":latest"
	t.Cleanup(func() { exec.Command("docker", "rmi", "--force", image).Run() })

	vamos(t, project(t, helloBuild), "load", "app", image, "-C", projectDir).succeeds(t)

	out, err := exec.Command("docker", "run", "--rm", image).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run %s: %v\n%s", image, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "hello from vamos" {
		t.Errorf("docker run %s printed %q, want %q", image, got, "hello from vamos")
	}
}

func TestRunStep(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
glob(name="script", include=["build.sh"])
layer(
    name="app",
    base="busybox",
    steps=[
        add("script", "/src"),
        run(["sh", "/src/build.sh"]),
    ],
)
`,
		"build.sh": "echo compiling\necho 'built!' > /built.txt\necho 'a warning' >&2\n",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	r := vamos(t, fsys, "write", "app", out, "-C", projectDir).succeeds(t)

	// The run() step's output reaches the user as it happens.
	if !strings.Contains(r.stdout, "compiling") {
		t.Errorf("stdout = %q, want the run() step's output", r.stdout)
	}
	if !strings.Contains(r.stderr, "a warning") {
		t.Errorf("stderr = %q, want the run() step's errors", r.stderr)
	}
	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := imageFile(t, img, "/built.txt"); got != "built!\n" {
		t.Errorf("/built.txt = %q, want the run() step's output file", got)
	}
}

func TestRunStepSucceeds(t *testing.T) {
	requireBuildah(t)
	for _, run := range []string{`["true"]`, `["echo", "hi"]`, `["sh", "-c", "exit 0"]`} {
		t.Run(run, func(t *testing.T) {
			fsys := project(t, map[string]string{
				"BUILD": pullBusybox + `layer(name="app", base="busybox", steps=[run(` + run + `)])`,
			})
			vamos(t, fsys, "write", "app", filepath.Join(t.TempDir(), "app.tar"), "-C", projectDir).succeeds(t)
		})
	}
}

func TestRunStepFailures(t *testing.T) {
	requireBuildah(t)
	tests := []struct {
		name       string
		build      string
		files      map[string]string
		want       []string
		wantStderr string
	}{
		{
			name:  "false",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[run(["false"])])`,
			want:  []string{`run(["false"]) failed`, "exit status 1"},
		},
		{
			name:  "exit code",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[run(["sh", "-c", "exit 3"])])`,
			want:  []string{`run(["sh", "-c", "exit 3"]) failed`, "exit status 3"},
		},
		{
			name: "command not found",
			build: pullBusybox +
				`layer(name="app", base="busybox", steps=[run(["no-such-compiler", "main.c"])])`,
			want: []string{`run(["no-such-compiler", "main.c"]) failed`, "no-such-compiler"},
		},
		{
			// A file stands where a directory must be made.
			name:  "workdir within a file",
			build: pullBusybox + `layer(name="app", base="busybox", steps=[workdir("/bin/sh/app")])`,
			want:  []string{`layer "app": workdir("/bin/sh/app") failed`},
		},
		{
			name: "source doesn't compile",
			build: pullBusybox + `
glob(name="script", include=["app.sh"])
layer(
    name="app",
    base="busybox",
    steps=[
        add("script", "/src"),
        run(["sh", "-n", "/src/app.sh"]),
    ],
)
`,
			files:      map[string]string{"app.sh": "if true; then\n  echo unfinished\n"},
			want:       []string{`run(["sh", "-n", "/src/app.sh"]) failed`, "exit status 2"},
			wantStderr: "syntax error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"BUILD": tt.build}
			for name, content := range tt.files {
				files[name] = content
			}
			out := t.TempDir()
			r := vamos(t, project(t, files), "write", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
				failsWith(t, false, tt.want...)
			if !strings.Contains(r.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want the run() step's errors %q", r.stderr, tt.wantStderr)
			}
			assertNoFiles(t, out)
		})
	}
}

func TestInterrupted(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox +
			`layer(name="app", base="busybox", steps=[run(["sh", "-c", "echo started; sleep 1"])])`,
	})
	out := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Interrupt, as main does on Ctrl-C, once the run() step has started.
	watch := writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "started") {
			cancel()
		}
		return len(p), nil
	})

	vamosWithOutput(t, fsys, ctx, watch, "write", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, "context canceled")
	assertNoFiles(t, out)
}

// writerFunc is an io.Writer that calls itself.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestPullImageNotFound(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": `pull(name="base", image="vamos-no-such-image")` + "\n" + `layer(name="app", base="base")`,
	})
	out := t.TempDir()

	vamos(t, fsys, "write", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, `pull "base": image "docker.io/library/vamos-no-such-image:latest"`, "check the name")
	assertNoFiles(t, out)
}

func TestPullImageUnreachable(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": `pull(name="base", image="127.0.0.1:1/app")` + "\n" + `layer(name="app", base="base")`,
	})
	out := t.TempDir()

	r := vamos(t, fsys, "write", "app", filepath.Join(out, "app.tar"), "--tls-verify=false", "-C", projectDir).
		failsWith(t, false, `pull "base": image "127.0.0.1:1/app:latest"`, "connection refused")
	// The name is fine, so don't suggest checking it.
	if strings.Contains(r.err.Error(), "check the name") {
		t.Errorf("error = %q, want no hint about the name", r.err)
	}
	assertNoFiles(t, out)
}
