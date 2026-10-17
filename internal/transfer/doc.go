// Package transfer copies images between references, as containers/image does, with a trace of the blobs copied,
// and reads and writes the OCI layouts that carry an image's blobs through the content store.
//
// # Invariants
//
// Both sides of the executor boundary copy images: the executor pulls, pushes and publishes them,
// and the client delivers them; this package serves both and imports neither buildah nor the container store.
// Policy trusts local storage and layouts, where vamos has just put the images, and follows the system's policy
// for the rest.
//
// # Changes
//
// Image owns the progress events that become blob spans; blobTracker adapts containers/image's incomplete pairs.
// Layout and WriteLayoutIndex own how an image's blobs are laid out on disk for copying.
package transfer
