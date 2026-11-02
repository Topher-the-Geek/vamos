package testenv

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.podman.io/image/v5/pkg/sysregistriesv2"
)

// InsecureRegistry marks the registry at host, "host:port", insecure in a registries.conf of the test's own,
// so that an executor, which takes its registry settings from its environment, reaches it over plain HTTP.
// The client's own registry access needs --tls-verify=false still.
// Tests that call it can't run in parallel, since the environment is shared.
// insecureMarker opens the files InsecureRegistry and MirroredRegistry write, so that they recognize their own.
const insecureMarker = "# written by testenv.InsecureRegistry\n"

func InsecureRegistry(t *testing.T, host string) {
	t.Helper()
	writeRegistriesConf(t, host, "[[registry]]\nlocation = \""+host+"\"\ninsecure = true\n")
}

// MirroredRegistry marks the registries at host and at mirror insecure, as InsecureRegistry does, and makes mirror a
// mirror of host, which pulls of host's images try first.
func MirroredRegistry(t *testing.T, host, mirror string) {
	t.Helper()
	writeRegistriesConf(t, host, "[[registry]]\nlocation = \""+host+"\"\ninsecure = true\n"+
		"[[registry.mirror]]\nlocation = \""+mirror+"\"\ninsecure = true\n")
}

// writeRegistriesConf writes a registries.conf of the test's own, with the entries of the one this package wrote
// last, but for any of host, and entry, which describes host, and makes containers/image read it.
func writeRegistriesConf(t *testing.T, host, entry string) {
	t.Helper()
	// A test that marks a second registry keeps the first: the entries of a file this package wrote are kept.
	const header = "[[registry]]\n" // which opens each entry, a mirror's included in its registry's
	text := ""
	if existing := os.Getenv("CONTAINERS_REGISTRIES_CONF"); existing != "" {
		if data, err := os.ReadFile(existing); err == nil && bytes.HasPrefix(data, []byte(insecureMarker)) {
			for _, e := range strings.Split(string(data[len(insecureMarker):]), header) {
				if e != "" && !strings.HasPrefix(e, "location = \""+host+"\"\n") {
					text += header + e
				}
			}
		}
	}
	text += entry
	conf := filepath.Join(t.TempDir(), "registries.conf")
	Must(t, os.WriteFile(conf, []byte(insecureMarker+text), 0o644))
	t.Setenv("CONTAINERS_REGISTRIES_CONF", conf)
	// containers/image caches registries.conf without noticing the variable change.
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)
}
