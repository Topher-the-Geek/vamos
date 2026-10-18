// Package apitest tests implementations of api.Executor: JSON proves that an executor's values cross as JSON,
// and Contract checks the job contract on any implementation.
// It is for tests alone.
package apitest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Topher-the-Geek/vamos/internal/api"
)

// JSON is an executor that passes every request, result and event through JSON on its way to and from ex,
// as a transport would, so that a test sees what a remote client would.
type JSON struct {
	ex api.Executor
}

// NewJSON returns an executor that delegates to ex through JSON.
func NewJSON(ex api.Executor) *JSON {
	return &JSON{ex: ex}
}

// roundTrip encodes v as JSON and decodes it into a new value of its type, which it returns.
// It panics if v doesn't survive: a value that crosses the executor boundary always does.
func roundTrip[T any](v T) T {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("encoding %T: %v", v, err))
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("decoding %T from %s: %v", v, data, err))
	}
	return out
}

// roundTripError returns err as a transport would deliver it: by its message alone,
// but for the errors of api, which a proxy maps back to the same values, and a watcher's own error,
// which never crosses.
func roundTripError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, api.ErrNoJob):
		return fmt.Errorf("%w: %s", api.ErrNoJob, err)
	case errors.Is(err, api.ErrRunning):
		return fmt.Errorf("%w: %s", api.ErrRunning, err)
	case errors.Is(err, api.ErrFellBehind):
		return fmt.Errorf("%w: %s", api.ErrFellBehind, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err // the caller's own context ended the call
	}
	return errors.New(err.Error())
}

// Capabilities reports ex's capabilities, through JSON.
func (j *JSON) Capabilities(ctx context.Context) (api.Capabilities, error) {
	c, err := j.ex.Capabilities(ctx)
	return roundTrip(c), roundTripError(err)
}

// Submit submits req to ex, through JSON.
func (j *JSON) Submit(ctx context.Context, req api.Request) (api.JobID, error) {
	id, err := j.ex.Submit(ctx, roundTrip(req))
	return roundTrip(id), roundTripError(err)
}

// Watch watches the job id on ex, passing each event to fn through JSON.
// An error of fn is the watcher's own and crosses no transport, so it is returned as it is.
func (j *JSON) Watch(ctx context.Context, id api.JobID, from int64, fn func(api.Event) error) error {
	var fnErr error
	err := j.ex.Watch(ctx, roundTrip(id), from, func(e api.Event) error {
		fnErr = fn(roundTrip(e))
		return fnErr
	})
	if err != nil && fnErr != nil && err == fnErr {
		return fnErr
	}
	return roundTripError(err)
}

// Cancel cancels the job id on ex.
func (j *JSON) Cancel(ctx context.Context, id api.JobID) error {
	return roundTripError(j.ex.Cancel(ctx, roundTrip(id)))
}

// Result returns the outcome of the job id on ex, through JSON.
func (j *JSON) Result(ctx context.Context, id api.JobID) (api.Outcome, error) {
	o, err := j.ex.Result(ctx, roundTrip(id))
	return roundTrip(o), roundTripError(err)
}

// Release releases the job id on ex, through JSON.
func (j *JSON) Release(ctx context.Context, id api.JobID) error {
	return roundTripError(j.ex.Release(ctx, roundTrip(id)))
}

// Prune starts a prune job on ex.
func (j *JSON) Prune(ctx context.Context) (api.JobID, error) {
	id, err := j.ex.Prune(ctx)
	return roundTrip(id), roundTripError(err)
}
