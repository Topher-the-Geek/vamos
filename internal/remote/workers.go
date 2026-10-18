package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// The paths of the worker-facing API.
const (
	pathWorkers     = "/v1/workers"
	pathAssignments = "/v1/workers/{id}/assignments"
	pathHeartbeat   = "/v1/attempts/{id}/heartbeat"
	pathAttemptOut  = "/v1/attempts/{id}/events"
	pathReport      = "/v1/attempts/{id}/report"
)

// WorkerInfo is what a worker says of itself when it registers.
type WorkerInfo struct {
	Platform      imgspecv1.Platform   `json:"platform"`           // the one its run() steps run on without emulation
	Emulates      []imgspecv1.Platform `json:"emulates,omitempty"` // the platforms it can run under emulation
	Isolation     string               `json:"isolation"`          // how it runs commands, such as "chroot"
	Version       string               `json:"version"`            // of vamos
	ActionVersion int                  `json:"action_version"`     // what its keys mean; it must be the coordinator's
	Capacity      int                  `json:"capacity"`           // how many attempts it runs at once
}

// Registration is what a coordinator answers a worker's registration with.
type Registration struct {
	ID        string        `json:"id"`        // the worker's, for its calls
	Instance  string        `json:"instance"`  // the coordinator's, so that a worker tells a restart
	Lease     time.Duration `json:"lease"`     // how long an attempt lives without a heartbeat
	Heartbeat time.Duration `json:"heartbeat"` // how often to renew; shorter than the lease
	// CacheRepository is where built layers are pushed, tagged by image ID, such as registry:5000/vamos-cache;
	// "" asks for no push, as a local executor behind a coordinator makes none.
	CacheRepository string `json:"cache_repository,omitempty"`
	// PullThrough maps each upstream registry to the pull-through cache that mirrors it, by host.
	PullThrough map[string]string `json:"pull_through,omitempty"`
}

// Assignment is one attempt for a worker to run.
type Assignment struct {
	Attempt string         `json:"attempt"`
	Request *sched.Request `json:"request,omitempty"` // an action to execute
	Push    *PushWork      `json:"push,omitempty"`    // or a push to make
	Lease   time.Duration  `json:"lease"`             // how long the attempt lives without a heartbeat
	// Trace, if set, asks the worker to trace the attempt as part of the coordinator's span Parent,
	// so that its spans nest where the coordinator started it.
	Trace *tracing.CallContext `json:"trace,omitempty"`
}

// PushWork is a push for a worker to make, from the image's locations to a destination.
type PushWork struct {
	Result      api.Result `json:"result"`
	Destination string     `json:"destination"`
}

// Heartbeat is what a worker sends to renew an attempt's lease.
type Heartbeat struct {
	// Clock is how far the worker's clock is ahead of the coordinator's, as the worker estimates it from the
	// answers to its earlier heartbeats, so that the coordinator shifts the attempt's spans onto its own clock;
	// nil before the first answer.
	Clock *tracing.Offset `json:"clock,omitempty"`
}

// HeartbeatAnswer is what a coordinator answers a heartbeat with.
type HeartbeatAnswer struct {
	Kill  bool      `json:"kill,omitempty"` // the attempt is no longer wanted: kill it and clean up
	Clock time.Time `json:"clock"`          // the coordinator's clock as it answered, for the worker's estimate
}

// AttemptEvents is what an attempt printed and traced since the last batch.
type AttemptEvents struct {
	Output []OutputEvent  `json:"output,omitempty"`
	Spans  []tracing.Span `json:"spans,omitempty"`
}

// OutputEvent is one write of an attempt's run() steps.
type OutputEvent struct {
	Stream byte   `json:"stream"`
	Data   []byte `json:"data"`
}

// AttemptReport is how an attempt ended: a report, a push's outcome, or a failure.
type AttemptReport struct {
	Report  *sched.Report         `json:"report,omitempty"`
	Pushed  *api.Pushed           `json:"pushed,omitempty"`
	Failure *sched.AttemptFailure `json:"failure,omitempty"`
}

// ErrStaleAttempt says that a report or heartbeat is for an attempt the coordinator no longer counts live:
// its lease expired, it was killed, or it was replaced by a retry.
var ErrStaleAttempt = errors.New("the attempt is no longer live")

// Workers is the coordinator's side of the worker-facing API, which Server serves.
type Workers interface {
	// Register adds a worker, returning what it needs to work.
	Register(ctx context.Context, info WorkerInfo) (Registration, error)
	// NextAssignment waits for an attempt for the worker id to run, until ctx ends, when it returns nil.
	NextAssignment(ctx context.Context, worker string) (*Assignment, error)
	// Heartbeat renews the lease of attempt, and says whether to kill it; ErrStaleAttempt if it isn't live.
	Heartbeat(ctx context.Context, attempt string, beat Heartbeat) (HeartbeatAnswer, error)
	// Events passes on what attempt printed and traced; ErrStaleAttempt if it isn't live.
	Events(ctx context.Context, attempt string, events AttemptEvents) error
	// Report ends attempt with how it ended; ErrStaleAttempt if it isn't live, so that a late report is discarded.
	Report(ctx context.Context, attempt string, report AttemptReport) error
}

// codeStale is the code ErrStaleAttempt crosses as.
const codeStale errorCode = "stale_attempt"

// pollTimeout is how long a worker's wait for an assignment lasts before it answers with none and asks again;
// it is shorter than responseHeaderTimeout, so that an idle poll ends in an answer, not in the client's timeout.
const pollTimeout = 20 * time.Second

// registerWorkers adds the worker-facing handlers to mux, serving w.
func (s *Server) registerWorkers(mux *http.ServeMux, w Workers) {
	mux.HandleFunc("POST "+pathWorkers, func(rw http.ResponseWriter, r *http.Request) {
		var info WorkerInfo
		if err := decodeJSON(r.Body, &info); err != nil {
			writeError(rw, r, fmt.Errorf("decoding the registration: %w", err))
			return
		}
		reg, err := w.Register(r.Context(), info)
		if err != nil {
			writeError(rw, r, err)
			return
		}
		writeJSON(rw, http.StatusCreated, reg)
	})
	mux.HandleFunc("GET "+pathAssignments, func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
		defer cancel()
		a, err := w.NextAssignment(ctx, r.PathValue("id"))
		switch {
		case err != nil && r.Context().Err() == nil && ctx.Err() == nil:
			writeError(rw, r, err)
		case a == nil:
			rw.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(rw, http.StatusOK, a)
		}
	})
	mux.HandleFunc("POST "+pathHeartbeat, func(rw http.ResponseWriter, r *http.Request) {
		var beat Heartbeat
		if err := decodeJSON(r.Body, &beat); err != nil {
			writeError(rw, r, fmt.Errorf("decoding the heartbeat: %w", err))
			return
		}
		answer, err := w.Heartbeat(r.Context(), r.PathValue("id"), beat)
		if err != nil {
			writeError(rw, r, err)
			return
		}
		writeJSON(rw, http.StatusOK, answer)
	})
	mux.HandleFunc("POST "+pathAttemptOut, func(rw http.ResponseWriter, r *http.Request) {
		var events AttemptEvents
		if err := decodeJSON(r.Body, &events); err != nil {
			writeError(rw, r, fmt.Errorf("decoding the events: %w", err))
			return
		}
		if err := w.Events(r.Context(), r.PathValue("id"), events); err != nil {
			writeError(rw, r, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+pathReport, func(rw http.ResponseWriter, r *http.Request) {
		var report AttemptReport
		if err := decodeJSON(r.Body, &report); err != nil {
			writeError(rw, r, fmt.Errorf("decoding the report: %w", err))
			return
		}
		if err := w.Report(r.Context(), r.PathValue("id"), report); err != nil {
			writeError(rw, r, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
}

// WorkerClient is a worker's side of the worker-facing API: Workers, over HTTP.
// Its calls aren't retried on their own: a worker that can't renew a lease must kill the attempt before the lease
// would expire, so it decides for itself what a failure to reach the coordinator means.
type WorkerClient struct {
	c *Client
}

// NewWorkerClient returns a worker's client of the coordinator at base.
func NewWorkerClient(base string) (*WorkerClient, error) {
	c, err := NewClient(base)
	if err != nil {
		return nil, err
	}
	return &WorkerClient{c: c}, nil
}

// Close releases the client's idle connections.
func (w *WorkerClient) Close() error {
	return w.c.Close()
}

// call makes one request with body, if not nil, and decodes the answer into out, if not nil, expecting status.
func (w *WorkerClient) call(ctx context.Context, method, path string, body, out any, status int) error {
	var req *http.Request
	var err error
	if body != nil {
		data, merr := json.Marshal(body)
		if merr != nil {
			return merr
		}
		req, err = w.c.newRequest(ctx, method, path, "", bytes.NewReader(data))
		if req != nil {
			req.Header.Set("Content-Type", contentJSON)
		}
	} else {
		req, err = w.c.newRequest(ctx, method, path, "", nil)
	}
	if err != nil {
		return err
	}
	resp, err := w.c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent && out == nil {
		return nil
	}
	if resp.StatusCode != status {
		return readError(resp)
	}
	if out == nil {
		return nil
	}
	if err := decodeJSON(resp.Body, out); err != nil {
		return &transportError{err: fmt.Errorf("reading the answer: %w", err)}
	}
	return nil
}

// Register registers the worker with the coordinator.
func (w *WorkerClient) Register(ctx context.Context, info WorkerInfo) (Registration, error) {
	var reg Registration
	err := w.call(ctx, http.MethodPost, pathWorkers, info, &reg, http.StatusCreated)
	return reg, err
}

// NextAssignment waits for an attempt to run, returning nil if the coordinator had none within its poll timeout.
func (w *WorkerClient) NextAssignment(ctx context.Context, worker string) (*Assignment, error) {
	req, err := w.c.newRequest(ctx, http.MethodGet, jobPath(pathAssignments, api.JobID(worker)), "", nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var a Assignment
		if err := decodeJSON(resp.Body, &a); err != nil {
			return nil, &transportError{err: fmt.Errorf("reading the answer: %w", err)}
		}
		return &a, nil
	}
	return nil, readError(resp)
}

// Heartbeat renews the attempt's lease and learns whether to kill it.
func (w *WorkerClient) Heartbeat(ctx context.Context, attempt string, beat Heartbeat) (HeartbeatAnswer, error) {
	var answer HeartbeatAnswer
	err := w.call(ctx, http.MethodPost, jobPath(pathHeartbeat, api.JobID(attempt)), beat, &answer, http.StatusOK)
	return answer, err
}

// Events sends what the attempt printed and traced.
func (w *WorkerClient) Events(ctx context.Context, attempt string, events AttemptEvents) error {
	return w.call(ctx, http.MethodPost, jobPath(pathAttemptOut, api.JobID(attempt)), events, nil,
		http.StatusNoContent)
}

// Report ends the attempt with how it ended.
func (w *WorkerClient) Report(ctx context.Context, attempt string, report AttemptReport) error {
	return w.call(ctx, http.MethodPost, jobPath(pathReport, api.JobID(attempt)), report, nil, http.StatusNoContent)
}

// IsTransport reports whether err is a failure to reach the coordinator, as opposed to an answer, so that a worker
// tells a partition from a refusal.
func IsTransport(err error) bool {
	return isTransport(err) || isUnreachable(err)
}

// isUnreachable reports whether err says the coordinator couldn't be reached before it ever answered.
func isUnreachable(err error) bool {
	var ue *unreachableError
	return errors.As(err, &ue)
}
