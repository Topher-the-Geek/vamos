package testenv

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.podman.io/image/v5/pkg/sysregistriesv2"
)

// InsecureRegistry marks the registry at host, "host:port", insecure in a registries.conf of the test's own,
// so that an executor, which takes its registry settings from its environment, reaches it over plain HTTP.
// The client's own registry access needs --tls-verify=false still.
// Tests that call it can't run in parallel, since the environment is shared.
// insecureMarker opens the files InsecureRegistry writes, so that it recognizes its own.
const insecureMarker = "# written by testenv.InsecureRegistry\n"

func InsecureRegistry(t *testing.T, host string) {
	t.Helper()
	// A test that marks a second registry keeps the first: the entries of a file this helper wrote are kept.
	var entries []byte
	if existing := os.Getenv("CONTAINERS_REGISTRIES_CONF"); existing != "" {
		if data, err := os.ReadFile(existing); err == nil && bytes.HasPrefix(data, []byte(insecureMarker)) {
			entries = data[len(insecureMarker):]
		}
	}
	entries = append(entries, []byte("[[registry]]\nlocation = \""+host+"\"\ninsecure = true\n")...)
	conf := filepath.Join(t.TempDir(), "registries.conf")
	Must(t, os.WriteFile(conf, append([]byte(insecureMarker), entries...), 0o644))
	t.Setenv("CONTAINERS_REGISTRIES_CONF", conf)
	// containers/image caches registries.conf without noticing the variable change.
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)
}
