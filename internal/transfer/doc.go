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
// Layout and WriteLayoutIndex own how an image's blobs are laid out on disk for copying; LayoutIndex assembles an
// index from members at any source, for both executors' index pushes and the client's delivery.
// Push, Upload and Tag own writing to a registry, with retries and a read-back of every tag written:
// Push writes a tag at once, while Upload writes an image or index by digest, changing no tag,
// and Tag then writes the manifest Upload wrote to a tag, sending nothing else,
// so that a publication can upload everything before it writes any tag.
// SystemContext builds a process's registry settings from its environment alone.
package transfer
