// Package load turns reachable Starlark targets into an execution graph.
//
// # Invariants
//
// Evaluation, configuration and source capture happen on the client.
// A package's BUILD file is named BUILD or BUILD.vms, never both; findBuildFile owns the names.
// Package loading is demand-driven, including labels discovered while configuring;
// there is no workspace-wide validation pass.
// A package still loading, because one it loaded names its targets, gives the targets it has declared so far.
// BUILD mistakes retain source positions here rather than becoming executor graph-validation errors.
// The workspace root's vamos.lock is read once per load, and a malformed one, or one of a newer version, fails it;
// each pull's node takes the digest that the lockfile pins its normalized image and tag to, or none,
// and Request.Pin may pin the others, writing the lockfile, once the targets are configured and before any glob is
// read.
// A run() step's secrets are names alone; a step whose secret Request.Secrets lacks is refused once configured,
// before sources are captured.
//
// # Lifecycle
//
// Declaration, configuration, lowering and source capture are separate stages.
// Each honours the load's context: cancelWith stops a Starlark thread when it ends, and the load reports
// the interruption as the context's error, not the BUILD file's.
// Their state is private to one Load call.
// The returned graph and cached sources are the complete handoff; an executor must never need evaluation state.
//
// # Changes
//
// Start with Load and Request.
// LoadPublication loads a publication as the roots it checks and publishes, its checks then its images,
// and returns the Publication saying where each image goes; its repositories never enter the graph.
// expandPublication owns that expansion, which a build of a publication shares; resolve refuses a publication
// wherever a target names another, so that it is a root only.
// In buildfile.go, declaration and evalContext own deferred evaluation and builtin scope;
// loadError preserves nested failures.
// In config.go, confLabel, workspace.edge, workspace.template and configurer own target identity
// and dependency expansion; lowering constructs graph nodes.
// workspace.readGlobs captures sources after configuration.
// Step values and their lowering live in step.go; changes must also reach graph and run.
// Pulls, in pulls.go, returns the pulls that vamos update pins: those named, or every pull of the workspace, whose
// BUILD files it finds by walking it, the one workspace-wide scan, which skips hidden directories and other workspaces.
// lock.go owns the lockfile's format: ReadLock, Lock.Marshal, which gives the same bytes for the same pins,
// and UpdateLock, which holds the workspace's lock, an flock of its root on disk, while it reads the lockfile again and
// writes it durably through a temporary file renamed over it, so that concurrent writers each keep their pins;
// WouldUpdateLock says whether it would write, writing nothing.
// Fields that this vamos doesn't know are kept on rewrite, a pin's while the pin is unchanged, so raise LockVersion
// only for a change that an older vamos would get wrong even so.
package load
