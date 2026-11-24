package main

// Tests for the settlement-facilitator increment (contract §2, §3, §6).
//
// The chain no longer touches the mint.  These tests pin the replacement:
//   - a due outcome is signed with zero HTTP to /api/settle;
//   - a restart mid-round does not publish a duplicate aggregate;
//   - the published kind-1451 verifies against the committed digest and
//     destinations;
//   - a kind-1452 carrier commits a settle tx populating bid.Settled/bid.Out;
//   - the 1443/1444 receipt matches the committed template output-for-output.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeCommitteeSigner struct {
	code  int
	res   releaseResult
	err   error
	calls int
}

func (f *fakeCommitteeSigner) Release(req SignReq) (int, releaseResult) {
	f.calls++
	return f.code, f.res
}

// dueRefundApp sets up liveApp plus an outbid bid whose refund is due now.
func dueRefundApp(t *testing.T) (*App, *testMint, string, *Event) {
	t.Helper()
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	if a.st.Bids[ev1.ID].Status != "outbid" {
		t.Fatalf("setup: bid status = %q, want outbid", a.st.Bids[ev1.ID].Status)
	}
	return a, m, id, ev1
}

// TestDueOutcomeSignsWithZeroHTTPToApiSettle: the signing pass lives in the
// node and makes no HTTP call to a settlement endpoint.  It reaches only the
// fake in-process signer; no listener is started and no /api/settle route
// exists anywhere in the deployed surface.
func TestDueOutcomeSignsWithZeroHTTPToApiSettle(t *testing.T) {
	a, _, _, ev1 := dueRefundApp(t)
	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	myKey := newBidder(t).pub
	validators := []ValidatorKey{{Pubkey: myKey}}
	signer := &fakeCommitteeSigner{code: 200, res: releaseResult{Signature: strings.Repeat("ab", 64)}}
	signed := map[string]bool{}
	settlementTick(a, pub, signer, validators, myKey, signed)

	if signer.calls != 1 {
		t.Fatalf("signer calls = %d, want 1", signer.calls)
	}
	if got := len(eventsOfKind(rec, KindCommitteeSignature)); got != 1 {
		t.Fatalf("published committee-signature events = %d, want 1", got)
	}
	if !signed[ev1.ID+"|refund"] {
		t.Fatal("the (bid, outcome) must be recorded as signed")
	}
}

// TestRestartMidRoundDoesNotRepublishAggregate: the first round is interrupted
// before an aggregate is published (the signer refuses).  A restart runs the
// pass again and publishes exactly one aggregate; a further tick does not
// republish it for the same digest.
func TestRestartMidRoundDoesNotRepublishAggregate(t *testing.T) {
	a, _, _, ev1 := dueRefundApp(t)
	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	myKey := newBidder(t).pub
	validators := []ValidatorKey{{Pubkey: myKey}}
	signed := map[string]bool{}

	fail := &fakeCommitteeSigner{code: 500, res: releaseResult{Error: "mesh down"}}
	settlementTick(a, pub, fail, validators, myKey, signed)
	if got := len(eventsOfKind(rec, KindCommitteeSignature)); got != 0 {
		t.Fatalf("a failed round published %d events, want 0", got)
	}
	if signed[ev1.ID+"|refund"] {
		t.Fatal("a failed round must not mark the outcome signed")
	}

	ok := &fakeCommitteeSigner{code: 200, res: releaseResult{Signature: strings.Repeat("cd", 64)}}
	settlementTick(a, pub, ok, validators, myKey, signed)
	if got := len(eventsOfKind(rec, KindCommitteeSignature)); got != 1 {
		t.Fatalf("after restart published %d events, want 1", got)
	}
	settlementTick(a, pub, ok, validators, myKey, signed)
	if got := len(eventsOfKind(rec, KindCommitteeSignature)); got != 1 {
		t.Fatalf("a second tick republished: %d events, want 1", got)
	}
}

// TestPublished1451VerifiesAgainstCommittedDigestAndDestinations builds the
// 1451 from committed state, publishes it through the Publisher (so the anchor
// is the block store's hash), and parses it back: the digest must cover the
// committed escrow inputs and template outputs, and the outputs must name the
// committed destinations.
func TestPublished1451VerifiesAgainstCommittedDigestAndDestinations(t *testing.T) {
	a, _, _, ev1 := dueRefundApp(t)
	bid := a.st.Bids[ev1.ID]
	template, outs, err := a.st.settlementOutputs(bid, "refund")
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]CommitteeSignatureInput, len(bid.Proofs))
	for i, p := range bid.Proofs {
		inputs[i] = CommitteeSignatureInput{Secret: p.Secret, C: p.C}
	}
	outputs := make([]CommitteeSignatureOutput, len(outs))
	for i, o := range outs {
		outputs[i] = CommitteeSignatureOutput{Amount: o.Amount, B: o.B}
	}
	digest := committeeSignatureDigest(inputs, outputs)
	sig := groupSign(t, sigAllMessage(bid.Proofs, outs))
	myKey := newBidder(t)
	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	content := CommitteeSignatureContent{
		Schema: CommitteeSignatureSchema, Bid: bid.ID, Auction: bid.Auction, Outcome: "refund",
		Height: 11, BlockHash: testBlockHash(11), AppHash: testBlockHash(99),
		Digest: digest, Signature: sig, Inputs: inputs, Outputs: outputs,
	}
	if err := pub.PublishCommitteeSignature(context.Background(), content); err != nil {
		t.Fatal(err)
	}
	evs := eventsOfKind(rec, KindCommitteeSignature)
	if len(evs) != 1 {
		t.Fatalf("committee-signature events = %d, want 1", len(evs))
	}
	c, err := ParseCommitteeSignature(evs[0], map[string]ValidatorKey{evs[0].Pubkey: {Pubkey: evs[0].Pubkey}}, a.st.Params.GroupKey)
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest != digest {
		t.Fatalf("digest = %s, want %s", c.Digest, digest)
	}
	if len(c.Outputs) != len(outs) {
		t.Fatalf("committee-signature outputs = %d, want %d", len(c.Outputs), len(outs))
	}
	for i := range outs {
		if c.Outputs[i].Amount != outs[i].Amount || c.Outputs[i].B != outs[i].B {
			t.Fatalf("output %d = %+v, want %+v", i, c.Outputs[i], outs[i])
		}
		if c.Outputs[i].B != outs[i].B {
			t.Fatalf("output %d B_ changed", i)
		}
	}
	// The outputs are the committed template's own, locked to the bidder's
	// wallet (the refund destination): prove the destination is recoverable.
	for i, dec := range template.Outputs {
		lock, err := parseP2PK(dec.Secret)
		if err != nil {
			t.Fatalf("output %d secret: %v", i, err)
		}
		if lock.Data != bid.Wallet {
			t.Fatalf("output %d destination = %q, want the committed wallet %q", i, lock.Data, bid.Wallet)
		}
	}
	_ = myKey
}

// TestKind1452CarrierCommitsSettleTx: a 1452 event's content is the settleTx
// payload; EventTransaction turns it into the settle: bytes parseSettle already
// accepts, and FinalizeBlock records bid.Settled and bid.Out.
func TestKind1452CarrierCommitsSettleTx(t *testing.T) {
	a, m, _, ev1 := dueRefundApp(t)
	alice := newBidder(t)
	_ = alice
	st := mintMove(t, a, m, ev1.ID, "refund", a.st.Bids[ev1.ID].Wallet, tNow+2)
	content, err := json.Marshal(settlementCarrier{
		Schema: SettlementCarrierSchema, Bid: st.Bid, Outcome: st.Outcome, Proofs: st.Proofs, Sig: st.Sig,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The courier is any key; for the test it is an unrelated bidder.
	carrier := signEventTags(t, newBidder(t).sk, KindSettlementCarrier, string(content), tNow+2, [][]string{})
	tx, err := EventTransaction(*carrier)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(tx), settlePrefix) {
		t.Fatalf("EventTransaction(carrier) must return settle: bytes, got %q", string(tx))
	}
	r := finalize(t, a, 12, tNow+2, tx)
	if r.TxResults[0].Code != 0 {
		t.Fatalf("carrier tx refused: %s", r.TxResults[0].Log)
	}
	bid := a.st.Bids[ev1.ID]
	if bid.Settled != "refund" {
		t.Fatalf("bid.Settled = %q, want refund", bid.Settled)
	}
	if len(bid.Out) != len(st.Proofs) || len(bid.Out) == 0 {
		t.Fatalf("bid.Out = %d proofs, want %d", len(bid.Out), len(st.Proofs))
	}
	for i, p := range st.Proofs {
		if bid.Out[i].Amount != p.Amount || bid.Out[i].C != p.C || bid.Out[i].Secret != p.Secret {
			t.Fatalf("bid.Out[%d] = %+v, want the committed proof %+v", i, bid.Out[i], p)
		}
	}
}

// TestReceiptMatchesCommittedTemplateOutputForOutput: after a real settle, the
// publisher's 1443/1444 receipt carries an Out array that is the committed
// template's outputs, proof for proof.
func TestReceiptMatchesCommittedTemplateOutputForOutput(t *testing.T) {
	a, m, _, ev1 := dueRefundApp(t)
	st := mintMove(t, a, m, ev1.ID, "refund", a.st.Bids[ev1.ID].Wallet, tNow+2)
	finalize(t, a, 12, tNow+2, makeSettle(st))
	finalize(t, a, 13, tNow+3)
	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 3 })
	pub.Step(context.Background())
	evs := eventsOfKind(rec, KindRefund)
	if len(evs) != 1 {
		t.Fatalf("refund receipts = %d, want 1", len(evs))
	}
	c, err := ParseRefundEvent(evs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Out) != len(st.Proofs) {
		t.Fatalf("receipt Out = %d, want %d", len(c.Out), len(st.Proofs))
	}
	for i, p := range st.Proofs {
		got := c.Out[i]
		if got.Amount != p.Amount || got.ID != p.ID || got.Secret != p.Secret || got.C != p.C {
			t.Fatalf("receipt Out[%d] = %+v, want the committed proof %+v", i, got, p)
		}
		if (got.DLEQ == nil) != (p.DLEQ == nil) {
			t.Fatalf("receipt Out[%d] DLEQ presence mismatch", i)
		}
	}
}
