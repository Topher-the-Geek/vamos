// Command vamos builds and delivers container images from Starlark BUILD files.
//
// Startup must initialize buildah re-exec before constructing the CLI, and the re-exec into a user namespace that
// container storage needs from a root without capabilities, which cli.UsesContainerStore says a command line needs:
// a coordinator, help and --version open no container storage and run without.
// Keep this package limited to process startup and final error reporting;
// behavior belongs under internal/cli and the load, run, coord, worker and finish packages it coordinates.
package main
