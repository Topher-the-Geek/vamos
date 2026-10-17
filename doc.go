// Command vamos builds and delivers container images from Starlark BUILD files.
//
// Startup must initialize buildah re-exec before constructing the CLI.
// Keep this package limited to process startup and final error reporting;
// behavior belongs under internal/cli and the load, run and finish packages it coordinates.
package main
