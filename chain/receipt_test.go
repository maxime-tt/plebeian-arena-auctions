package main

// Tests for the money receipts (kinds 1443 refund / 1444 settle): the wire
// contract's content and tag shape, the build/parse refusals, the publishing
// hook's idempotency and anchor discipline, and the A1b settle record's when.
// The receipt is the MONEY FACT a verdict only CLAIMS: it carries the minted
// outputs a relay-only client claims from, anchored to the SETTLE's committed
// height and block time -- never the publishing pass's.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// baseReceipt is a valid refund receipt content for build/parse tests.
func baseReceipt() ReceiptContent {
	return ReceiptContent{
		AuctionID:     "1440:seller:lot-42",
		BidID:         "bid-1",
		Outcome:       "refund",
		Height:        12,
		BlockHash:     strings.Repeat("ab", 32),
		SettledAt:     tNow + 2,
		ParamsVersion: "pv-digest",
		Out: []ReceiptOut{{
			Recipient: strings.Repeat("02", 33),
			Amount:    1,
			ID:        "keyset",
			Secret:    `["P2PK",{"nonce":"` + strings.Repeat("cc", 32) + `","data":"` + strings.Repeat("02", 33) + `"}]`,
			C:         strings.Repeat("dd", 33),
		}},
	}
}

// 1. A built refund and a built settle receipt carry exactly the contract's
// content keys and tag set; dleq is present-and-null when the proof has none.
func TestReceiptEventsCarryTheContractShape(t *testing.T) {
	sk := testVerifierKey(t)
	for _, tc := range []struct {
		name    string
		kind    int
		schema  string
		outcome string
		build   func(*btcec.PrivateKey, ReceiptContent, int64) (*Event, error)
		parse   func(*Event, map[string]ValidatorKey) (*ReceiptContent, error)
	}{
		{"refund", KindRefund, RefundSchema, "refund", BuildRefundEvent, ParseRefundEvent},
		{"settle", KindSettle, SettleSchema, "settle", BuildSettleEvent, ParseSettleEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := baseReceipt()
			rc.Outcome = tc.outcome
			ev, err := tc.build(sk, rc, tNow)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != tc.kind {
				t.Fatalf("kind = %d, want %d", ev.Kind, tc.kind)
			}
			if !regularKind(ev.Kind) {
				t.Fatal("a receipt must be a STORED (regular) kind")
			}
			if err := ev.Verify(); err != nil {
				t.Fatalf("receipt must verify: %v", err)
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(ev.Content), &raw); err != nil {
				t.Fatal(err)
			}
			want := []string{"schema", "auction_id", "bid_id", "outcome", "height",
				"block_hash", "settled_at", "params_version", "out"}
			if len(raw) != len(want) {
				t.Fatalf("content has %d keys, want %d: %v", len(raw), len(want), raw)
			}
			for _, k := range want {
				if _, ok := raw[k]; !ok {
					t.Fatalf("content is missing %q: %v", k, raw)
				}
			}
			var schema string
			_ = json.Unmarshal(raw["schema"], &schema)
			if schema != tc.schema {
				t.Fatalf("schema = %q, want %q", schema, tc.schema)
			}
			var out []map[string]json.RawMessage
			if err := json.Unmarshal(raw["out"], &out); err != nil || len(out) != 1 {
				t.Fatalf("out = %s", raw["out"])
			}
			outKeys := []string{"recipient", "amount", "id", "secret", "C", "dleq"}
			if len(out[0]) != len(outKeys) {
				t.Fatalf("output has %d keys, want %d: %v", len(out[0]), len(outKeys), out[0])
			}
			for _, k := range outKeys {
				if _, ok := out[0][k]; !ok {
					t.Fatalf("output is missing %q: %v", k, out[0])
				}
			}
			// dleq must be present and null when the proof has none.
			if string(out[0]["dleq"]) != "null" {
				t.Fatalf("dleq = %s, want null", out[0]["dleq"])
			}

			for _, tag := range [][2]string{
				{"auction", rc.AuctionID}, {"bid", rc.BidID},
				{"height", "12"}, {"hash", rc.BlockHash},
				{"outcome", tc.outcome}, {"p", NostrPubkeyHex(sk)},
			} {
				if got := tagOf(t, ev, tag[0]); got != tag[1] {
					t.Fatalf("%s tag = %q, want %q", tag[0], got, tag[1])
				}
			}
			if len(ev.Tags) != 6 {
				t.Fatalf("tags = %v, want exactly the six contract tags", ev.Tags)
			}

			c, err := tc.parse(ev, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.Schema != tc.schema || c.AuctionID != rc.AuctionID || c.BidID != rc.BidID ||
				c.Outcome != tc.outcome || c.Height != 12 || c.BlockHash != rc.BlockHash ||
				c.SettledAt != rc.SettledAt || c.ParamsVersion != rc.ParamsVersion {
				t.Fatalf("parsed content = %+v", c)
			}
			if len(c.Out) != 1 || c.Out[0].Recipient != rc.Out[0].Recipient || c.Out[0].DLEQ != nil {
				t.Fatalf("parsed out = %+v", c.Out)
			}
		})
	}
}

// 2. Unanchored content is refused by name at build; a hand-edited tag/content
// anchor mismatch is refused by name at parse.
func TestReceiptRefusesUnanchoredAndMismatched(t *testing.T) {
	sk := testVerifierKey(t)
	for _, tc := range []struct {
		name   string
		mutate func(*ReceiptContent)
	}{
		{"height 0", func(c *ReceiptContent) { c.Height = 0 }},
		{"empty hash", func(c *ReceiptContent) { c.BlockHash = "" }},
		{"settled_at 0", func(c *ReceiptContent) { c.SettledAt = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := baseReceipt()
			tc.mutate(&rc)
			if _, err := BuildRefundEvent(sk, rc, tNow); err != ErrReceiptUnanchored {
				t.Fatalf("want %v, got %v", ErrReceiptUnanchored, err)
			}
		})
	}

	rc := baseReceipt()
	ev, err := BuildRefundEvent(sk, rc, tNow)
	if err != nil {
		t.Fatal(err)
	}
	// A relay that re-frames the height tag cannot hide the content anchor.
	mismatch := resignCopy(t, sk, ev, replaceTag(ev.Tags, "height", "13"))
	if _, err := ParseRefundEvent(mismatch, nil); err != ErrReceiptUnanchored {
		t.Fatalf("tag/content height mismatch: want %v, got %v", ErrReceiptUnanchored, err)
	}
	mismatch = resignCopy(t, sk, ev, replaceTag(ev.Tags, "hash", strings.Repeat("ef", 32)))
	if _, err := ParseRefundEvent(mismatch, nil); err != ErrReceiptUnanchored {
		t.Fatalf("tag/content hash mismatch: want %v, got %v", ErrReceiptUnanchored, err)
	}
}

// 3. An off-roster signer is refused by name.
func TestReceiptOffRosterRefused(t *testing.T) {
	sk := testVerifierKey(t)
	rc := baseReceipt()
	ev, err := BuildRefundEvent(sk, rc, tNow)
	if err != nil {
		t.Fatal(err)
	}
	roster := map[string]ValidatorKey{NostrPubkeyHex(testVerifierKey(t)): {Pubkey: NostrPubkeyHex(testVerifierKey(t))}}
	if _, err := ParseRefundEvent(ev, roster); err == nil || !strings.Contains(err.Error(), ErrReceiptNotInRoster.Error()) {
		t.Fatalf("off-roster receipt must refuse as %v, got %v", ErrReceiptNotInRoster, err)
	}
}

// 4. An empty out list is refused by name at build and at parse.
func TestReceiptRefusesNoOutputs(t *testing.T) {
	sk := testVerifierKey(t)
	rc := baseReceipt()
	rc.Out = nil
	if _, err := BuildRefundEvent(sk, rc, tNow); err != ErrReceiptNoOutputs {
		t.Fatalf("build: want %v, got %v", ErrReceiptNoOutputs, err)
	}
	// Build a valid event, then empty the content's out list and re-sign.
	good := baseReceipt()
	ev, err := BuildRefundEvent(sk, good, tNow)
	if err != nil {
		t.Fatal(err)
	}
	var c ReceiptContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		t.Fatal(err)
	}
	c.Out = nil
	body, _ := json.Marshal(c)
	bad := resignContent(t, sk, ev, string(body))
	if _, err := ParseRefundEvent(bad, nil); err != ErrReceiptNoOutputs {
		t.Fatalf("parse: want %v, got %v", ErrReceiptNoOutputs, err)
	}
}

// 5. CheckParamsVersion refuses a different digest and accepts the right one.
func TestReceiptCheckParamsVersion(t *testing.T) {
	a, _, _ := liveApp(t)
	p := a.st.Params
	rc := baseReceipt()
	rc.ParamsVersion = paramsVersion(p)
	if err := rc.CheckParamsVersion(p); err != nil {
		t.Fatalf("the right digest must be accepted: %v", err)
	}
	rc.ParamsVersion = "not-the-digest"
	if err := rc.CheckParamsVersion(p); err != ErrReceiptParamsVersionMismatch {
		t.Fatalf("want %v, got %v", ErrReceiptParamsVersionMismatch, err)
	}
}

// 6. Publishing is idempotent: driving Step twice with a settled bid yields
// exactly one receipt event and one "receipt" record for that (bid, outcome).
func TestReceiptPublishingIsIdempotent(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	st := mintMove(t, a, m, ev1.ID, "refund", alice.wallet, tNow+2)
	finalize(t, a, 12, tNow+2, makeSettle(st))
	finalize(t, a, 13, tNow+3) // advance the pass height past the settle's

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 3 })
	ctx := context.Background()
	pub.Step(ctx)
	pub.Step(ctx)

	if got := len(eventsOfKind(rec, KindRefund)); got != 1 {
		t.Fatalf("refund events after two passes = %d, want 1", got)
	}
	if got := countRecords(pub, "receipt"); got != 1 {
		t.Fatalf("receipt records after two passes = %d, want 1", got)
	}
}

// 7. The refund receipt rides a real push refund: a committed refund, then
// Step, and the kind-1443 event's anchor is the SETTLE's height, not the pass's.
func TestRefundReceiptRidesARealPushRefund(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	st := mintMove(t, a, m, ev1.ID, "refund", alice.wallet, tNow+2)
	finalize(t, a, 12, tNow+2, makeSettle(st))
	finalize(t, a, 13, tNow+3) // the pass runs at 13, the money moved at 12

	bid := a.st.Bids[ev1.ID]
	if bid.Settled != "refund" {
		t.Fatalf("setup: not settled: %+v", bid)
	}

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 3 })
	pub.Step(context.Background())

	evs := eventsOfKind(rec, KindRefund)
	if len(evs) != 1 {
		t.Fatalf("refund events = %d, want 1", len(evs))
	}
	ev := evs[0]
	c, err := ParseRefundEvent(ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.AuctionID != id || c.BidID != ev1.ID || c.Outcome != "refund" {
		t.Fatalf("receipt names %q/%q/%q", c.AuctionID, c.BidID, c.Outcome)
	}
	if c.Height != 12 {
		t.Fatalf("receipt height = %d, want the settle's 12 (not the pass's 13)", c.Height)
	}
	if c.SettledAt != tNow+2 {
		t.Fatalf("settled_at = %d, want the settle block's %d", c.SettledAt, tNow+2)
	}
	if want, _ := blockHashOK(12); c.BlockHash != want {
		t.Fatalf("block_hash = %q, want %q", c.BlockHash, want)
	}
	if len(c.Out) != len(st.Proofs) || len(c.Out) == 0 {
		t.Fatalf("out = %d outputs, want %d", len(c.Out), len(st.Proofs))
	}
	for _, o := range c.Out {
		if o.Recipient != alice.wallet {
			t.Fatalf("recipient = %q, want the bidder's wallet %q", o.Recipient, alice.wallet)
		}
	}
	if c.Out[0].DLEQ == nil {
		t.Fatal("the mint signed a DLEQ: the receipt must carry it")
	}
}

// 8. The settle receipt rides a real payout (kind 1444).
func TestSettleReceiptRidesARealPayout(t *testing.T) {
	a, m, id := liveApp(t)
	au := a.st.Auctions[id]
	au.Mode, au.WindowSec, au.EndAt = "antisnipe", 60, tNow+1000
	seller := newBidder(t)
	au.SellerWallet = seller.wallet
	alice := newBidder(t)
	ev, tx := m.bidTx(t, a, alice, id, 150, "aa")
	finalize(t, a, 11, tNow+1, tx)
	au.EndAt = tNow + 50
	finalize(t, a, 12, tNow+50)
	pay := mintMove(t, a, m, ev.ID, "settle", seller.wallet, tNow+51)
	finalize(t, a, 13, tNow+51, makeSettle(pay))
	finalize(t, a, 14, tNow+52)

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 52 })
	pub.Step(context.Background())

	evs := eventsOfKind(rec, KindSettle)
	if len(evs) != 1 {
		t.Fatalf("settle events = %d, want 1", len(evs))
	}
	c, err := ParseSettleEvent(evs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.AuctionID != id || c.BidID != ev.ID || c.Outcome != "settle" {
		t.Fatalf("receipt names %q/%q/%q", c.AuctionID, c.BidID, c.Outcome)
	}
	if c.Height != 13 || c.SettledAt != tNow+51 {
		t.Fatalf("anchor = %d/%d, want 13/%d", c.Height, c.SettledAt, tNow+51)
	}
	if want, _ := blockHashOK(13); c.BlockHash != want {
		t.Fatalf("block_hash = %q, want %q", c.BlockHash, want)
	}
	for _, o := range c.Out {
		if o.Recipient != seller.wallet {
			t.Fatalf("recipient = %q, want the seller's wallet %q", o.Recipient, seller.wallet)
		}
	}
}

// 9. A bid whose settle predates SettledHeight is refused ONCE and never
// retried across repeated passes.
func TestReceiptPredatingSettledHeightRefusedOnce(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	st := mintMove(t, a, m, ev1.ID, "refund", alice.wallet, tNow+2)
	finalize(t, a, 12, tNow+2, makeSettle(st))

	// The shape of every settle record on the live root today: Settled and Out
	// set, the new when fields absent.
	a.st.Bids[ev1.ID].SettledHeight = 0
	a.st.Bids[ev1.ID].SettledAt = 0
	a.committed = a.st.canonical()

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 3 })
	ctx := context.Background()
	pub.Step(ctx)
	pub.Step(ctx)
	pub.Step(ctx)

	if got := len(eventsOfKind(rec, KindRefund)); got != 0 {
		t.Fatalf("a settle that predates settledHeight must not be published, got %d events", got)
	}
	refusals := 0
	for _, r := range pub.Records() {
		if r.Op == "refused" && r.Reason == "settle_predates_settled_height" {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("refusals = %d, want exactly 1 across three passes", refusals)
	}
}

// 10. A settle whose block hash is not yet readable is refused, and is
// published on a later pass once blockHash answers.
func TestReceiptRetriesUntilBlockHashIsReadable(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	st := mintMove(t, a, m, ev1.ID, "refund", alice.wallet, tNow+2)
	finalize(t, a, 12, tNow+2, makeSettle(st))

	ready := false
	rec := &recordingRelay{}
	pub, err := NewPublisher(PublisherConfig{
		Key: testVerifierKey(t), Relay: rec, App: a,
		BlockHash: func(h int64) (string, bool) {
			if !ready {
				return "", false
			}
			return blockHashOK(h)
		},
		Now: func() int64 { return tNow + 3 },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pub.Step(ctx)
	if got := len(eventsOfKind(rec, KindRefund)); got != 0 {
		t.Fatalf("unanchored receipt must not be published, got %d", got)
	}
	refused := false
	for _, r := range pub.Records() {
		if r.Op == "refused" && r.Reason == "committed_block_hash_unknown" {
			refused = true
		}
	}
	if !refused {
		t.Fatal("the unreadable anchor must be refused by name")
	}
	ready = true
	pub.Step(ctx)
	if got := len(eventsOfKind(rec, KindRefund)); got != 1 {
		t.Fatalf("after the anchor becomes readable, receipts = %d, want 1", got)
	}
}

// 11. A1b: applySettle records the committed block's height and time.
func TestApplySettleRecordsTheBlockWhen(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	st := mintMove(t, a, m, ev1.ID, "refund", alice.wallet, tNow+2)
	finalize(t, a, 12, tNow+2, makeSettle(st))
	b := a.st.Bids[ev1.ID]
	if b.SettledHeight != 12 || b.SettledAt != tNow+2 {
		t.Fatalf("settle when = %d/%d, want 12/%d", b.SettledHeight, b.SettledAt, tNow+2)
	}
}

// 12. Deploy-safety pin: a snapshot written WITHOUT settledHeight/settledAt --
// the shape of every settle record on the live root today -- still loads, and
// its bid keeps SettledHeight == 0.  This test fails if omitempty is dropped,
// because the canonical re-encode would then carry the zero fields and the
// load path's byte-identical check would refuse the file.
func TestSnapshotWithoutSettledWhenStillLoads(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	st := mintMove(t, a, m, ev1.ID, "refund", alice.wallet, tNow+2)
	finalize(t, a, 12, tNow+2, makeSettle(st))

	// Simulate the old record: the when fields are zero, so omitempty must drop
	// them from the canonical bytes.
	a.st.Bids[ev1.ID].SettledHeight = 0
	a.st.Bids[ev1.ID].SettledAt = 0
	raw := a.st.canonical()
	if strings.Contains(string(raw), "settledHeight") || strings.Contains(string(raw), "settledAt") {
		t.Fatal("omitempty is missing: the canonical state carries the zero when fields, so every existing snapshot would stop loading")
	}

	dir := t.TempDir()
	hash := a.st.hash()
	s := &snapshotter{dir: dir, every: 1}
	if err := s.write(12, tNow+2, hash, raw); err != nil {
		t.Fatal(err)
	}
	snap, err := loadSnapshot(dir, nil)
	if err != nil {
		t.Fatalf("an old-shape snapshot must load: %v", err)
	}
	if snap == nil || snap.height != 12 {
		t.Fatalf("load: %+v", snap)
	}
	if b := snap.state.Bids[ev1.ID]; b == nil || b.Settled != "refund" || b.SettledHeight != 0 {
		t.Fatalf("restored bid = %+v, want settled refund with SettledHeight 0", b)
	}
}

// resignContent re-signs an existing event with new content, keeping its kind
// and tags, so a test can build a validly signed but altered event.
func resignContent(t *testing.T, sk *btcec.PrivateKey, ev *Event, content string) *Event {
	t.Helper()
	return signEventTags(t, sk, ev.Kind, content, ev.CreatedAt, ev.Tags)
}
