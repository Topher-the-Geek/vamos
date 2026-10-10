package internal

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
)

// helloBuild copies a source file into busybox and sets a command that echoes.
var helloBuild = map[string]string{
	"BUILD": `
build(
    base="` + busybox + `",
    sources=["hello.txt"],
    destination="/data",
    command=["echo", "hello from vamos"],
)
`,
	"hello.txt": "hello, file\n",
}

func TestWrite(t *testing.T) {
	requireBuildah(t)
	out := filepath.Join(t.TempDir(), "app.tar")

	r := vamos(t, project(t, helloBuild), "write", out, "--tag", "app:test", "-C", projectDir).succeeds(t)
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

	vamos(t, project(t, helloBuild), "write", out, "-C", projectDir).succeeds(t)

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

	vamos(t, project(t, helloBuild), "write", out, "-C", projectDir).succeeds(t)

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
			args := []string{"write", out, "-C", projectDir}
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

			vamos(t, project(t, helloBuild), "write", out, "-C", projectDir).succeeds(t)

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
		"BUILD":     `build(base="` + busybox + `", sources=["."], destination="/app")`,
		"hello.txt": "hello, file\n",
	}, nil)
	out := filepath.Join(root, projectDir, "app.tar")

	vamos(t, fsys, "write", out, "-C", projectDir).succeeds(t)

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
	fsys := project(t, map[string]string{"BUILD": `build(base="` + busybox + `", run=["false"])`})

	vamos(t, fsys, "write", out, "-C", projectDir).failsWith(t, false, `run ["false"] failed`)

	if b, _ := os.ReadFile(out); string(b) != "an old image" {
		t.Errorf("app.tar = %q, want the old file untouched", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("files in output directory = %v, want only app.tar", entries)
	}
}

func TestSourceDirectory(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD":           `build(base="` + busybox + `", sources=["site"], destination="/srv")`,
		"site/index.html": "<h1>hi</h1>",
		"site/css/a.css":  "h1 {}",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	vamos(t, fsys, "write", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Like Docker's COPY, a directory's contents are copied into the destination.
	if got := imageFile(t, img, "/srv/index.html"); got != "<h1>hi</h1>" {
		t.Errorf("/srv/index.html = %q", got)
	}
	if got := imageFile(t, img, "/srv/css/a.css"); got != "h1 {}" {
		t.Errorf("/srv/css/a.css = %q", got)
	}
}

func TestSourceSymlinks(t *testing.T) {
	requireBuildah(t)
	fsys, root := diskProject(t, map[string]string{
		"BUILD":           `build(base="` + busybox + `", sources=["site"], destination="/srv")`,
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

	vamos(t, fsys, "write", out, "-C", projectDir).succeeds(t)

	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Like Docker's COPY, links are copied as links, so nothing outside the project is read.
	for path, want := range map[string]string{
		"/srv/home.html": "index.html",
		"/srv/secret":    "/etc/hostname",
		"/srv/lib":       "../shared",
	} {
		if got := imageLink(t, img, path); got != want {
			t.Errorf("%s links to %q, want %q", path, got, want)
		}
	}
}

func TestPush(t *testing.T) {
	requireBuildah(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	image := host + "/team/app:v1"

	r := vamos(t, project(t, helloBuild), "push", image, "--tls-verify=false", "-C", projectDir).succeeds(t)
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
	vamos(t, project(t, helloBuild), "push", host+"/app", "-C", projectDir).succeeds(t)
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

	vamos(t, project(t, helloBuild), "push", host+"/app", "--tls-verify=false", "-C", projectDir).succeeds(t)
}

func TestPushFailures(t *testing.T) {
	requireBuildah(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")

	t.Run("registry over HTTP without --tls-verify=false", func(t *testing.T) {
		vamos(t, project(t, helloBuild), "push", host+"/app", "-C", projectDir).
			failsWith(t, false, "pushing to "+host+"/app:latest", "HTTP response to HTTPS client")
	})
	t.Run("registry not running", func(t *testing.T) {
		vamos(t, project(t, helloBuild), "push", "127.0.0.1:1/app", "--tls-verify=false", "-C", projectDir).
			failsWith(t, false, "pushing to 127.0.0.1:1/app:latest", "connection refused")
	})
}

func TestLoad(t *testing.T) {
	requireBuildah(t)
	// Over TCP, like Docker, vamos uses TLS only when $DOCKER_CERT_PATH is set.
	for _, network := range []string{"unix", "tcp"} {
		t.Run(network, func(t *testing.T) {
			loaded := fakeDocker(t, network, "")

			r := vamos(t, project(t, helloBuild), "load", "app:dev", "-C", projectDir).succeeds(t)
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

	vamos(t, project(t, helloBuild), "load", "app:dev", "-C", projectDir).
		failsWith(t, false, "loading into Docker: no space left on device")
}

func TestLoadWithoutDocker(t *testing.T) {
	requireBuildah(t)
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")

	fsys := project(t, map[string]string{
		"BUILD": `build(base="` + busybox + `", run=["echo", "build completed"])`,
	})
	r := vamos(t, fsys, "load", "app:dev", "-C", projectDir).
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

	vamos(t, project(t, helloBuild), "load", image, "-C", projectDir).succeeds(t)

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
		"BUILD": `
build(
    base="` + busybox + `",
    sources=["build.sh"],
    destination="/src",
    run=["sh", "/src/build.sh"],
)
`,
		"build.sh": "echo compiling\necho 'built!' > /built.txt\necho 'a warning' >&2\n",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	r := vamos(t, fsys, "write", out, "-C", projectDir).succeeds(t)

	// The run step's output reaches the user as it happens.
	if !strings.Contains(r.stdout, "compiling") {
		t.Errorf("stdout = %q, want the run step's output", r.stdout)
	}
	if !strings.Contains(r.stderr, "a warning") {
		t.Errorf("stderr = %q, want the run step's errors", r.stderr)
	}
	img, err := tarball.ImageFromPath(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := imageFile(t, img, "/built.txt"); got != "built!\n" {
		t.Errorf("/built.txt = %q, want the run step's output file", got)
	}
}

func TestRunStepSucceeds(t *testing.T) {
	requireBuildah(t)
	for _, run := range []string{`["true"]`, `["echo", "hi"]`, `["sh", "-c", "exit 0"]`} {
		t.Run(run, func(t *testing.T) {
			fsys := project(t, map[string]string{"BUILD": `build(base="` + busybox + `", run=` + run + `)`})
			vamos(t, fsys, "write", filepath.Join(t.TempDir(), "app.tar"), "-C", projectDir).succeeds(t)
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
			build: `build(base="` + busybox + `", run=["false"])`,
			want:  []string{`run ["false"] failed`, "exit status 1"},
		},
		{
			name:  "exit code",
			build: `build(base="` + busybox + `", run=["sh", "-c", "exit 3"])`,
			want:  []string{`run ["sh" "-c" "exit 3"] failed`, "exit status 3"},
		},
		{
			name:  "command not found",
			build: `build(base="` + busybox + `", run=["no-such-compiler", "main.c"])`,
			want:  []string{`run ["no-such-compiler" "main.c"] failed`, "no-such-compiler"},
		},
		{
			name: "source doesn't compile",
			build: `
build(
    base="` + busybox + `",
    sources=["app.sh"],
    destination="/src",
    run=["sh", "-n", "/src/app.sh"],
)
`,
			files:      map[string]string{"app.sh": "if true; then\n  echo unfinished\n"},
			want:       []string{`run ["sh" "-n" "/src/app.sh"] failed`, "exit status 2"},
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
			r := vamos(t, project(t, files), "write", filepath.Join(out, "app.tar"), "-C", projectDir).
				failsWith(t, false, tt.want...)
			if !strings.Contains(r.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want the run step's errors %q", r.stderr, tt.wantStderr)
			}
			assertNoFiles(t, out)
		})
	}
}

func TestInterrupted(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": `build(base="` + busybox + `", run=["sh", "-c", "echo started; sleep 1"])`,
	})
	out := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Interrupt, as main does on Ctrl-C, once the run step has started.
	watch := writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "started") {
			cancel()
		}
		return len(p), nil
	})

	vamosWithOutput(t, fsys, ctx, watch, "write", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, "context canceled")
	assertNoFiles(t, out)
}

// writerFunc is an io.Writer that calls itself.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestBaseImageNotFound(t *testing.T) {
	requireBuildah(t)
	base := "docker.io/library/vamos-no-such-image:latest"
	fsys := project(t, map[string]string{"BUILD": `build(base="` + base + `")`})
	out := t.TempDir()

	vamos(t, fsys, "write", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, `base image "`+base+`"`, "check the name")
	assertNoFiles(t, out)
}

func TestBaseImageUnreachable(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": `build(base="127.0.0.1:1/app")`})
	out := t.TempDir()

	r := vamos(t, fsys, "write", filepath.Join(out, "app.tar"), "--tls-verify=false", "-C", projectDir).
		failsWith(t, false, `base image "127.0.0.1:1/app"`, "connection refused")
	// The name is fine, so don't suggest checking it.
	if strings.Contains(r.err.Error(), "check the name") {
		t.Errorf("error = %q, want no hint about the name", r.err)
	}
	assertNoFiles(t, out)
}
