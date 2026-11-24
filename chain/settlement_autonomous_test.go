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
	"encoding/hex"
	"encoding/json"
	"strconv"
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
	// The pass refuses an aggregate that does not cover this node's committed
	// digest, so the fake signer returns a real group signature over it.
	bid := a.st.Bids[ev1.ID]
	_, outs, err := a.st.settlementOutputs(bid, "refund")
	if err != nil {
		t.Fatal(err)
	}
	signer := &fakeCommitteeSigner{code: 200, res: releaseResult{Signature: groupSign(t, sigAllMessage(bid.Proofs, outs))}}
	state := newSignerState()
	settlementTick(a, pub, signer, validators, myKey, state)

	if signer.calls != 1 {
		t.Fatalf("signer calls = %d, want 1", signer.calls)
	}
	if got := len(eventsOfKind(rec, KindCommitteeSignature)); got != 1 {
		t.Fatalf("published committee-signature events = %d, want 1", got)
	}
	if !state.signed[ev1.ID+"|refund"] {
		t.Fatal("the (bid, outcome) must be recorded as signed")
	}
}

// TestSettleRefusesProofAmountNotInTheCommittedTemplate pins the amount binding
// in verifySettle. Once a 1451 committee signature is public, anyone can mint a
// proof for the committed (secret, r) — `B_ = H(secret) + r*G` carries no amount
// — under a smaller denomination and carry it as a settlement. Without the
// binding the chain would record a fraction of the payout as settled and leave
// the untouched escrow refundable to the bidder after locktime.
func TestSettleRefusesProofAmountNotInTheCommittedTemplate(t *testing.T) {
	a, m, _, ev1 := dueRefundApp(t)
	bid := a.st.Bids[ev1.ID]
	template, outs, err := a.st.settlementOutputs(bid, "refund")
	if err != nil {
		t.Fatal(err)
	}
	// The same committed blinded point, under a different pinned denomination.
	other := otherPinnedAmount(t, a.st.Params.Keys, outs[0].Amount)
	forged := append([]SignOutput(nil), outs...)
	forged[0] = SignOutput{Amount: other, ID: outs[0].ID, B: outs[0].B}

	proofs := make([]Proof, len(forged))
	for i, o := range forged {
		bs := m.blindSign(t, o)
		A, err := parsePub(a.st.Params.Keys[strconv.FormatInt(o.Amount, 10)])
		if err != nil {
			t.Fatal(err)
		}
		Cb, err := parsePub(bs.C_)
		if err != nil {
			t.Fatal(err)
		}
		r, err := parseScalar(template.Outputs[i].R)
		if err != nil {
			t.Fatal(err)
		}
		C := add(Cb, neg(mul(r, A)))
		pr := Proof{ID: o.ID, Amount: o.Amount, Secret: template.Outputs[i].Secret,
			C:    hex.EncodeToString(C.SerializeCompressed()),
			DLEQ: &DLEQ{E: bs.DLEQ.E, S: bs.DLEQ.S, R: template.Outputs[i].R}}
		if err := verifyDLEQ(pr, A); err != nil {
			t.Fatalf("the forged proof must be a real mint signature for its own amount: %v", err)
		}
		proofs[i] = pr
	}
	// The committee signature is the honest, committed one: it covers the
	// template's amounts, which is exactly why the amount must be checked.
	st := &settleTx{Bid: ev1.ID, Outcome: "refund", Proofs: proofs, Sig: groupSign(t, sigAllMessage(bid.Proofs, outs))}

	_, err = a.st.verifySettle(st, tNow+2)
	if err == nil {
		t.Fatal("a proof whose amount is not the committed template's must be refused")
	}
	if !strings.Contains(err.Error(), "amount") {
		t.Fatalf("refusal = %v, want the amount binding", err)
	}
	if r := finalize(t, a, 12, tNow+2, makeSettle(st)); r.TxResults[0].Code == 0 {
		t.Fatal("a forged-amount settle must not commit")
	}
	if a.st.Bids[ev1.ID].Settled != "" {
		t.Fatal("a forged-amount settle must not record a settlement")
	}
}

// otherPinnedAmount returns the smallest pinned denomination other than not.
func otherPinnedAmount(t *testing.T, keys map[string]string, not int64) int64 {
	t.Helper()
	best := int64(-1)
	for k := range keys {
		v, err := strconv.ParseInt(k, 10, 64)
		if err != nil || v == not {
			continue
		}
		if best == -1 || v < best {
			best = v
		}
	}
	if best == -1 {
		t.Fatal("keyset has no denomination other than the template's")
	}
	return best
}

// TestCommitteeSignatureNormalisesTheStoreCase is the live-run regression: the CometBFT block
// store prints hex UPPER case and the FROST binary returns an upper-case aggregate, while the
// fact's own schema (and the app's parser) demand lower-case hex. Publishing must normalise
// rather than refuse — refusing is what left the deployed root with no kind-1451 at all.
func TestCommitteeSignatureNormalisesTheStoreCase(t *testing.T) {
	a, _, _, ev1 := dueRefundApp(t)
	bid := a.st.Bids[ev1.ID]
	_, outs, err := a.st.settlementOutputs(bid, "refund")
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
	rec := &recordingRelay{}
	pub, err := NewPublisher(PublisherConfig{
		Key: testVerifierKey(t), Relay: rec, App: a, Every: StateCommitEvery,
		// Exactly what the real BlockStore returns.
		BlockHash: func(height int64) (string, bool) { return strings.ToUpper(testBlockHash(height)), true },
		Now:       func() int64 { return tNow + 2 },
	})
	if err != nil {
		t.Fatal(err)
	}
	content := CommitteeSignatureContent{
		Schema: CommitteeSignatureSchema, Bid: bid.ID, Auction: bid.Auction, Outcome: "refund",
		Height: 11, AppHash: testBlockHash(99),
		Digest: committeeSignatureDigest(inputs, outputs), Signature: strings.ToUpper(groupSign(t, sigAllMessage(bid.Proofs, outs))),
		Inputs: inputs, Outputs: outputs,
	}
	if err := pub.PublishCommitteeSignature(context.Background(), content); err != nil {
		t.Fatalf("an upper-case store hash and aggregate must publish, not refuse: %v", err)
	}
	evs := eventsOfKind(rec, KindCommitteeSignature)
	if len(evs) != 1 {
		t.Fatalf("committee-signature events = %d, want 1", len(evs))
	}
	// The published fact is lower-case throughout, so the app's strict parser accepts it.
	if _, err := ParseCommitteeSignature(evs[0], map[string]ValidatorKey{evs[0].Pubkey: {Pubkey: evs[0].Pubkey}}, a.st.Params.GroupKey); err != nil {
		t.Fatalf("the published fact must parse back: %v", err)
	}
}

// TestARefusedOutcomeIsBackedOff pins the retry shape: a move the round keeps refusing must
// NOT be retried on every tick, or a permanently unsettleable legacy escrow logs once a
// second for the life of the process.
func TestARefusedOutcomeIsBackedOff(t *testing.T) {
	a, _, _, _ := dueRefundApp(t)
	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	myKey := newBidder(t).pub
	validators := []ValidatorKey{{Pubkey: myKey}}
	state := newSignerState()
	fail := &fakeCommitteeSigner{code: 409, res: releaseResult{Error: "refused"}}

	settlementTick(a, pub, fail, validators, myKey, state)
	if fail.calls != 1 {
		t.Fatalf("signer calls = %d, want 1", fail.calls)
	}
	// A second tick inside the backoff window must not reach the round again.
	settlementTick(a, pub, fail, validators, myKey, state)
	if fail.calls != 1 {
		t.Fatalf("a backed-off move was retried immediately: calls = %d", fail.calls)
	}
	if got := signerRetryInSec(1); got != 5 {
		t.Fatalf("first retry = %ds, want 5", got)
	}
	if got := signerRetryInSec(20); got != 300 {
		t.Fatalf("retry after many failures = %ds, want the 300s cap", got)
	}
}

// TestAggregateForAnotherDigestIsNotPublished: a round that aggregates a
// message other than this node's committed digest must not produce a 1451 —
// the fact would carry a digest its own signature does not cover.
func TestAggregateForAnotherDigestIsNotPublished(t *testing.T) {
	a, _, _, ev1 := dueRefundApp(t)
	rec := &recordingRelay{}
	pub := newTestPublisher(t, a, rec, StateCommitEvery, 0, func() int64 { return tNow + 2 })
	myKey := newBidder(t).pub
	validators := []ValidatorKey{{Pubkey: myKey}}
	state := newSignerState()

	// A valid group signature, but over a different message.
	bad := &fakeCommitteeSigner{code: 200, res: releaseResult{Signature: groupSign(t, "another message entirely")}}
	settlementTick(a, pub, bad, validators, myKey, state)

	if got := len(eventsOfKind(rec, KindCommitteeSignature)); got != 0 {
		t.Fatalf("an aggregate over another digest published %d events, want 0", got)
	}
	if state.signed[ev1.ID+"|refund"] {
		t.Fatal("an inconsistent aggregate must not mark the outcome signed")
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
	state := newSignerState()
	bid := a.st.Bids[ev1.ID]
	_, outs, err := a.st.settlementOutputs(bid, "refund")
	if err != nil {
		t.Fatal(err)
	}

	fail := &fakeCommitteeSigner{code: 500, res: releaseResult{Error: "mesh down"}}
	settlementTick(a, pub, fail, validators, myKey, state)
	if got := len(eventsOfKind(rec, KindCommitteeSignature)); got != 0 {
		t.Fatalf("a failed round published %d events, want 0", got)
	}
	if state.signed[ev1.ID+"|refund"] {
		t.Fatal("a failed round must not mark the outcome signed")
	}

	ok := &fakeCommitteeSigner{code: 200, res: releaseResult{Signature: groupSign(t, sigAllMessage(bid.Proofs, outs))}}
	// A restart is a NEW process: the in-memory signed/backoff maps are gone, so the
	// pass runs the round again even though the previous one failed (and was backed off).
	restarted := newSignerState()
	settlementTick(a, pub, ok, validators, myKey, restarted)
	if got := len(eventsOfKind(rec, KindCommitteeSignature)); got != 1 {
		t.Fatalf("after restart published %d events, want 1", got)
	}
	settlementTick(a, pub, ok, validators, myKey, restarted)
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
