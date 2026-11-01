package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"

	"go.podman.io/image/v5/pkg/sysregistriesv2"
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

// TestRegistryPolicies checks how a coordinator tells clients it reaches its cache repository and pull-through caches:
// as registries.conf says for each, by its longest matching prefix, with the prefixes it gives within one, so that a
// repository insecure at a secure host, or secure at an insecure host, stays so; all insecure if it verifies no
// certificates; and none if it verifies them, or if none is insecure.
func TestRegistryPolicies(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "registries.conf")
	must(t, os.WriteFile(conf, []byte(`
[[registry]]
location = "marked:5000"
insecure = true

[[registry]]
location = "marked:5000/secure"

[[registry]]
location = "plain:5000/vamos-cache"
insecure = true

[[registry]]
location = "hub-proxy:5000/library"
insecure = true
`), 0o644))
	t.Setenv("CONTAINERS_REGISTRIES_CONF", conf)
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)

	pullThrough := map[string]string{"docker.io": "marked:5000", "quay.io": "quay-proxy:5000"}
	for _, example := range []struct {
		name        string
		verify      types.OptionalBool
		cacheRepo   string
		pullThrough map[string]string
		want        []api.RegistryPolicy
	}{{
		name:      "an insecure repository at a secure host",
		cacheRepo: "plain:5000/vamos-cache",
		want:      []api.RegistryPolicy{{Prefix: "plain:5000/vamos-cache", Insecure: true}},
	}, {
		name:      "a secure repository at an insecure host",
		cacheRepo: "marked:5000/secure",
	}, {
		name:      "an unmarked cache repository",
		cacheRepo: "registry.example:5000/vamos-cache",
	}, {
		name:        "an insecure host with a secure repository, and a secure host",
		cacheRepo:   "marked:5000/vamos-cache",
		pullThrough: pullThrough,
		want: []api.RegistryPolicy{
			{Prefix: "marked:5000", Insecure: true}, {Prefix: "marked:5000/secure"},
			{Prefix: "marked:5000/vamos-cache", Insecure: true}, {Prefix: "quay-proxy:5000"},
		},
	}, {
		name:        "a secure pull-through cache with an insecure repository",
		cacheRepo:   "registry.example:5000/vamos-cache",
		pullThrough: map[string]string{"docker.io": "hub-proxy:5000"},
		want: []api.RegistryPolicy{
			{Prefix: "hub-proxy:5000"}, {Prefix: "hub-proxy:5000/library", Insecure: true},
			{Prefix: "registry.example:5000/vamos-cache"},
		},
	}, {
		name:        "verifying no certificates",
		verify:      types.OptionalBoolTrue,
		cacheRepo:   "marked:5000/secure",
		pullThrough: pullThrough,
		want: []api.RegistryPolicy{
			{Prefix: "marked:5000", Insecure: true}, {Prefix: "marked:5000/secure", Insecure: true},
			{Prefix: "quay-proxy:5000", Insecure: true},
		},
	}, {
		name:        "verifying certificates",
		verify:      types.OptionalBoolFalse,
		cacheRepo:   "plain:5000/vamos-cache",
		pullThrough: pullThrough,
	}} {
		sys := &types.SystemContext{DockerInsecureSkipTLSVerify: example.verify}
		if got := registryPolicies(sys, example.cacheRepo, example.pullThrough); !slices.Equal(got, example.want) {
			t.Errorf("registry policies for %s = %+v, want %+v", example.name, got, example.want)
		}
	}
}
