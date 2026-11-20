package main

// The additive rebid chain (R1-A, R2-A, R3, R4).
//
// A bid's Amount is a DELTA added to the bidder's POSITION; the floor compares
// the resulting position against top + the raise the top demands (2% of the
// top, rounded up, at least 1 sat); a leader raising their own position must
// also cover that raise (the old R2-A exemption is gone); the lead changing
// hands outbids EVERY leg of the defeated chain; close promotes every leading
// leg of the winner; settlement walks every won leg (one SIG_ALL swap per
// leg); a bid that is economically late on arrival is REFUSED at admission
// (R3) and never recorded; and the chain publishes the per-bid ADMISSION fact
// with its resulting position (R4).
//
// The invariant these tests pin, in the chain's own words (ADR-0003 S4.9):
// after any accepted bid, HighAmount == Σ { bid.Amount : bid.Status ==
// "leading" }, and after close HighAmount == Σ { bid.Amount : bid.Status ==
// "won" }.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
)

// bidSum sums the amounts of the lot's accepted bids carrying the status.
func bidSum(s *State, id, status string) int64 {
	var sum int64
	for _, bid := range s.Auctions[id].Bids {
		if b := s.Bids[bid]; b != nil && b.Status == status {
			sum += b.Amount
		}
	}
	return sum
}

// mustBid commits one bid at height h and fails unless it is ACCEPTED.
func mustBid(t *testing.T, a *App, tx []byte, h, at int64) {
	t.Helper()
	if r := finalize(t, a, h, at, tx).TxResults[0]; r.Code != 0 {
		t.Fatalf("bid refused at height %d: %s", h, r.Log)
	}
}

// --- a leader may raise, but must cover the 2% raise ----------------------

// 1. The leader's own rebid is legal -- both legs stay leading and the position
// is the SUM of the deltas -- but the rebid must cover the 2% raise, which on a
// top of 500 is exactly the 10 sats bid here.
func TestLeaderRebidIsAccepted(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 500, "aa")
	ev2, tx2 := m.bidTx(t, a, alice, id, 10, "bb")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)

	au := a.st.Auctions[id]
	if au.HighBid != ev2.ID || au.HighAmount != 510 {
		t.Fatalf("high = %s/%d, want the newest leg %s and position 510", au.HighBid, au.HighAmount, ev2.ID)
	}
	if a.st.Bids[ev1.ID].Status != "leading" || a.st.Bids[ev2.ID].Status != "leading" {
		t.Fatalf("both legs of the leading chain must stay leading: %q/%q",
			a.st.Bids[ev1.ID].Status, a.st.Bids[ev2.ID].Status)
	}
	if a.st.Bids[ev2.ID].Amount != 10 {
		t.Fatalf("the leg's amount is its own delta: got %d, want 10", a.st.Bids[ev2.ID].Amount)
	}
	if got := bidSum(a.st, id, "leading"); got != au.HighAmount {
		t.Fatalf("HighAmount %d != Σ leading legs %d", au.HighAmount, got)
	}
	if due := a.st.dueMoves(tNow + 3); len(due) != 0 {
		t.Fatalf("no leg of the leading chain may be refundable: %+v", due)
	}
}

// 2. The minimum raise is now 2% of the top, not the ladder's static Next: on
// a top of 500 the raise is 10 sats, so a 1-sat SELF-raise is REFUSED (the old
// R2-A leader exemption is gone) while a raise of exactly 10 is accepted.
func TestLeaderRebidOfOneSatIsRefused(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	_, tx1 := m.bidTx(t, a, alice, id, 500, "aa")
	mustBid(t, a, tx1, 11, tNow+1)

	_, txShort := m.bidTx(t, a, alice, id, 1, "bb")
	ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: txShort})
	if ck.Code != 2 || !strings.Contains(ck.Log, "bid must add at least 10 sats") {
		t.Fatalf("a 1-sat self-raise must be refused (the raise is 2%% of the top, 10 sats): %d %q", ck.Code, ck.Log)
	}

	_, txRaise := m.bidTx(t, a, alice, id, 10, "cc")
	mustBid(t, a, txRaise, 13, tNow+3)
	if au := a.st.Auctions[id]; au.HighAmount != 510 {
		t.Fatalf("position = %d, want 510", au.HighAmount)
	}
}

// 3. HighAmount is the POSITION on the wire (the client view), not the last
// delta: 300 then 200 leaves 500, not 200.
func TestHighAmountIsPositionNotDelta(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 300, "aa")
	ev2, tx2 := m.bidTx(t, a, alice, id, 200, "bb")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)
	if a.st.Auctions[id].HighAmount != 500 {
		t.Fatalf("chain HighAmount = %d, want 500", a.st.Auctions[id].HighAmount)
	}
	v := clientView(a.st, nil, 1)
	if v.Auctions[id].HighAmount != 500 || v.Auctions[id].HighBid != ev2.ID {
		t.Fatalf("client view high = %d/%s, want 500/%s", v.Auctions[id].HighAmount, v.Auctions[id].HighBid, ev2.ID)
	}
	if v.Bids[ev1.ID].Amount != 300 || v.Bids[ev2.ID].Amount != 200 {
		t.Fatalf("the view must carry each leg's own delta: %+v %+v", v.Bids[ev1.ID], v.Bids[ev2.ID])
	}
}

// 4. A challenger's position floor is the top plus the 2% raise: on a top of
// 1,000 that is 20, so a delta of 1,020 is admitted, 1,019 is refused, and the
// position is the delta, not a total.
func TestPositionFloorAdmitsReachingDelta(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 1000, "aa")
	mustBid(t, a, tx1, 11, tNow+1) // top 1,000: the raise is 20, the floor 1,020

	_, txShort := m.bidTx(t, a, bob, id, 1019, "bb")
	ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: txShort})
	if ck.Code != 2 || !strings.Contains(ck.Log, "bid must add at least 1020 sats") {
		t.Fatalf("a challenger's 1,019 must be refused (the floor is 1,000 + 20): %d %q", ck.Code, ck.Log)
	}
	ev2, tx2 := m.bidTx(t, a, bob, id, 1020, "cc")
	mustBid(t, a, tx2, 12, tNow+2)

	au := a.st.Auctions[id]
	if au.HighBid != ev2.ID || au.HighAmount != 1020 {
		t.Fatalf("high = %s/%d, want %s/1020", au.HighBid, au.HighAmount, ev2.ID)
	}
	if a.st.Bids[ev2.ID].Status != "leading" || a.st.Bids[ev1.ID].Status != "outbid" {
		t.Fatalf("the challenger leads and the defeated leg is outbid: %q/%q",
			a.st.Bids[ev2.ID].Status, a.st.Bids[ev1.ID].Status)
	}
	if got := bidSum(a.st, id, "leading"); got != 1020 {
		t.Fatalf("Σ leading legs = %d, want just the challenger's 1020", got)
	}
}

// 4b. The raise is 2% of the top, ROUNDED UP: for a small top and a large top,
// inc-1 sats is refused (naming the required delta, the position it would
// reach, and the floor) and exactly inc sats is accepted.
func TestRaiseIsTwoPercentOfTopRoundedUp(t *testing.T) {
	cases := []struct {
		name string
		top  int64
		inc  int64 // ceil(top * 2 / 100)
	}{
		{"small top rounds up", 101, 3}, // 2.02% -> 3
		{"large top", 10_000, 200},      // exactly 2%
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, m, id := liveApp(t)
			alice := newBidder(t)
			_, tx := m.bidTx(t, a, alice, id, tc.top, "aa")
			mustBid(t, a, tx, 11, tNow+1)

			_, short := m.bidTx(t, a, alice, id, tc.inc-1, "bb")
			ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: short})
			if ck.Code != 2 {
				t.Fatalf("a raise of %d (one below the %d raise) must be refused, got code %d (%q)",
					tc.inc-1, tc.inc, ck.Code, ck.Log)
			}
			want := fmt.Sprintf("bid must add at least %d sats: your position would reach %d, the floor is %d",
				tc.inc, tc.top+tc.inc-1, tc.top+tc.inc)
			if ck.Log != want {
				t.Fatalf("refusal = %q, want %q", ck.Log, want)
			}

			_, exact := m.bidTx(t, a, alice, id, tc.inc, "cc")
			mustBid(t, a, exact, 12, tNow+2)
			if au := a.st.Auctions[id]; au.HighAmount != tc.top+tc.inc {
				t.Fatalf("position = %d, want %d", au.HighAmount, tc.top+tc.inc)
			}
		})
	}
}

// --- the lead changing hands ----------------------------------------------

// 5. A challenger's win outbids EVERY leg of the defeated chain, and every
// leg's escrow is due for its own refund.
func TestLeadChangeOutbidsEveryLeg(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 300, "aa")
	ev2, tx2 := m.bidTx(t, a, alice, id, 200, "bb")
	ev3, tx3 := m.bidTx(t, a, bob, id, 510, "cc")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)
	mustBid(t, a, tx3, 13, tNow+3)

	au := a.st.Auctions[id]
	if au.HighAmount != 510 || au.HighBid != ev3.ID {
		t.Fatalf("high = %d/%s, want 510/%s", au.HighAmount, au.HighBid, ev3.ID)
	}
	if a.st.Bids[ev1.ID].Status != "outbid" || a.st.Bids[ev2.ID].Status != "outbid" {
		t.Fatalf("every leg of the dethroned chain is outbid: %q/%q",
			a.st.Bids[ev1.ID].Status, a.st.Bids[ev2.ID].Status)
	}
	due := a.st.dueMoves(tNow + 4)
	refunds := 0
	for _, mv := range due {
		if mv.Outcome == "refund" && mv.To == alice.wallet {
			refunds++
		}
	}
	if refunds != 2 {
		t.Fatalf("both defeated legs must be refundable to Alice: %+v", due)
	}
}

// 6. A leader's raise keeps the whole chain leading (no refund is due for
// either leg), and a later challenger outbids BOTH of them.
func TestLeaderRaiseKeepsExistingLegsLeading(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 500, "aa")
	ev2, tx2 := m.bidTx(t, a, alice, id, 10, "bb")
	_, tx3 := m.bidTx(t, a, bob, id, 521, "cc") // top 510 + the 11-sat raise (2% of 510, rounded up)
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)
	if due := a.st.dueMoves(tNow + 3); len(due) != 0 {
		t.Fatalf("a raise must not make any leg refundable: %+v", due)
	}
	mustBid(t, a, tx3, 13, tNow+3)
	for _, ev := range []string{ev1.ID, ev2.ID} {
		if a.st.Bids[ev].Status != "outbid" {
			t.Fatalf("both raised legs must be outbid by the challenger: %s is %q", ev, a.st.Bids[ev].Status)
		}
	}
}

// --- close ----------------------------------------------------------------

// 7. Every leading leg of the winner becomes won; none becomes lost, and
// Winner is the leg that set the position.
func TestCloseMarksEveryWinningLegWon(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 300, "aa")
	ev2, tx2 := m.bidTx(t, a, alice, id, 200, "bb")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)
	finalize(t, a, 13, tNow+101) // the closing block

	au := a.st.Auctions[id]
	if au.Status != "closed" || au.Winner != au.HighBid || au.Winner != ev2.ID {
		t.Fatalf("winner = %s/%s, want HighBid %s", au.Winner, au.HighBid, ev2.ID)
	}
	if a.st.Bids[ev1.ID].Status != "won" || a.st.Bids[ev2.ID].Status != "won" {
		t.Fatalf("every leading leg must be won: %q/%q",
			a.st.Bids[ev1.ID].Status, a.st.Bids[ev2.ID].Status)
	}
	if au.HighAmount != 500 || bidSum(a.st, id, "won") != 500 {
		t.Fatalf("HighAmount %d != Σ won legs %d (want 500)", au.HighAmount, bidSum(a.st, id, "won"))
	}
	if lost := bidSum(a.st, id, "lost"); lost != 0 {
		t.Fatalf("no leg of the winner may be lost: %d", lost)
	}
}

// 8. A REFUNDED leg is never promoted: close keys on the status, not the
// bidder, so a bidder's earlier outbid legs stay lost.
func TestCloseDoesNotPromoteRefundedLegs(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	evA, txA := m.bidTx(t, a, alice, id, 500, "aa")
	evB, txB := m.bidTx(t, a, bob, id, 520, "bb")
	mustBid(t, a, txA, 11, tNow+1)
	mustBid(t, a, txB, 12, tNow+2)
	// Alice's 500 is outbid and is refunded before she re-enters below the floor
	// (Bob's 520 demands a 2% raise of ceil(520*2/100) = 11, so the floor is 531).
	refund := mintMove(t, a, m, evA.ID, "refund", alice.wallet, tNow+3)
	finalize(t, a, 13, tNow+3, makeSettle(refund))
	evLate, txLate := m.bidTx(t, a, alice, id, 30, "cc")
	res := finalize(t, a, 14, tNow+4, txLate)
	if !strings.HasPrefix(res.TxResults[0].Log, "void: bid must add at least 531") {
		t.Fatalf("the below-floor re-entry must be void at delivery: %q", res.TxResults[0].Log)
	}
	finalize(t, a, 15, tNow+101) // close

	au := a.st.Auctions[id]
	if a.st.Bids[evA.ID].Status != "lost" || a.st.Bids[evA.ID].Settled != "refund" {
		t.Fatalf("the refunded leg must stay lost: %+v", a.st.Bids[evA.ID])
	}
	if a.st.Bids[evB.ID].Status != "won" || a.st.Bids[evLate.ID].Status != "void" {
		t.Fatalf("only the leading leg wins; the void leg stays void: %q/%q",
			a.st.Bids[evB.ID].Status, a.st.Bids[evLate.ID].Status)
	}
	if bidSum(a.st, id, "won") != 520 || au.HighAmount != 520 {
		t.Fatalf("won %d, want exactly Bob's 520 (HighAmount %d)", bidSum(a.st, id, "won"), au.HighAmount)
	}
}

// --- settlement over every leg --------------------------------------------

// 9. A multi-leg winner settles EVERY leg: one SIG_ALL swap per leg, each into
// its own committed template, and the paid total is the position. A lost leg
// never pays, and no leg pays twice.
func TestMultiLegWinnerSettlesEveryLeg(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	_, tx1 := m.bidTx(t, a, alice, id, 300, "a1")
	_, tx2 := m.bidTx(t, a, alice, id, 200, "a2")
	evB, txB := m.bidTx(t, a, bob, id, 510, "bb")
	ev1, tx3 := m.bidTx(t, a, alice, id, 521, "a3") // 510 + the 11-sat raise (2% of 510, rounded up)
	ev2, tx4 := m.bidTx(t, a, alice, id, 20, "a4")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)
	mustBid(t, a, txB, 13, tNow+3)
	mustBid(t, a, tx3, 14, tNow+4)
	mustBid(t, a, tx4, 15, tNow+5)
	finalize(t, a, 16, tNow+101) // close

	au := a.st.Auctions[id]
	if au.HighAmount != 541 || bidSum(a.st, id, "won") != 541 {
		t.Fatalf("the winning position is 521+20=541: got HighAmount %d, Σ won %d",
			au.HighAmount, bidSum(a.st, id, "won"))
	}
	if a.st.Bids[evB.ID].Status != "lost" {
		t.Fatalf("the defeated challenger must be lost, got %q", a.st.Bids[evB.ID].Status)
	}

	due := a.st.dueMoves(tNow + 102)
	var settles int
	for _, mv := range due {
		if mv.Outcome == "settle" {
			settles++
		}
	}
	if settles != 2 {
		t.Fatalf("both won legs must be due to settle, got %d: %+v", settles, due)
	}

	var total int64
	for i, evID := range []string{ev1.ID, ev2.ID} {
		st := mintMove(t, a, m, evID, "settle", au.SellerWallet, tNow+102)
		for _, p := range st.Proofs {
			total += p.Amount
		}
		if r := finalize(t, a, 17+int64(i), tNow+102+int64(i), makeSettle(st)); r.TxResults[0].Code != 0 {
			t.Fatalf("settle of %s: %s", evID, r.TxResults[0].Log)
		}
		b := a.st.Bids[evID]
		if b.Settled != "settle" || len(b.Out) == 0 {
			t.Fatalf("leg %s not settled: %+v", evID, b)
		}
		again := mintMove(t, a, m, evID, "settle", au.SellerWallet, tNow+102)
		if r := finalize(t, a, 20+int64(i), tNow+110, makeSettle(again)); r.TxResults[0].Code == 0 {
			t.Fatalf("a settle must not apply twice for %s", evID)
		}
	}
	if total != au.HighAmount {
		t.Fatalf("Σ settled leg amounts = %d, want the position %d", total, au.HighAmount)
	}
	// A LOST leg never pays.
	lost := mintMove(t, a, m, evB.ID, "settle", au.SellerWallet, tNow+103)
	if ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: makeSettle(lost)}); ck.Code == 0 {
		t.Fatal("a lost leg must never settle")
	}
	// Nothing is left to SETTLE; the defeated legs are refundable, not payable.
	for _, mv := range a.st.dueMoves(tNow + 120) {
		if mv.Outcome == "settle" {
			t.Fatalf("every won leg is settled; still due: %+v", mv)
		}
	}
}

// 10. settle is due only for a WON leg: an open lot's leading bid, an outbid
// bid and a void bid all refuse it.
func TestSettleRequiresWonStatus(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	evLead, tx1 := m.bidTx(t, a, alice, id, 200, "aa")
	evOut, tx2 := m.bidTx(t, a, bob, id, 210, "bb")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)

	if _, err := a.st.outcomeDue(a.st.Bids[evLead.ID], "settle", tNow+3, true); err == nil {
		t.Fatal("an open lot's leading bid must not be settle-able")
	}
	if _, err := a.st.outcomeDue(a.st.Bids[evOut.ID], "settle", tNow+3, true); err == nil {
		t.Fatal("a leading bid of an open lot must not be settle-able")
	}
	finalize(t, a, 13, tNow+101) // close
	if _, err := a.st.outcomeDue(a.st.Bids[evLead.ID], "settle", tNow+102, true); err == nil {
		t.Fatal("an outbid leg must not settle after close")
	}
	if _, code, _ := releasePolicy(a, SignReq{Bid: evLead.ID, Outcome: "settle"}); code == 200 {
		t.Fatal("the signing policy must refuse settling an outbid leg")
	}
	if _, code, err := releasePolicy(a, SignReq{Bid: evOut.ID, Outcome: "settle"}); code != 200 {
		t.Fatalf("the winning leg must be signable: %d %v", code, err)
	}
	// A void leg refuses settle too (only refund).
	evVoid, txV := m.bidTx(t, a, bob, id, 30, "cc")
	_ = finalize(t, a, 14, tNow+103, txV)
	if a.st.Bids[evVoid.ID].Status != "void" {
		t.Fatalf("setup: the bid must be void, got %q", a.st.Bids[evVoid.ID].Status)
	}
	if _, err := a.st.outcomeDue(a.st.Bids[evVoid.ID], "settle", tNow+104, true); err == nil {
		t.Fatal("a void leg must not settle")
	}
	if _, code, err := releasePolicy(a, SignReq{Bid: evVoid.ID, Outcome: "refund"}); code != 200 {
		t.Fatalf("a void leg must still be refundable: %d %v", code, err)
	}
}

// 11. Partial settlement is a visible state: one leg paid, the other still due,
// and the lot's paid total is a strict fraction of the position.
func TestPartialChainSettlementIsVisible(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 300, "aa")
	ev2, tx2 := m.bidTx(t, a, alice, id, 200, "bb")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)
	finalize(t, a, 13, tNow+101) // close

	out := tNow + 102
	if r := finalize(t, a, 14, out, makeSettle(mintMove(t, a, m, ev1.ID, "settle", a.st.Auctions[id].SellerWallet, out))); r.TxResults[0].Code != 0 {
		t.Fatalf("leg 1: %s", r.TxResults[0].Log)
	}
	due := a.st.dueMoves(out + 1)
	if len(due) != 1 || due[0].Bid.ID != ev2.ID || due[0].Outcome != "settle" {
		t.Fatalf("leg 2 must still be due: %+v", due)
	}
	var paid int64
	for _, bid := range a.st.Auctions[id].Bids {
		if b := a.st.Bids[bid]; b.Settled == "settle" {
			paid += b.Amount
		}
	}
	if paid != 300 || a.st.Auctions[id].HighAmount != 500 {
		t.Fatalf("paid %d of %d, want 300 of 500", paid, a.st.Auctions[id].HighAmount)
	}
}

// --- V4V per leg ----------------------------------------------------------

// 12. With a committed split, each leg's settle pays the split of ITS OWN
// amount; the seller takes each leg's remainder; the line items sum to the
// position; and a delta the split cannot represent is REFUSED at admission.
func TestV4VMultiLegSettlementSumsToPosition(t *testing.T) {
	rows := []SplitRow{splitRow("v4v", realPub(t, 31), 2500)} // 25% to one recipient
	a, m, id, _ := v4vCreateApp(t, rows)
	recipient := a.st.Auctions[id].V4VSplit[0].RecipientPubkey
	alice := newBidder(t)
	ev1, _ := v4vBid(t, a, m, alice, id, 300, "aa", nil, nil)
	ev2, _ := v4vBid(t, a, m, alice, id, 200, "bb", nil, nil)
	// Every split line item of every leg must be a keys the fake mint signs.
	for _, amt := range []int64{75, 225, 50, 150} {
		a.st.Params.Keys[fmt.Sprint(amt)] = hex.EncodeToString(m.A.SerializeCompressed())
	}
	if err := a.st.applyBid(ev1, tNow, 11, true, true); err != nil {
		t.Fatalf("leg 1 refused: %v", err)
	}
	if err := a.st.applyBid(ev2, tNow, 12, true, true); err != nil {
		t.Fatalf("leg 2 refused: %v", err)
	}
	if a.st.Auctions[id].HighAmount != 500 {
		t.Fatalf("position = %d, want 500", a.st.Auctions[id].HighAmount)
	}
	finalize(t, a, 13, tNow+101) // close: both legs won
	if bidSum(a.st, id, "won") != 500 {
		t.Fatalf("Σ won legs = %d, want 500", bidSum(a.st, id, "won"))
	}

	var paid, toRecipient int64
	for i, evID := range []string{ev1.ID, ev2.ID} {
		st := mintMove(t, a, m, evID, "settle", a.st.Auctions[id].SellerWallet, tNow+102)
		if r := finalize(t, a, 14+int64(i), tNow+102, makeSettle(st)); r.TxResults[0].Code != 0 {
			t.Fatalf("settle of leg %d: %s", i+1, r.TxResults[0].Log)
		}
		for _, p := range st.Proofs {
			paid += p.Amount
			if recipientMatchesForTest(p, recipient) {
				toRecipient += p.Amount
			}
		}
	}
	if paid != 500 {
		t.Fatalf("Σ settled line items = %d, want the position 500", paid)
	}
	// floor(300*2500/10000) + floor(200*2500/10000) = 75 + 50 = 125, which is
	// floor(500*2500/10000): the per-leg rounding costs nothing here.
	if toRecipient != 125 {
		t.Fatalf("the recipient received %d, want 75+50=125", toRecipient)
	}

	// A delta below the split's representability floor is REFUSED AT ADMISSION
	// (not void).  The raise rule is a Params term now, so pin it to its 1-sat
	// minimum (1 bp of the top) and the 3-sat leg clears the floor; but
	// floor(3*2500/10000) = 0, so the committed split cannot represent it.
	a.st.Params.MinIncrementBps = 1
	ev3, _ := v4vBid(t, a, m, alice, id, 3, "cc", nil, nil) // floor(3*0.25) = 0
	err := a.st.applyBid(ev3, tNow, 18, true, true)
	var late *lateBid
	if err == nil || errors.As(err, &late) {
		t.Fatalf("an unrepresentable delta must be refused outright, got %v", err)
	}
	if !strings.Contains(err.Error(), "rounds to 0 sats") {
		t.Fatalf("refusal must name the representability rule, got %v", err)
	}
}

// recipientMatchesForTest reports whether a proof's secret is P2PK-locked to
// key, so a test can attribute a settled line item.
func recipientMatchesForTest(pr Proof, key string) bool {
	l, err := parseP2PK(pr.Secret)
	if err != nil {
		return false
	}
	rx, err1 := xOnlyOf(l.Data)
	kx, err2 := xOnlyOf(key)
	return err1 == nil && err2 == nil && rx == kx
}

// --- the invariant --------------------------------------------------------

// 13. HighAmount is monotone and always equals the sum of the live legs.
func TestHighAmountMonotoneAndInvariant(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	seq := []struct {
		b      bidder
		amount int64
		nonce  string
	}{
		{alice, 300, "a1"}, {alice, 100, "a2"}, {bob, 410, "b1"},
		{bob, 10, "b2"}, {alice, 430, "a3"},
	}
	last := int64(0)
	for i, s := range seq {
		_, tx := m.bidTx(t, a, s.b, id, s.amount, s.nonce)
		mustBid(t, a, tx, int64(11+i), tNow+int64(1+i))
		au := a.st.Auctions[id]
		if au.HighAmount < last {
			t.Fatalf("step %d: HighAmount fell from %d to %d", i, last, au.HighAmount)
		}
		if got := bidSum(a.st, id, "leading"); got != au.HighAmount {
			t.Fatalf("step %d: HighAmount %d != Σ leading %d", i, au.HighAmount, got)
		}
		last = au.HighAmount
	}
	if last != 430 {
		t.Fatalf("final position = %d, want 430", last)
	}
	finalize(t, a, 20, tNow+101) // close
	au := a.st.Auctions[id]
	if won := bidSum(a.st, id, "won"); won != au.HighAmount || won != 430 {
		t.Fatalf("after close HighAmount %d != Σ won %d (want 430)", au.HighAmount, won)
	}
}

// 14. The overflow guard refuses by name instead of wrapping.
func TestOverflowPositionRefused(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 100, "aa")
	mustBid(t, a, tx1, 11, tNow+1)
	a.st.Auctions[id].HighAmount = math.MaxInt64 - 1 // the leader's position, one sat from the ceiling
	ev2, tx2 := m.bidTx(t, a, alice, id, 2, "bb")

	err := a.st.applyBid(ev2, tNow, 12, true, true)
	var late *lateBid
	if err == nil || errors.As(err, &late) || err.Error() != "position overflows" {
		t.Fatalf("want a named overflow refusal, got %v", err)
	}
	if a.st.Bids[ev2.ID] != nil {
		t.Fatal("a refused bid must leave no trace")
	}
	ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx2})
	if ck.Code != 2 || ck.Log != "position overflows" {
		t.Fatalf("CheckTx: %d %q", ck.Code, ck.Log)
	}
	_ = ev1
}

// 15. A state with a multi-leg chain round-trips through a snapshot
// byte-identically, so a restart keeps the same app hash.
func TestSnapshotRoundTripAfterRebid(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	_, tx1 := m.bidTx(t, a, alice, id, 300, "aa")
	_, tx2 := m.bidTx(t, a, alice, id, 200, "bb")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)
	finalize(t, a, 13, tNow+101) // close: two won legs

	raw := a.st.canonical()
	dir := t.TempDir()
	s := &snapshotter{dir: dir, every: 1}
	if err := s.write(13, tNow+101, a.st.hash(), raw); err != nil {
		t.Fatal(err)
	}
	snap, err := loadSnapshot(dir, nil)
	if err != nil || snap == nil {
		t.Fatalf("load: %v %v", snap, err)
	}
	if !bytes.Equal(snap.state.canonical(), raw) {
		t.Fatal("the state must re-encode byte-identically")
	}
	au := snap.state.Auctions[id]
	if au.HighAmount != 500 || bidSum(snap.state, id, "won") != 500 {
		t.Fatalf("restored chain = %d/%d, want 500", au.HighAmount, bidSum(snap.state, id, "won"))
	}
	leading := 0
	for _, bid := range au.Bids {
		if snap.state.Bids[bid].Status == "won" {
			leading++
		}
	}
	if leading != 2 {
		t.Fatalf("restored won legs = %d, want 2", leading)
	}
}

// --- R3: the floor is a refusal at admission ------------------------------

// 16. A below-floor bid is REFUSED at CheckTx: never committed, no state entry
// (bid, used proofs, void list), and the escrow is untouched. A delta that
// reaches the floor is admitted.
func TestBelowFloorBidIsRefusedAtAdmission(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 200, "aa")
	mustBid(t, a, tx1, 11, tNow+1) // Alice leads with 200: the 2% raise is 4, so a challenger's floor is 204
	usedBefore := len(a.st.UsedY)

	ev2, tx2 := m.bidTx(t, a, bob, id, 150, "bb")
	ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx2})
	if ck.Code != 2 {
		t.Fatalf("a below-floor bid must be REFUSED at admission, got code %d (%q)", ck.Code, ck.Log)
	}
	if !strings.HasPrefix(ck.Log, "bid must add at least 204") ||
		!strings.Contains(ck.Log, "the floor is 204") {
		t.Fatalf("the refusal must state the required delta and the floor: %q", ck.Log)
	}

	// No state entry anywhere: the bid, its escrow's proofs, and the void list.
	if a.st.Bids[ev2.ID] != nil {
		t.Fatal("a refused bid must not be recorded")
	}
	if len(a.st.UsedY) != usedBefore {
		t.Fatalf("a refused bid must not consume its proofs (%d used before, %d now)", usedBefore, len(a.st.UsedY))
	}
	var bc BidContent
	if err := json.Unmarshal([]byte(ev2.Content), &bc); err != nil {
		t.Fatal(err)
	}
	for _, p := range bc.Proofs {
		y, _ := proofY(p.Secret)
		if a.st.UsedY[y] != "" {
			t.Fatal("the refused escrow must be untouched, so the bidder can still spend it")
		}
	}
	au := a.st.Auctions[id]
	if len(au.Voids) != 0 || len(au.Bids) != 1 || au.HighBid != ev1.ID || au.HighAmount != 200 {
		t.Fatalf("the lot must be untouched: voids=%v bids=%v high=%s/%d", au.Voids, au.Bids, au.HighBid, au.HighAmount)
	}
	// The same tx is refused again, still leaving nothing behind.
	if ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx2}); ck.Code != 2 {
		t.Fatalf("the refusal must be repeatable, got %d", ck.Code)
	}

	// A delta that reaches the floor IS admitted, and the challenger takes the
	// lead with the position that delta creates.
	ev3, tx3 := m.bidTx(t, a, bob, id, 204, "cc")
	if ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx3}); ck.Code != 0 {
		t.Fatalf("a reaching delta must be admitted: %d %q", ck.Code, ck.Log)
	}
	mustBid(t, a, tx3, 12, tNow+2)
	if au := a.st.Auctions[id]; au.HighBid != ev3.ID || au.HighAmount != 204 || au.HighBid != a.st.Bids[ev3.ID].ID {
		t.Fatalf("the challenger must lead with 204: %+v", au)
	}
}

// 17. The other economic refusals are refusals at admission too: a closed lot,
// a lot whose end has passed, and the seller's own bid.
func TestClosedLotAndSellerBidsRefusedAtAdmission(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	_, tx := m.bidTx(t, a, alice, id, 200, "aa")
	finalize(t, a, 11, tNow+101) // close the lot
	if ck, _ := a.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx}); ck.Code != 2 || ck.Log != "auction closed" {
		t.Fatalf("a closed lot must refuse at admission: %d %q", ck.Code, ck.Log)
	}

	// A lot whose committed end has passed but which no block has closed yet:
	// the wall clock decides, because the delivering block closes it first.
	b, m2, id2 := liveApp(t)
	b.st.Auctions[id2].EndAt = time.Now().Unix() - 5
	_, tx2 := m2.bidTx(t, b, alice, id2, 200, "bb")
	if ck, _ := b.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx2}); ck.Code != 2 || ck.Log != "auction closed" {
		t.Fatalf("a lot past its end must refuse at admission: %d %q", ck.Code, ck.Log)
	}

	// The seller's own bid names the seller rule, not the floor.
	c, m3, id3, seller := liveAppSeller(t)
	_, sellerTx := m3.bidTx(t, c, seller, id3, 200, "cc")
	if ck, _ := c.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: sellerTx}); ck.Code != 2 || ck.Log != "seller cannot bid on own lot" {
		t.Fatalf("the seller's bid must refuse by name: %d %q", ck.Code, ck.Log)
	}
}

// --- R4: the admission fact is published ----------------------------------

// 18. The chain publishes a kind-1449 arena.bidadmit.v1 event per admitted bid,
// anchored at the bid's OWN committed height, carrying the leg's delta and the
// position it belongs to; a second pass publishes nothing new. A pure lot event
// is not an admission fact and vice versa.
func TestBidAdmissionFactIsPublished(t *testing.T) {
	a, m, id := liveApp(t)
	alice := newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 500, "aa")
	ev2, tx2 := m.bidTx(t, a, alice, id, 10, "bb")
	mustBid(t, a, tx1, 11, tNow+1)
	mustBid(t, a, tx2, 12, tNow+2)
	finalize(t, a, 13, tNow+3) // advance the pass height past the bids'

	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 4 })
	ctx := context.Background()
	pub.Step(ctx)

	got := map[string]*BidAdmitContent{}
	for _, ev := range eventsOfKind(rec, KindLot) {
		c, err := ParseBidAdmitEvent(ev, nil)
		if err != nil {
			continue
		}
		if c.Height != a.st.Bids[c.BidID].Height {
			t.Fatalf("admission for %s anchored at %d, want the bid's %d", c.BidID, c.Height, a.st.Bids[c.BidID].Height)
		}
		got[c.BidID] = c
	}
	if len(got) != 2 {
		t.Fatalf("admission events = %d, want one per admitted bid", len(got))
	}
	c1, c2 := got[ev1.ID], got[ev2.ID]
	if c1 == nil || c2 == nil {
		t.Fatalf("missing an admission fact: %v", got)
	}
	if c1.AuctionID != id || c1.Bidder != alice.pub || c1.Amount != 500 || c1.Position != 510 || c1.HighBid != ev2.ID {
		t.Fatalf("leg 1 admission = %+v", c1)
	}
	if c2.Amount != 10 || c2.Position != 510 || c2.HighBid != ev2.ID {
		t.Fatalf("leg 2 admission = %+v", c2)
	}
	if c1.Height != 11 || c1.BlockHash != testBlockHash(11) {
		t.Fatalf("leg 1 anchor = %d/%s, want 11/%s", c1.Height, c1.BlockHash, testBlockHash(11))
	}
	if c2.Height != 12 || c2.BlockHash != testBlockHash(12) {
		t.Fatalf("leg 2 anchor = %d/%s, want 12/%s", c2.Height, c2.BlockHash, testBlockHash(12))
	}
	if countRecords(pub, "bid_admit") != 2 {
		t.Fatalf("bid_admit records = %d, want 2", countRecords(pub, "bid_admit"))
	}

	// Idempotent: a second pass publishes nothing new.
	pub.Step(ctx)
	if n := countRecords(pub, "bid_admit"); n != 2 {
		t.Fatalf("bid_admit records after two passes = %d, want 2", n)
	}

	// The shapes of kind 1449 are told apart by the schema: an admission event
	// is refused by the lot parser and a lot event by the admission parser.
	var admitEv, lotEv *Event
	for _, ev := range eventsOfKind(rec, KindLot) {
		if _, err := ParseBidAdmitEvent(ev, nil); err == nil {
			admitEv = ev
		} else if _, err := ParseLotEvent(ev, nil); err == nil {
			lotEv = ev
		}
	}
	if admitEv == nil {
		t.Fatal("no admission event to cross-check")
	}
	if _, err := ParseLotEvent(admitEv, nil); !errors.Is(err, ErrLotBadSchema) {
		t.Fatalf("the lot parser must refuse an admission event by schema, got %v", err)
	}
	if lotEv != nil {
		if _, err := ParseBidAdmitEvent(lotEv, nil); !errors.Is(err, ErrBidAdmitBadSchema) {
			t.Fatalf("the admission parser must refuse a lot event by schema, got %v", err)
		}
	}
}

// 19. The admission event's own rules: schema, field rules, the MANDATORY
// anchor, tag/content agreement, and the roster.
func TestBidAdmitEventRefusals(t *testing.T) {
	sk := testVerifierKey(t)
	good := BidAdmitContent{Schema: BidAdmitSchema, AuctionID: "1440:aa:lot", BidID: strings.Repeat("ab", 32),
		Bidder: strings.Repeat("cd", 32), Amount: 10, Position: 510, HighBid: strings.Repeat("ef", 32),
		Height: 12, BlockHash: testBlockHash(12)}
	ev, err := BuildBidAdmitEvent(sk, good, tNow)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseBidAdmitEvent(ev, nil)
	if err != nil || c.Schema != BidAdmitSchema || c.BidID != good.BidID || c.Position != 510 {
		t.Fatalf("round trip: %+v %v", c, err)
	}

	// Field rules, refused by name.
	for name, mutate := range map[string]func(*BidAdmitContent){
		"no auction":  func(c *BidAdmitContent) { c.AuctionID = "" },
		"no bid":      func(c *BidAdmitContent) { c.BidID = "" },
		"no bidder":   func(c *BidAdmitContent) { c.Bidder = "" },
		"no amount":   func(c *BidAdmitContent) { c.Amount = 0 },
		"short":       func(c *BidAdmitContent) { c.Position = 5 },
		"no high bid": func(c *BidAdmitContent) { c.HighBid = "" },
		"unanchored":  func(c *BidAdmitContent) { c.BlockHash = "" },
	} {
		bad := good
		mutate(&bad)
		if _, err := BuildBidAdmitEvent(sk, bad, tNow); err == nil {
			t.Fatalf("%s: build must refuse", name)
		}
	}

	// A re-signed schema swap and a re-signed anchor swap are refused.
	body := func(c BidAdmitContent) string { b, _ := json.Marshal(c); return string(b) }
	other := good
	other.Schema = LotSchema
	if _, err := ParseBidAdmitEvent(resignContent(t, sk, ev, body(other)), nil); !errors.Is(err, ErrBidAdmitBadSchema) {
		t.Fatalf("schema swap: want %v", ErrBidAdmitBadSchema)
	}
	moved := good
	moved.Height = 13
	if _, err := ParseBidAdmitEvent(resignContent(t, sk, ev, body(moved)), nil); !errors.Is(err, ErrBidAdmitUnanchored) {
		t.Fatalf("anchor swap: want %v, got %v", ErrBidAdmitUnanchored, err)
	}
	// The e tag must name the bid the content names: re-signed so the ONLY
	// fault is the tag.
	retagged := signEventTags(t, sk, KindLot, ev.Content, tNow, [][]string{{"e", strings.Repeat("11", 32)},
		{"auction", good.AuctionID}, {"height", "12"}, {"hash", testBlockHash(12)}})
	if _, err := ParseBidAdmitEvent(retagged, nil); !errors.Is(err, ErrBidAdmitBadSchema) {
		t.Fatalf("e-tag mismatch: want %v, got %v", ErrBidAdmitBadSchema, err)
	}

	// An off-roster signer is named.
	roster := map[string]ValidatorKey{NostrPubkeyHex(testVerifierKey(t)): {Index: 1}}
	if _, err := ParseBidAdmitEvent(ev, roster); !errors.Is(err, ErrBidAdmitNotInRoster) {
		t.Fatalf("off roster: want %v", ErrBidAdmitNotInRoster)
	}
	if _, err := ParseBidAdmitEvent(ev, map[string]ValidatorKey{NostrPubkeyHex(sk): {Index: 1}}); err != nil {
		t.Fatalf("on roster: %v", err)
	}

	// A lot event is not an admission event.
	lot, err := BuildLotEvent(sk, LotContent{AuctionID: "1440:aa:lot", EndTime: 20,
		OriginalEndTime: 10, Height: 12, BlockHash: testBlockHash(12)}, tNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseBidAdmitEvent(lot, nil); !errors.Is(err, ErrBidAdmitBadSchema) {
		t.Fatalf("a lot event: want %v", ErrBidAdmitBadSchema)
	}
}
