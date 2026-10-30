package features

import "testing"

// newCoordinatorWorld starts a scenario of the coordinator feature, from its background: an empty project, since a
// coordinator reads none. `vamos coordinator` coordinates builds for clients on the network, across the workers
// that register with it, until interrupted; its flags are checked before it listens.
func newCoordinatorWorld(t *testing.T) *world {
	t.Helper()
	return newWorld(t)
}

// TestCoordinator_PullThroughNamesRegistryHosts specifies that --pull-through maps a registry to a pull-through
// cache, each named by its host: one with a dot or a port, or localhost.
func TestCoordinator_PullThroughNamesRegistryHosts(t *testing.T) {
	for _, example := range []struct {
		name, mapping string
	}{
		{"docker.io is cached by a host with a port", "docker.io=hub-proxy:5000"},
		{"Docker Hub's index is cached by a host with a dot", "index.docker.io=registry.example.com"},
		// Docker Hub's index has a dot, though reference parsing treats it as docker.io's alias.
		{"A registry is cached by Docker Hub's index", "registry.example.com=index.docker.io"},
		{"localhost is cached by localhost with a port", "localhost=localhost:5000"},
		{"A host with a port is cached by an address with a port", "registry.example.com:5000=127.0.0.1:5000"},
		{"docker.io is cached by localhost", "docker.io=localhost"},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newCoordinatorWorld(t)
			w.WhenIStartACoordinatorUntilItListens("--pull-through", example.mapping)
			w.ThenItEndsCleanlyOnceInterrupted()
		})
	}

	for _, example := range []struct {
		name, mapping, error string
	}{
		{
			name:    "A cache without a dot or port fails",
			mapping: "docker.io=hub-proxy",
			error: `--pull-through docker.io=hub-proxy: "hub-proxy" isn't a registry host; want one with a dot or a ` +
				"port, or localhost, such as docker.io or hub-proxy:5000",
		},
		{
			name:    "A registry without a dot or port fails",
			mapping: "library=hub-proxy:5000",
			error: `--pull-through library=hub-proxy:5000: "library" isn't a registry host; want one with a dot or ` +
				"a port, or localhost, such as docker.io or hub-proxy:5000",
		},
		{
			name:    "A registry with a path fails",
			mapping: "docker.io/library=hub-proxy:5000",
			error: `--pull-through docker.io/library=hub-proxy:5000: "docker.io/library" isn't a registry host; ` +
				"want one with a dot or a port, or localhost, such as docker.io or hub-proxy:5000",
		},
		{
			name:    "A pull-through without a mapping fails",
			mapping: "docker.io",
			error:   "--pull-through docker.io: want registry=cache, such as docker.io=hub-proxy:5000",
		},
		{
			name:    "A pull-through without a registry fails",
			mapping: "=hub-proxy:5000",
			error:   "--pull-through =hub-proxy:5000: want registry=cache, such as docker.io=hub-proxy:5000",
		},
		{
			name:    "A pull-through without a cache fails",
			mapping: "docker.io=",
			error:   "--pull-through docker.io=: want registry=cache, such as docker.io=hub-proxy:5000",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newCoordinatorWorld(t)
			w.WhenIStartACoordinatorUntilItListens("--pull-through", example.mapping)
			w.ThenItFailsWith(example.error)
		})
	}
}

// TestCoordinator_OffersPlatformsItNames specifies that the platforms a coordinator offers, natively or emulated,
// are os/arch, or os/arch/variant, and that its default execution platform is one that --offer names.
func TestCoordinator_OffersPlatformsItNames(t *testing.T) {
	t.Run("A default execution platform that isn't offered fails", func(t *testing.T) {
		w := newCoordinatorWorld(t)
		w.WhenIStartACoordinatorUntilItListens("--offer", "linux/amd64", "--default-exec-platform", "linux/arm64")
		w.ThenItFailsWith("--default-exec-platform linux/arm64: give --offer linux/arm64 too, so that it is " +
			"offered whether or not a worker of it is registered")
	})

	t.Run("A default execution platform offered only emulated fails", func(t *testing.T) {
		w := newCoordinatorWorld(t)
		w.WhenIStartACoordinatorUntilItListens("--offer-emulated", "linux/arm64",
			"--default-exec-platform", "linux/arm64")
		w.ThenItFailsWith("--default-exec-platform linux/arm64: give --offer linux/arm64 too")
	})

	t.Run("An offered platform without an architecture fails", func(t *testing.T) {
		w := newCoordinatorWorld(t)
		w.WhenIStartACoordinatorUntilItListens("--offer", "linux")
		w.ThenItFailsWith(`--offer linux: platform "linux": want os/arch or os/arch/variant, such as "linux/arm64"`)
	})

	t.Run("An emulated platform with a variant starts", func(t *testing.T) {
		w := newCoordinatorWorld(t)
		w.WhenIStartACoordinatorUntilItListens("--offer-emulated", "linux/arm/v7")
		w.ThenItEndsCleanlyOnceInterrupted()
	})

	t.Run("An emulated platform without an architecture fails", func(t *testing.T) {
		w := newCoordinatorWorld(t)
		w.WhenIStartACoordinatorUntilItListens("--offer-emulated", "linux")
		w.ThenItFailsWith(`--offer-emulated linux: platform "linux": want os/arch or os/arch/variant, ` +
			`such as "linux/arm64"`)
	})

	t.Run("A default execution platform that is offered starts", func(t *testing.T) {
		w := newCoordinatorWorld(t)
		w.WhenIStartACoordinatorUntilItListens("--offer", "linux/amd64", "--default-exec-platform", "linux/amd64")
		w.ThenItEndsCleanlyOnceInterrupted()
	})
}
