// Command echo is the payload of the Go cross-compilation BUILD example.
//
// BUILD owns the compiler platform, output platforms and release/debug settings.
// Keep the payload independent of vamos internals so the example exercises a standalone Go module.
// Its project README owns build and runtime checks.
package main
