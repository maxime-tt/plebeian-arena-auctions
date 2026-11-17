package main

// Tests for the publish GATE (handover §1) and the params event (handover §4).
//
// The gate is what changed: the base published a state commitment only when
// `height%p.every == 0 || publishedVerdict || p.lastCommit == 0`, and blocks
// are produced on demand, so that cadence had no wall-clock bound at all — an
// idle chain could leave its committed clock stale indefinitely.  These tests
// pin the replacement gate: a verdict, the first pass, a moved height, the
// StateCommitMaxInterval heartbeat, or params needing an anchor.
//
// They also pin the ephemeral pulse (published every pass, ignored when the
// relay answers "no one was listening") and the kind-1448 params event that
// carries the escrow's terms on the relay.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestPublisher builds a publisher with a HELD clock and the given cadence
// and heartbeat interval, so a test can advance wall-clock time by hand.
func newTestPublisher(t *testing.T, a *App, rec RelayWriter, every int64, maxInterval time.Duration, now func() int64) *Publisher {
	t.Helper()
	pub, err := NewPublisher(PublisherConfig{
		Key: testVerifierKey(t), Relay: rec, App: a, Every: every,
		MaxInterval: maxInterval, BlockHash: blockHashOK, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// eventRecorder is the in-memory recorder surface the helpers below need.
type eventRecorder interface{ all() []*Event }

func eventsOfKind(rec eventRecorder, kind int) []*Event {
	var out []*Event
	for _, ev := range rec.all() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func countRecords(pub *Publisher, op string) int {
	n := 0
	for _, r := range pub.Records() {
		if r.Op == op {
			n++
		}
	}
	return n
}

// --- the heartbeat gate (handover §1.2) -----------------------------------

// TestCommitmentHeartbeatPublishesOnAnIdleChain is the point of the change and
// the write-path proof for the heartbeat: the committed HEIGHT IS FIXED and
// only wall-clock time passes, and a commitment must still go out.  On the base
// commit this fails: the old gate was `height%p.every == 0 || publishedVerdict
// || p.lastCommit == 0`, and with the height held and no verdict the cadence
// (100) never fires again.
func TestCommitmentHeartbeatPublishesOnAnIdleChain(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 11, tNow+1) // one commit; the chain is then IDLE at height 11
	rec := &recordingRelay{}
	now := tNow + 2
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 10*time.Second, func() int64 { return now })
	ctx := context.Background()

	pub.Step(ctx) // first pass: publishes (lastCommitTime == 0)
	if got := countRecords(pub, "state_commitment"); got != 1 {
		t.Fatalf("the first pass must publish one commitment, got %d: %v", got, pub.Records())
	}

	// Inside the interval, same height, no verdict: nothing new.
	now += 5
	pub.Step(ctx)
	if got := countRecords(pub, "state_commitment"); got != 1 {
		t.Fatalf("inside the interval at one height no commitment may go out: got %d: %v", got, pub.Records())
	}

	// The chain stays idle at height 11; only the clock moves past the interval.
	now += 10
	pub.Step(ctx)
	if got := countRecords(pub, "state_commitment"); got != 2 {
		t.Fatalf("an idle chain past the max interval MUST heartbeat, got %d: %v", got, pub.Records())
	}
	commits := eventsOfKind(rec, KindStateCommit)
	last := commits[len(commits)-1]
	if got := tagOf(t, last, "height"); got != "11" {
		t.Fatalf("the heartbeat's anchor height = %q, want the unchanged height 11", got)
	}
}

// TestCommitmentPublishesWhenHeightMoves: the interval has NOT elapsed and the
// height changes -> a commitment is published.  This is the write-path proof
// for §1.2's "the height moved" condition.
func TestCommitmentPublishesWhenHeightMoves(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 11, tNow+1)
	rec := &recordingRelay{}
	now := tNow + 2
	// a one-hour interval, so ONLY the height move can fire.
	pub := newTestPublisher(t, a, rec, StateCommitEvery, time.Hour, func() int64 { return now })
	ctx := context.Background()

	pub.Step(ctx)
	if got := countRecords(pub, "state_commitment"); got != 1 {
		t.Fatalf("first pass: %d commitments: %v", got, pub.Records())
	}

	now++ // nowhere near the interval
	finalize(t, a, 12, tNow+2)
	pub.Step(ctx)
	if got := countRecords(pub, "state_commitment"); got != 2 {
		t.Fatalf("a moved height must publish a commitment, got %d: %v", got, pub.Records())
	}
	commits := eventsOfKind(rec, KindStateCommit)
	if got := tagOf(t, commits[len(commits)-1], "height"); got != "12" {
		t.Fatalf("the new commitment's anchor height = %q, want 12", got)
	}
}

// TestCommitmentDoesNotSpamWithinInterval: same height, inside the interval, no
// verdict, not the first pass -> NO new commitment, however many passes run.
func TestCommitmentDoesNotSpamWithinInterval(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 11, tNow+1)
	rec := &recordingRelay{}
	now := tNow + 2
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 10*time.Second, func() int64 { return now })
	ctx := context.Background()

	pub.Step(ctx)
	first := countRecords(pub, "state_commitment")
	for range 5 {
		now++ // one second per pass: five seconds, inside the ten-second interval
		pub.Step(ctx)
	}
	if got := countRecords(pub, "state_commitment"); got != first {
		t.Fatalf("five passes inside the interval at one height must not spam: %d -> %d", first, got)
	}
	if got := len(eventsOfKind(rec, KindStateCommit)); got != first {
		t.Fatalf("the relay saw %d commitments, want %d", got, first)
	}
}

// TestCommitmentFirstPassStillPublishes: the base's write-path proof survives —
// the first pass publishes a signed, anchored commitment.
func TestCommitmentFirstPassStillPublishes(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 11, tNow+1)
	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, time.Hour, func() int64 { return tNow + 2 })

	pub.Step(context.Background())
	commits := eventsOfKind(rec, KindStateCommit)
	if len(commits) != 1 {
		t.Fatalf("the first pass must publish exactly one commitment, got %d", len(commits))
	}
	if err := commits[0].Verify(); err != nil {
		t.Fatalf("the commitment must verify: %v", err)
	}
	c, err := ParseStateCommitment(commits[0], nil)
	if err != nil {
		t.Fatalf("the commitment must parse with its mandatory anchor: %v", err)
	}
	if c.Height != 11 || c.AppHash == "" {
		t.Fatalf("first-pass commitment anchor = %+v, want height 11 and a non-empty app hash", c)
	}
}

// --- the ephemeral pulse (handover §8.1) ----------------------------------

// TestPulsePublishedEveryStepWithAdvancingTime: two passes at ONE height pulse
// twice, and the pulse's Time advances while (height, app_hash) stays put — so
// a subscriber can place the chain's clock without a new block.
func TestPulsePublishedEveryStepWithAdvancingTime(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 11, tNow+1)
	rec := &recordingRelay{}
	now := tNow + 2
	pub := newTestPublisher(t, a, rec, StateCommitEvery, time.Hour, func() int64 { return now })
	ctx := context.Background()

	pub.Step(ctx)
	now += 3
	pub.Step(ctx)

	if got := countRecords(pub, "state_pulse"); got != 2 {
		t.Fatalf("two passes at one height must pulse twice, got %d: %v", got, pub.Records())
	}
	pulses := eventsOfKind(rec, KindStatePulse)
	if len(pulses) != 2 {
		t.Fatalf("the relay saw %d pulses, want 2", len(pulses))
	}
	if pulses[0].Kind != KindStatePulse {
		t.Fatalf("pulse kind = %d, want %d", pulses[0].Kind, KindStatePulse)
	}
	c1, err := ParseStatePulse(pulses[0], nil)
	if err != nil {
		t.Fatalf("pulse 1 must parse: %v", err)
	}
	c2, err := ParseStatePulse(pulses[1], nil)
	if err != nil {
		t.Fatalf("pulse 2 must parse: %v", err)
	}
	if c1.Height != 11 || c2.Height != 11 {
		t.Fatalf("both pulses must be anchored to the one height 11: %d and %d", c1.Height, c2.Height)
	}
	if c1.AppHash == "" || c1.AppHash != c2.AppHash {
		t.Fatalf("the anchor app hash must be unchanged: %q vs %q", c1.AppHash, c2.AppHash)
	}
	if c2.Time <= c1.Time {
		t.Fatalf("the pulse Time must advance: %d then %d", c1.Time, c2.Time)
	}
	if tagOf(t, pulses[0], "hash") != tagOf(t, pulses[1], "hash") {
		t.Fatal("the pulse anchor hash must not move between two passes at one height")
	}
}

// muteOnPulseRelay answers the pulse with the relay's "no one was listening"
// refusal and accepts everything else.  It counts the pulse attempts, so the
// test proves the publisher TRIED rather than silently skipping.
type muteOnPulseRelay struct {
	inner recordingRelay
	mu    sync.Mutex
	tried int
}

func (r *muteOnPulseRelay) Publish(_ context.Context, ev *Event) error {
	if ev.Kind == KindStatePulse {
		r.mu.Lock()
		r.tried++
		r.mu.Unlock()
		return errors.New("relay rejected event 00: mute: no one was listening for this")
	}
	return r.inner.Publish(context.Background(), ev)
}

func (r *muteOnPulseRelay) all() []*Event { return r.inner.all() }

func (r *muteOnPulseRelay) attempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tried
}

// TestPulseNoListenerFailureIsIgnored: a pulse the relay drops for having no
// listener is a NON-EVENT — no record, no error surfaced, and nothing else in
// the pass is affected (handover §8.1 correction 3).
func TestPulseNoListenerFailureIsIgnored(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 11, tNow+1)
	rec := &muteOnPulseRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, time.Hour, func() int64 { return tNow + 2 })

	pub.Step(context.Background())

	if rec.attempts() == 0 {
		t.Fatal("the publisher must still ATTEMPT the pulse; a silent skip would not exercise the rule")
	}
	for _, r := range pub.Records() {
		if r.Op == "state_pulse" {
			t.Fatalf("a muted pulse must leave no record: %+v", r)
		}
		if r.Error != "" {
			t.Fatalf("a muted pulse must not surface an error: %+v", r)
		}
	}
	if got := countRecords(pub, "state_commitment"); got != 1 {
		t.Fatalf("the commitment in the same pass must still publish, got %d: %v", got, pub.Records())
	}
	if got := len(eventsOfKind(rec, KindStateCommit)); got != 1 {
		t.Fatalf("the relay must have seen the commitment: got %d", got)
	}
	if got := len(eventsOfKind(rec, KindParams)); got != 1 {
		t.Fatalf("the params event in the same pass must still publish: got %d", got)
	}
}

// --- the params event (handover §4) ---------------------------------------

// TestParamsEventCarriesEveryTermAndTheAnchor: a fully-populated Params round
// trips through the event, every term §4 names is present, and the anchor is
// the (height, app_hash) pair with the same tag/content cross-check the
// commitment uses.
func TestParamsEventCarriesEveryTermAndTheAnchor(t *testing.T) {
	sk := testVerifierKey(t)
	p := Params{
		MintURL: "http://127.0.0.1:3338", KeysetID: "0102b930f4dc2d6a",
		Keys:         map[string]string{"1": "02aa", "2": "02bb", "64": "02cc"},
		GroupKey:     "03be47f12cc26f63cedb617ecc5dae10485aab91a63bd7ba808ae7c4f23e853da4",
		MinWindowSec: 60, MaxWindowSec: 900,
		SettlementSafetySec: 900, BidToConsensusLatencySec: 15,
		MinDurationSec: 20, MaxDurationSec: 100000, MaxProofs: 64, Threshold: 3,
		Validators: []ValidatorKey{
			{Pubkey: NostrPubkeyHex(sk), Name: "v1", Index: 1},
			{Pubkey: strings.Repeat("ab", 32), Name: "v2", Index: 2},
		},
	}
	roster := p.Roster()
	hash := strings.Repeat("cd", 32)

	ev, err := BuildParamsEvent(sk, p, 77, hash, tNow)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != KindParams {
		t.Fatalf("params kind = %d, want %d", ev.Kind, KindParams)
	}
	if !regularKind(ev.Kind) {
		t.Fatal("the params event must be a STORED (regular) kind")
	}
	if err := ev.Verify(); err != nil {
		t.Fatalf("params event must verify: %v", err)
	}
	if got := tagOf(t, ev, "height"); got != "77" {
		t.Fatalf("height tag = %q", got)
	}
	if got := tagOf(t, ev, "hash"); got != hash {
		t.Fatalf("hash tag = %q", got)
	}

	c, err := ParseParamsEvent(ev, roster)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schema != ParamsSchema || c.Height != 77 || c.AppHash != hash {
		t.Fatalf("anchor/schema = %+v", c)
	}
	if c.ParamsVersion != paramsVersion(p) {
		t.Fatalf("params_version = %q, want the content digest %q", c.ParamsVersion, paramsVersion(p))
	}
	if c.GroupKey != p.GroupKey || c.MintURL != p.MintURL || c.KeysetID != p.KeysetID {
		t.Fatalf("key/mint/keyset did not round trip: %+v", c)
	}
	if len(c.Keys) != len(p.Keys) {
		t.Fatalf("keys = %v, want %v", c.Keys, p.Keys)
	}
	for k, v := range p.Keys {
		if c.Keys[k] != v {
			t.Fatalf("keys[%q] = %q, want %q", k, c.Keys[k], v)
		}
	}
	if c.SettlementSafetySec != 900 || c.BidToConsensusLatencySec != 15 ||
		c.MinWindowSec != 60 || c.MaxWindowSec != 900 ||
		c.MinDurationSec != 20 || c.MaxDurationSec != 100000 ||
		c.MaxProofs != 64 || c.Threshold != 3 {
		t.Fatalf("terms did not round trip: %+v", c)
	}
	if len(c.Validators) != 2 || c.Validators[1].Name != "v2" || c.Validators[1].Index != 2 ||
		c.Validators[0].Pubkey != NostrPubkeyHex(sk) {
		t.Fatalf("roster did not round trip: %+v", c.Validators)
	}

	// the anchor is mandatory, exactly as on the commitment
	if _, err := BuildParamsEvent(sk, p, 0, hash, tNow); err != ErrParamsUnanchored {
		t.Fatalf("zero height must refuse as %v, got %v", ErrParamsUnanchored, err)
	}
	if _, err := BuildParamsEvent(sk, p, 77, "", tNow); err != ErrParamsUnanchored {
		t.Fatalf("empty app hash must refuse as %v, got %v", ErrParamsUnanchored, err)
	}
	// a tag that disagrees with the content is refused BY NAME
	mismatch := resignCopy(t, sk, ev, replaceTag(ev.Tags, "height", "78"))
	if _, err := ParseParamsEvent(mismatch, roster); err != ErrParamsUnanchored {
		t.Fatalf("tag/content disagreement: want %v, got %v", ErrParamsUnanchored, err)
	}
	// an off-roster signer is refused
	off, err := BuildParamsEvent(testVerifierKey(t), p, 77, hash, tNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseParamsEvent(off, roster); err == nil || !strings.Contains(err.Error(), ErrParamsNotInRoster.Error()) {
		t.Fatalf("off-roster params must refuse as %v, got %v", ErrParamsNotInRoster, err)
	}
}

// TestParamsEventRepublishedOnChangeAndNotOtherwise: a terms change produces a
// new event with a DIFFERENT params_version; unchanged terms inside the cadence
// produce nothing (handover §4 "on change as well as on the cadence").
func TestParamsEventRepublishedOnChangeAndNotOtherwise(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 11, tNow+1)
	rec := &recordingRelay{}
	now := tNow + 2
	// cadence 100 blocks and a one-hour heartbeat: only a real change can fire.
	pub := newTestPublisher(t, a, rec, StateCommitEvery, time.Hour, func() int64 { return now })
	ctx := context.Background()

	pub.Step(ctx) // first pass: lastParams == "" -> published
	if got := countRecords(pub, "params"); got != 1 {
		t.Fatalf("the first pass must publish the params, got %d: %v", got, pub.Records())
	}
	first := eventsOfKind(rec, KindParams)
	c1, err := ParseParamsEvent(first[0], nil)
	if err != nil {
		t.Fatal(err)
	}

	now++ // unchanged terms, inside the cadence -> nothing
	pub.Step(ctx)
	if got := countRecords(pub, "params"); got != 1 {
		t.Fatalf("unchanged params inside the cadence must not republish, got %d: %v", got, pub.Records())
	}

	// a real terms change, committed
	a.st.Params.MintURL = "http://127.0.0.1:9999"
	finalize(t, a, 12, tNow+2)
	now++
	pub.Step(ctx)
	if got := countRecords(pub, "params"); got != 2 {
		t.Fatalf("a changed params must republish, got %d: %v", got, pub.Records())
	}
	all := eventsOfKind(rec, KindParams)
	c2, err := ParseParamsEvent(all[len(all)-1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if c2.ParamsVersion == c1.ParamsVersion {
		t.Fatalf("a changed params must carry a different params_version (both %q)", c1.ParamsVersion)
	}
	if c2.MintURL != "http://127.0.0.1:9999" {
		t.Fatalf("republished mint_url = %q, want the changed one", c2.MintURL)
	}
}

// TestParamsEventAnchorsToTheSameHeightAsTheCommitment: when the params go out,
// a commitment with the SAME (height, app_hash) goes out in the same pass, so a
// client can tell the terms belong to that committed state (handover §4).
func TestParamsEventAnchorsToTheSameHeightAsTheCommitment(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 11, tNow+1)
	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, time.Hour, func() int64 { return tNow + 2 })

	pub.Step(context.Background())

	params := eventsOfKind(rec, KindParams)
	commits := eventsOfKind(rec, KindStateCommit)
	if len(params) != 1 || len(commits) != 1 {
		t.Fatalf("params and a commitment must go out in the same pass: %d params, %d commitments",
			len(params), len(commits))
	}
	pc, err := ParseParamsEvent(params[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	cc, err := ParseStateCommitment(commits[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if pc.Height != cc.Height || pc.AppHash != cc.AppHash {
		t.Fatalf("params anchor (%d,%s) must equal the commitment anchor (%d,%s)",
			pc.Height, pc.AppHash, cc.Height, cc.AppHash)
	}
	if pc.Height != 11 {
		t.Fatalf("params anchor height = %d, want the committed height 11", pc.Height)
	}
}

// TestParamsCadenceDoesNotRepublishWhileIdleAtAMultipleOfEvery: blocks are
// produced on demand, so a chain can sit at one height indefinitely. The params
// cadence is `every` blocks and must fire ONCE when the committed height reaches
// a multiple of `every` -- not on every pass while it stays there. Measured live
// before this was fixed: 61 events per validator at a single height.
func TestParamsCadenceDoesNotRepublishWhileIdleAtAMultipleOfEvery(t *testing.T) {
	a, _, _ := liveApp(t)
	finalize(t, a, 100, tNow+1)
	rec := &recordingRelay{}
	now := tNow + 2
	// Cadence 100 blocks with a one-hour heartbeat: only the cadence can fire.
	pub := newTestPublisher(t, a, rec, StateCommitEvery, time.Hour, func() int64 { return now })
	ctx := context.Background()

	pub.Step(ctx) // first pass
	if got := countRecords(pub, "params"); got != 1 {
		t.Fatalf("the first pass must publish the params once, got %d: %v", got, pub.Records())
	}
	// The chain stays at height 100 -- a multiple of the cadence -- for a while.
	for i := 0; i < 5; i++ {
		now++
		pub.Step(ctx)
	}
	if got := countRecords(pub, "params"); got != 1 {
		t.Fatalf("idle at a multiple of the cadence must publish once, got %d: %v", got, pub.Records())
	}

	// Moving on to the NEXT multiple of the cadence republishes.
	finalize(t, a, 200, tNow+2)
	now++
	pub.Step(ctx)
	if got := countRecords(pub, "params"); got != 2 {
		t.Fatalf("the next cadence height must republish, got %d: %v", got, pub.Records())
	}
}
