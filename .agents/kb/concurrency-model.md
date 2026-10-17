# Concurrency, callbacks and ownership

`run.Local` currently walks pulls and targets sequentially.
Container copies and stdout/stderr delivery can still involve multiple goroutines.
Do not infer that event consumers are single-threaded from the target scheduler.
The local executor is command-scoped; simultaneous calls on one instance are outside its contract.
Before adding parallel target scheduling, review `Local` initialization and shutdown, `targetRunner`'s result maps,
and when log writers stop before `LogRecorder.Save` or `Discard`.
Parallel targets within one run and simultaneous calls on one executor
require separate decisions about shared state and cleanup.

Concurrency contracts belong with their symbols, not in a parallel list of locks here.
The relevant owners are `run.Events`, `spanBatch`, `blobTracker`, `cache.LogRecorder`,
`tracing.Recorder`, `tracing.Active`, `cli.eventPrinter` and `cli.traceSession`.
Their files are routed by the corresponding package `doc.go` files.
Changes to callbacks must inspect both caller and consumer: data may be borrowed, callbacks may arrive in parallel,
and invoking user code while holding an internal lock changes the deadlock and latency behavior.

Cache publication uses filesystem operations rather than a process-wide build lock.
It does not provide a transaction over a graph, all its blobs and container images.
Do not run prune against an active build and assume snapshot isolation;
missing cached content is handled by reuse checks, not coordinated reclamation.
The commit/discard contracts are on `cache.pendingFile` and `cache.LogRecorder`.

Trace transport crosses several owners: the client attaches `tracing.CallContext` to executor requests,
the executor emits completed spans through `run.Events`, and `cli.traceSession` aligns clocks before writing the trace.
Each executor call must finish producing events before its final flush and return.
Inspect `forCall` and `tracedCopy` when adding asynchronous work;
a context passed to a local method is not a remote trace protocol.

Tracing should do no trace-only computation when disabled; guard argument preparation with `tracing.Enabled`.
Track nesting, flow endpoints and timestamp encoding are owned by [tracing](../../internal/tracing/doc.go).
`blobTracker` in [copy.go](../../internal/run/copy.go) adapts incomplete library progress events;
those events are not a reliable byte-level network audit (a config may be reported copied even when already present).
`TestTracePerfetto` in [trace_test.go](../../internal/cli/trace_test.go) verifies the importer's view,
beyond checking the JSON ourselves.
