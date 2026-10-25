package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/worker"
)

// coordinatorOf serves a local executor on the test's stores over HTTP, as a coordinator does,
// and returns its URL.
func coordinatorOf(t *testing.T) string {
	t.Helper()
	local, err := run.NewLocal(stores(t).Options)
	must(t, err)
	t.Cleanup(func() { local.Close() })
	content, err := local.ContentStore()
	must(t, err)
	server := httptest.NewServer((&remote.Server{Executor: local, Content: content}).Handler())
	t.Cleanup(server.Close)
	return server.URL
}

// TestRemoteBuild checks that the commands build on a coordinator when --coordinator names one: a layer is saved,
// its result reused, and a task's files exported, with the client reading everything from the coordinator's store.
func TestRemoteBuild(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD": `
glob(name="src", include=["hello.txt"])
layer(name="app", base="scratch", steps=[add("src", "/data")])
task(name="copy", base="scratch", steps=[add("src", "/out")], source="/out", include=["hello.txt"])
`,
		"hello.txt": nonce + "\n",
	})
	url := coordinatorOf(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "app.tar")
	r := vamos(t, fsys, "save", "app", out, "--coordinator", url, "-C", projectDir).succeeds(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "building"})
	if got := imageFile(t, readImage(t, out), "/data/hello.txt"); got != nonce+"\n" {
		t.Errorf("/data/hello.txt = %q, want the glob's file", got)
	}
	r = vamos(t, fsys, "build", "app", "--coordinator", url, "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "cached"})

	exported := filepath.Join(dir, "exported")
	vamos(t, fsys, "export", "copy", exported, "--coordinator", url, "-C", projectDir).ran(t)
	if data, err := os.ReadFile(filepath.Join(exported, "hello.txt")); err != nil || string(data) != nonce+"\n" {
		t.Errorf("exported hello.txt = %q, %v; want the task's file", data, err)
	}

	// The variable names the coordinator too.
	t.Setenv("VAMOS_COORDINATOR", url)
	r = vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "cached"})
}

// TestRemoteMistakes checks the errors for a coordinator that can't be used: a URL that isn't one,
// one that runs other graph versions, and one that can't be reached.
func TestRemoteMistakes(t *testing.T) {
	fsys := project(t, map[string]string{"BUILD": `layer(name="app", base="scratch")`})
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Capabilities{GraphVersions: []int{graph.Version + 1000}})
	}))
	t.Cleanup(old.Close)
	tests := []struct {
		name        string
		coordinator string
		want        []string
	}{
		{
			name:        "not a URL",
			coordinator: "coordinator:8080",
			want:        []string{`--coordinator: coordinator "coordinator:8080": want an http or https URL`},
		},
		{
			name:        "other graph version",
			coordinator: old.URL,
			want: []string{fmt.Sprintf("the coordinator at %s runs graph versions [%d], not this vamos's, %d; "+
				"use a vamos of its version", old.URL, graph.Version+1000, graph.Version)},
		},
		{
			// Port 9 is discard, which nothing listens on here: the failure is at once, not after a minute of retries.
			name:        "can't be reached",
			coordinator: "http://127.0.0.1:9",
			want:        []string{"can't reach the coordinator at http://127.0.0.1:9: "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vamos(t, fsys, "build", "app", "--coordinator", tt.coordinator, "-C", projectDir).
				failsWith(t, false, tt.want...)
		})
	}
	// A secret is refused before the coordinator is reached, so nothing carries it there.
	t.Run("secret", func(t *testing.T) {
		vamos(t, fsys, "build", "app", "--coordinator", "http://127.0.0.1:9", "--secret", "id=token,env=HOME",
			"-C", projectDir).
			failsWith(t, false, "--secret token can't go to a coordinator, which has no authentication or TLS; "+
				"build in this process, without --coordinator or $VAMOS_COORDINATOR")
	})
}

// TestCoordinatorCommand checks that `vamos coordinator --local` serves builds at the address it listens on,
// to a client that gives --coordinator, until it is interrupted.
func TestCoordinatorCommand(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": `glob(name="src", include=["hello.txt"])` + "\n" +
			`layer(name="app", base="scratch", steps=[add("src", "/data")])`,
		"hello.txt": newNonce() + "\n",
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listening := &addressWatch{found: make(chan string, 1)}
	done := make(chan result, 1)
	go func() {
		done <- vamosWithOutput(t, fsys, ctx, listening, "coordinator", "--local", "--listen", "127.0.0.1:0")
	}()
	var address string
	select {
	case address = <-listening.found:
	case r := <-done:
		t.Fatalf("the coordinator ended before listening: %+v", r)
	}
	out := filepath.Join(t.TempDir(), "app.tar")
	vamos(t, fsys, "save", "app", out, "--coordinator", "http://"+address, "-C", projectDir).succeeds(t)

	// A build that waits on a registry that never answers is watched when the coordinator is interrupted:
	// the job is cancelled, the watch ends, and the command ends cleanly without waiting for the watcher.
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	_, regHost := insecureRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	// The client reaches the coordinator through a proxy that tells when the watch has reached it,
	// since the job sends nothing while it waits on the registry.
	target, err := url.Parse("http://" + address)
	must(t, err)
	watching := make(chan struct{})
	var watchingOnce sync.Once
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			watchingOnce.Do(func() { close(watching) })
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	client, err := remote.NewClient(front.URL)
	must(t, err)
	defer client.Close()
	g := &graph.Graph{Version: graph.Version, ExecPlatform: native, Roots: []graph.Ref{0}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pos: "BUILD:1:1", Pull: &graph.Pull{Image: regHost + "/base:v1",
			Platform: native}},
	}}
	must(t, g.Validate())
	stored, err := client.PutBytes(ctx, graph.Encode(g))
	must(t, err)
	platform, err := graph.ParsePlatform(native)
	must(t, err)
	id, err := client.Submit(ctx, api.Request{Graph: &api.GraphRequest{Graph: stored, Version: graph.Version,
		Platform: platform}})
	must(t, err)
	// The watch has its own context, as a client's would, so that it ends by the coordinator's doing alone.
	watched := make(chan error, 1)
	var last api.Event
	go func() {
		watched <- client.Watch(t.Context(), id, 0, func(e api.Event) error { last = e; return nil })
	}()
	select {
	case <-watching:
	case err := <-watched:
		t.Fatalf("the watch ended before the coordinator was interrupted: %v", err)
	}
	if _, err := client.Result(ctx, id); !errors.Is(err, api.ErrRunning) {
		t.Fatalf("Result = %v, want %v", err, api.ErrRunning)
	}
	start := time.Now()
	cancel()
	if r := <-done; r.err != nil {
		t.Errorf("the coordinator interrupted = %v, want a clean end", r.err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("the coordinator took %s to end, want well under its 10s shutdown deadline", took)
	}
	if err := <-watched; err != nil || last.Kind != api.EventDone || last.Outcome == nil || !last.Outcome.Cancelled {
		t.Errorf("the watch ended with %v after %+v; want it ended cleanly by the cancelled job's terminal event",
			err, last)
	}
}

// addressWatch watches the coordinator's output for the address it listens on.
type addressWatch struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	found chan string
	done  bool
}

// listening matches the line that says where the coordinator listens.
var listening = regexp.MustCompile(`Coordinating builds at (\S+)`)

// Write keeps what is written, and reports the address once it is printed.
func (w *addressWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if m := listening.FindStringSubmatch(w.buf.String()); m != nil && !w.done {
		w.done = true
		w.found <- strings.TrimSpace(m[1])
	}
	return len(p), nil
}

// TestCoordinatorWithWorker checks that `vamos coordinator` and `vamos worker` work together: a client's build runs
// on the worker, its layer is pushed to the cache repository, and a second build is a hit.
func TestCoordinatorWithWorker(t *testing.T) {
	requireBuildah(t)
	usePrivateStores(t) // the worker's store, which the attempts build in
	_, regHost := insecureRegistry(t, nil)
	t.Setenv(worker.ChildEnv, "1") // the worker runs this test binary as its attempts' subprocess
	fsys := project(t, map[string]string{
		"BUILD": `glob(name="src", include=["hello.txt"])` + "\n" +
			`layer(name="app", base="scratch", steps=[add("src", "/data")])`,
		"hello.txt": newNonce() + "\n",
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listening := &addressWatch{found: make(chan string, 1)}
	coordinatorDone := make(chan result, 1)
	go func() {
		coordinatorDone <- vamosWithOutput(t, fsys, ctx, listening, "coordinator", "--listen", "127.0.0.1:0",
			"--cache-dir", t.TempDir(), "--cache-repository", regHost+"/vamos-cache", "--lease", "2s")
	}()
	var address string
	select {
	case address = <-listening.found:
	case r := <-coordinatorDone:
		t.Fatalf("the coordinator ended before listening: %+v", r)
	}
	url := "http://" + address
	workerDone := make(chan result, 1)
	go func() {
		workerDone <- vamosWithOutput(t, fsys, ctx, nil, "worker", "--coordinator", url, "--capacity", "2")
	}()
	// Without a worker the build waits; the client's capabilities ask reports no platform until one registers.
	client, err := remote.NewClient(url)
	must(t, err)
	defer client.Close()
	for start := time.Now(); ; {
		caps, err := client.Capabilities(ctx)
		must(t, err)
		if len(caps.Platforms) > 0 {
			break
		}
		if time.Since(start) > 20*time.Second {
			t.Fatal("the worker never registered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := vamos(t, fsys, "build", "app", "--coordinator", url, "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "building"})
	r = vamos(t, fsys, "build", "app", "--coordinator", url, "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "cached"})

	// Delivery reads the image from the cache repository, with the client's settings, which allow plain HTTP;
	// the coordinator exports nothing. A push from the client copies from there too.
	out := filepath.Join(t.TempDir(), "app.tar")
	r = vamos(t, fsys, "save", "app", out, "--coordinator", url, "-C", projectDir).succeeds(t)
	if got := imageFile(t, readImage(t, out), "/data/hello.txt"); !strings.HasSuffix(got, "\n") {
		t.Errorf("/data/hello.txt = %q, want the glob's file", got)
	}
	vamos(t, fsys, "push", "app", regHost+"/pushed:v1", "--coordinator", url, "--tls-verify=false",
		"--push-from=client", "-C", projectDir).succeeds(t)
	// The executor's push runs on the worker, from the image's location, with the worker's settings.
	vamos(t, fsys, "push", "app", regHost+"/pushed:v2", "--coordinator", url, "-C", projectDir).succeeds(t)
	resp, err := http.Head("http://" + regHost + "/v2/pushed/manifests/v2")
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("the worker's push left %v, %v at the registry; want the tag", resp, err)
	}
	if err == nil {
		resp.Body.Close()
	}
	cancel()
	<-workerDone
	if r := <-coordinatorDone; r.err != nil {
		t.Errorf("the coordinator interrupted = %v, want a clean end", r.err)
	}
}

// TestUsesContainerStore checks which command lines need container storage, and so the re-exec that gives a root
// without capabilities a user namespace: every command but a coordinator, unless it builds locally,
// and but help and --version.
func TestUsesContainerStore(t *testing.T) {
	for args, want := range map[string]bool{
		"build app":                     true,
		"worker --coordinator http://c": true,
		"attempt":                       true,
		"coordinator --listen :8080":    false,
		"coordinator --local":           true,
		"coordinator --local=true":      true,
		"coordinator --local=false":     false,
		"-C dir coordinator":            false,
		"-C coordinator build app":      true,
		"build coordinator":             true,
		"save coordinator out.tar":      true,
		"":                              false,
		"--nonsense":                    true,
		"--version":                     false,
		"-v":                            false,
		"--help":                        false,
		"help build":                    false,
		"completion bash":               false,
		"build --help":                  false,
		"build -h app":                  false,
		"-C dir build app":              true,
	} {
		if got := UsesContainerStore(strings.Fields(args)); got != want {
			t.Errorf("UsesContainerStore(%q) = %v, want %v", args, got, want)
		}
	}
}

// TestCheckRegistryHost checks which hosts a pull-through mapping accepts: the smoke checks found docker.io refused
// by a check that wanted the canonical name.
func TestCheckRegistryHost(t *testing.T) {
	for _, host := range []string{"docker.io", "index.docker.io", "hub-proxy:5000", "localhost", "localhost:5000",
		"registry.example.com", "127.0.0.1:5000"} {
		if err := checkRegistryHost(host); err != nil {
			t.Errorf("checkRegistryHost(%q) = %v, want it accepted", host, err)
		}
	}
	for _, host := range []string{"hub-proxy", "library", "", "docker.io/library"} {
		if err := checkRegistryHost(host); err == nil {
			t.Errorf("checkRegistryHost(%q) accepted a name that isn't a registry host", host)
		}
	}
}

// TestCoordinatorFlagMistakes checks the messages for coordinator flags that name no registry host or platform.
func TestCoordinatorFlagMistakes(t *testing.T) {
	fsys := project(t, map[string]string{"BUILD": `layer(name="app")` + "\n"})
	dir := t.TempDir()
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "pull-through cache without a dot or port",
			args: []string{"--pull-through", "docker.io=hub-proxy"},
			want: `--pull-through docker.io=hub-proxy: "hub-proxy" isn't a registry host; want one with a dot or a ` +
				"port, or localhost, such as docker.io or hub-proxy:5000",
		},
		{
			name: "pull-through without a mapping",
			args: []string{"--pull-through", "docker.io"},
			want: "--pull-through docker.io: want registry=cache, such as docker.io=hub-proxy:5000",
		},
		{
			name: "default execution platform not offered",
			args: []string{"--offer", "linux/amd64", "--default-exec-platform", "linux/arm64"},
			want: "--default-exec-platform linux/arm64: give --offer linux/arm64 too, so that it is offered whether " +
				"or not a worker of it is registered",
		},
		{
			name: "offered emulation without an architecture",
			args: []string{"--offer-emulated", "linux"},
			want: `--offer-emulated linux: platform "linux": want os/arch or os/arch/variant, such as "linux/arm64"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"coordinator", "--listen", "127.0.0.1:0", "--cache-dir", dir}, tt.args...)
			vamos(t, fsys, args...).failsWith(t, false, tt.want)
		})
	}
}
