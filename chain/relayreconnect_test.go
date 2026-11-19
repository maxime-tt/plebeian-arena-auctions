package main

// Deterministic, socket-only-on-loopback tests for the reconnect layer.
//
// The incident these pin (2026-11-19): the relay restarted, the publisher's
// socket died, and the publisher kept writing to the dead client for 1h45m.
// These tests hold the three behaviours the fix must have:
//
//	1. a relay that drops the connection mid-run -> the publish re-dials and
//	   succeeds, and the failure is visible;
//	2. a relay that stays down -> attempts are gated by an exponential
//	   backoff, counted, and logged once then rate-limited;
//	3. a healthy relay -> one dial, no retries, no log lines.
//
// They use httptest on 127.0.0.1 and a fake clock, so they need no nak, no
// mint, no public relay and no sleeps.

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeRelay is a NIP-01-shaped relay good enough for the publish path: it
// accepts EVENT frames and answers OK, with two scripted failures.
type fakeRelay struct {
	srv *httptest.Server

	mu        sync.Mutex
	conns     int
	live      []*websocket.Conn // server side, so Close can really kill them
	events    []string
	dropFirst bool // close the FIRST connection right after its first EVENT, no OK
	reject    bool // answer every EVENT with OK=false
}

func newFakeRelay(t *testing.T, dropFirst, reject bool) *fakeRelay {
	t.Helper()
	f := &fakeRelay{dropFirst: dropFirst, reject: reject}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns++
		n := f.conns
		f.live = append(f.live, conn)
		f.mu.Unlock()
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame []json.RawMessage
			if json.Unmarshal(data, &frame) != nil || len(frame) < 2 {
				continue
			}
			var kind string
			if json.Unmarshal(frame[0], &kind) != nil || kind != "EVENT" {
				continue
			}
			var ev Event
			if json.Unmarshal(frame[1], &ev) != nil {
				continue
			}
			f.mu.Lock()
			f.events = append(f.events, ev.ID)
			count := len(f.events)
			f.mu.Unlock()
			if f.dropFirst && n == 1 && count == 1 {
				return // the socket dies under the publisher, mid-run
			}
			ok := !f.reject
			msg := ""
			if f.reject {
				msg = "blocked: this relay does not take that kind"
			}
			reply, _ := json.Marshal([]any{"OK", ev.ID, ok, msg})
			if err := conn.WriteMessage(websocket.TextMessage, reply); err != nil {
				return
			}
		}
	}))
	return f
}

// Close kills the server side of every socket (httptest does not track
// hijacked websocket connections) and then the listener, which is exactly the
// "the relay restarted" shape.
func (f *fakeRelay) Close() {
	f.mu.Lock()
	for _, c := range f.live {
		_ = c.Close()
	}
	f.live = nil
	f.mu.Unlock()
	f.srv.Close()
}

func (f *fakeRelay) WSURL() string { return strings.Replace(f.srv.URL, "http://", "ws://", 1) }

func (f *fakeRelay) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func (f *fakeRelay) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// testClock is a hand-advanced clock: backoff and log rate limiting are
// asserted without sleeping.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Unix(1_800_000_000, 0)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func fakeRelayEvent(seed string) *Event {
	return &Event{
		ID: strings.Repeat(seed, 64), Pubkey: strings.Repeat("d", 64), CreatedAt: 1,
		Kind: KindStateCommit, Tags: [][]string{}, Content: "state", Sig: strings.Repeat("e", 128),
	}
}

// A connection that dies mid-run is replaced on the NEXT publish attempt: the
// retry happens inside the same Publish, on a fresh socket, and the event that
// the dead socket swallowed is the one that lands.
func TestReconnectingWriterRedialsAfterDroppedConnection(t *testing.T) {
	relay := newFakeRelay(t, true, false)
	defer relay.Close()
	logs := &syncBuffer{}
	w := NewReconnectingWriter(relay.WSURL())
	w.Log = log.New(logs, "", 0)
	defer w.Close()
	ctx := context.Background()

	if err := w.Publish(ctx, fakeRelayEvent("a")); err != nil {
		t.Fatalf("publish across a dropped connection: %v", err)
	}
	if got := relay.connCount(); got != 2 {
		t.Fatalf("relay connections = %d, want 2 (the dead one plus the re-dial)", got)
	}
	if got := len(relay.seen()); got != 2 {
		t.Fatalf("events received = %d, want 2 (the lost write and the retry)", got)
	}
	h := w.PublishHealth()
	if !h.Connected || h.Dials != 2 || h.Failures != 1 || h.ConsecutiveFailures != 0 {
		t.Fatalf("health after recovery = %+v, want connected, 2 dials, 1 failed attempt, 0 consecutive", h)
	}
	if !strings.Contains(logs.String(), relay.WSURL()) {
		t.Fatalf("failure log does not name the relay URL: %q", logs.String())
	}
	t.Logf("health after the drop+retry: %+v", h)
	t.Logf("log: %s", strings.TrimSpace(logs.String()))

	// The live connection is reused: a further publish must not dial again.
	if err := w.Publish(ctx, fakeRelayEvent("b")); err != nil {
		t.Fatalf("publish on the recovered connection: %v", err)
	}
	if h = w.PublishHealth(); relay.connCount() != 2 || h.Dials != 2 {
		t.Fatalf("healthy publish re-dialled: connections=%d dials=%d, want 2/2", relay.connCount(), h.Dials)
	}

	// And a publish on a socket whose peer has vanished must RETURN, not hang:
	// this is the shape that froze the live publisher.
	relay.Close()
	done := make(chan error, 1)
	go func() { done <- w.Publish(ctx, fakeRelayEvent("c")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("publish after the relay went away succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publish hung on a dead connection instead of failing")
	}
}

// A relay that stays down is not dialled once per publish pass: the backoff
// gate defers, the failure is counted, and the log is written once and then
// rate-limited.  The delay doubles and is capped.
func TestReconnectingWriterBacksOffCountsAndLogs(t *testing.T) {
	// A port nothing listens on: bind, read the address, close the listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	url := "ws://" + ln.Addr().String()
	ln.Close()

	clock := newTestClock()
	logs := &syncBuffer{}
	w := NewReconnectingWriter(url)
	w.Now = clock.Now
	w.Log = log.New(logs, "", 0)
	w.BackoffBase = time.Second
	w.BackoffMax = 8 * time.Second
	w.LogEvery = 30 * time.Second
	ctx := context.Background()
	ev := fakeRelayEvent("f")

	for i := range 3 {
		if err := w.Publish(ctx, ev); err == nil {
			t.Fatalf("publish %d to a dead relay succeeded", i+1)
		}
	}
	h := w.PublishHealth()
	if h.Dials != 1 {
		t.Fatalf("dials after 3 passes inside the backoff window = %d, want 1", h.Dials)
	}
	if h.Failures != 1 || h.Deferrals != 2 || h.Connected || h.ConsecutiveFailures != 1 {
		t.Fatalf("health = %+v, want 1 failed attempt, 2 deferrals, disconnected", h)
	}
	if h.LastError == "" || !strings.Contains(h.LastError, url) {
		t.Fatalf("lastError = %q, want it to name %s", h.LastError, url)
	}
	t.Logf("health while down (3 passes inside the window): %+v", h)
	t.Logf("log: %s", strings.TrimSpace(logs.String()))
	if got := logLineCount(logs.String()); got != 1 {
		t.Fatalf("log lines after 3 failures inside the window = %d, want 1 (first failure only):\n%s", got, logs.String())
	}
	first := logs.String()
	if !strings.Contains(first, url) || !strings.Contains(first, "dial") {
		t.Fatalf("failure log must name the relay and the error, got %q", first)
	}

	// Past the backoff the writer dials again, and the delay doubles (capped).
	wantDelay := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, want := range wantDelay {
		clock.Advance(10 * time.Second) // always past the current window
		if err := w.Publish(ctx, ev); err == nil {
			t.Fatalf("publish %d to a dead relay succeeded", i+1)
		}
		h = w.PublishHealth()
		if h.Dials != int64(i+2) {
			t.Fatalf("dials = %d after retry %d, want %d", h.Dials, i+1, i+2)
		}
		got := time.Unix(h.NextDialAt, 0).Sub(clock.Now())
		if got != want {
			t.Fatalf("backoff after dial %d = %s, want %s", h.Dials, got, want)
		}
	}
	t.Logf("health after %d gated+retried passes: %+v", 3+len(wantDelay), h)
	t.Logf("log lines total: %d (rate-limited to one per LogEvery)", logLineCount(logs.String()))
	if h.Failures != 1+int64(len(wantDelay)) || h.Deferrals != 2 {
		t.Fatalf("health = %+v, want %d failures and 2 deferrals", h, 1+len(wantDelay))
	}
}

// A healthy relay keeps the behaviour it had: one dial, one attempt per event,
// no retry and no log line.
func TestReconnectingWriterHealthyRelayDoesNotRedial(t *testing.T) {
	relay := newFakeRelay(t, false, false)
	defer relay.Close()
	logs := &syncBuffer{}
	w := NewReconnectingWriter(relay.WSURL())
	w.Log = log.New(logs, "", 0)
	defer w.Close()
	ctx := context.Background()

	for _, seed := range []string{"1", "2", "3"} {
		if err := w.Publish(ctx, fakeRelayEvent(seed)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if relay.connCount() != 1 {
		t.Fatalf("relay connections = %d, want 1", relay.connCount())
	}
	if len(relay.seen()) != 3 {
		t.Fatalf("events = %d, want 3", len(relay.seen()))
	}
	h := w.PublishHealth()
	if !h.Connected || h.Dials != 1 || h.Failures != 0 || h.ConsecutiveFailures != 0 || h.Rejections != 0 || h.Deferrals != 0 {
		t.Fatalf("health = %+v, want a clean single-dial transport", h)
	}
	if h.LastSuccessAt == 0 {
		t.Fatal("lastSuccessAt is unset after a successful publish")
	}
	if logs.String() != "" {
		t.Fatalf("healthy publishing logged: %q", logs.String())
	}
}

// OK=false is the relay's own answer, not a broken socket: the connection is
// kept, the rejection is counted separately, and no transport failure is
// logged (the publisher records the rejection on its PublishRecord).
func TestReconnectingWriterKeepsSocketOnRelayRejection(t *testing.T) {
	relay := newFakeRelay(t, false, true)
	defer relay.Close()
	logs := &syncBuffer{}
	w := NewReconnectingWriter(relay.WSURL())
	w.Log = log.New(logs, "", 0)
	defer w.Close()
	ctx := context.Background()

	for i := range 2 {
		err := w.Publish(ctx, fakeRelayEvent("9"))
		if err == nil || !strings.Contains(err.Error(), "relay rejected event") {
			t.Fatalf("publish %d error = %v, want a relay rejection", i+1, err)
		}
	}
	h := w.PublishHealth()
	if !h.Connected || h.Dials != 1 || h.Rejections != 2 || h.Failures != 0 || h.Deferrals != 0 {
		t.Fatalf("health = %+v, want 1 dial, 2 rejections, no transport failure", h)
	}
	if relay.connCount() != 1 {
		t.Fatalf("relay connections = %d, want 1 (a rejection must not re-dial)", relay.connCount())
	}
	if logs.String() != "" {
		t.Fatalf("a relay rejection was logged as a transport failure: %q", logs.String())
	}
}

// logLineCount counts non-empty log lines.
func logLineCount(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
