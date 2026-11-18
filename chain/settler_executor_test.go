package main

// The settler is an EXECUTOR, not an author (spec §5). These tests pin the two
// properties that make that true, without needing the FROST tool:
//
//   - the settler derives its outputs from the bid's COMMITTED outcome
//     template (templateMaterial), never from anything caller-supplied;
//   - the swap it submits carries TWO witness signatures: the bidder's
//     pre-signature from the committed template, and the committee's -- in
//     that order. With only one, the mint refuses.
//
// The coordinator's own FROST round is exercised by the /frost handlers and by
// releasePolicy; here we drive the swap directly against a stub mint, so the
// assertions are about the witness the settler builds and the template it
// reads, not about HTTP plumbing.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSettlerSwapCarriesBidderAndCommitteeSignatures runs the settler's swap
// against a stub mint and asserts the witness order and the outputs submitted.
func TestSettlerSwapCarriesBidderAndCommitteeSignatures(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	bid := a.st.Bids[ev1.ID]
	if bid.Status != "outbid" {
		t.Fatal("setup")
	}

	var gotWitness []string
	var gotOutputs []SignOutput
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/swap" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Inputs []struct {
				Witness string `json:"witness"`
			} `json:"inputs"`
			Outputs []SignOutput `json:"outputs"`
		}
		json.Unmarshal(body, &req)
		gotOutputs = req.Outputs
		var w2 struct {
			Signatures []string `json:"signatures"`
		}
		json.Unmarshal([]byte(req.Inputs[0].Witness), &w2)
		gotWitness = w2.Signatures
		sigs := make([]blindSig, len(req.Outputs))
		for i, o := range req.Outputs {
			sigs[i] = m.blindSign(t, o)
		}
		json.NewEncoder(w).Encode(map[string]any{"signatures": sigs})
	}))
	defer srv.Close()

	st := &settler{p: a.st.Params, mint: srv.URL, hc: &http.Client{}}
	outs, _, bidderSig, err := st.templateMaterial(bid, "refund")
	if err != nil {
		t.Fatal(err)
	}
	const committeeSig = "00" // placeholder: the stub mint does not verify it
	if _, err := st.swap(bid.Proofs, outs, bidderSig, committeeSig); err != nil {
		t.Fatalf("swap: %v", err)
	}

	if len(gotWitness) != 2 {
		t.Fatalf("the witness must carry BOTH signatures, got %d", len(gotWitness))
	}
	if gotWitness[0] != bidderSig {
		t.Fatal("the first witness signature must be the bidder's committed pre-signature")
	}
	if gotWitness[1] != committeeSig {
		t.Fatal("the second witness signature must be the committee's")
	}
	// The outputs submitted are exactly the committed refund template's.
	want, _, _, _ := st.templateMaterial(bid, "refund")
	if len(gotOutputs) != len(want) {
		t.Fatalf("outputs submitted %d, want %d", len(gotOutputs), len(want))
	}
	for i := range want {
		if gotOutputs[i].B != want[i].B || gotOutputs[i].Amount != want[i].Amount {
			t.Fatalf("output %d is not the committed template's output", i)
		}
	}
}

// §5 failure posture: a refund spend that cannot complete must never lose the
// obligation.  No validator answers, so the settler's attempt fails at the
// committee half and no swap can be submitted; the escrow must still be due --
// and must still settle once the spend can complete.
func TestFailedSpendLeavesTheObligationDue(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)

	due := func(now int64) []dueMove { return a.st.dueMoves(now) }
	if items := due(tNow + 2); len(items) != 1 || items[0].Outcome != "refund" || items[0].To != alice.wallet {
		t.Fatalf("setup: the outbid escrow must be due: %+v", items)
	}
	escrow := len(a.st.Bids[ev1.ID].Proofs)
	// The escrow's own proofs are already recorded as used from admission; a
	// failed spend must add nothing to that set.
	usedBefore := len(a.st.UsedY)

	// The mint answers nothing, and no validator answers either: the committee
	// half never exists, so the spend cannot complete.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "mint is down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := testSettler(t, a)
	s.mint, s.hc = srv.URL, &http.Client{}
	s.rc = &releaseCoordinator{groupKey: a.st.Params.GroupKey,
		post: func(int, string, any, any) (int, error) { return 0, errors.New("validator offline") }}
	if err := s.settle(due(tNow+2)[0], 12); err == nil {
		t.Fatal("a settle whose committee half cannot be collected must fail")
	}

	// THE POSTURE: the failed spend lost nothing.  The escrow is still due, to
	// the bidder, with nothing recorded and nothing consumed, and the attempt is
	// journalled so the settler retries it.
	if items := due(tNow + 3); len(items) != 1 || items[0].Outcome != "refund" || items[0].To != alice.wallet {
		t.Fatalf("the obligation must survive a failed spend: %+v", items)
	}
	if b := a.st.Bids[ev1.ID]; b.Settled != "" || len(b.Out) != 0 {
		t.Fatalf("a failed spend must record nothing: %+v", b)
	}
	if len(a.st.Bids[ev1.ID].Proofs) != escrow || len(a.st.UsedY) != usedBefore {
		t.Fatal("a failed spend must not consume the escrow")
	}
	if len(s.tries[ev1.ID]) != 1 {
		t.Fatal("the failed attempt must be journalled for the retry")
	}

	// The retry, once the spend can complete, settles that same escrow.
	st := mintMove(t, a, m, ev1.ID, "refund", alice.wallet, tNow+3)
	if r := finalize(t, a, 12, tNow+3, makeSettle(st)); r.TxResults[0].Code != 0 {
		t.Fatalf("the retry must settle: %s", r.TxResults[0].Log)
	}
	if b := a.st.Bids[ev1.ID]; b.Settled != "refund" || len(b.Out) == 0 {
		t.Fatalf("the retry must record the refund: %+v", b)
	}
	if items := due(tNow + 4); len(items) != 0 {
		t.Fatalf("a settled escrow is no longer due: %+v", items)
	}
}

// TestSettlerRefusesTemplatelessBid: the settler never builds an output of its
// own, so a bid with no committed template cannot be moved by it.
func TestSettlerRefusesTemplatelessBid(t *testing.T) {
	a, _, id := liveApp(t)
	st := &settler{p: a.st.Params}
	bid := &Bid{ID: hx(1), Auction: id, Wallet: hx(2), Amount: 100, Proofs: []Proof{{ID: a.st.Params.KeysetID, Amount: 100, Secret: "s", C: "c"}}}
	if _, _, _, err := st.templateMaterial(bid, "refund"); err == nil {
		t.Fatal("a template-less bid must not be movable by the settler")
	}
}

// TestReleasePolicySignsCommittedOutputs: the committee's hash is over the
// committed template's outputs, so the committee signs exactly what the bidder
// pre-signed. A leading bid is not refundable; an unknown outcome is refused.
func TestReleasePolicySignsCommittedOutputs(t *testing.T) {
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	ev2, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)

	h, code, err := releasePolicy(a, SignReq{Bid: ev1.ID, Outcome: "refund"})
	if code != 200 {
		t.Fatalf("committed refund refused: %d %v", code, err)
	}
	outs, _, _, _ := (&settler{p: a.st.Params}).templateMaterial(a.st.Bids[ev1.ID], "refund")
	want := sha256Sum(sigAllMessage(a.st.Bids[ev1.ID].Proofs, outs))
	if h != want {
		t.Fatal("the committee hash must cover the committed template's outputs")
	}
	if _, code, _ := releasePolicy(a, SignReq{Bid: ev2.ID, Outcome: "refund"}); code == 200 {
		t.Fatal("a leading bid must not be refundable")
	}
	if _, code, _ := releasePolicy(a, SignReq{Bid: ev1.ID, Outcome: "nonsense"}); code == 200 {
		t.Fatal("an unknown outcome must be refused")
	}
}
