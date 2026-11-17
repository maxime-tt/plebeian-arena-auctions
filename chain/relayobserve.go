package main

// Relay ingest: instead of (only) accepting a signed Nostr event over HTTP at
// the gateway, every validator subscribes to the configured relay(s) and
// submits the events it sees there into its own node's mempool.  The relay is
// transport and discovery; the chain stays the durable record.
//
// This is a port of the observer half of the sibling `internal/relay` package
// onto this module.  It keeps the behaviour that mattered there:
//
//   - the event's Schnorr signature is verified BEFORE anything else, so a
//     hostile or malformed event is dropped rather than crashing the observer;
//   - events are de-duplicated by id, so a republish (or a replay) commits once;
//   - a reconnect re-subscribes from the last created_at actually seen, so a
//     dropped connection loses nothing the relay still holds;
//   - a transient submit refusal is retried with backoff in its own goroutine,
//     so the receive loop keeps draining the socket and the event is not lost
//     when the node is still catching up;
//   - a durable per-relay cursor (relaycursor.go) lets a restart resume from the
//     last event processed instead of re-reading the whole relay;
//   - the last transport error and the live-subscription flag are exposed for
//     readiness probes and tests.
//
// The relay itself is not implemented here.  It is an external process (for
// tests, `nak serve`; in production, any NIP-01 relay) and it never timestamps,
// never orders, and never decides validity beyond a signature check.  Every
// time rule keeps running on committed block time.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// RelayKinds are the only kinds the ingest transports: auction create and bid.
var RelayKinds = []int{KindCreate, KindBid}

// IsRelayKind reports whether kind is one this ingest transports.
func IsRelayKind(kind int) bool {
	for _, k := range RelayKinds {
		if kind == k {
			return true
		}
	}
	return false
}

// ObserverConfig configures one observer.
type ObserverConfig struct {
	// URL is the relay websocket, e.g. ws://127.0.0.1:10547.
	URL string
	// Submitter delivers each observed event to the chain.
	Submitter Submitter
	// Kinds overrides the subscribed kinds; defaults to RelayKinds.
	Kinds []int
	// Since is the earliest created_at to request on the first subscribe.  It
	// advances as events are seen so a reconnect only re-fetches the window it
	// might have missed.  Zero means "from the beginning of the relay".
	Since *time.Time
	// ResumeEventID is the id that goes with Since in the startup log, so an
	// operator can see exactly which event a restart resumed after.  It is
	// empty when Since is unknown.
	ResumeEventID string
	// Cursor records how far this relay has been processed, durably.  When set,
	// it is advanced on a success or a terminal rejection, and never on a
	// transient failure.  Nil disables cursor persistence.
	Cursor CursorRecorder
	// ReconnectDelay is how long to wait before retrying a dropped connection.
	ReconnectDelay time.Duration
	// RetryBackoff is the first delay before retrying a refused submit; it
	// doubles up to retryBackoffMax.  Zero means retryBackoff.  Tests set it
	// small so a path that needs many retries does not cost wall-clock seconds.
	RetryBackoff time.Duration
	// OnSubmit, when set, is called after a successful submission.  It must not
	// block.
	OnSubmit func(event Event, result SubmitResult)
	// Logger receives progress; nil means log.Default.
	Logger *log.Logger
}

// Observer reads auction events from a relay and submits them to the chain.  One
// observer per validator is the intended deployment.  The chain's own state
// guard (a create for an existing auction, a bid already recorded) makes a
// replay a no-op, so running more than one observer is safe too.
type Observer struct {
	config ObserverConfig

	mu         sync.Mutex
	lastSeen   time.Time
	seenIDs    map[string]struct{}
	submitted  int
	dropped    int
	lastError  error
	subscribed bool

	// queue holds events whose submit failed transiently, in arrival order; the
	// retry worker drains it with exponential backoff.  workerOnce keeps exactly
	// one worker per observer, however many times Run is entered.
	queue        *retryQueue
	workerOnce   sync.Once
	workerActive bool

	// pending is how many events are queued or in flight (unsettled); while it
	// is non-zero the durable cursor must not move, or a restart would resume
	// past an event that never reached the chain.  settled is the highest
	// position seen settled while pending was non-zero, flushed once it drains.
	pending    int
	settled    cursorEntry
	settledSet bool
}

// NewObserver validates configuration and returns an observer ready to Run.
func NewObserver(config ObserverConfig) (*Observer, error) {
	if config.URL == "" {
		return nil, errors.New("observer requires a relay URL")
	}
	if config.Submitter == nil {
		return nil, errors.New("observer requires a submitter")
	}
	if len(config.Kinds) == 0 {
		config.Kinds = append([]int(nil), RelayKinds...)
	}
	if config.ReconnectDelay <= 0 {
		config.ReconnectDelay = time.Second
	}
	if config.RetryBackoff <= 0 {
		config.RetryBackoff = retryBackoff
	}
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	observer := &Observer{config: config, seenIDs: map[string]struct{}{}, queue: newRetryQueue()}
	if config.Since != nil {
		observer.lastSeen = *config.Since
	}
	return observer, nil
}

// retryBackoff is the first retry delay; it doubles up to retryBackoffMax.  A
// refused submit is usually "the node is not ready yet", which clears in well
// under a second, so the first attempt comes back quickly and only a node that
// stays down waits the cap.
const (
	retryBackoff    = 250 * time.Millisecond
	retryBackoffMax = 5 * time.Second
)

// maxAppRetries bounds how many times an application rejection is submitted
// before the event is dropped, counting the receive loop's first attempt.  The
// refusal is deterministic for the committed state the node had when it
// answered, and that state moves: a bid or supersession published before its
// create's block commits is rejected with "unknown auction" or "update must
// reference the auction root" and succeeds one block later.  The default
// backoff sequence (250 ms, 500 ms, 1 s, 2 s) covers ~3.75 s, comfortably more
// than one block, so the race resolves; an event the chain keeps rejecting is
// dropped after that instead of being retried forever.
const maxAppRetries = 5

// retryItem is one queued event and how many submit attempts it has cost.
type retryItem struct {
	event    Event
	attempts int
}

// retryQueue is an unbounded, arrival-ordered FIFO of events awaiting retry,
// de-duplicated by id so a relay replay cannot queue a second copy.  next blocks
// until an item is queued or ctx is done; it is not a channel so the receive
// loop never blocks on a full buffer.
type retryQueue struct {
	mu    sync.Mutex
	items []retryItem
	ids   map[string]struct{}
	wake  chan struct{}
}

func newRetryQueue() *retryQueue {
	return &retryQueue{ids: map[string]struct{}{}, wake: make(chan struct{}, 1)}
}

// push appends the event unless its id is already queued, then wakes the worker.
// The item starts at one attempt because the receive loop has already made the
// first one.
func (queue *retryQueue) push(event Event) {
	queue.mu.Lock()
	if _, queued := queue.ids[event.ID]; queued {
		queue.mu.Unlock()
		return
	}
	queue.ids[event.ID] = struct{}{}
	queue.items = append(queue.items, retryItem{event: event, attempts: 1})
	queue.mu.Unlock()
	queue.signal()
}

// pushFront puts an event a worker took but could not finish back at the head,
// preserving arrival order and its attempt count across a cancelled worker.
func (queue *retryQueue) pushFront(item retryItem) {
	queue.mu.Lock()
	if _, queued := queue.ids[item.event.ID]; !queued {
		queue.ids[item.event.ID] = struct{}{}
		queue.items = append([]retryItem{item}, queue.items...)
	}
	queue.mu.Unlock()
	queue.signal()
}

// next blocks until an event is queued or ctx is done.
func (queue *retryQueue) next(ctx context.Context) (retryItem, bool) {
	for {
		queue.mu.Lock()
		if len(queue.items) > 0 {
			item := queue.items[0]
			queue.items = queue.items[1:]
			queue.mu.Unlock()
			return item, true
		}
		queue.mu.Unlock()
		select {
		case <-ctx.Done():
			return retryItem{}, false
		case <-queue.wake:
		}
	}
}

// done forgets the queued id once the event is finished (committed or dropped).
func (queue *retryQueue) done(id string) {
	queue.mu.Lock()
	delete(queue.ids, id)
	queue.mu.Unlock()
}

// drain returns and clears everything still queued.
func (queue *retryQueue) drain() []Event {
	queue.mu.Lock()
	items := queue.items
	queue.items = nil
	queue.ids = map[string]struct{}{}
	queue.mu.Unlock()
	events := make([]Event, 0, len(items))
	for _, item := range items {
		events = append(events, item.event)
	}
	return events
}

func (queue *retryQueue) signal() {
	select {
	case queue.wake <- struct{}{}:
	default:
	}
}

// startWorker starts the retry worker exactly once.  Run calls it for the whole
// Run; RunRetriesForTest calls it so a test can retry without a socket.
func (observer *Observer) startWorker(ctx context.Context) {
	observer.workerOnce.Do(func() {
		observer.mu.Lock()
		observer.workerActive = true
		observer.mu.Unlock()
		go observer.retryWorker(ctx)
	})
}

// RunRetriesForTest starts the transient-retry worker without opening a relay
// socket, so a test can pair it with HandleForTest and exercise retry in
// isolation.  Production code reaches the same worker through Run.
func (observer *Observer) RunRetriesForTest(ctx context.Context) {
	observer.startWorker(ctx)
}

// retryWorker drains the retry queue, retrying each event with exponential
// backoff until it commits, is terminally rejected, or ctx is done.  It runs in
// its own goroutine so the receive loop keeps draining the socket while a retry
// is pending.  Anything still queued when it exits is handed back to the next
// session's replay: its id is forgotten so the re-subscription is not skipped.
func (observer *Observer) retryWorker(ctx context.Context) {
	defer func() {
		observer.forget(observer.queue.drain())
		observer.mu.Lock()
		observer.workerActive = false
		// Nothing is queued or in flight any more, and the drained events were
		// forgotten so the next session's replay re-reads them: the cursor
		// stays behind them rather than being flushed past them.
		observer.pending = 0
		observer.settledSet = false
		observer.mu.Unlock()
	}()
	for {
		item, ok := observer.queue.next(ctx)
		if !ok {
			return
		}
		backoff := observer.config.RetryBackoff
		for {
			// The initial attempt already happened in handle; every retry here
			// waits first, starting at RetryBackoff and doubling to the 5 s cap.
			select {
			case <-ctx.Done():
				observer.queue.pushFront(item)
				return
			case <-time.After(backoff):
			}
			item.attempts++
			result, err := observer.config.Submitter.SubmitEvent(ctx, item.event)
			if err == nil {
				observer.queue.done(item.event.ID)
				observer.committed(item.event, result, true)
				break
			}
			appRejection := IsAppRejection(err)
			if !IsTransientSubmit(err) && !appRejection {
				observer.queue.done(item.event.ID)
				observer.droppedEvent(item.event, err, true)
				break
			}
			if appRejection && item.attempts >= maxAppRetries {
				// The application has answered this many times; the state that
				// would change its mind has not moved, so stop.
				observer.queue.done(item.event.ID)
				observer.config.Logger.Printf("relay observer: dropping %s after %d attempts: %v", item.event.ID, item.attempts, err)
				observer.droppedEvent(item.event, err, true)
				break
			}
			observer.recordError(fmt.Errorf("submit %s: %w", item.event.ID, err))
			if backoff *= 2; backoff > retryBackoffMax {
				backoff = retryBackoffMax
			}
			observer.config.Logger.Printf("relay observer: retrying %s in %s: %v", item.event.ID, backoff, err)
		}
	}
}

// forget removes ids from the in-memory dedupe set, so a later session's replay
// re-handles an event the retry worker could not finish.
func (observer *Observer) forget(events []Event) {
	if len(events) == 0 {
		return
	}
	observer.mu.Lock()
	for _, event := range events {
		delete(observer.seenIDs, event.ID)
	}
	observer.mu.Unlock()
}

// Run observes until ctx is cancelled, reconnecting on transport failure.
func (observer *Observer) Run(ctx context.Context) error {
	observer.startWorker(ctx)
	if since, id := observer.resumePoint(); !since.IsZero() {
		observer.config.Logger.Printf("relay observer: resuming from cursor %s (%s)", since.UTC().Format(time.RFC3339), id)
	}
	for {
		if err := observer.session(ctx); err != nil && !errors.Is(err, context.Canceled) {
			observer.recordError(err)
			observer.config.Logger.Printf("relay observer: session ended: %v (retrying in %s)", err, observer.config.ReconnectDelay)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(observer.config.ReconnectDelay):
		}
	}
}

// resumePoint is the cursor value the observer starts from, if any: the
// configured Since (seeded from the durable cursor) and the event id that goes
// with it.
func (observer *Observer) resumePoint() (time.Time, string) {
	if observer.config.Since == nil {
		return time.Time{}, ""
	}
	return *observer.config.Since, observer.config.ResumeEventID
}

// session runs one connection.  Every fresh subscription asks for events since
// the last one it actually processed, so anything published while disconnected
// is delivered by the replay rather than lost.
func (observer *Observer) session(ctx context.Context) error {
	observer.mu.Lock()
	since := observer.lastSeen
	kinds := append([]int(nil), observer.config.Kinds...)
	observer.mu.Unlock()

	filters := make([]nostr.Filter, 0, len(kinds))
	for _, kind := range kinds {
		filter := nostr.Filter{Kinds: []int{kind}}
		if !since.IsZero() {
			// One second of slack so an event created in the same second as
			// the last one seen is not skipped by the boundary.
			stamp := nostr.Timestamp(since.Unix() - 1)
			filter.Since = &stamp
		}
		filters = append(filters, filter)
	}

	observer.config.Logger.Printf("relay observer: subscribing to %s since=%s kinds=%v", observer.config.URL, describeSince(since), kinds)
	subscription, err := connectAndSubscribe(ctx, observer.config.URL, filters)
	if err != nil {
		return err
	}
	observer.setSubscribed(true)
	defer func() {
		observer.setSubscribed(false)
		subscription.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-subscription.Events:
			if !ok {
				return subscription.CloseReason()
			}
			if event == nil {
				continue
			}
			observer.handle(ctx, event)
		}
	}
}

// handle verifies and submits one event.  A verification failure is a drop, not
// a crash: a hostile or malformed event must not take a validator's observer
// down, and the chain would reject it anyway.
func (observer *Observer) handle(ctx context.Context, event *nostr.Event) {
	signed := toChainEvent(event)
	// The same gate decodeTx applies: canonical serializer + Schnorr verify.
	// Verifying here means a relay can forward garbage cheaply without ever
	// reaching this node's mempool.
	if err := signed.Verify(); err != nil {
		observer.config.Logger.Printf("relay observer: dropping unverifiable event %s: %v", event.ID, err)
		return
	}
	if !IsRelayKind(signed.Kind) {
		observer.config.Logger.Printf("relay observer: dropping unsupported kind %d (%s)", signed.Kind, event.ID)
		return
	}
	observer.mu.Lock()
	if _, duplicate := observer.seenIDs[event.ID]; duplicate {
		observer.mu.Unlock()
		return
	}
	observer.seenIDs[event.ID] = struct{}{}
	observer.mu.Unlock()

	result, err := observer.config.Submitter.SubmitEvent(ctx, signed)
	if err != nil {
		observer.recordError(fmt.Errorf("submit %s: %w", event.ID, err))
		// An application rejection is queued too: the committed state it was
		// decided against may move (a bid published before its create's block),
		// so the worker retries it a bounded number of times.
		if IsTransientSubmit(err) || IsAppRejection(err) {
			if observer.workerRunning() {
				// Keep the id in seenIDs: the event is queued, and a relay
				// replay of it must not queue a second copy.  The retry worker
				// owns it from here.
				observer.config.Logger.Printf("relay observer: submit failed for %s (retrying): %v", event.ID, err)
				// Mark it unsettled BEFORE queueing: a later event settling out
				// of order must not advance the cursor past this one.
				observer.markPending()
				observer.queue.push(signed)
				return
			}
			// No worker (a bare HandleForTest): keep the pre-retry behaviour of
			// forgetting the id, so a reconnect's replay tries again.
			observer.forget([]Event{signed})
			observer.config.Logger.Printf("relay observer: submit failed for %s: %v", event.ID, err)
			return
		}
		// A terminal rejection is an answer: drop the event, record it, and let
		// the cursor move past it so it is never re-read.
		observer.droppedEvent(signed, err, false)
		return
	}
	observer.committed(signed, result, false)
}

// workerRunning reports whether the retry worker is live; without it a
// transient failure has to fall back to the reconnect replay.
func (observer *Observer) workerRunning() bool {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.workerActive
}

// committed records a successful submission: it advances lastSeen, counts the
// event, notifies OnSubmit, and settles the cursor.  fromQueue says whether the
// event came out of the retry queue (and so releases one pending slot).
func (observer *Observer) committed(event Event, result SubmitResult, fromQueue bool) {
	observer.mu.Lock()
	if created := time.Unix(event.CreatedAt, 0); created.After(observer.lastSeen) {
		observer.lastSeen = created
	}
	observer.submitted++
	observer.mu.Unlock()
	if observer.config.OnSubmit != nil {
		observer.config.OnSubmit(event, result)
	}
	observer.settle(event, fromQueue)
	observer.config.Logger.Printf("relay observer: committed %s kind=%d height=%d", event.ID, event.Kind, result.Height)
}

// droppedEvent records a terminal rejection: the chain will never accept this
// event, so it is dropped (not retried) and settled past.
func (observer *Observer) droppedEvent(event Event, err error, fromQueue bool) {
	observer.mu.Lock()
	if created := time.Unix(event.CreatedAt, 0); created.After(observer.lastSeen) {
		observer.lastSeen = created
	}
	observer.dropped++
	observer.mu.Unlock()
	observer.settle(event, fromQueue)
	observer.config.Logger.Printf("relay observer: dropping %s: terminal rejection: %v", event.ID, err)
}

// settle records that an event is settled (committed or terminally dropped) and
// advances the durable cursor only once nothing is still queued or in flight.
//
// The cursor means "everything at or before this position is settled".  A crash
// must never resume past an event the observer had not finished with, so a later
// event settling out of order waits here until the older one drains, and the
// highest settled position is then flushed in one go.  A transient failure never
// reaches this function, so it is never persisted as settled.
//
// fromQueue says whether the event came out of the retry queue, which is what
// releases one pending slot; an event submitted straight from the receive loop
// was never pending.
func (observer *Observer) settle(event Event, fromQueue bool) {
	if observer.config.Cursor == nil {
		return
	}
	position := cursorEntry{CreatedAt: event.CreatedAt, EventID: event.ID}
	observer.mu.Lock()
	if fromQueue && observer.pending > 0 {
		observer.pending--
	}
	if !observer.settledSet || cursorAfter(position, observer.settled) {
		observer.settled, observer.settledSet = position, true
	}
	if observer.pending > 0 {
		// Something older is still unsettled: hold the position back rather
		// than skip over it.
		observer.mu.Unlock()
		return
	}
	position, observer.settledSet = observer.settled, false
	observer.mu.Unlock()
	if err := observer.config.Cursor.Advance(observer.config.URL, position.CreatedAt, position.EventID); err != nil {
		observer.recordError(fmt.Errorf("cursor advance %s: %w", position.EventID, err))
		observer.config.Logger.Printf("relay observer: cursor advance failed for %s: %v", position.EventID, err)
	}
}

// markPending registers an event handed to the retry queue as unsettled, so no
// other event's success can advance the cursor past it.
func (observer *Observer) markPending() {
	if observer.config.Cursor == nil {
		return
	}
	observer.mu.Lock()
	observer.pending++
	observer.mu.Unlock()
}

// cursorAfter reports whether a is a later position than b.
func cursorAfter(a, b cursorEntry) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.EventID > b.EventID
}

// Stats reports how many events this observer committed, the newest created_at
// it has seen, and the last transport error; readiness probes and tests use them.
func (observer *Observer) Stats() (submitted int, lastSeen time.Time, lastError error) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.submitted, observer.lastSeen, observer.lastError
}

// Dropped reports how many events were terminally rejected and dropped.
func (observer *Observer) Dropped() int {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.dropped
}

// HandleForTest runs one event through the same verification and submission path
// a live subscription uses, without a socket.  It exists so a delivery whose
// bytes a relay might refuse can still be tested at the verification boundary;
// production code reaches this path only through session().
func (observer *Observer) HandleForTest(ctx context.Context, event *nostr.Event) {
	observer.handle(ctx, event)
}

// Subscribed reports whether a relay subscription is currently open.  Tests and
// readiness probes use it to avoid publishing before the observer is listening
// (an ephemeral relay may drop an EVENT that has no matching subscriber).
func (observer *Observer) Subscribed() bool {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.subscribed
}

func (observer *Observer) setSubscribed(value bool) {
	observer.mu.Lock()
	observer.subscribed = value
	observer.mu.Unlock()
}

func (observer *Observer) recordError(err error) {
	observer.mu.Lock()
	observer.lastError = err
	observer.mu.Unlock()
}

func describeSince(since time.Time) string {
	if since.IsZero() {
		return "beginning"
	}
	return since.UTC().Format(time.RFC3339)
}

// toChainEvent converts a go-nostr event into this module's wire form.  Tag
// order and values are preserved exactly: the event id is a hash over them, so
// any reordering or loss would invalidate a legitimate event.
func toChainEvent(event *nostr.Event) Event {
	converted := Event{
		ID:        event.ID,
		Pubkey:    event.PubKey,
		CreatedAt: int64(event.CreatedAt),
		Kind:      event.Kind,
		Content:   event.Content,
		Sig:       event.Sig,
	}
	converted.Tags = make([][]string, len(event.Tags))
	for index, tag := range event.Tags {
		converted.Tags[index] = append([]string(nil), tag...)
	}
	return converted
}

// SortedSubmissions returns the events a MemorySubmitter recorded in chain
// order; a small helper for tests and callers that want a stable ordering.
func SortedSubmissions(submitter *MemorySubmitter) []Event {
	events := submitter.Submissions()
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].CreatedAt != events[j].CreatedAt {
			return events[i].CreatedAt < events[j].CreatedAt
		}
		return events[i].ID < events[j].ID
	})
	return events
}
