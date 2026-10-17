// Package tracing records spans independently of vamos's build implementation.
//
// # Invariants
//
// Spans are transportable data; recording is optional and context-scoped.
// This package imports no other vamos package.
// Synchronous spans on a track must nest; overlapping work needs separate tracks or asynchronous spans.
//
// # Lifecycle
//
// Active spans become completed data for a recorder or its callback.
// CallContext carries parentage across the executor boundary;
// callers own event transport, final flushing and clock alignment.
// Recording without a callback retains spans in memory until the recorder is released;
// callback mode delegates retention.
//
// # Changes
//
// Inspect Recorder and Active for synchronization and ownership.
// StartTrack, StartAsync and Span's flow fields own visualization constraints.
// EstimateOffset and Shift in clock.go align clocks.
// WriteChrome in chrome.go owns ordering and timestamp precision required by Perfetto;
// consumers must preserve those rules.
package tracing
