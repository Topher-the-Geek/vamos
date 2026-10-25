package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Executor runs jobs: graph runs, pushes, exports and prunes.
// run.Local runs them in this process; a proxy to a remote executor is another implementation.
// Each method honours its context for that call alone: a job, once submitted, runs until it ends or is cancelled.
// Values cross by copy: an implementation keeps nothing of what it is handed, and what it returns is the caller's.
type Executor interface {
	// Capabilities reports what the executor is and can run, and the time by its clock.
	Capabilities(ctx context.Context) (Capabilities, error)
	// Submit accepts req as a job, returning its handle, or refuses it before any work starts:
	// a graph of an unsupported version, or configured for another execution platform, is refused.
	Submit(ctx context.Context, req Request) (JobID, error)
	// Watch delivers the events of the job id from position from, each to fn, until fn fails,
	// ctx ends, or the job's terminal event has been delivered, when it returns nil;
	// from a position past the terminal event it delivers nothing.
	// The end of ctx stops it before the next event, even when events are waiting to be delivered.
	// A position before the first retained is replaced by a gap event, at that position, naming the first retained,
	// which follows.
	// A watcher that falls behind what the executor retains is dropped with ErrFellBehind,
	// and may watch again from the position it last saw, where it meets a gap.
	Watch(ctx context.Context, id JobID, from int64, fn func(Event) error) error
	// Cancel asks the job id to stop.
	// Its effect is reported in the job's terminal event; Cancel doesn't wait for it.
	Cancel(ctx context.Context, id JobID) error
	// Result returns the outcome of the job id, or ErrRunning if it hasn't ended.
	// It never waits: watch to the terminal event for that.
	Result(ctx context.Context, id JobID) (Outcome, error)
	// Prune starts a job that removes every cached result from the executor's stores, returning its handle,
	// or refuses it while another job runs; no job is accepted while a prune runs.
	Prune(ctx context.Context) (JobID, error)
	// Release says the client is done with the job id, delivery included: its state may be freed, and a prune
	// may run; after it, the job's other calls may fail with ErrExpired. A job is held while a watch of it is open,
	// and for the executor's grace period after the last watch ended, or after submit if it was never watched;
	// a watch from past its terminal event, which delivers nothing, renews the hold.
	// A running build that is released has its demand withdrawn, as one whose hold lapsed does.
	Release(ctx context.Context, id JobID) error
}

// The errors that the executor's methods return about jobs.
var (
	ErrNoJob      = errors.New("no such job")
	ErrRunning    = errors.New("the job is still running")
	ErrFellBehind = errors.New("the watcher fell behind the events retained")
	// ErrExpired says the job existed on this executor but was released, or held by no one for its grace period,
	// and its state has been freed; its finished work is cached.
	ErrExpired = errors.New("the job expired")
	// ErrSettling refuses a prune while attempts of ended jobs may still be writing, as a cancelled build's are
	// until its workers have heard, or an earlier instance's until their leases expire; prune again shortly.
	ErrSettling = errors.New("the executor's attempts are still settling; prune again shortly")
)

// Capabilities is what an executor reports about itself.
type Capabilities struct {
	// Platforms are the execution platforms it offers: those that run() steps run on without emulation.
	// A local executor offers its host's; a coordinator every platform its workers may have.
	Platforms []imgspecv1.Platform `json:"platforms"`
	// ExecPlatform, if set, is the execution platform that a client configures a graph for when none is asked
	// for, one of Platforms, so that clients on any platform configure EXEC_PLATFORM alike and share cache keys.
	// A coordinator gives it only if configured to; a client of an executor without one chooses its own.
	ExecPlatform  *imgspecv1.Platform `json:"exec_platform,omitempty"`
	GraphVersions []int               `json:"graph_versions"`      // the graph versions it runs
	Isolation     string              `json:"isolation,omitempty"` // how it runs commands, such as "chroot"
	Version       string              `json:"version,omitempty"`   // of vamos
	Clock         time.Time           `json:"clock"`               // the time by its clock, when it answered
	// Instance identifies the executor's jobs: new each time it starts without those of an earlier run, so that a
	// client whose connection dropped can tell a restart that lost its jobs from a drop alone. A coordinator keeps
	// its jobs on disk, and its instance with them, so that a restart that keeps its disk keeps its instance too.
	Instance string `json:"instance,omitempty"`
	// GracePeriod is how long a build whose client has gone is kept before its demand is withdrawn,
	// and so how long a client may take to come back; zero if the executor doesn't withdraw builds.
	GracePeriod time.Duration `json:"grace_period,omitempty"`
	// Generation identifies the executor's cache between prunes, for an executor that fences submissions by it, as a
	// coordinator does: a submission made for an earlier generation is refused, since what it captured may be gone.
	// A fencing executor's is never zero, so that a zero submitted asks for no check and can't pass for the first
	// generation. Zero for an executor that doesn't fence, as the local one, whose lock covers a build from capture
	// to finish.
	Generation uint64 `json:"generation,omitempty"`
}

// Offers reports whether the executor offers platform as an execution platform.
func (c Capabilities) Offers(platform imgspecv1.Platform) bool {
	want := platforms.Format(platforms.Normalize(platform))
	for _, p := range c.Platforms {
		if platforms.Format(platforms.Normalize(p)) == want {
			return true
		}
	}
	return false
}

// Request asks an executor for a job.
// Exactly one of its fields is set.
type Request struct {
	Graph  *GraphRequest  `json:"graph,omitempty"`
	Push   *PushRequest   `json:"push,omitempty"`
	Export *ExportRequest `json:"export,omitempty"`
}

// PushRequest asks an executor to push a result's image or index to a registry,
// with the executor's own credentials and registry settings.
type PushRequest struct {
	Result Result `json:"result"`
	// Destination is a full registry reference with a tag and no digest, such as ghcr.io/me/app:1.0;
	// the client resolves short names, so that both sides mean the same registry.
	Destination string `json:"destination"`
	// Trace, if set, asks for the spans of a trace of the push, as events of the job.
	Trace *tracing.CallContext `json:"trace,omitempty"`
}

// ExportRequest asks an executor to publish a result's content to the content store, for a client to deliver:
// an image as its manifest, config and layers, an index as those of each image and the index, a task as its tree.
type ExportRequest struct {
	Result Result `json:"result"`
	// Trace, if set, asks for the spans of a trace of the export, as events of the job.
	Trace *tracing.CallContext `json:"trace,omitempty"`
}

// GraphRequest asks an executor to run a graph.
type GraphRequest struct {
	Graph   cache.Blob `json:"graph"`   // the encoded graph, in the content store
	Version int        `json:"version"` // the graph's version, so that an executor can refuse one it can't run
	// Platform is the execution platform the graph was configured for, as EXEC_PLATFORM named it;
	// an executor that doesn't offer it refuses the graph.
	Platform  imgspecv1.Platform `json:"platform"`
	Rebuild   bool               `json:"rebuild,omitempty"`  // run the roots again, and resolve their tags again
	Timestamp time.Time          `json:"timestamp,omitzero"` // the earliest time that targets which run stamp with
	// Emulate permits run() steps in images for another platform than Platform, which run under emulation;
	// without it, where an executor requires the permission, such a target fails before anything runs.
	// It is no part of any action's key: emulation is provenance, not identity.
	Emulate bool `json:"emulate,omitempty"`
	// Trace, if set, asks for the spans of a trace of the run, as events of the job.
	Trace *tracing.CallContext `json:"trace,omitempty"`
	// Generation is the executor's cache generation that the client read before capturing the build's sources;
	// a submission of an earlier generation than the executor's is refused, since what it captured may be gone.
	// Zero asks for no check.
	Generation uint64 `json:"generation,omitempty"`
	// Secrets are the values of the secrets that the graph's run() steps are given, by ID.
	// They are never encoded, so no transport carries them: only an executor in the client's process gets them,
	// and a client refuses to send a build that uses one elsewhere.
	Secrets map[string][]byte `json:"-"`
}

// JobID is the handle of a job that an executor accepted.
type JobID string

// EventKind says what an event reports.
type EventKind string

// The kinds of event.
const (
	EventStatus EventKind = "status" // what is happening to nodes, such as "building" or "cached"
	EventOutput EventKind = "output" // what a node's run() steps print, or printed when its cached result was made
	EventSpans  EventKind = "spans"  // a batch of spans of the trace the job asked for
	EventGap    EventKind = "gap"    // events before First were not retained; a watch from before it gets this first
	EventDone   EventKind = "done"   // the job's last event, carrying its outcome
)

// Event is one thing that happened in a job.
// Kind says which other fields it uses.
type Event struct {
	Position int64     `json:"position"` // in the job's sequence, from zero
	Kind     EventKind `json:"kind"`
	// Nodes are the nodes of the job that the event concerns, and Action the key of the action serving them,
	// once actions have keys; an event about the job as a whole names neither.
	Nodes  []graph.Ref    `json:"nodes,omitempty"`
	Action digest.Digest  `json:"action,omitempty"`
	Status string         `json:"status,omitempty"` // status
	Stream byte           `json:"stream,omitempty"` // output: cache.StreamStdout or cache.StreamStderr
	Data   []byte         `json:"data,omitempty"`   // output
	Spans  []tracing.Span `json:"spans,omitempty"`  // spans
	First  int64          `json:"first,omitempty"`  // gap: the first position retained; Position is the one asked for
	// Restarted says of a gap that the executor restarted since Position: the events from First on are of the job's
	// run since, which reports again what finished before as cached, or its outcome alone, if the restart ended it.
	Restarted bool     `json:"restarted,omitempty"`
	Outcome   *Outcome `json:"outcome,omitempty"` // done
}

// Outcome is how a job ended.
// A job that succeeded sets the field of its kind; one that failed sets Failure.
type Outcome struct {
	Results   []Result  `json:"results,omitempty"`   // a graph job's, one for each root, in order
	Pushed    *Pushed   `json:"pushed,omitempty"`    // a push job's
	Exported  *Exported `json:"exported,omitempty"`  // an export job's
	Pruned    *Pruned   `json:"pruned,omitempty"`    // a prune job's
	Failure   *Failure  `json:"failure,omitempty"`   // why the job failed, if it did
	Cancelled bool      `json:"cancelled,omitempty"` // it ended because it was cancelled
}

// Pushed is what a push made at its registry: exactly one of its fields is set.
type Pushed struct {
	Image string `json:"image,omitempty"` // an image's ID there, which may differ from Result.Image if converted
	Index string `json:"index,omitempty"` // an index's manifest digest there, as algorithm:hex
}

// Exported is a result's content, published to the content store, which a client delivers from there.
// An image or index sets Manifest, MediaType and Blobs; a task sets Files and Epoch.
type Exported struct {
	Manifest  cache.Blob `json:"manifest,omitzero"` // the image's manifest, or the index's
	MediaType string     `json:"media_type,omitempty"`
	// Blobs is what Manifest refers to: an image's config and layers, and, for an index, each image's manifest
	// with its config and layers.
	Blobs []cache.Blob `json:"blobs,omitempty"`
	Files *cache.Blob  `json:"files,omitempty"` // a task's files, as a tree
	Epoch int64        `json:"epoch,omitempty"` // when Files' files were stamped, in Unix seconds
}

// Result is what a root of a graph made.
// Exactly one of Image, Index and Files is set, unless the root is a task without output.
type Result struct {
	Node  graph.Ref `json:"node"`
	Image string    `json:"image,omitempty"` // a pull's or layer's image ID: the digest of its config, encoded
	// Locations is where Image's bytes are, in the order to try them, so that delivery and push can read the image
	// from wherever it is; the image is identified by Image alone.
	Locations []cache.Location `json:"locations,omitempty"`
	Index     []Manifest       `json:"index,omitempty"` // an index's images, in order
	Files     *cache.Blob      `json:"files,omitempty"` // a task's files, as a tree
	Epoch     int64            `json:"epoch"`           // when Files' files were stamped, in Unix seconds
}

// Manifest is an image of an index.
type Manifest struct {
	Image     string           `json:"image"`               // its ID, as Result.Image gives it
	Platform  string           `json:"platform"`            // as platforms.Format gives it
	Locations []cache.Location `json:"locations,omitempty"` // where its bytes are, as Result.Locations gives them
}

// Pruned counts what a prune job removed.
type Pruned struct {
	Images  int `json:"images"`         // of cached layers, from the executor's image store
	Results int `json:"results"`        // action results
	Blobs   int `json:"blobs"`          // content blobs
	Tags    int `json:"tags,omitempty"` // image tags removed from a coordinator's cache repository
}

// Failure reports why a job failed.
// Nodes, Action and Step are set as far as they are known: a failure before any action existed,
// such as a refused graph, names none.
// The client renders it against its graph with Describe; the executor never names a target.
type Failure struct {
	Nodes   []graph.Ref   `json:"nodes,omitempty"`
	Action  digest.Digest `json:"action,omitempty"`
	Step    *int          `json:"step,omitempty"` // the index of the step that failed, among its node's
	Message string        `json:"message"`
	Hint    string        `json:"hint,omitempty"` // what to do about it, if there is advice
	Log     *cache.Blob   `json:"log,omitempty"`  // what the failed attempt printed, as a log blob
}

// Describe returns the failure as a message for the user, naming the node it concerns and the step that failed
// as g has them; without g, or for a failure that names no node of g, the message and hint alone.
func (f *Failure) Describe(g *graph.Graph) string {
	msg := f.Message
	if f.Hint != "" {
		msg += "; " + f.Hint
	}
	if g == nil || len(f.Nodes) == 0 || f.Nodes[0] < 0 || int(f.Nodes[0]) >= len(g.Nodes) {
		return msg
	}
	n := g.Node(f.Nodes[0])
	if f.Step != nil && *f.Step >= 0 && *f.Step < len(n.Steps()) {
		return fmt.Sprintf("%s: %s failed: %s", n.Desc, n.Steps()[*f.Step].Display, msg)
	}
	return fmt.Sprintf("%s: %s", n.Desc, msg)
}
