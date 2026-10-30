// Package testenv supplies shared project fixtures and build prerequisites.
//
// # Invariants
//
// Helpers exercise real filesystem implementations and buildah, rather than replacing build behavior.
// In-memory projects use a stable logical root;
// disk projects keep host paths separate from paths visible through afero.
//
// # Changes
//
// Use Project for in-memory fixtures and DiskProject when a test needs symlinks or writes to the project.
// Main in main.go is the TestMain of packages whose tests build.
// NewNonce gives values new to each call, so that actions run rather than come from the cache.
// WalkImage and ImagePath in image.go read a saved image's flattened filesystem.
// SuiteStores and PrivateStores in stores.go give tests container stores and build caches of their own,
// so that no test touches the developer's; see RemoveSuiteStores for cleanup.
// Their UnnamedImages lists what a failed build may leave in the store.
// RequireBuildah and RequireOwnership probe for what a test needs and skip it when that is missing,
// or fail it when RequireIntegrationEnv says the suite must run whole;
// they do not configure privileges, isolation or networking.
// Tests in several packages use EditedFs to edit sources during a read, and DeniedFs to refuse to check a file.
// Give each test its own EditedFs, since it keeps a count of opens to decide when to edit.
// InsecureRegistry in registry.go lets an executor reach a test's registry over plain HTTP,
// and FakeDocker in docker.go serves what loading an image into Docker uses.
// Unsetenv unsets an environment variable for the rest of a test.
// Find the behavior specifications, and the steps they are written in, in internal/features.
package testenv
