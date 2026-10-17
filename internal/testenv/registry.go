package testenv

import (
	"os"
	"path/filepath"
	"testing"

	"go.podman.io/image/v5/pkg/sysregistriesv2"
)

// InsecureRegistry marks the registry at host, "host:port", insecure in a registries.conf of the test's own,
// so that an executor, which takes its registry settings from its environment, reaches it over plain HTTP.
// The client's own registry access needs --tls-verify=false still.
// Tests that call it can't run in parallel, since the environment is shared.
func InsecureRegistry(t *testing.T, host string) {
	t.Helper()
	conf := filepath.Join(t.TempDir(), "registries.conf")
	Must(t, os.WriteFile(conf, []byte("[[registry]]\nlocation = \""+host+"\"\ninsecure = true\n"), 0o644))
	t.Setenv("CONTAINERS_REGISTRIES_CONF", conf)
	// containers/image caches registries.conf without noticing the variable change.
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)
}
