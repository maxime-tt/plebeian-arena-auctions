package main

// A minimal NIP-01 relay client: exactly the two operations the verdict-publish
// path needs -- publish a signed event, and subscribe to read events back.
//
// It is deliberately NOT go-nostr.  The ingest branch (feat/relay-ingest) uses
// go-nostr v0.52.1 and carries a documented internal data race on connection
// teardown; the publish path needs none of go-nostr's surface (no NIP-19, no
// pool, no nip42), so a small client over gorilla/websocket -- already in this
// module's graph via CometBFT -- keeps the dependency light and the teardown
// race-free.  If the two branches converge, go-nostr can back this interface
// without touching the publisher.
//
// Wire format (NIP-01):
//
//	client -> relay: ["EVENT", <event>]
//	                 ["REQ", <subid>, <filter>, ...]
//	                 ["CLOSE", <subid>]
//	relay -> client: ["OK", <event_id>, true|false, <message>]
//	                 ["EVENT", <subid>, <event>]
//	                 ["EOSE", <subid>]
//	                 ["NOTICE", <message>]

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// RelayFilter is the subset of a NIP-01 filter this client uses: kinds and
// authors.  Both are optional; an empty filter matches everything.
type RelayFilter struct {
	Kinds   []int    `json:"kinds,omitempty"`
	Authors []string `json:"authors,omitempty"`
}

// RelayClient is one websocket connection to a relay.  Writes are serialised;
// reads run on one goroutine that fans out OK receipts and subscription events.
//
// A RelayClient is ONE socket.  It is never re-dialled: its dead channel closes
// the moment the read loop ends, and every later Publish fails at once with
// ErrRelayClosed instead of waiting on a socket nobody will answer on.  The
// re-dial policy lives in ReconnectingWriter, which owns the client's lifetime.
type RelayClient struct {
	url  string
	conn *websocket.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	okWait  map[string]chan error
	subs    map[string]chan *Event
	subSeq  int64
	readErr error

	closeOnce sync.Once
	closed    chan struct{}
	dead      chan struct{} // closed once, when the read loop ends or Close runs
	deadOnce  sync.Once
}

// ErrRelayClosed is what a publish on a socket whose read loop has ended
// returns.  It is a TRANSPORT failure: another socket may well succeed.
var ErrRelayClosed = errors.New("relay connection closed")

// DialRelay connects to a relay websocket.  It is the caller's job to Close.
func DialRelay(ctx context.Context, url string) (*RelayClient, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial %s: %w (http %d)", url, err, resp.StatusCode)
		}
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	c := &RelayClient{url: url, conn: conn, okWait: map[string]chan error{}, subs: map[string]chan *Event{},
		closed: make(chan struct{}), dead: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

// readLoop dispatches relay frames until the connection ends, then marks the
// client dead, fails every pending publish and closes every subscription so no
// caller blocks forever.
func (c *RelayClient) readLoop() {
	defer func() {
		c.markDead()
		c.failAll(c.closedError())
	}()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		var frame []json.RawMessage
		if json.Unmarshal(data, &frame) != nil || len(frame) == 0 {
			continue
		}
		var kind string
		if json.Unmarshal(frame[0], &kind) != nil {
			continue
		}
		switch kind {
		case "OK":
			if len(frame) < 3 {
				continue
			}
			var id string
			var ok bool
			if json.Unmarshal(frame[1], &id) != nil || json.Unmarshal(frame[2], &ok) != nil {
				continue
			}
			var msg string
			if len(frame) >= 4 {
				_ = json.Unmarshal(frame[3], &msg)
			}
			c.mu.Lock()
			ch := c.okWait[id]
			delete(c.okWait, id)
			c.mu.Unlock()
			if ch != nil {
				if ok {
					ch <- nil
				} else {
					ch <- relayRejection{id: id, msg: msg}
				}
			}
		case "EVENT":
			if len(frame) < 3 {
				continue
			}
			var subid string
			if json.Unmarshal(frame[1], &subid) != nil {
				continue
			}
			var ev Event
			if json.Unmarshal(frame[2], &ev) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.subs[subid]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- &ev:
				case <-c.closed:
				}
			}
		}
	}
}

// markDead closes the client's dead channel exactly once.
func (c *RelayClient) markDead() {
	c.deadOnce.Do(func() { close(c.dead) })
}

// closedError names the relay and the underlying read error, so a publish
// failure is actionable in a log line.
func (c *RelayClient) closedError() error {
	c.mu.Lock()
	err := c.readErr
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrRelayClosed, c.url, err)
	}
	return fmt.Errorf("%w: %s", ErrRelayClosed, c.url)
}

// failAll releases every waiter and closes every subscription channel.
func (c *RelayClient) failAll(reason error) {
	c.mu.Lock()
	pending := c.okWait
	c.okWait = map[string]chan error{}
	subs := c.subs
	c.subs = map[string]chan *Event{}
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- reason:
		default:
		}
	}
	for _, ch := range subs {
		close(ch)
	}
}

// relayRejection is the relay's own OK=false answer.  Retrying the SAME event
// on a fresh socket cannot change it, so it must not be mistaken for a
// transport failure: only a relay-rejection-shaped error skips the re-dial.
type relayRejection struct{ id, msg string }

func (e relayRejection) Error() string { return "relay rejected event " + e.id + ": " + e.msg }

// publishAckTimeout bounds the wait for OK once the EVENT frame is away.  A
// relay that accepts the frame and then answers nothing leaves the socket
// half-alive; without a bound the caller would wait until its context dies --
// in the chain that was until the PROCESS exited, the exact shape of the
// 2026-11-19 freeze.  A missing OK is treated as a transport failure.
const publishAckTimeout = 15 * time.Second

// Publish sends a signed event and waits for the relay's OK.  It fails at once
// on a socket whose read loop has ended (ErrRelayClosed) rather than waiting
// out a receipt that will never come.
func (c *RelayClient) Publish(ctx context.Context, ev *Event) error {
	if ev == nil {
		return errors.New("publish: nil event")
	}
	select {
	case <-c.dead:
		return c.closedError()
	default:
	}
	receipt := make(chan error, 1)
	c.mu.Lock()
	c.okWait[ev.ID] = receipt
	c.mu.Unlock()
	drop := func() {
		c.mu.Lock()
		delete(c.okWait, ev.ID)
		c.mu.Unlock()
	}

	frame := []any{"EVENT", ev}
	body, err := json.Marshal(frame)
	if err != nil {
		drop()
		return err
	}
	if err := c.write(body); err != nil {
		drop()
		return fmt.Errorf("%s: write event %s: %w", c.url, ev.ID, err)
	}
	ack := time.NewTimer(publishAckTimeout)
	defer ack.Stop()
	select {
	case err := <-receipt:
		return err
	case <-ctx.Done():
		drop()
		return ctx.Err()
	case <-c.dead:
		drop()
		return c.closedError()
	case <-ack.C:
		drop()
		return fmt.Errorf("%s: no OK for event %s within %s", c.url, ev.ID, publishAckTimeout)
	}
}

// Subscribe opens a subscription and returns a channel of events plus a cancel
// function that sends CLOSE.  The channel is closed when the connection drops.
func (c *RelayClient) Subscribe(ctx context.Context, filters ...RelayFilter) (<-chan *Event, func(), error) {
	c.mu.Lock()
	c.subSeq++
	subid := "arena-" + strconv.FormatInt(c.subSeq, 10)
	ch := make(chan *Event, 256)
	c.subs[subid] = ch
	c.mu.Unlock()

	frame := []any{"REQ", subid}
	for _, f := range filters {
		frame = append(frame, f)
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return nil, nil, err
	}
	if err := c.write(body); err != nil {
		return nil, nil, err
	}
	cancel := func() {
		c.mu.Lock()
		delete(c.subs, subid)
		c.mu.Unlock()
		closeFrame, _ := json.Marshal([]any{"CLOSE", subid})
		_ = c.write(closeFrame)
	}
	return ch, cancel, nil
}

func (c *RelayClient) write(body []byte) error {
	select {
	case <-c.dead:
		return ErrRelayClosed
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, body)
}

// Close shuts the connection down.  Safe to call more than once.
func (c *RelayClient) Close() error {
	var err error
	c.markDead()
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.conn.Close()
	})
	return err
}

// RelayWriter is the publisher's transport: anything that can publish a signed
// event.  A ReconnectingWriter (the production writer) and the tests' in-memory
// recorders both satisfy it.  The publisher never knows whether the relay is
// nak, a public relay, or a fake.
type RelayWriter interface {
	Publish(ctx context.Context, ev *Event) error
}

// Default reconnect/visibility bounds for ReconnectingWriter.
const (
	DefaultPublishBackoffBase = time.Second
	DefaultPublishBackoffMax  = 30 * time.Second
	DefaultPublishLogEvery    = 30 * time.Second
)

// ReconnectingWriter is the publisher's transport for ONE relay, and it owns
// the connection's lifetime.  The pre-fix publisher was handed the RelayClient
// DialRelays returned and never replaced it: when the relay restarted and the
// socket died, every pass wrote to that dead socket for as long as the process
// lived (measured: 1h45m with nothing published, incident 2026-11-19).
//
// Policy:
//   - a publish on a broken socket drops the client, re-dials and retries the
//     SAME event once, immediately;
//   - a relay that stays down is retried on an exponential backoff
//     (BackoffBase doubling to BackoffMax); an attempt made inside the backoff
//     window is a cheap deferred failure, not a dial;
//   - the first failure is logged with the relay URL and the error, and repeats
//     are logged at most every LogEvery, so an outage is visible in the log
//     instead of being discovered by a human noticing stale events;
//   - PublishHealth reports the counters and the last error, so a gateway can
//     serve the transport's state to an operator.
//
// A relay-rejection answer (OK=false) is NOT a transport failure: a fresh
// socket cannot change the relay's answer to the same event, so it is counted
// separately and not retried here.  The publisher's own retry policy for a
// claim is unchanged: it retries on the next Step.
type ReconnectingWriter struct {
	URL string

	// BackoffBase and BackoffMax bound the delay between re-dials while the
	// relay is unreachable.  Zero means the Default* constants.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// LogEvery is the minimum gap between two failure log lines while a failure
	// persists.  Zero means DefaultPublishLogEvery.  The FIRST failure is always
	// logged.
	LogEvery time.Duration
	// Log receives the failures and the recovery line.  Nil means log.Default().
	Log *log.Logger
	// Now and Dial are seams for tests.  Nil means time.Now and DialRelay.
	Now  func() time.Time
	Dial func(ctx context.Context, url string) (*RelayClient, error)

	opMu sync.Mutex // serialises publishes: the connection swap must not race

	mu          sync.Mutex
	client      *RelayClient
	dials       int64
	failures    int64
	consecutive int64
	rejections  int64
	deferrals   int64
	attempts    int // consecutive failed dials/publishes; the backoff exponent
	closed      bool
	lastErr     error
	lastErrAt   time.Time
	lastOK      time.Time
	lastLog     time.Time
	nextDial    time.Time
}

// NewReconnectingWriter returns a writer for url.  It dials lazily on the first
// publish unless the caller uses DialRelays, which dials eagerly.
func NewReconnectingWriter(url string) *ReconnectingWriter {
	return &ReconnectingWriter{URL: url}
}

// Publish publishes ev, replacing a dead connection and retrying once.
func (w *ReconnectingWriter) Publish(ctx context.Context, ev *Event) error {
	w.opMu.Lock()
	defer w.opMu.Unlock()

	c, err := w.clientFor(ctx, false)
	if err != nil {
		return err // clientFor counted it (dial failure or a deferred attempt)
	}
	err = c.Publish(ctx, ev)
	if err == nil {
		w.recordSuccess()
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	if isRelayRejection(err) {
		w.recordRejection()
		return err
	}
	// The socket is gone or never answered: drop it, dial again and retry the
	// SAME event once.  This is the retry the pre-fix publisher never made.
	w.discard(c)
	w.recordFailure(err, true)
	c, derr := w.clientFor(ctx, true)
	if derr != nil {
		return derr
	}
	err = c.Publish(ctx, ev)
	if err == nil {
		w.recordSuccess()
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	if isRelayRejection(err) {
		w.recordRejection()
		return err
	}
	w.discard(c)
	w.recordFailure(err, true)
	return err
}

// clientFor returns a live client, dialing when there is none.  Unless force is
// set it refuses to dial before nextDial: that gate is the bounded backoff, and
// it is what keeps a down relay from costing a dial per publish pass.
func (w *ReconnectingWriter) clientFor(ctx context.Context, force bool) (*RelayClient, error) {
	w.mu.Lock()
	c := w.client
	next := w.nextDial
	lastErr := w.lastErr
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("relay %s: writer closed", w.URL)
	}
	if c != nil {
		return c, nil
	}
	now := w.now()
	if !force && !next.IsZero() && now.Before(next) {
		w.mu.Lock()
		w.deferrals++
		w.mu.Unlock()
		if lastErr == nil {
			lastErr = errors.New("relay unreachable")
		}
		return nil, fmt.Errorf("relay %s: next dial in %s: %v", w.URL, next.Sub(now).Round(time.Millisecond), lastErr)
	}
	c, err := w.dial(ctx)
	if err != nil {
		w.recordFailure(err, true)
		return nil, err
	}
	return c, nil
}

func (w *ReconnectingWriter) dial(ctx context.Context) (*RelayClient, error) {
	dial := w.Dial
	if dial == nil {
		dial = DialRelay
	}
	w.mu.Lock()
	w.dials++
	w.mu.Unlock()
	c, err := dial(ctx, w.URL)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.client = c
	w.mu.Unlock()
	return c, nil
}

// discard closes c and forgets it if it is still the current client.
func (w *ReconnectingWriter) discard(c *RelayClient) {
	w.mu.Lock()
	if w.client == c {
		w.client = nil
	}
	w.mu.Unlock()
	_ = c.Close()
}

// recordFailure counts one failed attempt, advances the backoff when asked,
// stores the error and logs it: always the first time, then rate-limited.
func (w *ReconnectingWriter) recordFailure(err error, advanceBackoff bool) {
	now := w.now()
	w.mu.Lock()
	w.failures++
	w.consecutive++
	w.lastErr = err
	w.lastErrAt = now
	if advanceBackoff {
		w.attempts++
		w.nextDial = now.Add(w.backoffFor(w.attempts))
	}
	var retry time.Duration
	if !w.nextDial.IsZero() && w.nextDial.After(now) {
		retry = w.nextDial.Sub(now)
	}
	logIt := w.lastLog.IsZero() || now.Sub(w.lastLog) >= w.logEvery()
	if logIt {
		w.lastLog = now
	}
	consecutive := w.consecutive
	w.mu.Unlock()
	if !logIt {
		return
	}
	if retry > 0 {
		w.logger().Printf("relay %s: publish relay failure (%d consecutive, next dial in %s): %v",
			w.URL, consecutive, retry.Round(time.Millisecond), err)
		return
	}
	w.logger().Printf("relay %s: publish relay failure (%d consecutive): %v", w.URL, consecutive, err)
}

func (w *ReconnectingWriter) recordSuccess() {
	now := w.now()
	w.mu.Lock()
	recovered := w.consecutive
	w.consecutive = 0
	w.attempts = 0
	w.nextDial = time.Time{}
	w.lastOK = now
	w.mu.Unlock()
	if recovered > 0 {
		w.logger().Printf("relay %s: publishing again after %d failed attempt(s)", w.URL, recovered)
	}
}

func (w *ReconnectingWriter) recordRejection() {
	w.mu.Lock()
	w.rejections++
	w.mu.Unlock()
}

// Close drops the current connection.  Safe to call more than once.
func (w *ReconnectingWriter) Close() error {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	w.mu.Lock()
	c := w.client
	w.client = nil
	w.closed = true
	w.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Close()
}

// PublishHealth is the observable state of one relay publisher: a gateway
// serves it so a persistent failure is visible from OUTSIDE the process,
// rather than being inferred from events that stopped appearing.
type PublishHealth struct {
	URL                 string `json:"url"`
	Connected           bool   `json:"connected"`
	Dials               int64  `json:"dials"`
	Failures            int64  `json:"failures"`
	ConsecutiveFailures int64  `json:"consecutiveFailures"`
	Rejections          int64  `json:"rejections"`
	Deferrals           int64  `json:"deferrals"`
	LastError           string `json:"lastError,omitempty"`
	LastErrorAt         int64  `json:"lastErrorAt,omitempty"`
	LastSuccessAt       int64  `json:"lastSuccessAt,omitempty"`
	NextDialAt          int64  `json:"nextDialAt,omitempty"`
}

// PublishHealth reports the writer's state.
func (w *ReconnectingWriter) PublishHealth() PublishHealth {
	w.mu.Lock()
	defer w.mu.Unlock()
	h := PublishHealth{
		URL: w.URL, Connected: w.client != nil, Dials: w.dials,
		Failures: w.failures, ConsecutiveFailures: w.consecutive,
		Rejections: w.rejections, Deferrals: w.deferrals,
	}
	if w.lastErr != nil {
		h.LastError = w.lastErr.Error()
	}
	if !w.lastErrAt.IsZero() {
		h.LastErrorAt = w.lastErrAt.Unix()
	}
	if !w.lastOK.IsZero() {
		h.LastSuccessAt = w.lastOK.Unix()
	}
	if !w.nextDial.IsZero() {
		h.NextDialAt = w.nextDial.Unix()
	}
	return h
}

func (w *ReconnectingWriter) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *ReconnectingWriter) logger() *log.Logger {
	if w.Log != nil {
		return w.Log
	}
	return log.Default()
}

func (w *ReconnectingWriter) logEvery() time.Duration {
	if w.LogEvery > 0 {
		return w.LogEvery
	}
	return DefaultPublishLogEvery
}

// backoffFor is BackoffBase doubled (attempts-1) times, capped at BackoffMax.
func (w *ReconnectingWriter) backoffFor(attempts int) time.Duration {
	base := w.BackoffBase
	if base <= 0 {
		base = DefaultPublishBackoffBase
	}
	max := w.BackoffMax
	if max <= 0 {
		max = DefaultPublishBackoffMax
	}
	d := base
	for i := 1; i < attempts && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// isRelayRejection reports whether the relay itself refused the event
// (OK=false) rather than the transport failing.
func isRelayRejection(err error) bool {
	var r relayRejection
	return errors.As(err, &r)
}

// MultiRelayWriter publishes to several relays.  Publishing to more than one
// relay is the mitigation for a single relay showing different verdicts to
// different clients (equivocation): the client diffs them.  A publish succeeds
// if at least one relay accepts it; if every relay fails, the error is returned
// so the publisher retries on the next Step.
type MultiRelayWriter struct {
	Writers []RelayWriter
}

func (m MultiRelayWriter) Publish(ctx context.Context, ev *Event) error {
	if len(m.Writers) == 0 {
		return errors.New("no relays configured")
	}
	var lastErr error
	accepted := 0
	for _, w := range m.Writers {
		if err := w.Publish(ctx, ev); err != nil {
			lastErr = err
			continue
		}
		accepted++
	}
	if accepted == 0 {
		return lastErr
	}
	return nil
}

// Health reports every writer that can describe itself.  In production each
// writer is a ReconnectingWriter; a test recorder has nothing to say and is
// skipped.
func (m MultiRelayWriter) Health() []PublishHealth {
	out := make([]PublishHealth, 0, len(m.Writers))
	for _, w := range m.Writers {
		if h, ok := w.(interface{ PublishHealth() PublishHealth }); ok {
			out = append(out, h.PublishHealth())
		}
	}
	return out
}

// DialRelays builds a reconnecting writer per relay URL and dials each once to
// warm it, returning a MultiRelayWriter plus a close function.
//
// It is an error only when NO relay URL is configured.  A relay that is down at
// startup is NOT an error any more: the writer keeps retrying on its backoff and
// publishing begins when the relay returns.  The old "no relay could be reached"
// error disabled the publisher for the life of the process, which turned a
// restart-order race into permanent silence.
func DialRelays(ctx context.Context, urls []string) (*MultiRelayWriter, func(), error) {
	if len(urls) == 0 {
		return nil, func() {}, errors.New("no relay configured")
	}
	w := &MultiRelayWriter{}
	closers := make([]func(), 0, len(urls))
	for _, url := range urls {
		rw := NewReconnectingWriter(url)
		_, _ = rw.clientFor(ctx, true) // failure is recorded and logged by the writer
		w.Writers = append(w.Writers, rw)
		closers = append(closers, func() { _ = rw.Close() })
	}
	return w, func() {
		for _, closeOne := range closers {
			closeOne()
		}
	}, nil
}
