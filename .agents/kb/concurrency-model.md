# Concurrency, callbacks and ownership

`run.Local` walks each job's pulls and targets sequentially, but runs jobs at once.
Each job runs in a goroutine of its own, with a context that only `Cancel` ends,
and appends its events to a `run.job`, which retains a bounded number of them, `defaultRetention`;
watchers read them from any position they hold without blocking the job, a position no longer retained gets
a gap event, and a watcher that falls behind as it reads is dropped with `api.ErrFellBehind`.
Cancellation takes effect between commands, which buildah can't interrupt, and between the files of a checkout,
the records of a replayed log, the directories of a capture and the statements of a BUILD file.
Container copies and stdout/stderr delivery can still involve multiple goroutines.
Do not infer that event consumers are single-threaded from the target scheduler.
The local executor runs several jobs at once and deduplicates the actions in flight by key, in `run.sharing`:
an ordinary job that misses the cache attaches to the attempt of that key already running,
including one a forced rebuild started; a forced rebuild, or a fresh resolution of a tag, never attaches
and consults no cache, and its attempt replaces any in flight as the one later jobs attach to;
each attempt returns its own result to every job attached, and the latest write wins the record.
The starting job gets the attempt's output live; a job that attached later gets the result's log replayed.
Cancelling a job detaches it; an attempt runs while any job is attached and is cancelled when none is.
The attempt's spans go to the starting job's trace while it is attached;
once it detaches before others, they are dropped, so that no job produces events after its final flush.
`Local.Close` cancels the jobs still running and waits for them, so that their containers and pending files
are removed, and reports what failed.
Parallel targets within one job are still sequential; that is a separate decision.

Concurrency contracts belong with their symbols, not in a parallel list of locks here.
The relevant owners are `run.Events`, `spanBatch`, `blobTracker`, `cache.LogRecorder`,
`tracing.Recorder`, `tracing.Active`, `cli.eventPrinter` and `cli.traceSession`.
Their files are routed by the corresponding package `doc.go` files.
Changes to callbacks must inspect both caller and consumer: data may be borrowed, callbacks may arrive in parallel,
and invoking user code while holding an internal lock changes the deadlock and latency behavior.

The directory store publishes with filesystem operations rather than a process-wide build lock.
It does not provide a transaction over a graph, all its blobs and container images.
A prune and every other use of the directory exclude each other, across processes as within one:
an open `cache.Directory` holds an advisory lock on a file in it shared, from the client's source capture through
finish, since the client and the local executor share one handle, and `Directory.Prune` converts that handle's
lock to exclusive for the wipe, removing the cached images from the container store under it too.
Both take the lock without waiting, so a prune that meets an open store and a use that meets a prune each fail
at once, naming the other; the executor also refuses a prune while another of its jobs runs.
The lock is one host's kernel lock, not a lease: it survives nothing, and a shared network store wouldn't honour it.
Missing cached content is still handled by reuse checks, not coordinated reclamation.
The commit/discard contracts are on `cache.pendingFile` and `cache.LogRecorder`.

Trace transport crosses several owners: the client attaches `tracing.CallContext` to executor requests,
the executor emits completed spans through `run.Events`, which a job turns into `api.EventSpans` events,
and `cli.traceSession` aligns clocks, by `api.Capabilities.Clock`, before writing the trace.
Each job must finish producing events before its final flush and terminal event.
Inspect `forCall` and `transfer.Image` when adding asynchronous work;
a context passed to a local method is not a remote trace protocol.

Tracing should do no trace-only computation when disabled; guard argument preparation with `tracing.Enabled`.
Track nesting, flow endpoints and timestamp encoding are owned by [tracing](../../internal/tracing/doc.go).
`blobTracker` in [transfer.go](../../internal/transfer/transfer.go) adapts incomplete library progress events;
those events are not a reliable byte-level network audit (a config may be reported copied even when already present).
`TestTracePerfetto` in [trace_test.go](../../internal/cli/trace_test.go) verifies the importer's view,
beyond checking the JSON ourselves.
