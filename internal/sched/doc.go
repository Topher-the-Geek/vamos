// Package sched schedules a graph's actions: it walks the graph in dependency order, computes each action's key,
// looks its result up, and hands a Runner the actions that must run, sharing each in flight among the jobs that need
// it and recording what the runner reports.
//
// # Invariants
//
// The package imports neither buildah, the container store nor Starlark, so that a coordinator, which never
// executes an action itself, links none of them; TestImportRules holds it to that.
// A Scheduler never opens container storage: a Request carries everything an action needs, the tag record of a
// resolve included, and the Runner reads no record and no graph. Request and Report cross as JSON, from a coordinator
// to a worker and back.
// Only Sharing writes the record store, once the runner has published the artifact and returned its Report,
// so that no record names an artifact that isn't there; the latest write wins.
// Action keys exclude provenance, emulation, locations and target naming; ActionVersion changes when what a key
// covers, or how a target is built, changes.
// A key names a run() step's secrets by ID and how they are given, never by value; Request.Secrets, the values an
// action's steps use, is never encoded, so it reaches only a Runner in the job's own process, never a worker.
// Demanded tells a coordinator whether any attached job still wants an attempt, so that one waiting for a worker is
// taken back once every job that needed it has expired.
// Emulation is permission, not identity: a Request says whether its commands run emulated and whether its job
// permits that; Sharing lets a job without the permission attach to an emulating attempt in flight, since the
// result would be a hit moments later, but starts none for it, returning ErrEmulationNeeded, which the Scheduler
// renders as the mismatch message naming both platforms and both ways out; Permitted tells a coordinator whether a
// permitting job still needs an attempt, before it starts or retries one.
//
// # Lifecycle
//
// Jobs holds an executor's jobs: each runs in a goroutine of its own, appends its events to a Job, which retains a
// bounded number of them for watchers, by count and by bytes of output, and ends with a terminal event that carries
// its api.Outcome. A build, which StartBuild adds, is held by its watchers: unwatched for the grace period, its
// demand is withdrawn, which Withdrawn(ctx) tells the walk, so that nothing more starts while what runs finishes and
// is recorded; a finished job is evicted after the window, or once released, and remembered as expired.
// Without a grace period, as locally, nothing expires.
// Jobs keeps nothing on disk; an executor that does, as a coordinator does, reserves each Job's place, with Reserve,
// records it, then starts it, so that it records no Job the table refuses; is given each terminal event, with its
// position, before watchers see it, Spec.Ending; restores the finished ones after a restart, with Restore; and is
// told of evictions, OnEvict. A Job resumed after a restart gives positions past those of its earlier runs,
// Spec.First and Spec.Resumed, so that a watch from one of those gets a gap that says the executor restarted.
// A Scheduler resolves pulls first, then dispatches ready targets up to Options.Bound at once, checking cached
// results through a Checker and running the rest through the Runner, which Sharing wraps: an ordinary job attaches to
// an attempt of the same key in flight, a forced one never does, and an attempt is cancelled once no job is attached.
// Once a target fails nothing more is dispatched, what runs finishes and is recorded, and the first failure is the
// job's, as a failure naming its node, which the client renders against its graph.
//
// # Changes
//
// scheduler.go owns dispatch, keys, epochs, cached resolves, index assembly, tag pinning and the emulation check;
// share.go owns attempts and the record writes; jobs.go owns events, retention and the bound on open jobs;
// action.go owns the keyed payload; events.go carries spans to events; failure.go crosses a Runner's error as data,
// AttemptFailure, and back; destination.go parses push destinations for both executors.
// The run package implements Runner and Checker with buildah;
// scheduler_test.go drives the scheduler with a fake, one target at a time and several.
package sched
