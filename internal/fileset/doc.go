// Package fileset gives project globs and task outputs one file-selection model.
//
// # Invariants
//
// Selection has a canonical serialized form for action identity and private parsed state for walking.
// Compile before walking, including after decoding a selection.
// The filesystem root is supplied by the caller; container-root resolution belongs to run, not to this package.
// Selection never implies a scan of the whole project.
//
// # Lifecycle
//
// Compile normalizes selection policy without reading files.
// Walk applies that policy and calls the supplied content reader, and stops between entries when its context ends.
// Store and Hash select whether to persist content; both use the same walk.
// Each walk owns its traversal state.
//
// # Changes
//
// Selection.Compile owns normalization; Walk and checkInclude own traversal and link rules.
// Keep Store and Hash aligned when changing limits or metadata.
// The caller adds BUILD positions or target context to errors from this package.
package fileset
