package main

// Timing rule (handover §5): an anti-snipe extension moves the clock only when
// the bid CHANGES THE LEAD. A leader raising their own position must not extend
// the lot, and the locktime floor must be derived from the end the bid actually
// establishes. These tests pin the ordering in applyBid (resolve extends, then
// derive the floor, then check the lock) and the uncapped extension rule.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// bidWithLocktime builds a signed bid whose escrow proofs carry an explicit
// locktime instead of the chain's minLocktime. It lets a test prove the
// admission floor: a lock that clears the OLD end but not the NEW one must be
// refused by name. The outcome templates are the honest ones.
func bidWithLocktime(t *testing.T, a *App, m *testMint, b bidder, auction string, amount, lt int64, nonce string) (*Event, []byte) {
	t.Helper()
	au := a.st.Auctions[auction]
	var proofs []Proof
	left, i := amount, 0
	for d := int64(512); d >= 1; d /= 2 {
		for left >= d {
			proofs = append(proofs, m.sign(t, d, lockForSigAll(a.st.Params.GroupKey, b.wallet, lt, fmt.Sprintf("%s%02d", nonce, i))))
			left -= d
			i++
		}
	}
	c, _ := json.Marshal(BidContent{Auction: au.ID, Amount: amount, Wallet: b.wallet, Name: "t", Proofs: proofs,
		OutcomeTemplates: m.templates(t, b, au.SellerWallet, proofs, nonce)})
	ev := signEventTags(t, b.sk, KindBid, string(c), a.st.Time, [][]string{{"e", au.RootID}, {"a", au.ID}})
	tx, _ := json.Marshal(ev)
	return ev, tx
}

// (a) A leader raising their own position inside the window does NOT extend:
// EndAt and Extensions stay put and the bid carries no extension state. Its
// escrow must still clear the CURRENT end plus the settlement safety.
func TestSelfRaiseInsideWindowDoesNotExtend(t *testing.T) {
	a, m, id := liveApp(t)
	au := a.st.Auctions[id]
	au.Mode, au.WindowSec, au.EndAt = "antisnipe", 60, tNow+40

	alice := newBidder(t)
	a.st.Time = tNow + 20
	_, tx1 := m.bidTx(t, a, alice, id, 100, "aa")
	mustBid(t, a, tx1, 11, tNow+20) // first bid takes the lead: extends to tNow+80
	if au.EndAt != tNow+80 || au.Extensions != 1 {
		t.Fatalf("first lead-taking bid must extend: end %d extensions %d", au.EndAt, au.Extensions)
	}
	end, ext := au.EndAt, au.Extensions

	// The self-raise arrives with 50 s left (< window 60): the old code would
	// have extended here. It must not move the lot.
	ev2, tx2 := m.bidTx(t, a, alice, id, 10, "bb")
	mustBid(t, a, tx2, 12, tNow+30)
	if au.EndAt != end || au.Extensions != ext {
		t.Fatalf("a self-raise inside the window must not extend: end %d/%d, want %d/%d",
			au.EndAt, au.Extensions, end, ext)
	}
	b := a.st.Bids[ev2.ID]
	if b.Extended || b.EstablishedEnd != 0 || b.ExtensionOrdinal != 0 {
		t.Fatalf("a self-raise must carry no extension state: %+v", b)
	}
	floor := end + a.st.Params.SettlementSafetySec + a.st.Params.BidToConsensusLatencySec
	if b.Locktime < floor {
		t.Fatalf("a self-raise must clear the current end + safety + latency: locktime %d < %d", b.Locktime, floor)
	}
}

// (b) A challenger's bid inside the window DOES move the end to now+window and
// increments the extension count; the bid records the end it established.
func TestChallengerInsideWindowExtends(t *testing.T) {
	a, m, id := liveApp(t)
	au := a.st.Auctions[id]
	au.Mode, au.WindowSec, au.EndAt = "antisnipe", 60, tNow+40

	alice := newBidder(t)
	a.st.Time = tNow + 20
	_, tx1 := m.bidTx(t, a, alice, id, 100, "aa")
	mustBid(t, a, tx1, 11, tNow+20) // extends to tNow+80

	bob := newBidder(t)
	a.st.Time = tNow + 30
	ev2, tx2 := m.bidTx(t, a, bob, id, 102, "bb")
	mustBid(t, a, tx2, 12, tNow+30) // 50 s left: challenger extends to tNow+90
	if au.EndAt != tNow+90 || au.Extensions != 2 {
		t.Fatalf("a challenger inside the window must extend: end %d extensions %d", au.EndAt, au.Extensions)
	}
	b := a.st.Bids[ev2.ID]
	if !b.Extended || b.EstablishedEnd != tNow+90 || b.ExtensionOrdinal != 2 {
		t.Fatalf("challenger extension state = %+v, want end %d ordinal 2", b, tNow+90)
	}
}

// (c) A bid whose locktime clears the OLD end but not the NEW one is refused
// at admission: the floor is computed from the end THIS bid establishes, after
// resolving the extension.
func TestExtendingBidMustClearNewEnd(t *testing.T) {
	a, m, id := liveApp(t)
	au := a.st.Auctions[id]
	au.Mode, au.WindowSec, au.EndAt = "antisnipe", 60, tNow+40

	alice := newBidder(t)
	a.st.Time = tNow + 20
	_, tx1 := m.bidTx(t, a, alice, id, 100, "aa")
	mustBid(t, a, tx1, 11, tNow+20) // extends to tNow+80
	oldEnd, oldExt := au.EndAt, au.Extensions

	bob := newBidder(t)
	oldFloor := oldEnd + a.st.Params.SettlementSafetySec + a.st.Params.BidToConsensusLatencySec
	_, tx2 := bidWithLocktime(t, a, m, bob, id, 102, oldFloor, "bb")
	r := finalize(t, a, 12, tNow+30, tx2)
	if r.TxResults[0].Code != 2 || !strings.Contains(r.TxResults[0].Log, "locktime below the floor for this lot") {
		t.Fatalf("a lock clearing only the old end must be refused by name: code %d log %q",
			r.TxResults[0].Code, r.TxResults[0].Log)
	}
	if au.EndAt != oldEnd || au.Extensions != oldExt {
		t.Fatalf("a refused bid must not move the lot: end %d/%d, want %d/%d",
			au.EndAt, au.Extensions, oldEnd, oldExt)
	}
}

// (d) No cap: successive challengers keep extending the lot, each to its own
// arrival time + window. The end follows the last LEAD-CHANGE time, not the
// last bid time.
func TestAntiSnipeUncappedByChallengers(t *testing.T) {
	a, m, id := liveApp(t)
	au := a.st.Auctions[id]
	au.Mode, au.WindowSec, au.EndAt = "antisnipe", 20, tNow+45

	at, amount := tNow+30, int64(100)
	for h := int64(11); h < 16; h++ { // five successive challengers
		a.st.Time = at
		_, tx := m.bidTx(t, a, newBidder(t), id, amount, fmt.Sprintf("e%02d", h))
		mustBid(t, a, tx, h, at)
		if au.EndAt != at+20 {
			t.Fatalf("bid %d with less than the window left must reset the end to now+20: end %d, now %d", h, au.EndAt, at)
		}
		at = au.EndAt - 5
		amount += 10
	}
	if au.Extensions < 3 {
		t.Fatalf("anti-snipe must keep extending by successive challengers, got %d extensions", au.Extensions)
	}
	if au.Status != "open" {
		t.Fatalf("the lot must still be open while bids keep arriving: %+v", au)
	}
}

// The state-walking invariant: for every lot in `closed` state, every leg with
// status `won` has Locktime > ClosedAt + SettlementSafetySec. This is the
// tripwire for a future change that moves an end without moving the floor.
func TestClosedWonLegsClearSettlementSafety(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	a.st.Time = tNow + 1
	_, tx := m.bidTx(t, a, alice, id, 100, "aa")
	mustBid(t, a, tx, 11, tNow+1)
	finalize(t, a, 12, tNow+100) // close at the lot's end

	s := a.st
	closed := 0
	for _, au := range s.Auctions {
		if au.Status != "closed" {
			continue
		}
		closed++
		floor := au.ClosedAt + s.Params.SettlementSafetySec
		for _, bidID := range au.Bids {
			b := s.Bids[bidID]
			if b == nil || b.Status != "won" {
				continue
			}
			if b.Locktime <= floor {
				t.Fatalf("invariant violated: lot %s bid %s locktime %d <= closedAt %d + safety %d = %d",
					au.ID, bidID, b.Locktime, au.ClosedAt, s.Params.SettlementSafetySec, floor)
			}
		}
	}
	if closed == 0 {
		t.Fatal("no closed lot to walk: the invariant test would pass vacuously")
	}
}
