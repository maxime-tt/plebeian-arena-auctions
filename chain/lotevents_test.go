package main

// Tests for the lot-state (kind 1449) and refusal (kind 1450) events: the wire
// contract's content and tag shape, the build/parse refusals by name, and the
// publisher's anchor discipline, dedupe and retry behaviour.
//
// The receipt family's tests are the template (receipt_test.go): a real
// SignEvent under a real key, a roster for the membership check, and mutated
// re-signed copies for every refusal -- never a fake signature.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
)

// lotFixture is valid kind-1449 content for the build/parse tests.
func lotFixture() LotContent {
	return LotContent{
		Schema:          LotSchema,
		AuctionID:       "1440:seller:lot-42",
		EndTime:         tNow + 150,
		OriginalEndTime: tNow + 100,
		Extensions:      1,
		Height:          12,
		BlockHash:       testBlockHash(12),
	}
}

// refusedFixture is valid kind-1450 content for the build/parse tests.
func refusedFixture() RefusedContent {
	return RefusedContent{
		Schema:    RefusedSchema,
		EventID:   strings.Repeat("ab", 32),
		Kind:      KindCreate,
		Reason:    "title required (max 120 chars)",
		Height:    11,
		BlockHash: testBlockHash(11),
	}
}

// 1. Build -> parse round-trip for both kinds: kind, content field order, tag
// order, id and signature all agree, and the roster check passes.
func TestLotAndRefusedEventsRoundTrip(t *testing.T) {
	sk := testVerifierKey(t)
	roster := map[string]ValidatorKey{
		NostrPubkeyHex(sk): {Pubkey: NostrPubkeyHex(sk), Name: "v1", Index: 1},
	}

	t.Run("lot", func(t *testing.T) {
		lc := lotFixture()
		ev, err := BuildLotEvent(sk, lc, tNow)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind != KindLot || !regularKind(ev.Kind) {
			t.Fatalf("kind = %d, want the STORED kind %d", ev.Kind, KindLot)
		}
		if err := ev.Verify(); err != nil {
			t.Fatalf("the lot event must verify: %v", err)
		}
		// The content field order is part of the contract.
		wantKeys := []string{"schema", "auction_id", "end_time", "original_end_time",
			"extensions", "height", "block_hash"}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ev.Content), &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) != len(wantKeys) {
			t.Fatalf("content has %d keys, want %d: %v", len(raw), len(wantKeys), raw)
		}
		last := -1
		for _, k := range wantKeys {
			if _, ok := raw[k]; !ok {
				t.Fatalf("content is missing %q: %v", k, raw)
			}
			i := strings.Index(ev.Content, `"`+k+`"`)
			if i <= last {
				t.Fatalf("content keys are not in the contract order: %s", ev.Content)
			}
			last = i
		}
		// Tags, in order, and exactly these four.
		wantTags := [][2]string{
			{"auction", lc.AuctionID}, {"height", "12"},
			{"hash", lc.BlockHash}, {"p", NostrPubkeyHex(sk)},
		}
		if len(ev.Tags) != len(wantTags) {
			t.Fatalf("tags = %v, want exactly the four contract tags", ev.Tags)
		}
		for i, want := range wantTags {
			if len(ev.Tags[i]) < 2 || ev.Tags[i][0] != want[0] || ev.Tags[i][1] != want[1] {
				t.Fatalf("tag %d = %v, want %v", i, ev.Tags[i], want)
			}
		}
		c, err := ParseLotEvent(ev, roster)
		if err != nil {
			t.Fatal(err)
		}
		if c.Schema != LotSchema || c.AuctionID != lc.AuctionID || c.EndTime != lc.EndTime ||
			c.OriginalEndTime != lc.OriginalEndTime || c.Extensions != lc.Extensions ||
			c.Height != lc.Height || c.BlockHash != lc.BlockHash {
			t.Fatalf("parsed content = %+v", c)
		}
	})

	t.Run("refused", func(t *testing.T) {
		rc := refusedFixture()
		ev, err := BuildRefusedEvent(sk, rc, tNow)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind != KindRefused || !regularKind(ev.Kind) {
			t.Fatalf("kind = %d, want the STORED kind %d", ev.Kind, KindRefused)
		}
		if err := ev.Verify(); err != nil {
			t.Fatalf("the refusal event must verify: %v", err)
		}
		wantKeys := []string{"schema", "event_id", "kind", "reason", "height", "block_hash"}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ev.Content), &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) != len(wantKeys) {
			t.Fatalf("content has %d keys, want %d: %v", len(raw), len(wantKeys), raw)
		}
		last := -1
		for _, k := range wantKeys {
			if _, ok := raw[k]; !ok {
				t.Fatalf("content is missing %q: %v", k, raw)
			}
			i := strings.Index(ev.Content, `"`+k+`"`)
			if i <= last {
				t.Fatalf("content keys are not in the contract order: %s", ev.Content)
			}
			last = i
		}
		wantTags := [][2]string{
			{"e", rc.EventID}, {"height", "11"},
			{"hash", rc.BlockHash}, {"p", NostrPubkeyHex(sk)},
		}
		if len(ev.Tags) != len(wantTags) {
			t.Fatalf("tags = %v, want exactly the four contract tags", ev.Tags)
		}
		for i, want := range wantTags {
			if len(ev.Tags[i]) < 2 || ev.Tags[i][0] != want[0] || ev.Tags[i][1] != want[1] {
				t.Fatalf("tag %d = %v, want %v", i, ev.Tags[i], want)
			}
		}
		c, err := ParseRefusedEvent(ev, roster)
		if err != nil {
			t.Fatal(err)
		}
		if c.Schema != RefusedSchema || c.EventID != rc.EventID || c.Kind != rc.Kind ||
			c.Reason != rc.Reason || c.Height != rc.Height || c.BlockHash != rc.BlockHash {
			t.Fatalf("parsed content = %+v", c)
		}
	})
}

// 2. Every fault is refused BY NAME: wrong kind, tampered content (the
// signature binds it), tampered anchor tags, an empty anchor, an off-roster
// signer, and -- for the refusal -- an e tag naming a different event.
func TestLotEventRefusals(t *testing.T) {
	sk := testVerifierKey(t)
	lc := lotFixture()
	ev, err := BuildLotEvent(sk, lc, tNow)
	if err != nil {
		t.Fatal(err)
	}

	// Wrong kind is refused before anything else.
	other := *ev
	other.Kind = KindRefused
	if _, err := ParseLotEvent(&other, nil); !errors.Is(err, ErrLotKind) {
		t.Fatalf("wrong kind: want %v, got %v", ErrLotKind, err)
	}

	// Content edited without re-signing: the signature no longer covers it.
	var c LotContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		t.Fatal(err)
	}
	c.EndTime++
	body, _ := json.Marshal(c)
	unsigned := *ev
	unsigned.Content = string(body)
	if _, err := ParseLotEvent(&unsigned, nil); !errors.Is(err, ErrLotBadSignature) {
		t.Fatalf("tampered content: want %v, got %v", ErrLotBadSignature, err)
	}

	// Re-signed tampering with the schema.
	c = lc
	c.Schema = "arena.other.v1"
	body, _ = json.Marshal(c)
	if _, err := ParseLotEvent(resignContent(t, sk, ev, string(body)), nil); !errors.Is(err, ErrLotBadSchema) {
		t.Fatalf("bad schema: want %v, got %v", ErrLotBadSchema, err)
	}

	// Re-signed tampering with the content's anchor (tags still say 12).
	c = lc
	c.Height = 13
	body, _ = json.Marshal(c)
	if _, err := ParseLotEvent(resignContent(t, sk, ev, string(body)), nil); !errors.Is(err, ErrLotUnanchored) {
		t.Fatalf("content height vs tags: want %v, got %v", ErrLotUnanchored, err)
	}

	// A re-framed height tag cannot hide the content anchor.
	if _, err := ParseLotEvent(resignCopy(t, sk, ev, replaceTag(ev.Tags, "height", "13")), nil); !errors.Is(err, ErrLotUnanchored) {
		t.Fatalf("height tag mismatch: want %v, got %v", ErrLotUnanchored, err)
	}
	// Nor can a re-framed hash tag.
	if _, err := ParseLotEvent(resignCopy(t, sk, ev, replaceTag(ev.Tags, "hash", testBlockHash(99))), nil); !errors.Is(err, ErrLotUnanchored) {
		t.Fatalf("hash tag mismatch: want %v, got %v", ErrLotUnanchored, err)
	}
	// A missing anchor tag is unanchored, not merely malformed.
	if _, err := ParseLotEvent(resignCopy(t, sk, ev, dropTag(ev.Tags, "hash")), nil); !errors.Is(err, ErrLotUnanchored) {
		t.Fatalf("missing hash tag: want %v, got %v", ErrLotUnanchored, err)
	}

	// An empty block hash in the content is unanchored at build and at parse.
	bad := lotFixture()
	bad.BlockHash = ""
	if _, err := BuildLotEvent(sk, bad, tNow); !errors.Is(err, ErrLotUnanchored) {
		t.Fatalf("build with empty hash: want %v, got %v", ErrLotUnanchored, err)
	}
	c = lc
	c.BlockHash = ""
	body, _ = json.Marshal(c)
	if _, err := ParseLotEvent(resignContent(t, sk, ev, string(body)), nil); !errors.Is(err, ErrLotUnanchored) {
		t.Fatalf("parse with empty hash: want %v, got %v", ErrLotUnanchored, err)
	}

	// The field rules the builder enforces.
	for name, mutate := range map[string]func(*LotContent){
		"no auction":    func(c *LotContent) { c.AuctionID = "" },
		"end zero":      func(c *LotContent) { c.EndTime = 0 },
		"original zero": func(c *LotContent) { c.OriginalEndTime = 0 },
		"negative ext":  func(c *LotContent) { c.Extensions = -1 },
	} {
		bad := lotFixture()
		mutate(&bad)
		if _, err := BuildLotEvent(sk, bad, tNow); !errors.Is(err, ErrLotBadSchema) {
			t.Fatalf("build %s: want %v, got %v", name, ErrLotBadSchema, err)
		}
	}

	// An off-roster signer is refused by name.
	otherKey := testVerifierKey(t)
	roster := map[string]ValidatorKey{NostrPubkeyHex(otherKey): {Pubkey: NostrPubkeyHex(otherKey), Index: 1}}
	if _, err := ParseLotEvent(ev, roster); !errors.Is(err, ErrLotNotInRoster) {
		t.Fatalf("off-roster: want %v, got %v", ErrLotNotInRoster, err)
	}
}

// 2b. The refusal event's own fault set, including an e tag that names a
// different event than the content and a target kind outside auction traffic.
func TestRefusedEventRefusals(t *testing.T) {
	sk := testVerifierKey(t)
	rc := refusedFixture()
	ev, err := BuildRefusedEvent(sk, rc, tNow)
	if err != nil {
		t.Fatal(err)
	}

	other := *ev
	other.Kind = KindLot
	if _, err := ParseRefusedEvent(&other, nil); !errors.Is(err, ErrRefusedKind) {
		t.Fatalf("wrong kind: want %v, got %v", ErrRefusedKind, err)
	}

	var c RefusedContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		t.Fatal(err)
	}
	c.Reason = "something the chain never said"
	body, _ := json.Marshal(c)
	unsigned := *ev
	unsigned.Content = string(body)
	if _, err := ParseRefusedEvent(&unsigned, nil); !errors.Is(err, ErrRefusedBadSignature) {
		t.Fatalf("tampered content: want %v, got %v", ErrRefusedBadSignature, err)
	}

	c = rc
	c.Schema = "arena.other.v1"
	body, _ = json.Marshal(c)
	if _, err := ParseRefusedEvent(resignContent(t, sk, ev, string(body)), nil); !errors.Is(err, ErrRefusedBadSchema) {
		t.Fatalf("bad schema: want %v, got %v", ErrRefusedBadSchema, err)
	}

	// The target kind must stay inside auction traffic (1440 create, 1441 bid).
	c = rc
	c.Kind = KindChat
	body, _ = json.Marshal(c)
	if _, err := ParseRefusedEvent(resignContent(t, sk, ev, string(body)), nil); !errors.Is(err, ErrRefusedBadTarget) {
		t.Fatalf("chat target kind: want %v, got %v", ErrRefusedBadTarget, err)
	}
	if _, err := BuildRefusedEvent(sk, c, tNow); !errors.Is(err, ErrRefusedBadTarget) {
		t.Fatalf("build chat target kind: want %v, got %v", ErrRefusedBadTarget, err)
	}

	c = rc
	c.Height = 13
	body, _ = json.Marshal(c)
	if _, err := ParseRefusedEvent(resignContent(t, sk, ev, string(body)), nil); !errors.Is(err, ErrRefusedUnanchored) {
		t.Fatalf("content height vs tags: want %v, got %v", ErrRefusedUnanchored, err)
	}

	if _, err := ParseRefusedEvent(resignCopy(t, sk, ev, replaceTag(ev.Tags, "height", "13")), nil); !errors.Is(err, ErrRefusedUnanchored) {
		t.Fatalf("height tag mismatch: want %v, got %v", ErrRefusedUnanchored, err)
	}
	if _, err := ParseRefusedEvent(resignCopy(t, sk, ev, replaceTag(ev.Tags, "hash", testBlockHash(99))), nil); !errors.Is(err, ErrRefusedUnanchored) {
		t.Fatalf("hash tag mismatch: want %v, got %v", ErrRefusedUnanchored, err)
	}

	// An e tag naming a different event is refused by name.
	if _, err := ParseRefusedEvent(resignCopy(t, sk, ev, replaceTag(ev.Tags, "e", strings.Repeat("cd", 32))), nil); !errors.Is(err, ErrRefusedBadEvent) {
		t.Fatalf("e tag naming another event: want %v, got %v", ErrRefusedBadEvent, err)
	}

	// Empty block hash is unanchored at build and at parse.
	bad := refusedFixture()
	bad.BlockHash = ""
	if _, err := BuildRefusedEvent(sk, bad, tNow); !errors.Is(err, ErrRefusedUnanchored) {
		t.Fatalf("build with empty hash: want %v, got %v", ErrRefusedUnanchored, err)
	}
	c = rc
	c.BlockHash = ""
	body, _ = json.Marshal(c)
	if _, err := ParseRefusedEvent(resignContent(t, sk, ev, string(body)), nil); !errors.Is(err, ErrRefusedUnanchored) {
		t.Fatalf("parse with empty hash: want %v, got %v", ErrRefusedUnanchored, err)
	}

	// Empty event id and empty reason are refused by name.
	for name, mutate := range map[string]func(*RefusedContent){
		"no event":  func(c *RefusedContent) { c.EventID = "" },
		"no reason": func(c *RefusedContent) { c.Reason = "" },
	} {
		bad := refusedFixture()
		mutate(&bad)
		if _, err := BuildRefusedEvent(sk, bad, tNow); !errors.Is(err, ErrRefusedBadSchema) {
			t.Fatalf("build %s: want %v, got %v", name, ErrRefusedBadSchema, err)
		}
	}

	otherKey := testVerifierKey(t)
	roster := map[string]ValidatorKey{NostrPubkeyHex(otherKey): {Pubkey: NostrPubkeyHex(otherKey), Index: 1}}
	if _, err := ParseRefusedEvent(ev, roster); !errors.Is(err, ErrRefusedNotInRoster) {
		t.Fatalf("off-roster: want %v, got %v", ErrRefusedNotInRoster, err)
	}
}

// --- the publisher --------------------------------------------------------

// lotShapeEvents returns the kind-1449 events that carry the arena.lot.v1
// shape, so a test counts LOT events without counting the bid-ADMISSION facts
// that ride the same kind (R4).
func lotShapeEvents(t *testing.T, rec eventRecorder) []*Event {
	t.Helper()
	var out []*Event
	for _, ev := range eventsOfKind(rec, KindLot) {
		if _, err := ParseLotEvent(ev, nil); err == nil {
			out = append(out, ev)
		}
	}
	return out
}

// extendingLot commits an antisnipe bid at height 12 that extends the lot past
// its original end (tNow+100).  The bid's build-time clock matches the block's
// so the escrow locktime clears: commit the clock at 11 first.
func extendingLot(t *testing.T) (a *App, coord string, bidID string) {
	t.Helper()
	a, m, coord := liveApp(t)
	au := a.st.Auctions[coord]
	au.Mode, au.WindowSec = "antisnipe", 60
	finalize(t, a, 11, tNow+90) // commits the window; nothing extends yet
	au.EndAt = tNow + 95
	alice := newBidder(t)
	ev, tx := m.bidTx(t, a, alice, coord, 150, "aa")
	if res := finalize(t, a, 12, tNow+90, tx); res.TxResults[0].Code != 0 {
		t.Fatalf("extending bid refused: %s", res.TxResults[0].Log)
	}
	return a, coord, ev.ID
}

// 3. An extending bid publishes kind 1449 with end_time > original_end_time,
// extensions >= 1, anchored at the BID's height and that block's hash; a second
// pass publishes nothing new.
func TestLotEventForAnExtendingBid(t *testing.T) {
	a, coord, bidID := extendingLot(t)
	bid := a.st.Bids[bidID]
	if bid.EstablishedEnd != tNow+150 || bid.ExtensionOrdinal != 1 {
		t.Fatalf("bid lot state = %d/%d, want %d/1", bid.EstablishedEnd, bid.ExtensionOrdinal, tNow+150)
	}

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 91 })
	ctx := context.Background()
	pub.Step(ctx)

	evs := lotShapeEvents(t, rec)
	if len(evs) != 1 {
		t.Fatalf("lot events = %d, want 1", len(evs))
	}
	c, err := ParseLotEvent(evs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.AuctionID != coord || c.EndTime <= c.OriginalEndTime || c.Extensions < 1 {
		t.Fatalf("lot content = %+v, want end > original end and an extension", c)
	}
	if c.OriginalEndTime != tNow+100 {
		t.Fatalf("original_end_time = %d, want the create's %d", c.OriginalEndTime, tNow+100)
	}
	if c.Height != bid.Height || c.Height != 12 {
		t.Fatalf("anchor height = %d, want the bid's %d", c.Height, bid.Height)
	}
	if c.BlockHash != testBlockHash(12) {
		t.Fatalf("anchor hash = %q, want %q", c.BlockHash, testBlockHash(12))
	}

	// A second pass publishes nothing new: dedupe by (auction_id, end_time).
	pub.Step(ctx)
	if got := len(lotShapeEvents(t, rec)); got != 1 {
		t.Fatalf("lot events after two passes = %d, want 1", got)
	}
	if got := countRecords(pub, "lot"); got != 1 {
		t.Fatalf("lot records after two passes = %d, want 1", got)
	}
}

// 4. A closed lot that never extended publishes exactly one kind 1449 at close,
// where end_time == original_end_time and extensions == 0, anchored at ClosedH.
func TestLotEventForAClosedLotWithoutExtension(t *testing.T) {
	a, m, coord := liveApp(t)
	alice := newBidder(t)
	_, tx := m.bidTx(t, a, alice, coord, 200, "bb")
	finalize(t, a, 11, tNow+1, tx)
	finalize(t, a, 12, tNow+101) // the closing block
	au := a.st.Auctions[coord]
	if au.Status != "closed" || au.ClosedH != 12 || au.Extensions != 0 {
		t.Fatalf("setup: status=%q closedH=%d extensions=%d", au.Status, au.ClosedH, au.Extensions)
	}

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 102 })
	pub.Step(context.Background())

	evs := lotShapeEvents(t, rec)
	if len(evs) != 1 {
		t.Fatalf("lot events = %d, want exactly 1", len(evs))
	}
	c, err := ParseLotEvent(evs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.AuctionID != coord {
		t.Fatalf("auction = %q, want %q", c.AuctionID, coord)
	}
	if c.EndTime != c.OriginalEndTime || c.EndTime != tNow+100 {
		t.Fatalf("end/original = %d/%d, want the unchanged %d", c.EndTime, c.OriginalEndTime, tNow+100)
	}
	if c.Extensions != 0 {
		t.Fatalf("extensions = %d, want 0", c.Extensions)
	}
	if c.Height != 12 || c.BlockHash != testBlockHash(12) {
		t.Fatalf("anchor = %d/%s, want 12/%s", c.Height, c.BlockHash, testBlockHash(12))
	}
}

// 5. A rejected create publishes one kind 1450 naming the event and carrying
// the chain's reason VERBATIM, anchored at the rejecting block; a chat
// rejection or one with no event id publishes nothing.
func TestRefusalEventForARejectedCreate(t *testing.T) {
	a, _, _ := liveApp(t)
	seller := newBidder(t)
	bad := createEv(t, seller.sk, "bad-lot", CreateContent{Title: "", StartPrice: 100,
		MinIncrement: 10, DurationSec: 100, SellerWallet: seller.wallet, Mode: "fixed"}, tNow)
	tx, _ := json.Marshal(bad)
	res := finalize(t, a, 11, tNow+1, tx)
	if res.TxResults[0].Code == 0 {
		t.Fatal("setup: the titleless create must be rejected")
	}

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	ctx := context.Background()
	pub.Step(ctx)

	evs := eventsOfKind(rec, KindRefused)
	if len(evs) != 1 {
		t.Fatalf("refusal events = %d, want 1", len(evs))
	}
	c, err := ParseRefusedEvent(evs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.EventID != bad.ID || c.Kind != KindCreate {
		t.Fatalf("refusal names %q/%d, want %q/%d", c.EventID, c.Kind, bad.ID, KindCreate)
	}
	if c.Reason != "title required (max 120 chars)" {
		t.Fatalf("reason = %q, want the chain's verbatim reason", c.Reason)
	}
	if c.Height != 11 || c.BlockHash != testBlockHash(11) {
		t.Fatalf("anchor = %d/%s, want 11/%s", c.Height, c.BlockHash, testBlockHash(11))
	}

	// A chat rejection and a rejection with no event id are not auction answers:
	// neither may publish anything, even when they are committed state.
	a.st.Rejections = append(a.st.Rejections,
		Rejection{Height: 11, Time: tNow + 1, Kind: KindChat, Pubkey: "x", Event: strings.Repeat("ee", 32), Reason: "chat ignored"},
		Rejection{Height: 11, Time: tNow + 1, Kind: KindCreate, Pubkey: "x", Reason: "no event id"},
	)
	a.committed = a.st.canonical()
	pub.Step(ctx)
	if got := len(eventsOfKind(rec, KindRefused)); got != 1 {
		t.Fatalf("refusal events after a chat/empty-id rejection = %d, want 1", got)
	}
	if got := countRecords(pub, "refusal"); got != 1 {
		t.Fatalf("refusal records = %d, want 1", got)
	}
}

// 6. An unreadable anchor block hash refuses the lot event (nothing published,
// one "refused" record); once the hash is readable the next pass publishes it.
func TestLotRefusedWhenAnchorHashUnknown(t *testing.T) {
	a, m, coord := liveApp(t)
	alice := newBidder(t)
	_, tx := m.bidTx(t, a, alice, coord, 200, "cc")
	finalize(t, a, 11, tNow+1, tx)
	finalize(t, a, 12, tNow+101) // closes at ClosedH 12, the anchor the lot event needs

	readable := false
	rec := &recordingRelay{}
	pub, err := NewPublisher(PublisherConfig{Key: testVerifierKey(t), Relay: rec, App: a, Every: 1,
		BlockHash: func(h int64) (string, bool) {
			if !readable {
				return "", false
			}
			return testBlockHash(h), true
		},
		Now: func() int64 { return tNow + 102 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pub.Step(ctx)

	if got := len(lotShapeEvents(t, rec)); got != 0 {
		t.Fatalf("an unanchorable lot event must not be published, got %d", got)
	}
	found := false
	for _, r := range pub.Records() {
		if r.Op == "refused" && r.Height == 12 && r.AuctionID == coord && r.Reason == "committed_block_hash_unknown" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a refused record for the lot's anchor, got %v", pub.Records())
	}

	// The hash becomes readable: the very next pass publishes it.
	readable = true
	pub.Step(ctx)
	evs := lotShapeEvents(t, rec)
	if len(evs) != 1 {
		t.Fatalf("lot events after the hash became readable = %d, want 1", len(evs))
	}
	c, err := ParseLotEvent(evs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Height != 12 || c.BlockHash != testBlockHash(12) {
		t.Fatalf("anchor = %d/%s, want 12/%s", c.Height, c.BlockHash, testBlockHash(12))
	}
}

// 7. Snapshot compatibility: the two new bid fields are omitted when zero (so
// an old snapshot re-encodes byte-identically) and survive a real snapshot
// write/load when set.
func TestBidLotFieldsAreSnapshotCompatible(t *testing.T) {
	a, m, coord := liveApp(t)
	alice := newBidder(t)
	ev, tx := m.bidTx(t, a, alice, coord, 150, "dd")
	finalize(t, a, 11, tNow+1, tx)

	// Old shape: a Bid without the fields marshals without them.
	old := Bid{ID: "old-bid", Auction: coord, Amount: 1}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "establishedEnd") || strings.Contains(string(raw), "extensionOrdinal") {
		t.Fatalf("zero lot fields must be omitted: %s", raw)
	}

	// An old-shape state re-encodes without them and loads.
	bid := a.st.Bids[ev.ID]
	bid.EstablishedEnd, bid.ExtensionOrdinal = 0, 0
	oldState := a.st.canonical()
	if strings.Contains(string(oldState), "establishedEnd") || strings.Contains(string(oldState), "extensionOrdinal") {
		t.Fatal("an old snapshot must stay byte-identical: the zero lot fields leaked into the canonical state")
	}
	dir := t.TempDir()
	s := &snapshotter{dir: dir, every: 1}
	if err := s.write(11, tNow+1, a.st.hash(), oldState); err != nil {
		t.Fatal(err)
	}
	snap, err := loadSnapshot(dir, nil)
	if err != nil {
		t.Fatalf("an old-shape snapshot must load: %v", err)
	}
	if b := snap.state.Bids[ev.ID]; b == nil || b.EstablishedEnd != 0 || b.ExtensionOrdinal != 0 {
		t.Fatalf("restored old bid = %+v, want zero lot fields", b)
	}

	// New shape: the fields survive a snapshot write/load.
	a.st.Bids[ev.ID].EstablishedEnd, a.st.Bids[ev.ID].ExtensionOrdinal = tNow+150, 1
	newRaw := a.st.canonical()
	dir = t.TempDir()
	s = &snapshotter{dir: dir, every: 1}
	if err := s.write(11, tNow+1, a.st.hash(), newRaw); err != nil {
		t.Fatal(err)
	}
	snap, err = loadSnapshot(dir, nil)
	if err != nil {
		t.Fatalf("a snapshot with the lot fields must load: %v", err)
	}
	if b := snap.state.Bids[ev.ID]; b == nil || b.EstablishedEnd != tNow+150 || b.ExtensionOrdinal != 1 {
		t.Fatalf("restored bid = %+v, want the committed lot state", b)
	}
}

// --- admission: a refused create must reach a block -----------------------

// refusedCreate is a kind-1440 create the deterministic transition refuses: a
// 200-character title (the handover's measured acceptance case).
func refusedCreate(t *testing.T, a *App) (*Event, []byte) {
	t.Helper()
	seller := newBidder(t)
	ev := createEv(t, seller.sk, "too-long-title", CreateContent{
		Title: strings.Repeat("x", 200), StartPrice: 100, MinIncrement: 10,
		DurationSec: 100, SellerWallet: seller.wallet, Mode: "fixed"}, tNow)
	tx, _ := json.Marshal(ev)
	return ev, tx
}

// 1. CheckTx ADMITS a create the transition refuses (Code 0, not 2): a create
// refused in the mempool never reaches a block, so the chain could never record
// the refusal and a relay-only client would never learn the reason.
func TestCheckTxAdmitsARefusedCreate(t *testing.T) {
	a, _, _ := liveApp(t)
	_, tx := refusedCreate(t, a)
	ck, err := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx})
	if err != nil {
		t.Fatal(err)
	}
	if ck.Code != 0 {
		t.Fatalf("refused create must be admitted for delivery, got code %d log %q", ck.Code, ck.Log)
	}
	if !strings.HasPrefix(ck.Log, "will be decided at delivery: title required (max 120 chars)") {
		t.Fatalf("admission log = %q, want the delivery note carrying the chain's reason", ck.Log)
	}
}

// 2. Delivered through the block path, that create is refused again and the
// refusal is RECORDED: kind 1440, the event id, the block's height, the reason
// verbatim; and no auction is created.
func TestRefusedCreateIsRecordedAtDelivery(t *testing.T) {
	a, _, _ := liveApp(t)
	ev, tx := refusedCreate(t, a)
	res := finalize(t, a, 11, tNow+1, tx)
	if res.TxResults[0].Code != 2 {
		t.Fatalf("delivery must refuse the create, got code %d", res.TxResults[0].Code)
	}
	var r *Rejection
	for i := range a.st.Rejections {
		if a.st.Rejections[i].Event == ev.ID {
			r = &a.st.Rejections[i]
		}
	}
	if r == nil {
		t.Fatal("the refused create must be recorded in State.Rejections")
	}
	if r.Kind != KindCreate || r.Height != 11 || r.Reason != "title required (max 120 chars)" {
		t.Fatalf("rejection = %+v, want the create kind at height 11 with the verbatim reason", *r)
	}
	if r.Pubkey != ev.Pubkey {
		t.Fatalf("rejection pubkey = %q, want %q", r.Pubkey, ev.Pubkey)
	}
	if au := a.st.Auctions[coordOf(ev.Pubkey, "too-long-title")]; au != nil {
		t.Fatalf("a refused create must leave no auction, got %+v", au)
	}
}

// 3. The end-to-end property the admission exists for: one publisher pass over
// that state builds and publishes a kind 1450 naming the event and the verbatim
// reason, anchored at the rejection's height.
func TestRefusedCreatePublishesKind1450(t *testing.T) {
	a, _, _ := liveApp(t)
	ev, tx := refusedCreate(t, a)
	finalize(t, a, 11, tNow+1, tx)

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	pub.Step(context.Background())

	evs := eventsOfKind(rec, KindRefused)
	if len(evs) != 1 {
		t.Fatalf("refusal events = %d, want 1", len(evs))
	}
	c, err := ParseRefusedEvent(evs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.EventID != ev.ID || c.Kind != KindCreate {
		t.Fatalf("refusal names %q/%d, want %q/%d", c.EventID, c.Kind, ev.ID, KindCreate)
	}
	if c.Reason != "title required (max 120 chars)" {
		t.Fatalf("reason = %q, want the chain's verbatim reason", c.Reason)
	}
	if c.Height != 11 || c.BlockHash != testBlockHash(11) {
		t.Fatalf("anchor = %d/%s, want 11/%s", c.Height, c.BlockHash, testBlockHash(11))
	}
}

// 4. The BID branch stays strict: a bid that clears the economy but carries an
// escrow proof from an unpinned keyset is still refused by CheckTx with Code 2
// and the ESCROW reason. applyBid decides the economy first, but a bid it
// passes is not admitted cheaply -- the escrow is verified before CheckTx
// admits it.
func TestCheckTxStillRefusesAnUnverifiedBid(t *testing.T) {
	a, _, coord := liveApp(t)
	au := a.st.Auctions[coord]
	alice := newBidder(t)
	unpinned := []Proof{{ID: "unpinned-keyset", Amount: 1, Secret: "s", C: "c"}}

	// A bid that is BOTH economically late (below the opening price) and
	// escrow-bad is refused by the economy short-circuit, before any escrow
	// work: code stays 2, the reason is the economy's.
	late, _ := json.Marshal(BidContent{Auction: au.ID, Amount: 1, Wallet: alice.wallet, Name: "t", Proofs: unpinned})
	lateEv := signEventTags(t, alice.sk, KindBid, string(late), tNow, [][]string{{"e", au.RootID}, {"a", au.ID}})
	lateTx, _ := json.Marshal(lateEv)
	if ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: lateTx}); ck.Code != 2 || !strings.Contains(ck.Log, "bid must add at least") {
		t.Fatalf("a late, unverified bid must be refused for the economy: %d %q", ck.Code, ck.Log)
	}

	// A bid that CLEARS the economy reaches the escrow check and is refused
	// there, by name: admission never admits an unverified bid.
	b, _ := json.Marshal(BidContent{Auction: au.ID, Amount: au.StartPrice, Wallet: alice.wallet, Name: "t", Proofs: unpinned})
	ev := signEventTags(t, alice.sk, KindBid, string(b), tNow, [][]string{{"e", au.RootID}, {"a", au.ID}})
	tx, _ := json.Marshal(ev)
	ck, err := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx})
	if err != nil {
		t.Fatal(err)
	}
	if ck.Code != 2 {
		t.Fatalf("an unverified bid must stay refused in the mempool, got code %d log %q", ck.Code, ck.Log)
	}
	if !strings.Contains(ck.Log, "proof from unpinned keyset") {
		t.Fatalf("refusal log = %q, want the escrow reason", ck.Log)
	}
}

// 5. An undecodable tx is still refused at admission with Code 1.
func TestCheckTxStillRejectsUndecodableTx(t *testing.T) {
	a, _, _ := liveApp(t)
	ck, err := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: []byte("not a nostr event")})
	if err != nil {
		t.Fatal(err)
	}
	if ck.Code != 1 {
		t.Fatalf("an undecodable tx must be refused with code 1, got %d", ck.Code)
	}
}
