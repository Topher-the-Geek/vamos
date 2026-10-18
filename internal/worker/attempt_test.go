package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// TestEstimateOffset checks the estimate one heartbeat gives of this clock's offset from the coordinator's,
// and that the surer estimate is kept.
func TestEstimateOffset(t *testing.T) {
	sent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// The coordinator's clock is 10s behind: it read 00:00:01 at the midpoint of a 2s round trip.
	remote := sent.Add(time.Second).Add(-10 * time.Second)
	got := estimateOffset(sent, sent.Add(2*time.Second), remote, nil)
	if got == nil || got.Offset != 10*time.Second || got.Uncertainty != time.Second {
		t.Fatalf("estimate = %+v, want this clock 10s ahead, give or take 1s", got)
	}
	// A longer round trip is less sure, so the first estimate is kept; a shorter one replaces it.
	worse := estimateOffset(sent, sent.Add(4*time.Second), remote.Add(time.Second), got)
	if worse != got {
		t.Errorf("a less sure estimate %+v replaced %+v", worse, got)
	}
	better := estimateOffset(sent, sent.Add(time.Second), sent.Add(500*time.Millisecond).Add(-9*time.Second), got)
	if better == nil || better.Offset != 9*time.Second || better.Uncertainty != 500*time.Millisecond {
		t.Errorf("a surer estimate = %+v, want 9s give or take 500ms", better)
	}
	// An answer without a clock changes nothing.
	if estimateOffset(sent, sent.Add(time.Second), time.Time{}, got) != got {
		t.Error("an answer without a clock changed the estimate")
	}
	var zero *tracing.Offset
	if estimateOffset(sent, sent.Add(time.Second), time.Time{}, zero) != nil {
		t.Error("an answer without a clock made an estimate from nothing")
	}
}

// fencedAfter runs a heartbeat against a coordinator that answers the first request after delay, unless delay is
// zero, and hangs every other, and returns how long after renewed, the first renewal's send, the attempt was killed.
func fencedAfter(t *testing.T, reg remote.Registration, delay time.Duration, renewed time.Time) time.Duration {
	t.Helper()
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 && delay > 0 {
			time.Sleep(delay)
			rw.Header().Set("Content-Type", "application/json")
			rw.Write([]byte("{}"))
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) }) // before the server's close, which waits for the hung requests
	client, err := remote.NewWorkerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	w := &Worker{client: client}
	killed := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.heartbeat(ctx, reg, "a", renewed, nil, func(error) { killed <- time.Now() })
	select {
	case at := <-killed:
		return at.Sub(renewed)
	case <-time.After(3 * reg.Lease):
		t.Fatal("the attempt was never fenced")
		return 0
	}
}

// TestFenceCountsFromSend checks that a renewal whose answer is slow extends the fence only from when the request
// was sent, since the coordinator renewed on receiving it: otherwise the worker would outlive the lease.
func TestFenceCountsFromSend(t *testing.T) {
	reg := remote.Registration{Lease: 900 * time.Millisecond, Heartbeat: 300 * time.Millisecond}
	// The first heartbeat goes at 300ms and is answered at 550ms; the fence is 80% of the lease after its send,
	// 1020ms, where counting from the answer would be 1270ms.
	got := fencedAfter(t, reg, 250*time.Millisecond, time.Now())
	if got < 950*time.Millisecond || got > 1150*time.Millisecond {
		t.Errorf("fenced %s after the start, want about 1020ms, counted from the send of the last renewal", got)
	}
}

// TestFenceCountsFromRenewal checks that the fence counts from the first renewal's send, not from when the
// attempt's process started, which setup can delay.
func TestFenceCountsFromRenewal(t *testing.T) {
	reg := remote.Registration{Lease: 900 * time.Millisecond, Heartbeat: 300 * time.Millisecond}
	got := fencedAfter(t, reg, 0, time.Now().Add(-600*time.Millisecond))
	if got < 700*time.Millisecond || got > 850*time.Millisecond {
		t.Errorf("fenced %s after the renewal, want about 720ms", got)
	}
}

// renewing returns a worker whose coordinator answers heartbeats with answer, after delay, and counts them.
func renewing(t *testing.T, delay time.Duration, answer func(rw http.ResponseWriter)) (*Worker, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body) // the server notices the client leaving only once it has read the request
		select {
		case <-time.After(delay):
			answer(rw)
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) }) // before the server's close, which waits for the hung requests
	client, err := remote.NewWorkerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &Worker{client: client}, &requests
}

// TestRenewFirst checks that an attempt starts only once a renewal is answered, counting its fence from that
// renewal's send: the assignment may have arrived late in a lease that started before it was sent.
func TestRenewFirst(t *testing.T) {
	reg := remote.Registration{Lease: 900 * time.Millisecond, Heartbeat: 300 * time.Millisecond}
	answered := func(rw http.ResponseWriter) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"clock":"2026-01-01T00:00:00Z"}`))
	}

	w, _ := renewing(t, 100*time.Millisecond, answered)
	before := time.Now()
	renewed, clock, err := w.renewFirst(t.Context(), reg, "a", before.Add(-time.Hour))
	if err != nil || renewed.Before(before) || time.Since(renewed) < 100*time.Millisecond || clock == nil {
		t.Errorf("renewFirst = %s after the call, %+v, %v; want the send of the answered renewal and an estimate",
			renewed.Sub(before), clock, err)
	}

	w, _ = renewing(t, 0, func(rw http.ResponseWriter) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusConflict)
		rw.Write([]byte(`{"code":"stale_attempt","message":"the attempt is no longer live"}`))
	})
	if _, _, err := w.renewFirst(t.Context(), reg, "a", time.Now()); !errors.Is(err, remote.ErrStaleAttempt) {
		t.Errorf("renewFirst of a stale attempt = %v, want ErrStaleAttempt", err)
	}

	w, _ = renewing(t, 0, func(rw http.ResponseWriter) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"kill":true}`))
	})
	if _, _, err := w.renewFirst(t.Context(), reg, "a", time.Now()); err == nil {
		t.Error("renewFirst of an attempt the coordinator kills succeeded")
	}

	// A coordinator that never answers is asked again each interval, until a lease since the assignment has passed.
	w, requests := renewing(t, time.Hour, answered)
	assigned := time.Now()
	_, _, err = w.renewFirst(t.Context(), reg, "a", assigned)
	took := time.Since(assigned)
	if err == nil || took < reg.Lease || took > reg.Lease+reg.Heartbeat+200*time.Millisecond {
		t.Errorf("renewFirst with no answer = %v after %s; want a failure once the lease had passed", err, took)
	}
	if got := requests.Load(); got < 3 {
		t.Errorf("renewFirst sent %d heartbeats, want one each interval", got)
	}
}

// TestKillWithinSharesItsDeadline checks that shutdown gives up on every attempt that doesn't end once the one deadline
// has passed, instead of waiting on the second for a timer the first has consumed.
func TestKillWithinSharesItsDeadline(t *testing.T) {
	var logged atomic.Int32
	w := &Worker{attempts: map[string]*running{}, cfg: Config{Log: countingWriter{&logged}}}
	for _, id := range []string{"a", "b"} {
		w.attempts[id] = &running{id: id, cancel: func() {}, done: make(chan struct{})} // never ends
	}
	finished := make(chan struct{})
	go func() {
		w.killWithin(200 * time.Millisecond)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("killWithin was still waiting long after its deadline, with two attempts that don't end")
	}
	if got := logged.Load(); got != 2 {
		t.Errorf("killWithin logged %d attempts that didn't end, want both", got)
	}
}

// countingWriter counts its writes.
type countingWriter struct{ n *atomic.Int32 }

// Write counts one write.
func (c countingWriter) Write(p []byte) (int, error) {
	c.n.Add(1)
	return len(p), nil
}

// TestRelayBoundsEvents checks that output too large for one request crosses in batches the transport takes, in
// order, and that a coordinator that doesn't take events stops the subprocess's output from being read, rather than
// the worker holding it all.
func TestRelayBoundsEvents(t *testing.T) {
	var mu sync.Mutex
	var got bytes.Buffer
	var largest int
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		<-release
		body, _ := io.ReadAll(r.Body)
		var events remote.AttemptEvents
		if err := json.Unmarshal(body, &events); err != nil {
			t.Errorf("decoding a batch: %v", err)
		}
		mu.Lock()
		largest = max(largest, len(body))
		for _, o := range events.Output {
			got.Write(o.Data)
		}
		mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client, err := remote.NewWorkerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	w := &Worker{client: client, cfg: Config{Log: io.Discard}}

	// 48 writes of 1.5MiB each: far more than a request may hold, or than may wait.
	var want bytes.Buffer
	var written atomic.Int64
	r, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for i := range 48 {
			data := bytes.Repeat([]byte{byte('a' + i%26)}, 3<<19)
			want.Write(data)
			line, _ := json.Marshal(run.AttemptLine{Output: &run.OutputLine{Stream: 1, Data: data}})
			n, err := pw.Write(append(line, '\n'))
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.relay(t.Context(), "a", r)
	}()
	time.Sleep(time.Second)
	// What was read is what waits, a line over the bound at most, and one more line in the scanner.
	if n := written.Load(); n > maxPending+2*(2<<20)+maxBatch {
		t.Errorf("%d bytes were read while the coordinator took nothing, want at most about %d", n, maxPending)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the relay never finished")
	}
	if largest > maxBatch+1<<10 {
		t.Errorf("the largest batch was %d bytes, want at most %d", largest, maxBatch)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Errorf("the coordinator received %d bytes of output, want the %d written, in order", got.Len(), want.Len())
	}
}
