package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"

	"github.com/opencontainers/go-digest"
)

// Client uses a coordinator over HTTP as an api.Executor and a cache.ContentStore.
// Once the coordinator has answered once, a request that gets no answer, because the connection failed or
// dropped, is retried with backoff: reads, submissions, which carry an ID so that the coordinator runs each once,
// and watches, which resume from the last position delivered; a blob being read resumes where it was cut.
// The retries last the coordinator's grace period, as its capabilities give it, since a build whose client has been
// gone longer may have been withdrawn; a coordinator that answers with another instance ID has restarted without
// its jobs, and the build is lost, while one that kept them answers with the same, and the watch resumes.
// A coordinator that never answered isn't retried: a wrong URL, or one that is down, fails at once.
// Warn, if set, is told when the connection drops and when it resumes.
type Client struct {
	base string
	http *http.Client
	Warn func(msg string)
	// StreamTimeout is how long a watch waits for a line, events or the coordinator's empty keep-alive lines,
	// before taking the connection for dead; defaultStreamTimeout if zero.
	StreamTimeout time.Duration

	mu        sync.Mutex
	contacted bool          // the coordinator has answered once, so a failure to answer is a drop
	instance  string        // of the coordinator, as its capabilities first said; "" until asked
	grace     time.Duration // how long it keeps a build whose client has gone, as its capabilities said
}

var (
	_ api.Executor       = (*Client)(nil)
	_ cache.ContentStore = (*Client)(nil)
)

// NewClient returns a client of the coordinator at base, an http or https URL.
func NewClient(base string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("coordinator %q: %w", base, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("coordinator %q: want an http or https URL, such as http://coordinator:8080", base)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &Client{base: strings.TrimSuffix(u.String(), "/"), http: &http.Client{Transport: transport}}, nil
}

// Close releases the client's idle connections.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// The pace of retries after a connection drops, and the waits that tell a dead connection.
const (
	firstBackoff = 200 * time.Millisecond
	maxBackoff   = 2 * time.Second
	// defaultGrace is how long a client retries when the coordinator reported no grace period.
	defaultGrace = time.Minute
	// responseHeaderTimeout is how long a request waits for the coordinator to start answering.
	responseHeaderTimeout = 30 * time.Second
	// defaultStreamTimeout is how long a watch waits for a line; the coordinator sends one every 15 seconds at least.
	defaultStreamTimeout = time.Minute
)

// warn tells the owner msg, if it listens.
func (c *Client) warn(msg string) {
	if c.Warn != nil {
		c.Warn(msg)
	}
}

// graceWindow returns how long to retry after a drop.
func (c *Client) graceWindow() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.grace > 0 {
		return c.grace
	}
	return defaultGrace
}

// newRequest returns a request for path on the coordinator, with query, if not "", and body.
func (c *Client) newRequest(ctx context.Context, method, path, query string, body io.Reader) (*http.Request, error) {
	u := c.base + path
	if query != "" {
		u += "?" + query
	}
	return http.NewRequestWithContext(ctx, method, u, body)
}

// do sends req and returns the answer, or a transportError if none came, so that the caller may retry;
// a coordinator that has never answered isn't retried, and its failure says it can't be reached.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		switch ctxErr := req.Context().Err(); {
		case errors.Is(ctxErr, context.Canceled):
			return nil, ctxErr
		case ctxErr != nil:
			return nil, &transportError{err: ctxErr} // the call's deadline passed without an answer
		}
		c.mu.Lock()
		contacted := c.contacted
		c.mu.Unlock()
		if !contacted {
			return nil, &unreachableError{base: c.base, err: err}
		}
		return nil, &transportError{err: err}
	}
	c.mu.Lock()
	c.contacted = true
	c.mu.Unlock()
	return resp, nil
}

// outage is a call's dropped connection, which it recovers from by trying again: when the connection dropped,
// and how long to wait before the next try.
// A call keeps one across all its requests, and ends it only once the coordinator is heard from again,
// so that a connection that drops again at once, however far its request got, counts against the grace period.
type outage struct {
	c       *Client
	what    string        // what the call is doing, for the warnings
	dropped time.Time     // when the connection dropped; zero while it is up
	backoff time.Duration // to wait before the next try
}

// failed records err, a transport failure, and waits before the next try: it warns when the connection drops,
// and fails once the grace period has passed since, or when ctx ends.
func (o *outage) failed(ctx context.Context, err error) error {
	if o.dropped.IsZero() {
		o.dropped, o.backoff = time.Now(), firstBackoff
		o.c.warn(fmt.Sprintf("the connection to the coordinator dropped while %s (%v); retrying", o.what, err))
	} else if lost := time.Since(o.dropped); lost > o.c.graceWindow() {
		return fmt.Errorf("the connection to the coordinator was lost for %s while %s: %w; "+
			"the build may have been withdrawn, and its completed work is cached, so run the command again",
			lost.Round(time.Second), o.what, err)
	}
	select {
	case <-time.After(o.backoff):
	case <-ctx.Done():
		return ctx.Err()
	}
	o.backoff = min(2*o.backoff, maxBackoff)
	return nil
}

// recovered ends the outage, if there is one, warning that the connection is back.
func (o *outage) recovered() {
	if !o.dropped.IsZero() {
		o.dropped = time.Time{}
		o.c.warn("the connection to the coordinator is back")
	}
}

// retrying runs attempt, which makes one request and handles its answer, again on each transport failure,
// with backoff, until it gets an answer or the grace period has passed since the first failure.
// It warns once when the connection drops and once when it is back.
func (c *Client) retrying(ctx context.Context, what string, attempt func() error) error {
	o := outage{c: c, what: what}
	for {
		err := attempt()
		if err == nil || !isTransport(err) {
			if err == nil {
				o.recovered()
			}
			return err
		}
		if err := o.failed(ctx, err); err != nil {
			return err
		}
	}
}

// getJSON asks for path and decodes the answer into v, retrying transport failures.
func (c *Client) getJSON(ctx context.Context, what, path, query string, v any) error {
	return c.retrying(ctx, what, func() error {
		req, err := c.newRequest(ctx, http.MethodGet, path, query, nil)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return readError(resp)
		}
		if err := decodeJSON(resp.Body, v); err != nil {
			return &transportError{err: fmt.Errorf("reading the answer: %w", err)}
		}
		return nil
	})
}

// Capabilities asks the coordinator what it offers, and remembers which instance answered and its grace period.
func (c *Client) Capabilities(ctx context.Context) (api.Capabilities, error) {
	var caps api.Capabilities
	if err := c.getJSON(ctx, "asking its capabilities", pathCapabilities, "", &caps); err != nil {
		return api.Capabilities{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.instance == "" {
		c.instance = caps.Instance
	}
	c.grace = caps.GracePeriod
	return caps, nil
}

// errRestarted says that the coordinator isn't the one the build was submitted to: it restarted without its jobs.
var errRestarted = errors.New("the coordinator restarted, so the build is lost; running the command again reuses " +
	"what finished")

// checkInstance asks the coordinator which instance it is, after a drop, and reports a restart as errRestarted.
// A transport failure is returned as such, so that the caller retries.
func (c *Client) checkInstance(ctx context.Context) error {
	c.mu.Lock()
	known := c.instance
	c.mu.Unlock()
	req, err := c.newRequest(ctx, http.MethodGet, pathCapabilities, "", nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	var caps api.Capabilities
	if err := decodeJSON(resp.Body, &caps); err != nil {
		return &transportError{err: fmt.Errorf("reading the answer: %w", err)}
	}
	if known != "" && caps.Instance != known {
		return errRestarted
	}
	return nil
}

// newSubmission returns an ID for a submission, which its retries carry.
func newSubmission() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// submit posts body to path as a submission, retrying transport failures with the same submission ID,
// so that the coordinator starts one job, and returns the job's handle.
// A retry names the instance of the coordinator the client knows, if it asked, so that one that has restarted
// since, which can't know whether the submission was accepted, refuses it; that fails with restarted.
func (c *Client) submit(ctx context.Context, what, path string, body []byte, restarted error) (api.JobID, error) {
	submission := newSubmission()
	retry := false
	var handle jobHandle
	err := c.retrying(ctx, what, func() error {
		req, err := c.newRequest(ctx, http.MethodPost, path, "", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set(headerSubmission, submission)
		if body != nil {
			req.Header.Set("Content-Type", contentJSON)
		}
		c.mu.Lock()
		if instance := c.instance; retry && instance != "" {
			req.Header.Set(headerInstance, instance)
		}
		c.mu.Unlock()
		retry = true
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return readError(resp)
		}
		if err := decodeJSON(resp.Body, &handle); err != nil {
			return &transportError{err: fmt.Errorf("reading the answer: %w", err)}
		}
		return nil
	})
	if errors.Is(err, errOtherInstance) {
		return "", restarted
	}
	return handle.ID, err
}

// errRestartedSubmitting says that the coordinator restarted before it acknowledged a submission.
var errRestartedSubmitting = errors.New("the coordinator restarted before it acknowledged the submission, " +
	"so the job is lost if it started; running the command again reuses what finished")

// Submit asks the coordinator for the job req asks for.
// A push whose coordinator restarted before acknowledging it may have pushed: the failure says so.
func (c *Client) Submit(ctx context.Context, req api.Request) (api.JobID, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	restarted := errRestartedSubmitting
	if req.Push != nil {
		restarted = errors.New("the coordinator restarted before it acknowledged the push, so whether it pushed " +
			"is unknown; check the destination, or push again")
	}
	return c.submit(ctx, "submitting a job", pathJobs, body, restarted)
}

// Prune asks the coordinator for a prune job.
func (c *Client) Prune(ctx context.Context) (api.JobID, error) {
	return c.submit(ctx, "submitting a prune", pathPrune, nil, errRestartedSubmitting)
}

// Cancel asks the job id to stop.
func (c *Client) Cancel(ctx context.Context, id api.JobID) error {
	return c.post(ctx, "cancelling the job", jobPath(pathCancel, id))
}

// Release says the client is done with the job id, retrying transport failures.
func (c *Client) Release(ctx context.Context, id api.JobID) error {
	return c.post(ctx, "releasing the job", jobPath(pathRelease, id))
}

// post posts to path, a call that answers with no content, retrying transport failures.
func (c *Client) post(ctx context.Context, what, path string) error {
	return c.retrying(ctx, what, func() error {
		req, err := c.newRequest(ctx, http.MethodPost, path, "", nil)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return readError(resp)
		}
		return nil
	})
}

// Result returns the outcome of the job id, or ErrRunning.
func (c *Client) Result(ctx context.Context, id api.JobID) (api.Outcome, error) {
	var outcome api.Outcome
	err := c.getJSON(ctx, "reading the job's outcome", jobPath(pathResult, id), "", &outcome)
	return outcome, err
}

// jobPath returns pattern, a path with the job's place in it, for the job id.
func jobPath(pattern string, id api.JobID) string {
	return strings.Replace(pattern, "{id}", url.PathEscape(string(id)), 1)
}

// Watch delivers the events of the job id from position from to fn, as api.Executor.Watch describes,
// resuming from the last position delivered when the connection drops, for the grace period;
// a coordinator that has restarted meanwhile ends the watch with errRestarted.
// The outage ends once the stream delivers a line again, so that a later drop has the whole grace period.
func (c *Client) Watch(ctx context.Context, id api.JobID, from int64, fn func(api.Event) error) error {
	next := from
	o := outage{c: c, what: "watching the build"}
	for {
		err := c.watchOnce(ctx, id, &next, fn, o.recovered)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case !isTransport(err):
			if !o.dropped.IsZero() && errors.Is(err, api.ErrNoJob) {
				return errRestarted // the coordinator that knew the job is gone
			}
			if errors.Is(err, api.ErrExpired) {
				return fmt.Errorf("%w: the coordinator held the build for its grace period without a watcher, "+
					"then let it go; its finished work is cached, so run the command again", err)
			}
			return err
		}
		if err := o.failed(ctx, err); err != nil {
			return err
		}
		if err := c.checkInstance(ctx); err != nil && !isTransport(err) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// watchOnce streams the job's events from *next to fn, moving *next past each one delivered, until the stream ends
// cleanly, with nil, or with the error it carried, or drops, with a transportError; a stream that sends nothing,
// not even a keep-alive line, for StreamTimeout while it is waited on is taken for dropped, but fn may take as long
// as it likes. Each line that arrives is told to alive.
func (c *Client) watchOnce(ctx context.Context, id api.JobID, next *int64, fn func(api.Event) error,
	alive func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, jobPath(pathEvents, id), "from="+strconv.FormatInt(*next, 10),
		nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	timeout := c.StreamTimeout
	if timeout <= 0 {
		timeout = defaultStreamTimeout
	}
	watchdog := time.AfterFunc(timeout, cancel) // a stream cut for silence is a drop

	defer watchdog.Stop()
	dec := json.NewDecoder(resp.Body)
	for {
		var line streamLine
		err := dec.Decode(&line)
		// The watchdog watches the wait for a line alone; one that fired, even as the line arrived, cut the stream.
		if !watchdog.Stop() {
			return &transportError{err: fmt.Errorf("nothing arrived for %s", timeout)}
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &transportError{err: fmt.Errorf("the event stream ended: %w", err)}
		}
		alive()
		switch {
		case line.Event != nil:
			// The end of ctx stops the watch before the next event, though the line was already read.
			if err := ctx.Err(); err != nil {
				return err
			}
			e := *line.Event
			if err := fn(e); err != nil {
				return err
			}
			if e.Kind == api.EventGap {
				*next = e.First // the events before it are gone, so a resume needn't be told again
			} else {
				*next = e.Position + 1
			}
		case line.Error != nil:
			return line.Error
		case line.Done:
			return nil
		}
		watchdog.Reset(timeout)
	}
}

// blobQuery returns the query that names b's size.
func blobQuery(b cache.Blob) string {
	return "size=" + strconv.FormatInt(b.Size, 10)
}

// blobPath returns the path of the blob b.
func blobPath(b cache.Blob) string {
	return strings.Replace(pathBlob, "{digest}", string(b.Digest), 1)
}

// Has reports whether the coordinator's store holds b.
func (c *Client) Has(ctx context.Context, b cache.Blob) error {
	if err := b.Validate(); err != nil {
		return err
	}
	return c.retrying(ctx, "asking for a blob", func() error {
		req, err := c.newRequest(ctx, http.MethodHead, blobPath(b), blobQuery(b), nil)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return readError(resp)
		}
		return nil
	})
}

// postJSON posts v to path and decodes the answer into out, retrying transport failures.
func (c *Client) postJSON(ctx context.Context, what, path, query string, v, out any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.retrying(ctx, what, func() error {
		req, err := c.newRequest(ctx, http.MethodPost, path, query, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", contentJSON)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return readError(resp)
		}
		if out == nil {
			return nil
		}
		if err := decodeJSON(resp.Body, out); err != nil {
			return &transportError{err: fmt.Errorf("reading the answer: %w", err)}
		}
		return nil
	})
}

// Missing returns which of blobs the coordinator's store lacks, in one round trip.
func (c *Client) Missing(ctx context.Context, blobs []cache.Blob) ([]cache.Blob, error) {
	for _, b := range blobs {
		if err := b.Validate(); err != nil {
			return nil, err
		}
	}
	if blobs == nil {
		blobs = []cache.Blob{}
	}
	var missing []cache.Blob
	if err := c.postJSON(ctx, "asking which blobs it lacks", pathMissing, "", blobs, &missing); err != nil {
		return nil, err
	}
	return missing, nil
}

// limitedReader reads at most limit+1 bytes of r, counting them, so that a put of more than limit bytes is seen as
// over the limit on this side without sending the rest.
type limitedReader struct {
	r    io.Reader
	left int64
	read int64
}

// Read reads from the stream, up to the limit.
func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	l.read += int64(n)
	return n, err
}

// Put sends what r holds, up to limit bytes, to the coordinator's store, and returns its blob.
// A stream can't be sent again, so a transport failure isn't retried.
func (c *Client) Put(ctx context.Context, r io.Reader, limit int64) (cache.Blob, error) {
	lr := &limitedReader{r: r, left: limit + 1}
	req, err := c.newRequest(ctx, http.MethodPost, pathBlobs, "limit="+strconv.FormatInt(limit, 10), lr)
	if err != nil {
		return cache.Blob{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		if lr.read > limit {
			// The coordinator may have cut the connection on seeing too much; either way, the put is over.
			return cache.Blob{}, fmt.Errorf("%w: %d", cache.ErrOverLimit, limit)
		}
		return cache.Blob{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return cache.Blob{}, readError(resp)
	}
	var b cache.Blob
	if err := decodeJSON(resp.Body, &b); err != nil {
		return cache.Blob{}, &transportError{err: fmt.Errorf("reading the answer: %w", err)}
	}
	return b, nil
}

// PutBytes sends data to the coordinator's store and returns its blob.
func (c *Client) PutBytes(ctx context.Context, data []byte) (cache.Blob, error) {
	return c.Put(ctx, bytes.NewReader(data), int64(len(data)))
}

// Import sends what r holds to the coordinator's store as b, which it must be.
func (c *Client) Import(ctx context.Context, b cache.Blob, r io.Reader) error {
	if err := b.Validate(); err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPut, blobPath(b), blobQuery(b), &limitedReader{r: r, left: b.Size + 1})
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return readError(resp)
	}
	return nil
}

// Get returns b's content from the coordinator's store, as a stream that verifies it.
// A connection that drops while the blob is read is resumed where it was cut, as a watch is;
// a blob the coordinator finds corrupt as it sends it is reported as corrupt.
func (c *Client) Get(ctx context.Context, b cache.Blob) (io.ReadCloser, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	r := &resuming{client: c, ctx: ctx, blob: b, outage: outage{c: c, what: "reading a blob"}}
	if err := r.open(); err != nil {
		return nil, err
	}
	return cache.Verify(r, b), nil
}

// resuming is a blob's content as the coordinator sends it, which asks for the rest from where it was cut
// when the connection drops, so that a long read survives a drop.
// One outage spans the read, its answers and their bodies: it ends once content arrives again, so that a body that
// keeps dropping before sending anything is retried with backoff, and for the grace period alone.
type resuming struct {
	client *Client
	ctx    context.Context
	blob   cache.Blob
	outage outage
	body   io.ReadCloser  // the current answer's, nil between a drop and the next answer, and once ended
	resp   *http.Response // whose trailer says why the body ended early, if the coordinator cut it
	offset int64          // bytes delivered so far
	cut    error          // why the last body dropped, until the next is asked for
	ended  bool           // the coordinator ended the body, so there is nothing more to ask for
}

// open asks for the blob from the offset, retrying transport failures.
func (r *resuming) open() error {
	for {
		err := r.request()
		if err == nil || !isTransport(err) {
			return err
		}
		if err := r.outage.failed(r.ctx, err); err != nil {
			return err
		}
	}
}

// request asks for the blob from the offset once.
func (r *resuming) request() error {
	req, err := r.client.newRequest(r.ctx, http.MethodGet, blobPath(r.blob), blobQuery(r.blob), nil)
	if err != nil {
		return err
	}
	if r.offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.offset))
	}
	resp, err := r.client.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		return readError(resp)
	}
	r.body, r.resp = resp.Body, resp
	return nil
}

// Read reads the content, asking for the rest where the connection drops; a body the coordinator ended early
// ends the content, and the error in its trailer is reported, naming the blob.
func (r *resuming) Read(p []byte) (int, error) {
	for {
		if r.ended {
			return 0, io.EOF
		}
		if r.body == nil {
			// The connection dropped: wait, then ask for the rest, or report why it can't be had.
			if err := r.outage.failed(r.ctx, r.cut); err != nil {
				return 0, err
			}
			if err := r.open(); err != nil {
				return 0, err
			}
		}
		n, err := r.body.Read(p)
		r.offset += int64(n)
		if n > 0 || errors.Is(err, io.EOF) {
			r.outage.recovered()
		}
		switch {
		case err == nil:
			return n, nil
		case errors.Is(err, io.EOF):
			r.body.Close()
			r.body, r.ended = nil, true
			switch code := errorCode(r.resp.Trailer.Get(headerErrorCode)); code {
			case "":
				return n, io.EOF
			case codeCorrupt:
				return n, cache.Corrupt(r.blob, "the coordinator found it corrupt as it sent it")
			default:
				return n, &wireError{Code: code, Message: "the coordinator stopped sending the blob"}
			}
		case r.ctx.Err() != nil:
			return n, r.ctx.Err()
		}
		r.body.Close()
		r.body, r.cut = nil, &transportError{err: err}
		if n > 0 {
			return n, nil
		}
	}
}

// Close ends the read.
func (r *resuming) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

// Read returns b's content, verified, or ErrOverLimit if b is larger than limit, reading nothing.
func (c *Client) Read(ctx context.Context, b cache.Blob, limit int64) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if b.Size > limit {
		return nil, fmt.Errorf("blob %s holds %d bytes, %w: %d", b.Digest, b.Size, cache.ErrOverLimit, limit)
	}
	r, err := c.Get(ctx, b)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// ReadMany returns the content of each of blobs, verified, in as few round trips as the coordinator allows;
// each must be at most limit bytes.
func (c *Client) ReadMany(ctx context.Context, blobs []cache.Blob, limit int64) ([][]byte, error) {
	for _, b := range blobs {
		if err := b.Validate(); err != nil {
			return nil, err
		}
		if b.Size > limit {
			return nil, fmt.Errorf("blob %s holds %d bytes, %w: %d", b.Digest, b.Size, cache.ErrOverLimit, limit)
		}
	}
	contents := make([][]byte, 0, len(blobs))
	for start := 0; start < len(blobs); start += maxReadMany {
		read, err := c.readBatch(ctx, blobs[start:min(start+maxReadMany, len(blobs))], limit)
		if err != nil {
			return nil, err
		}
		contents = append(contents, read...)
	}
	return contents, nil
}

// readBatch returns the content of each of blobs, at most maxReadMany, in one round trip.
func (c *Client) readBatch(ctx context.Context, blobs []cache.Blob, limit int64) ([][]byte, error) {
	body, err := json.Marshal(blobs)
	if err != nil {
		return nil, err
	}
	var contents [][]byte
	err = c.retrying(ctx, "reading blobs", func() error {
		req, err := c.newRequest(ctx, http.MethodPost, pathReadMany, "limit="+strconv.FormatInt(limit, 10),
			bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", contentJSON)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return readError(resp)
		}
		contents = make([][]byte, len(blobs))
		for i, b := range blobs {
			data := make([]byte, b.Size)
			if _, err := io.ReadFull(resp.Body, data); err != nil {
				return &transportError{err: fmt.Errorf("reading blob %s: %w", b.Digest, err)}
			}
			if got := digest.FromBytes(data); got != b.Digest {
				return cache.Corrupt(b, "its content has digest "+got.String())
			}
			contents[i] = data
		}
		return nil
	})
	return contents, err
}

// ClearContent is refused: the coordinator's store is cleared by a prune, which waits for no build to use it.
func (c *Client) ClearContent(context.Context) (int, error) {
	return 0, errors.New("the coordinator's store can't be cleared directly while builds may use it; prune instead")
}
