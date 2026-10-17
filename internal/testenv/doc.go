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
// RequireBuildah skips image builds when prerequisites are missing;
// it does not configure privileges, isolation or networking.
// Tests in several packages use EditedFs to edit sources during a read.
// Give each test its own instance, since it keeps a count of opens to decide when to edit.
// Find command assertions and registry/daemon fixtures in internal/cli tests.
package testenv
