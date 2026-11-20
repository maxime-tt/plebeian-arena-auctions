package main

// Tests for positive create acceptance: Auction.AcceptedH set by the root
// create, the kind-1449 arena.createadmit.v1 fact (build/parse refusals by
// name), and the publisher pass that announces it -- deduped, retried after a
// failed publish, and with the deliberate legacy path for lots already in state
// when AcceptedH first shipped.
//
// The bid-admission tests (rebid_test.go) are the template: a real SignEvent
// under a real key, a roster for the membership check, and re-signed mutated
// copies for every refusal -- never a fake signature.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// createAdmitFixture is valid kind-1449 create-admission content for the
// build/parse tests.
func createAdmitFixture() CreateAdmitContent {
	return CreateAdmitContent{
		Schema:          CreateAdmitSchema,
		AuctionID:       "1440:seller:lot-9",
		Seller:          strings.Repeat("ab", 32),
		RootEventID:     strings.Repeat("cd", 32),
		AcceptedHeight:  12,
		BlockHash:       testBlockHash(12),
		EndTime:         tNow + 150,
		OriginalEndTime: tNow + 100,
		Mode:            "antisnipe",
	}
}

// 1. Build -> parse round-trip: kind, content, tag order, id and signature all
// agree, the anchor tags match the content, and the roster check passes.
func TestCreateAdmitEventRoundTrip(t *testing.T) {
	sk := testVerifierKey(t)
	roster := map[string]ValidatorKey{
		NostrPubkeyHex(sk): {Pubkey: NostrPubkeyHex(sk), Name: "v1", Index: 1},
	}
	ev, err := BuildCreateAdmitEvent(sk, createAdmitFixture(), tNow)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != KindLot || !regularKind(ev.Kind) {
		t.Fatalf("create admission must ride the regular lot kind %d, got %d", KindLot, ev.Kind)
	}
	if err := ev.Verify(); err != nil {
		t.Fatalf("signature/id must verify: %v", err)
	}
	if got := tagOf(t, ev, "e"); got != strings.Repeat("cd", 32) {
		t.Fatalf("e tag = %q, want the root event id", got)
	}
	if got := tagOf(t, ev, "auction"); got != "1440:seller:lot-9" {
		t.Fatalf("auction tag = %q", got)
	}
	if got := tagOf(t, ev, "height"); got != "12" {
		t.Fatalf("height tag = %q, want 12", got)
	}
	if got := tagOf(t, ev, "hash"); got != testBlockHash(12) {
		t.Fatalf("hash tag = %q", got)
	}
	if got := tagOf(t, ev, "p"); got != NostrPubkeyHex(sk) {
		t.Fatalf("p tag = %q, want the validator identity", got)
	}

	c, err := ParseCreateAdmitEvent(ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schema != CreateAdmitSchema || c.AuctionID != "1440:seller:lot-9" || c.Seller != strings.Repeat("ab", 32) ||
		c.RootEventID != strings.Repeat("cd", 32) || c.AcceptedHeight != 12 || c.EndTime != tNow+150 ||
		c.OriginalEndTime != tNow+100 || c.Mode != "antisnipe" {
		t.Fatalf("round trip lost a field: %+v", c)
	}

	if _, err := ParseCreateAdmitEvent(ev, roster); err != nil {
		t.Fatalf("on roster: %v", err)
	}
	offRoster := map[string]ValidatorKey{NostrPubkeyHex(testVerifierKey(t)): {Index: 1}}
	if _, err := ParseCreateAdmitEvent(ev, offRoster); !errors.Is(err, ErrCreateAdmitNotInRoster) {
		t.Fatalf("off roster: want %v, got %v", ErrCreateAdmitNotInRoster, err)
	}

	// The three shapes of kind 1449 are told apart by the schema: the lot and
	// bid-admission parsers refuse a create-admission fact by name.
	lot, err := BuildLotEvent(sk, lotFixture(), tNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCreateAdmitEvent(lot, nil); !errors.Is(err, ErrCreateAdmitBadSchema) {
		t.Fatalf("a lot event: want %v, got %v", ErrCreateAdmitBadSchema, err)
	}
	admit, err := BuildBidAdmitEvent(sk, BidAdmitContent{Schema: BidAdmitSchema, AuctionID: "1440:aa:lot",
		BidID: strings.Repeat("ab", 32), Bidder: strings.Repeat("cd", 32), Amount: 10, Position: 10,
		HighBid: strings.Repeat("ef", 32), Height: 12, BlockHash: testBlockHash(12)}, tNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCreateAdmitEvent(admit, nil); !errors.Is(err, ErrCreateAdmitBadSchema) {
		t.Fatalf("a bid-admission event: want %v, got %v", ErrCreateAdmitBadSchema, err)
	}
}

// 2. The create-admission event's own rules: wrong kind, a bad signature, a
// swapped schema, and a tag/content anchor mismatch are each refused by name.
func TestCreateAdmitEventRefusals(t *testing.T) {
	sk := testVerifierKey(t)
	good := createAdmitFixture()
	ev, err := BuildCreateAdmitEvent(sk, good, tNow)
	if err != nil {
		t.Fatal(err)
	}

	// A wrong kind is not a create admission, whatever its content says.
	body := func(c CreateAdmitContent) string { b, _ := json.Marshal(c); return string(b) }
	wrongKind := signEventTags(t, sk, KindBid, body(good), tNow, [][]string{
		{"e", good.RootEventID}, {"auction", good.AuctionID}, {"height", "12"}, {"hash", good.BlockHash}})
	if _, err := ParseCreateAdmitEvent(wrongKind, nil); !errors.Is(err, ErrCreateAdmitKind) {
		t.Fatalf("wrong kind: want %v, got %v", ErrCreateAdmitKind, err)
	}

	// A tampered signature is refused before any content is trusted.
	bad := *ev
	bad.Sig = strings.Repeat("00", 64)
	if _, err := ParseCreateAdmitEvent(&bad, nil); !errors.Is(err, ErrCreateAdmitBadSignature) {
		t.Fatalf("tampered signature: want %v, got %v", ErrCreateAdmitBadSignature, err)
	}

	// A re-signed schema swap is refused.
	other := good
	other.Schema = BidAdmitSchema
	if _, err := ParseCreateAdmitEvent(resignContent(t, sk, ev, body(other)), nil); !errors.Is(err, ErrCreateAdmitBadSchema) {
		t.Fatalf("schema swap: want %v, got %v", ErrCreateAdmitBadSchema, err)
	}

	// A re-signed anchor swap -- the content anchor moved off the tag anchor --
	// is refused, so a relay that re-framed one cannot hide the other.
	moved := good
	moved.AcceptedHeight = 13
	if _, err := ParseCreateAdmitEvent(resignContent(t, sk, ev, body(moved)), nil); !errors.Is(err, ErrCreateAdmitUnanchored) {
		t.Fatalf("anchor swap: want %v, got %v", ErrCreateAdmitUnanchored, err)
	}

	// The e tag must name the root the content names: re-signed so the ONLY
	// fault is the tag.
	retagged := signEventTags(t, sk, KindLot, ev.Content, tNow, [][]string{
		{"e", strings.Repeat("11", 32)}, {"auction", good.AuctionID},
		{"height", "12"}, {"hash", good.BlockHash}})
	if _, err := ParseCreateAdmitEvent(retagged, nil); !errors.Is(err, ErrCreateAdmitBadSchema) {
		t.Fatalf("e-tag mismatch: want %v, got %v", ErrCreateAdmitBadSchema, err)
	}

	// Field rules, refused by name at BUILD time (so a bad fact cannot be made).
	for name, mutate := range map[string]func(*CreateAdmitContent){
		"no auction":    func(c *CreateAdmitContent) { c.AuctionID = "" },
		"no seller":     func(c *CreateAdmitContent) { c.Seller = "" },
		"no root":       func(c *CreateAdmitContent) { c.RootEventID = "" },
		"bad mode":      func(c *CreateAdmitContent) { c.Mode = "dutch" },
		"no end":        func(c *CreateAdmitContent) { c.EndTime = 0 },
		"no orig end":   func(c *CreateAdmitContent) { c.OriginalEndTime = 0 },
		"unanchored":    func(c *CreateAdmitContent) { c.BlockHash = "" },
		"no height":     func(c *CreateAdmitContent) { c.AcceptedHeight = 0 },
		"empty content": func(c *CreateAdmitContent) { *c = CreateAdmitContent{} },
	} {
		badContent := good
		mutate(&badContent)
		if _, err := BuildCreateAdmitEvent(sk, badContent, tNow); err == nil {
			t.Fatalf("%s: build must refuse", name)
		}
	}
}

// 3. applyCreate records the acceptance height at the ROOT, never at a
// supersession, never for a refusal, and never on a CheckTx preview.
func TestApplyCreateRecordsAcceptedHeight(t *testing.T) {
	s := auctionState()
	seller := newBidder(t)
	root := createEv(t, seller.sk, "lot-acc", baseLot(), tNow)
	if err := s.applyCreate(root, tNow, 7, true); err != nil {
		t.Fatalf("root create: %v", err)
	}
	au := s.canonicalAuction(seller.pub, "lot-acc")
	if au == nil || au.AcceptedH != 7 {
		t.Fatalf("root create must set AcceptedH to the applied height: %+v", au)
	}

	// A supersession moves display fields and CreatedH, but NOT AcceptedH: an
	// already-accepted lot stays accepted at its original height.
	c := baseLot()
	c.Title = "Vintage camera (v2)"
	edit := createEv(t, seller.sk, "lot-acc", c, tNow+10, [2]string{"e", root.ID})
	if err := s.applyCreate(edit, tNow+10, 9, true); err != nil {
		t.Fatalf("supersession: %v", err)
	}
	au = s.canonicalAuction(seller.pub, "lot-acc")
	if au.AcceptedH != 7 {
		t.Fatalf("a supersession must not move AcceptedH: %+v", au)
	}
	if au.CreatedH != 9 {
		t.Fatalf("the supersession must still be the evidence that CreatedH moves: %+v", au)
	}

	// A refused create writes nothing at all: no Auction, hence no acceptance.
	bad := baseLot()
	bad.Title = ""
	badEv := createEv(t, seller.sk, "lot-bad", bad, tNow)
	if err := s.applyCreate(badEv, tNow, 11, true); err == nil {
		t.Fatal("a create without a title must be refused")
	}
	if s.Auctions[coordOf(seller.pub, "lot-bad")] != nil {
		t.Fatal("a refused create must leave no Auction behind")
	}

	// A CheckTx preview (apply == false) predicts but does not write.
	pre := createEv(t, seller.sk, "lot-preview", baseLot(), tNow)
	if err := s.applyCreate(pre, tNow, 11, false); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if s.Auctions[coordOf(seller.pub, "lot-preview")] != nil {
		t.Fatal("a CheckTx preview must not write an Auction")
	}
}

// acceptedLots returns the create-admission facts among the relayed kind-1449
// events, keyed by auction id.
func acceptedLots(rec eventRecorder) map[string]*CreateAdmitContent {
	got := map[string]*CreateAdmitContent{}
	for _, ev := range eventsOfKind(rec, KindLot) {
		c, err := ParseCreateAdmitEvent(ev, nil)
		if err != nil {
			continue
		}
		got[c.AuctionID] = c
	}
	return got
}

// 4. The chain publishes exactly one create-admission fact per accepted lot,
// anchored at the height the root create was committed; a second pass publishes
// nothing new.
func TestCreateAcceptanceFactIsPublished(t *testing.T) {
	a, _, id, seller := liveAppSeller(t) // the lot is applied at height 10
	finalize(t, a, 11, tNow+1)
	au := a.st.Auctions[id]
	if au == nil || au.AcceptedH != 10 {
		t.Fatalf("liveApp's lot must carry AcceptedH 10: %+v", au)
	}

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	ctx := context.Background()
	pub.Step(ctx)

	got := acceptedLots(rec)
	if len(got) != 1 {
		t.Fatalf("create-acceptance events = %d, want one per accepted lot", len(got))
	}
	c := got[id]
	if c == nil {
		t.Fatalf("no acceptance fact for %s: %v", id, got)
	}
	if c.Seller != seller.pub || c.RootEventID != au.RootID {
		t.Fatalf("acceptance names the wrong lot: %+v (want seller %s root %s)", c, seller.pub, au.RootID)
	}
	if c.AcceptedHeight != 10 || c.BlockHash != testBlockHash(10) {
		t.Fatalf("acceptance anchor = %d/%s, want 10/%s", c.AcceptedHeight, c.BlockHash, testBlockHash(10))
	}
	if c.EndTime != au.EndAt || c.OriginalEndTime != au.OriginalEnd() || c.Mode != au.Mode {
		t.Fatalf("acceptance must carry the lot's end and mode: %+v vs %+v", c, au)
	}
	if countRecords(pub, "create_admit") != 1 {
		t.Fatalf("create_admit records = %d, want 1", countRecords(pub, "create_admit"))
	}

	// Idempotent: a second pass publishes nothing new.
	pub.Step(ctx)
	if n := len(acceptedLots(rec)); n != 1 {
		t.Fatalf("create-acceptance events after two passes = %d, want 1", n)
	}
	if n := countRecords(pub, "create_admit"); n != 1 {
		t.Fatalf("create_admit records after two passes = %d, want 1", n)
	}
}

// failFirstAdmitRelay fails the first create-admission publish it sees, then
// behaves like a recordingRelay -- so a test can prove the dedupe marks only
// after a SUCCESSFUL publish.
type failFirstAdmitRelay struct {
	inner *recordingRelay
	fails int
}

func (r *failFirstAdmitRelay) Publish(ctx context.Context, ev *Event) error {
	if r.fails > 0 {
		if _, err := ParseCreateAdmitEvent(ev, nil); err == nil {
			r.fails--
			return errors.New("relay refused the create acceptance")
		}
	}
	return r.inner.Publish(ctx, ev)
}

func (r *failFirstAdmitRelay) all() []*Event { return r.inner.all() }

// 5. A relay failure on the create-admission publish does not mark it done: the
// next pass retries it, and it is the ONLY create-admission fact on the relay.
func TestCreateAcceptanceRetriesAfterAFailedPublish(t *testing.T) {
	a, _, id := liveApp(t)
	finalize(t, a, 11, tNow+1)

	rec := &failFirstAdmitRelay{inner: &recordingRelay{}, fails: 1}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	ctx := context.Background()

	// lastAdmit is the most recent create_admit record (the pass publishes
	// other ops after it, so "the last record" is not it).
	lastAdmit := func() *PublishRecord {
		recs := pub.Records()
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].Op == "create_admit" {
				return &recs[i]
			}
		}
		return nil
	}

	pub.Step(ctx)
	if n := len(acceptedLots(rec.inner)); n != 0 {
		t.Fatalf("the failed publish must not reach the relay, got %d events", n)
	}
	if n := countRecords(pub, "create_admit"); n != 1 {
		t.Fatalf("the attempt must be recorded once, got %d", n)
	}
	if first := lastAdmit(); first == nil || first.Error == "" {
		t.Fatalf("the failed attempt must carry its error, got %+v", first)
	}

	pub.Step(ctx)
	got := acceptedLots(rec.inner)
	if len(got) != 1 || got[id] == nil {
		t.Fatalf("the retry must publish exactly one acceptance for %s: %v", id, got)
	}
	if last := lastAdmit(); last == nil || last.Error != "" {
		t.Fatalf("the retry must succeed, got %+v", last)
	}
}

// 6. A refused create is not accepted: the chain publishes no create-admission
// fact for it, while the lot it DID accept still gets one.
func TestNoAcceptanceForARefusedCreate(t *testing.T) {
	a, _, id, seller := liveAppSeller(t)
	bad := baseLot()
	bad.Title = ""
	badEv := createEv(t, seller.sk, "lot-refused", bad, tNow)
	badTx, _ := json.Marshal(badEv)
	res := finalize(t, a, 11, tNow+1, badTx)
	if res.TxResults[0].Code == 0 {
		t.Fatalf("a create without a title must be refused, got %d", res.TxResults[0].Code)
	}

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	pub.Step(context.Background())

	got := acceptedLots(rec)
	if len(got) != 1 || got[id] == nil {
		t.Fatalf("only the accepted lot may be announced, got %v", got)
	}
}

// 7. The legacy path: a lot already in state when AcceptedH shipped has
// AcceptedH == 0, and the publisher anchors its acceptance at the lot's current
// CreatedH instead of orphaning it.
func TestLegacyLotAcceptanceAnchorsAtCreatedH(t *testing.T) {
	a, _, id := liveApp(t)
	// Simulate a lot that predates AcceptedH: zeroed before the block commits.
	a.st.Auctions[id].AcceptedH = 0
	finalize(t, a, 11, tNow+1)

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	pub.Step(context.Background())

	c := acceptedLots(rec)[id]
	if c == nil {
		t.Fatal("a legacy lot must still be announced, not orphaned")
	}
	if c.AcceptedHeight != 10 || c.BlockHash != testBlockHash(10) {
		t.Fatalf("legacy anchor = %d/%s, want its CreatedH 10/%s", c.AcceptedHeight, c.BlockHash, testBlockHash(10))
	}
}
