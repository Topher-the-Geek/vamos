package testenv

import (
	"fmt"
	"os"
	"testing"

	"go.podman.io/buildah"
)

// Main runs a test binary's tests, as the TestMain of a package whose tests build, and exits.
// It first lets buildah re-execute the binary to run a build step, and then each of children,
// such as worker.ChildFromEnv, run it as a child process; each returns true if it did, and Main returns.
// Builds use chroot isolation unless BUILDAH_ISOLATION says otherwise, and the suite's stores are removed at the end.
func Main(m *testing.M, children ...func() bool) {
	if buildah.InitReexec() {
		return
	}
	for _, child := range children {
		if child() {
			return
		}
	}
	if os.Getenv("BUILDAH_ISOLATION") == "" {
		os.Setenv("BUILDAH_ISOLATION", "chroot")
	}
	code := m.Run()
	if err := RemoveSuiteStores(); err != nil {
		fmt.Fprintf(os.Stderr, "removing the suite's stores: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
