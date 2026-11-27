package main

// Hermetic tests for the subscription-liveness fix (2026-11-27 incident).
//
// The observer's receive loop used to block on subscription.Events forever when
// the socket went half-open, because go-nostr reports no error for that: no
// reconnect, no log, a kind-1440 create silently never submitted.  These tests
// pin the three behaviours the fix must have:
//
//	1. a probe kind (1447) is received, counted as a liveness frame, and NOT
//	   submitted, de-duplicated, or allowed to advance the cursor;
//	2. a session that receives nothing trips the idle deadline, Run resubscribes
//	   from the advanced cursor, and a create published while the socket was
//	   dead is delivered by that replay;
//	3. a non-positive IdleTimeout disables the deadline.
//
// They use a scripted connect seam, a MemorySubmitter and a fake cursor, so they
// need no relay socket, no chain and no clock sleeps beyond a few hundred ms.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// fakeSubscription returns a live-looking subscription whose Events channel the
// test drives by hand.  inner stays nil, so Close() only flips the closed flag
// and no socket is involved.
func fakeSubscription() *relaySubscription {
	return &relaySubscription{Events: make(chan *nostr.Event, 8)}
}

// A probe kind arriving is a liveness frame: it updates the last-frame time but
// is never submitted, never enters the id de-dupe set, and never moves the
// cursor.  It must also not be mistaken for an unsupported kind.
func TestObserverProbeKindIsLivenessOnly(t *testing.T) {
	submitter := NewMemorySubmitter()
	cursor := newFakeCursor()
	observer, err := NewObserver(ObserverConfig{
		URL:         "ws://probe.example",
		Submitter:   submitter,
		Cursor:      cursor,
		Logger:      quietLogger(),
		IdleTimeout: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observer.config.ProbeKinds) != 1 || observer.config.ProbeKinds[0] != KindStateCommit {
		t.Fatalf("default probe kinds = %v, want [%d]", observer.config.ProbeKinds, KindStateCommit)
	}

	if _, _, lastFrame, _ := observer.Stats(); !lastFrame.IsZero() {
		t.Fatalf("lastFrame = %s before any frame, want the zero time", lastFrame)
	}

	probe := &nostr.Event{
		ID:        strings.Repeat("a", 64),
		PubKey:    strings.Repeat("b", 64),
		CreatedAt: 1_800_000_000,
		Kind:      KindStateCommit,
		Content:   "state",
	}
	observer.HandleForTest(context.Background(), probe)

	if n := len(submitter.Submissions()); n != 0 {
		t.Fatalf("probe kind submitted %d events, want 0", n)
	}
	if attempts := submitter.Attempts(probe.ID); attempts != 0 {
		t.Fatalf("probe kind reached the submitter %d times, want 0", attempts)
	}
	if _, _, lastFrame, _ := observer.Stats(); lastFrame.IsZero() {
		t.Fatal("probe kind did not update the last-frame time")
	}
	observer.mu.Lock()
	_, seen := observer.seenIDs[probe.ID]
	observer.mu.Unlock()
	if seen {
		t.Fatal("probe kind entered the id de-dupe set")
	}
	if advanced := cursor.advanced(); len(advanced) != 0 {
		t.Fatalf("probe kind advanced the cursor: %v", advanced)
	}
}

// A session with no frames returns the idle error, Run resubscribes, and the
// replay starts from the cursor so a create published while the socket was dead
// is delivered.  The last transport error is the idle deadline error, which is
// what makes the half-open socket visible in the logs.
func TestObserverIdleResubscribesAndReplays(t *testing.T) {
	submitter := NewMemorySubmitter()
	cursor := newFakeCursor()
	secret := nostr.GeneratePrivateKey()
	const resumeAt = 1_800_000_500
	since := time.Unix(resumeAt, 0).UTC()

	observer, err := NewObserver(ObserverConfig{
		URL:            "ws://idle.example",
		Submitter:      submitter,
		Cursor:         cursor,
		Logger:         quietLogger(),
		Since:          &since,
		IdleTimeout:    60 * time.Millisecond,
		ReconnectDelay: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The create that was published during the dead window; the re-subscription
	// must deliver it even though the first session never saw a frame.
	replayed := signedCreate(t, secret, "Idle replay", resumeAt+100)

	var (
		mu       sync.Mutex
		sessions int
		filters  [][]nostr.Filter
	)
	observer.connect = func(_ context.Context, _ string, f nostr.Filters) (*relaySubscription, error) {
		mu.Lock()
		sessions++
		n := sessions
		filters = append(filters, append([]nostr.Filter(nil), f...))
		mu.Unlock()
		sub := fakeSubscription()
		if n > 1 {
			sub.Events <- &replayed
		}
		return sub, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = observer.Run(ctx) }()

	waitFor(t, 4*time.Second, func() bool { return len(submitter.Submissions()) == 1 },
		"the create to replay after the idle resubscribe")

	mu.Lock()
	count := sessions
	allFilters := append([][]nostr.Filter(nil), filters...)
	mu.Unlock()
	if count < 2 {
		t.Fatalf("sessions = %d, want at least 2 (one torn down idle, one replay)", count)
	}
	// 3 real kinds + the 1447 probe.
	if len(allFilters[0]) != 4 {
		t.Fatalf("first subscription filters = %d, want 4", len(allFilters[0]))
	}
	// The real kinds resume from the cursor, not from zero.  The probe is
	// anchored near now instead: a cursor can lag days, and replaying every
	// state commitment since then would flood the session it exists to keep.
	for _, f := range allFilters[1] {
		if f.Since == nil {
			t.Fatalf("resumed filter %+v has no since", f)
		}
		if f.Kinds[0] == KindStateCommit {
			if int64(*f.Since) == resumeAt-1 {
				t.Fatalf("probe filter asked from the cursor %d; want a recent window", resumeAt-1)
			}
			continue
		}
		if int64(*f.Since) != resumeAt-1 {
			t.Fatalf("resumed filter %+v does not start from the cursor %d", f, resumeAt-1)
		}
	}
	if _, _, _, lastError := observer.Stats(); lastError == nil || !strings.Contains(lastError.Error(), "idle for") {
		t.Fatalf("lastError = %v, want the idle deadline error", lastError)
	}
	if got := submitter.Submissions()[0].ID; got != replayed.ID {
		t.Fatalf("committed %s, want the replayed create %s", got, replayed.ID)
	}
}

// The probe asks a recent window, never the durable cursor, and never the whole
// relay when the deadline is disabled.
func TestProbeWindowIsRecentAndBounded(t *testing.T) {
	want := defaultIdleTimeout + probeSlack
	if got := probeWindow(0); got != want {
		t.Fatalf("probeWindow(0) = %s, want %s", got, want)
	}
	if got := probeWindow(-1); got != want {
		t.Fatalf("probeWindow(-1) = %s, want the default window %s", got, want)
	}
	if got := probeWindow(2 * time.Minute); got != 2*time.Minute+probeSlack {
		t.Fatalf("probeWindow(2m) = %s, want %s", got, 2*time.Minute+probeSlack)
	}
}

// A non-positive IdleTimeout disables the deadline: no timer is armed, so a
// silent session is never torn down.  Zero takes the default instead, which the
// second observer below pins.
func TestObserverIdleTimeoutDisabled(t *testing.T) {
	submitter := NewMemorySubmitter()
	observer, err := NewObserver(ObserverConfig{
		URL:            "ws://noidle.example",
		Submitter:      submitter,
		Logger:         quietLogger(),
		IdleTimeout:    -1,
		ReconnectDelay: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observer.config.IdleTimeout > 0 {
		t.Fatalf("IdleTimeout = %s, want a non-positive value to disable the deadline", observer.config.IdleTimeout)
	}

	var mu sync.Mutex
	sessions := 0
	observer.connect = func(_ context.Context, _ string, _ nostr.Filters) (*relaySubscription, error) {
		mu.Lock()
		sessions++
		mu.Unlock()
		return fakeSubscription(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = observer.Run(ctx) }()

	waitFor(t, time.Second, observer.Subscribed, "the session to subscribe")
	time.Sleep(200 * time.Millisecond) // well past any small deadline a test would set
	mu.Lock()
	count := sessions
	mu.Unlock()
	if count != 1 {
		t.Fatalf("sessions = %d with the deadline disabled, want 1", count)
	}
	if _, _, _, lastError := observer.Stats(); lastError != nil {
		t.Fatalf("lastError = %v with the deadline disabled, want nil", lastError)
	}

	// Zero means "unset", not "disabled": it takes the default deadline.
	zeroed, err := NewObserver(ObserverConfig{URL: "ws://zero.example", Submitter: submitter, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if zeroed.config.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("zero IdleTimeout = %s, want the default %s", zeroed.config.IdleTimeout, defaultIdleTimeout)
	}
}
