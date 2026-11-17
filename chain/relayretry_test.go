package main

// Socket-free tests for the restart-proof ingest (handover §2, workstream B):
// the transient-retry queue, the terminal-rejection drop, and the durable
// cursor.  They use a MemorySubmitter and a fake cursor, so they need no relay
// and no chain and always run.

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// fakeCursor is an in-memory CursorRecorder that records what the observer asked
// it to remember, so a test can assert both the value and the timing.
type fakeCursor struct {
	mu      sync.Mutex
	entries map[string]cursorEntry
	calls   []cursorEntry
}

func newFakeCursor() *fakeCursor {
	return &fakeCursor{entries: map[string]cursorEntry{}}
}

func (cursor *fakeCursor) Advance(url string, createdAt int64, eventID string) error {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	entry := cursorEntry{CreatedAt: createdAt, EventID: eventID}
	cursor.entries[url] = entry
	cursor.calls = append(cursor.calls, entry)
	return nil
}

func (cursor *fakeCursor) since(url string) (time.Time, string) {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	entry, ok := cursor.entries[url]
	if !ok || entry.CreatedAt == 0 {
		return time.Time{}, ""
	}
	return time.Unix(entry.CreatedAt, 0).UTC(), entry.EventID
}

// last is the position of the newest Advance call, or the zero value if there
// was none.
func (cursor *fakeCursor) last() cursorEntry {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if len(cursor.calls) == 0 {
		return cursorEntry{}
	}
	return cursor.calls[len(cursor.calls)-1]
}

// advanced returns the event ids Advance was called with, oldest first.
func (cursor *fakeCursor) advanced() []string {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	ids := make([]string, 0, len(cursor.calls))
	for _, call := range cursor.calls {
		ids = append(ids, call.EventID)
	}
	return ids
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestObserverRetriesTransientSubmitFailure: a transient refusal must not lose
// the event.  The first submit fails, the worker retries with backoff, and once
// the node is ready the event commits -- and only then does the cursor advance,
// so a restart in between would still re-read it.
func TestObserverRetriesTransientSubmitFailure(t *testing.T) {
	secret := nostr.GeneratePrivateKey()
	event := signedCreate(t, secret, "Retry transient", 1_800_000_100)

	submitter := NewMemorySubmitter()
	transient := &SubmitFailure{Transient: true, Message: "broadcast rejected: endpoint is closed while node is catching up"}
	submitter.FailAlways(event.ID, transient)

	cursor := newFakeCursor()
	observer, err := NewObserver(ObserverConfig{URL: "ws://retry.example", Submitter: submitter, Cursor: cursor, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer.RunRetriesForTest(ctx)

	observer.HandleForTest(ctx, &event)

	// The initial attempt failed and the worker must retry (twice at least).
	waitFor(t, 5*time.Second, func() bool { return submitter.Attempts(event.ID) >= 2 },
		"the transient submit failure to be retried")
	if since, _ := cursor.since("ws://retry.example"); !since.IsZero() {
		t.Fatalf("cursor advanced to %s while the submit was still failing", since)
	}
	if committed, _, _ := observer.Stats(); committed != 0 {
		t.Fatalf("committed = %d before the node was ready, want 0", committed)
	}

	// The node becomes ready; the retry must commit without a reconnect.
	submitter.ClearFailures(event.ID)
	waitFor(t, 5*time.Second, func() bool { return len(submitter.Submissions()) == 1 },
		"the retried event to commit")
	waitFor(t, 5*time.Second, func() bool {
		since, id := cursor.since("ws://retry.example")
		return since.Unix() == int64(event.CreatedAt) && id == event.ID
	}, "the cursor to advance only after the success")

	if advanced := cursor.advanced(); len(advanced) != 1 || advanced[0] != event.ID {
		t.Fatalf("cursor advances = %v, want exactly [%s]", advanced, event.ID)
	}
}

// TestObserverDropsTerminalRejection: a terminal CheckTx rejection is an answer.
// The event must be dropped (never retried), the failure recorded, and the
// cursor advanced past it so a restart does not re-read it forever.
func TestObserverDropsTerminalRejection(t *testing.T) {
	secret := nostr.GeneratePrivateKey()
	event := signedCreate(t, secret, "Terminal drop", 1_800_000_200)

	submitter := NewMemorySubmitter()
	submitter.FailAlways(event.ID, &SubmitFailure{Message: "CheckTx rejected: unsupported kind 9999"})

	cursor := newFakeCursor()
	observer, err := NewObserver(ObserverConfig{URL: "ws://drop.example", Submitter: submitter, Cursor: cursor, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer.RunRetriesForTest(ctx)

	observer.HandleForTest(ctx, &event)

	if dropped := observer.Dropped(); dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if _, _, lastError := observer.Stats(); lastError == nil {
		t.Fatal("terminal rejection was not recorded as the last error")
	}
	since, id := cursor.since("ws://drop.example")
	if since.Unix() != int64(event.CreatedAt) || id != event.ID {
		t.Fatalf("cursor = (%s, %s), want (%s, %s): a terminal drop must advance past the event",
			since, id, time.Unix(int64(event.CreatedAt), 0).UTC(), event.ID)
	}

	// A terminal rejection must never be retried.
	time.Sleep(700 * time.Millisecond)
	if attempts := submitter.Attempts(event.ID); attempts != 1 {
		t.Fatalf("terminal rejection was submitted %d times, want exactly 1", attempts)
	}
	if committed, _, _ := observer.Stats(); committed != 0 {
		t.Fatalf("committed = %d, want 0 for a rejected event", committed)
	}
}

// TestCursorHoldsBehindAnUnsettledEvent: the cursor means "everything at or
// before this position is settled".  A newer event committing while an older one
// is still queued must NOT move it, or a crash would resume past the older event
// and lose it.
func TestCursorHoldsBehindAnUnsettledEvent(t *testing.T) {
	const url = "ws://hold.example"

	olderEvent := signedCreate(t, nostr.GeneratePrivateKey(), "Cursor hold older", 1_800_000_090)
	newerEvent := signedCreate(t, nostr.GeneratePrivateKey(), "Cursor hold newer", 1_800_000_101)
	older, newer := toChainEvent(&olderEvent), toChainEvent(&newerEvent)

	submitter := NewMemorySubmitter()
	submitter.FailAlways(older.ID, &SubmitFailure{Transient: true, Message: "broadcast rejected: endpoint is closed while node is catching up"})

	cursor := newFakeCursor()
	// The cursor had reached the older event's position; it must not move on to
	// the newer one while the older one is still unsettled.
	if err := cursor.Advance(url, older.CreatedAt, older.ID); err != nil {
		t.Fatal(err)
	}

	observer, err := NewObserver(ObserverConfig{URL: url, Submitter: submitter, Cursor: cursor, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer.RunRetriesForTest(ctx)

	observer.HandleForTest(ctx, &olderEvent) // transient: queued for retry
	observer.HandleForTest(ctx, &newerEvent) // commits

	if committed, _, _ := observer.Stats(); committed != 1 {
		t.Fatalf("committed = %d, want 1 (only the newer event)", committed)
	}
	// The newer event is settled, but the older one is not, so the persisted
	// position must still be the OLDER event's -- never the newer one.
	last := cursor.last()
	if last.CreatedAt != older.CreatedAt || last.EventID != older.ID {
		t.Fatalf("cursor = (%d, %s) after the newer event committed; want (%d, %s) while %s is still unsettled",
			last.CreatedAt, last.EventID, older.CreatedAt, older.ID, older.ID)
	}
	if last.CreatedAt >= newer.CreatedAt {
		t.Fatalf("cursor = %d skipped past the unsettled event at %d", last.CreatedAt, newer.CreatedAt)
	}

	// Once the older event is accepted too, the position catches up to the newer
	// one in a single flush.
	submitter.ClearFailures(older.ID)
	waitFor(t, 5*time.Second, func() bool { return len(submitter.Submissions()) == 2 },
		"the older event to commit after the node recovered")
	waitFor(t, 5*time.Second, func() bool {
		last := cursor.last()
		return last.CreatedAt == newer.CreatedAt && last.EventID == newer.ID
	}, "the cursor to catch up to the newer event")
}

// TestRelayCursorPersistsAndResumes: the cursor is a JSON object keyed by relay
// URL, so a restart reads it back and resumes exactly there.
func TestRelayCursorPersistsAndResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay-cursor.json")
	const (
		url       = "wss://relay.example"
		createdAt = int64(1_800_000_500)
		eventID   = "abc123def456"
	)

	cursor, err := loadRelayCursor(path)
	if err != nil {
		t.Fatalf("load empty cursor: %v", err)
	}
	if since, id := cursor.since(url); !since.IsZero() || id != "" {
		t.Fatalf("empty cursor reports (%s, %s), want the zero value", since, id)
	}
	if err := cursor.Advance(url, createdAt, eventID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	// A fresh load, as a restart does, must see the same value.
	reloaded, err := loadRelayCursor(path)
	if err != nil {
		t.Fatalf("reload cursor: %v", err)
	}
	since, id := reloaded.since(url)
	if since.Unix() != createdAt || id != eventID {
		t.Fatalf("reloaded cursor = (%s, %s), want (%s, %s)", since, id, time.Unix(createdAt, 0).UTC(), eventID)
	}

	// A new observer seeded from the cursor starts at that point: the Stats
	// surface shows it, and Run names it in the startup log.
	var logBuf syncBuffer
	observer, err := NewObserver(ObserverConfig{
		URL: url, Submitter: NewMemorySubmitter(), Cursor: reloaded,
		Since: &since, ResumeEventID: id, ReconnectDelay: 50 * time.Millisecond,
		Logger: log.New(&logBuf, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, lastSeen, _ := observer.Stats(); lastSeen.Unix() != createdAt {
		t.Fatalf("observer lastSeen = %s, want %s (seeded from the cursor)", lastSeen, time.Unix(createdAt, 0).UTC())
	}

	// An unparseable URL fails the connection without a socket; Run still logs
	// the resume point before it dials.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go observer.Run(ctx)
	want := "relay observer: resuming from cursor " + time.Unix(createdAt, 0).UTC().Format(time.RFC3339) + " (" + eventID + ")"
	waitFor(t, 3*time.Second, func() bool { return strings.Contains(logBuf.String(), want) },
		"the resume log line naming the cursor")
}

// TestRelayCursorNeverMovesBackwards: an out-of-order or replayed event must
// never rewind the resume point.
func TestRelayCursorNeverMovesBackwards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay-cursor.json")
	const url = "wss://relay.example"

	cursor, err := loadRelayCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cursor.Advance(url, 200, "newer"); err != nil {
		t.Fatal(err)
	}
	if err := cursor.Advance(url, 100, "older"); err != nil {
		t.Fatal(err)
	}
	if since, id := cursor.since(url); since.Unix() != 200 || id != "newer" {
		t.Fatalf("cursor = (%s, %s), want (200, newer)", since, id)
	}

	// An event in the same second as the newest one does move the id forward,
	// which is what keeps a same-second delivery from being skipped.
	if err := cursor.Advance(url, 200, "same-second"); err != nil {
		t.Fatal(err)
	}
	if since, id := cursor.since(url); since.Unix() != 200 || id != "same-second" {
		t.Fatalf("cursor = (%s, %s), want (200, same-second)", since, id)
	}

	reloaded, err := loadRelayCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	if since, id := reloaded.since(url); since.Unix() != 200 || id != "same-second" {
		t.Fatalf("persisted cursor = (%s, %s), want (200, same-second)", since, id)
	}
}

// TestRPCSubmitterFanOut: the fan-out succeeds when at least one endpoint takes
// the tx, and classifies a total failure by whether any refusal was transient.
func TestRPCSubmitterFanOut(t *testing.T) {
	secret := nostr.GeneratePrivateKey()
	event := signedCreate(t, secret, "Fan out", 1_800_000_300)
	wire := toChainEvent(&event)

	accept, _ := fakeRPCServer(t, `{"result":{"code":0,"log":"","hash":"CAFE"}}`)
	transient, _ := fakeRPCServer(t, `{"error":{"message":"endpoint is closed while node is catching up"}}`)
	terminal, _ := fakeRPCServer(t, `{"result":{"code":1,"log":"invalid signature"}}`)
	cache, _ := fakeRPCServer(t, `{"error":{"message":"tx already exists in cache"}}`)

	// One acceptor among failures is still success.
	result, err := RPCSubmitter{URLs: []string{transient.URL, accept.URL, terminal.URL}}.SubmitEvent(context.Background(), wire)
	if err != nil {
		t.Fatalf("fan-out with one acceptor: %v", err)
	}
	if result.TxHash != "CAFE" {
		t.Fatalf("hash = %q, want CAFE", result.TxHash)
	}

	// Already-in-cache counts as accepted.
	if _, err := (RPCSubmitter{URLs: []string{cache.URL}}).SubmitEvent(context.Background(), wire); err != nil {
		t.Fatalf("already-in-cache must be success, got %v", err)
	}

	// Every refusal transient -> transient.
	_, err = RPCSubmitter{URLs: []string{transient.URL}}.SubmitEvent(context.Background(), wire)
	if err == nil || !IsTransientSubmit(err) {
		t.Fatalf("all-transient fan-out error = %v, want transient", err)
	}

	// A transient refusal beside a terminal one is still retryable.
	_, err = RPCSubmitter{URLs: []string{terminal.URL, transient.URL}}.SubmitEvent(context.Background(), wire)
	if err == nil || !IsTransientSubmit(err) {
		t.Fatalf("mixed fan-out error = %v, want transient", err)
	}

	// Only terminal refusals -> terminal.
	_, err = RPCSubmitter{URLs: []string{terminal.URL}}.SubmitEvent(context.Background(), wire)
	if err == nil || IsTransientSubmit(err) {
		t.Fatalf("all-terminal fan-out error = %v, want terminal", err)
	}
	if !strings.Contains(err.Error(), "CheckTx rejected:") {
		t.Fatalf("terminal error %q lost the CheckTx text", err)
	}
}
