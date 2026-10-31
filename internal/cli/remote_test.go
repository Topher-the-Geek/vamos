package cli

import (
	"slices"
	"strings"
	"testing"

	"go.podman.io/image/v5/types"
)

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

// TestInsecureRegistries checks that a coordinator names, of its cache repository's and pull-through caches' hosts,
// those that registries.conf marks insecure, or all of them when it verifies no certificates.
func TestInsecureRegistries(t *testing.T) {
	_, marked := insecureRegistry(t, nil)
	pullThrough := map[string]string{"docker.io": marked, "quay.io": "quay-proxy:5000"}
	if got := insecureRegistries(&types.SystemContext{}, marked+"/vamos-cache", pullThrough); !slices.Equal(got,
		[]string{marked}) {
		t.Errorf("insecure registries as registries.conf marks them = %q, want %q", got, marked)
	}
	if got := insecureRegistries(&types.SystemContext{}, "registry.example:5000/vamos-cache", nil); len(got) != 0 {
		t.Errorf("insecure registries of a cache repository that registries.conf doesn't mark = %q, want none", got)
	}
	skipping := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	want := []string{marked, "quay-proxy:5000", "registry.example:5000"}
	slices.Sort(want)
	if got := insecureRegistries(skipping, "registry.example:5000/vamos-cache", pullThrough); !slices.Equal(got,
		want) {
		t.Errorf("insecure registries verifying no certificates = %q, want %q", got, want)
	}
}
