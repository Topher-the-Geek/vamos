package run

import (
	"io"
	"log"
	"os"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/api/apitest"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/google/go-containerregistry/pkg/registry"
	"go.podman.io/buildah"
)

func TestMain(m *testing.M) {
	// buildah re-executes the test binary to run build steps.
	if buildah.InitReexec() {
		return
	}
	if os.Getenv("BUILDAH_ISOLATION") == "" {
		os.Setenv("BUILDAH_ISOLATION", "chroot")
	}
	code := m.Run()
	if err := testenv.RemoveSuiteStores(); err != nil {
		os.Stderr.WriteString("removing the suite's stores: " + err.Error() + "\n")
		code = 1
	}
	os.Exit(code)
}

// retention is how many events the contract's executors retain, few enough for the retention tests.
const retention = 6

// localFixture returns a fixture whose executors are local ones, each on private stores,
// wrapped by wrap, which the JSON decorator's test sets.
func localFixture(wrap func(*Local) api.Executor) apitest.Fixture {
	contents := map[api.Executor]cache.ContentStore{}
	return apitest.Fixture{
		New: func(t *testing.T) api.Executor {
			t.Helper()
			stores := testenv.PrivateStores(t)
			local, err := NewLocal(stores.Options)
			testenv.Must(t, err)
			local.RetainEvents(retention)
			t.Cleanup(func() { local.Close() })
			ex := wrap(local)
			// The client shares the executor's handle on the directory, as the CLI does in one process:
			// a handle of its own would hold the lock that a prune needs exclusive.
			content, err := local.ContentStore()
			testenv.Must(t, err)
			contents[ex] = content
			return ex
		},
		Put: func(t *testing.T, ex api.Executor, data []byte) cache.Blob {
			t.Helper()
			b, err := contents[ex].PutBytes(t.Context(), data)
			testenv.Must(t, err)
			return b
		},
		Missing: func(t *testing.T, ex api.Executor, blobs []cache.Blob) []cache.Blob {
			t.Helper()
			missing, err := contents[ex].Missing(t.Context(), blobs)
			testenv.Must(t, err)
			return missing
		},
		Platform:      HostPlatform,
		Registry:      registry.New(registry.Logger(log.New(io.Discard, "", 0))),
		Insecure:      testenv.InsecureRegistry,
		Retention:     retention,
		RequireBuilds: testenv.RequireBuildah,
	}
}

// TestContract runs the executor contract on the local executor.
func TestContract(t *testing.T) {
	apitest.Contract(t, localFixture(func(l *Local) api.Executor { return l }))
}

// TestContractJSON runs the executor contract on the local executor through JSON,
// as a remote client would see it.
func TestContractJSON(t *testing.T) {
	apitest.Contract(t, localFixture(func(l *Local) api.Executor { return apitest.NewJSON(l) }))
}
