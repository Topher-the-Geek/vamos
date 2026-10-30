package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/afero"
	"go.podman.io/storage"
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

	vamos(t, fsys, "save", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/app/-vendor", "/app/-vendor/link", "/app/BUILD"}
	if got := imagePaths(t, img, "/app"); !slices.Equal(got, want) {
		t.Errorf("files in /app = %q, want %q", got, want)
	}
}

// TestSteps checks that a layer applies its steps in order: that files added later replace those added before,
// that env() and workdir() reach later runs and the image, as in a Dockerfile,
// that build_env() reaches only the target's later runs,
// and that values, add()'s dest and workdir()'s path refer to variables set before them.
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

	vamos(t, fsys, "save", "app", out, "-C", projectDir).succeeds(t)

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

// TestWorkdirUser checks that, as in a Dockerfile, workdir() makes its directory for the image's user,
// so that later runs, which run as that user, can write to it.
func TestWorkdirUser(t *testing.T) {
	requireBuildah(t)
	_, host := insecureRegistry(t, nil)

	// busybox runs as root, so push it to the registry with a user that isn't.
	// A pull() can be sent as it is.
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
	vamos(t, fsys, "save", "app", out, "--tls-verify=false", "-C", projectDir).succeeds(t)

	img, err = tarball.ImageFromPath(out, nil)
	must(t, err)
	if !walkImage(t, img, func(hdr *tar.Header, _ io.Reader) bool {
		return imagePath(hdr) == "/work/dir/made.txt" && hdr.Uid == 1000
	}) {
		t.Errorf("/work/dir/made.txt not found in image, owned by 1000")
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

	vamos(t, fsys, "save", "app", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := imageFile(t, img, "/app/file.txt"); got != "inside\n" {
		t.Errorf("/app/file.txt = %q, want the file from the container", got)
	}
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

	vamosWithOutput(t, fsys, ctx, watch, "save", "app", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, "context canceled")
	assertNoFiles(t, out)
	// The cancellation was reported only once the step's container was removed.
	withStore(t, func(store storage.Store) {
		containers, err := store.Containers()
		must(t, err)
		if len(containers) != 0 {
			t.Errorf("%d containers left in the store after the interrupted build", len(containers))
		}
	})
}

// writerFunc is an io.Writer that calls itself.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestSecretsNotInTrace checks that no secret's value reaches the trace, which records each step's argv;
// the secrets feature specifies the rest.
func TestSecretsNotInTrace(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": pullBusybox + `
layer(
    name="app",
    base="busybox",
    steps=[
        run(["echo", "` + newNonce() + `"]),
        # The values are encoded, so that the trace, which records argv, doesn't hold them.
        run(["sh", "-c", 'test "$TOKEN" = "$(echo ZW52LXZhbHVl | base64 -d)" && ' +
                         'test "$(cat /run/secrets/key)" = "$(echo ZmlsZS12YWx1ZQ== | base64 -d)" && echo got them'],
            secrets=[secret("token", env="TOKEN"), secret("key", file="/run/secrets/key")]),
    ],
)
`})
	keyFile := filepath.Join(t.TempDir(), "key")
	testenv.Must(t, os.WriteFile(keyFile, []byte("file-value"), 0o600))
	t.Setenv("VAMOS_TEST_TOKEN", "env-value")
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace.json")
	r := vamos(t, fsys, "save", "app", filepath.Join(dir, "app.tar"), "--trace", trace, "-C", projectDir,
		"--secret", "id=token,env=VAMOS_TEST_TOKEN", "--secret", "id=key,src="+keyFile).succeeds(t)
	if !strings.Contains(r.stdout, "got them") {
		t.Errorf("stdout = %q, want the step to have found both secrets", r.stdout)
	}
	data, err := os.ReadFile(trace)
	testenv.Must(t, err)
	for _, value := range []string{"env-value", "file-value"} {
		if strings.Contains(string(data), value) {
			t.Errorf("the trace has the secret value %q", value)
		}
	}
}
