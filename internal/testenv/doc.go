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
// SuiteStores and PrivateStores in stores.go give tests container stores and build caches of their own,
// so that no test touches the developer's; see RemoveSuiteStores for cleanup.
// RequireBuildah and RequireOwnership probe for what a test needs and skip it when that is missing,
// or fail it when RequireIntegrationEnv says the suite must run whole;
// they do not configure privileges, isolation or networking.
// Tests in several packages use EditedFs to edit sources during a read.
// Give each test its own instance, since it keeps a count of opens to decide when to edit.
// InsecureRegistry in registry.go lets an executor reach a test's registry over plain HTTP.
// Find command assertions and daemon fixtures in internal/cli tests.
package testenv
