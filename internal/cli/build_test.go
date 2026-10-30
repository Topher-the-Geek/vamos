package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"

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
