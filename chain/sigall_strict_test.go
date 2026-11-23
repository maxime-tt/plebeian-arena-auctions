package main

// Regression tests for the critical escrow defect: admission, settle
// verification and the FROST signing policy must all read a settle/refund
// template output's destination through the ONE strict parser
// (strictTemplateOutput). Before this they disagreed: admission looked only at
// `data` while the mint builds the signer set as [data]+tags["pubkeys"], so a
// settle output carrying ["pubkeys",<bidder>],["n_sigs","1"] was spendable by
// the bidder alone -- admitted by consensus, unsettleable on chain.
//
// This file also carries the audit probe
// (verification-go-admission-test.go) as a permanent regression: the crafted
// admission case must now fail to admit.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// craftedBidEvent returns an otherwise-valid liveApp bid by bob over 150 sats
// whose content mutate() has altered. The settle template's digest and the
// bidder's signature are re-derived over the mutated bytes, so it is the
// destination parser -- not a stale signature -- that decides admission.
func craftedBidEvent(t *testing.T, a *App, m *testMint, auction string, bob bidder, mutate func(c *BidContent)) *Event {
	t.Helper()
	ev, _ := m.bidTx(t, a, bob, auction, 150, "c1")
	var c BidContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		t.Fatal(err)
	}
	mutate(&c)
	settle := c.OutcomeTemplates.Settle
	digest, err := sigAllTemplateDigest(settle)
	if err != nil {
		t.Fatalf("mutated settle template does not digest: %v", err)
	}
	sig, err := schnorr.Sign(bob.walletSk, mustHex(digest))
	if err != nil {
		t.Fatal(err)
	}
	c.OutcomeTemplates.Settle.Digest = digest
	c.OutcomeTemplates.Settle.BidderSignature = hex.EncodeToString(sig.Serialize())
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	au := a.st.Auctions[auction]
	return &Event{ID: hx(90001), Pubkey: bob.pub, Tags: [][]string{{"e", au.RootID}}, Content: string(body)}
}

// insertCraftedSettle seeds state with a closed lot and a winning bid whose
// settle template is exactly one output carrying secret, bypassing admission,
// so the signing policy can be asked to sign it directly. The auction is left
// closed with this bid as the winner, which is what outcomeDue requires.
func insertCraftedSettle(t *testing.T, a *App, auction, secret string, amount int64) string {
	t.Helper()
	id := hx(4242)
	wallet := realPub(t, 31)
	a.st.Bids[id] = &Bid{
		ID: id, Auction: auction, Bidder: hx(7), Wallet: wallet, Amount: amount, Status: "won",
		OutcomeTemplates: &sigAllOutcomeTemplates{
			Settle: sigAllSignedTemplate{Outputs: []sigAllTemplateOutput{{
				Amount: amount, ID: a.st.Params.KeysetID, Secret: secret, R: strings.Repeat("11", 32)}}},
			Refund: sigAllSignedTemplate{Outputs: []sigAllTemplateOutput{{
				Amount: amount, ID: a.st.Params.KeysetID,
				Secret: p2pkOut(wallet, strings.Repeat("22", 32)), R: strings.Repeat("33", 32)}}},
		},
	}
	au := a.st.Auctions[auction]
	au.Status, au.Winner = "closed", id
	return id
}

// The honest wallet shape is accepted, and only when data is the committed key.
func TestStrictTemplateOutputHonestWalletShape(t *testing.T) {
	key := realPub(t, 17)
	nonce := strings.Repeat("ab", 32)
	honest := p2pkOut(key, nonce)
	if err := strictTemplateOutput(honest, key); err != nil {
		t.Fatalf("honest wallet shape refused: %v", err)
	}
	// x-only comparison: the other parity of the same point is the same key.
	other := "02" + key[2:]
	if key[:2] == "02" {
		other = "03" + key[2:]
	}
	if err := strictTemplateOutput(honest, other); err != nil {
		t.Fatalf("the same point with the other prefix must be accepted: %v", err)
	}
	// No sigflag tag at all is still the same single-key condition.
	noTag := `["P2PK",{"nonce":"` + nonce + `","data":"` + key + `","tags":[]}]`
	if err := strictTemplateOutput(noTag, key); err != nil {
		t.Fatalf("empty tag list refused: %v", err)
	}
	// A different destination key is refused.
	if err := strictTemplateOutput(honest, realPub(t, 19)); err == nil ||
		!strings.Contains(err.Error(), "not locked to the committed destination") {
		t.Fatalf("wrong destination not refused by name: %v", err)
	}
}

// Every shape the mint could read differently is refused by name at all three
// layers: consensus admission, settle verification and the signing policy.
func TestStrictTemplateOutputRefusedEverywhere(t *testing.T) {
	nonce := strings.Repeat("ef", 31)
	type strictCase struct {
		name string
		// secret builds the settle output; index makes each output's nonce
		// distinct for the accepted case.
		secret      func(seller, attacker, committee, nonce string) string
		admitSubstr string // "" means admission must accept
		signSubstr  string // "" means the signing policy must accept (only set with admitSubstr "")
	}
	cases := []strictCase{
		{
			name:   "honest single-key SIG_INPUTS output accepted",
			secret: func(seller, attacker, committee, nonce string) string { return p2pkOut(seller, nonce) },
		},
		{
			name: "crafted data=seller plus pubkeys=bidder n_sigs=1",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","tags":[["pubkeys","%s"],["n_sigs","1"],["sigflag","SIG_INPUTS"]]}]`, nonce, seller, attacker)
			},
			admitSubstr: "must not name additional pubkeys",
			signSubstr:  "must not name additional pubkeys",
		},
		{
			name: "pubkeys alone",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","tags":[["pubkeys","%s"],["sigflag","SIG_INPUTS"]]}]`, nonce, seller, attacker)
			},
			admitSubstr: "must not name additional pubkeys",
			signSubstr:  "must not name additional pubkeys",
		},
		{
			name: "locktime",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","tags":[["locktime","9999999999"],["sigflag","SIG_INPUTS"]]}]`, nonce, seller)
			},
			admitSubstr: "must not carry a locktime",
			signSubstr:  "must not carry a locktime",
		},
		{
			name: "refund path",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","tags":[["refund","%s"],["sigflag","SIG_INPUTS"]]}]`, nonce, seller, attacker)
			},
			admitSubstr: "must not carry a refund path",
			signSubstr:  "must not carry a refund path",
		},
		{
			name: "n_sigs_refund",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","tags":[["n_sigs_refund","1"],["sigflag","SIG_INPUTS"]]}]`, nonce, seller)
			},
			admitSubstr: "must not carry an n_sigs_refund",
			signSubstr:  "must not carry an n_sigs_refund",
		},
		{
			name: "duplicate data field",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","data":"%s","tags":[["sigflag","SIG_INPUTS"]]}]`, nonce, seller, attacker)
			},
			admitSubstr: "duplicate key",
			signSubstr:  "duplicate key",
		},
		{
			name: "uppercase hex nonce",
			secret: func(seller, attacker, committee, nonce string) string {
				return p2pkOut(seller, strings.ToUpper(nonce))
			},
			admitSubstr: "nonce is not 32 bytes of lowercase hex",
			signSubstr:  "nonce is not 32 bytes of lowercase hex",
		},
		{
			name: "extra JSON field",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","expiry":"1","tags":[["sigflag","SIG_INPUTS"]]}]`, nonce, seller)
			},
			admitSubstr: "unexpected key",
			signSubstr:  "unexpected key",
		},
		{
			name: "missing tags field",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s"}]`, nonce, seller)
			},
			admitSubstr: `missing "tags"`,
			signSubstr:  `missing "tags"`,
		},
		{
			name: "non-canonical serialisation whitespace",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK", {"nonce":"%s","data":"%s","tags":[["sigflag","SIG_INPUTS"]]}]`, nonce, seller)
			},
			admitSubstr: "not canonical",
			signSubstr:  "not canonical",
		},
		{
			name: "n_sigs two",
			secret: func(seller, attacker, committee, nonce string) string {
				return fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","tags":[["n_sigs","2"],["sigflag","SIG_INPUTS"]]}]`, nonce, seller)
			},
			admitSubstr: "n_sigs must be 1",
			signSubstr:  "n_sigs must be 1",
		},
		{
			name: "re-locks the escrow to the committee key",
			secret: func(seller, attacker, committee, nonce string) string {
				return p2pkOut(committee, nonce)
			},
			admitSubstr: "re-locks the escrow to the committee key",
			signSubstr:  "not locked to the committed destination",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, m, id := liveApp(t)
			bob := newBidder(t)
			au := a.st.Auctions[id]
			attacker := realPub(t, 21)
			crafted := func(index int) string {
				// 62 lowercase hex chars + a 2-char index = a 32-byte nonce.
				return tc.secret(au.SellerWallet, attacker, a.st.Params.GroupKey, fmt.Sprintf("%s%02x", nonce, index))
			}
			ev := craftedBidEvent(t, a, m, id, bob, func(c *BidContent) {
				for i := range c.OutcomeTemplates.Settle.Outputs {
					c.OutcomeTemplates.Settle.Outputs[i].Secret = crafted(i)
				}
			})
			admitErr := a.st.applyBid(ev, tNow, 11, true, true)
			if tc.admitSubstr == "" {
				if admitErr != nil {
					t.Fatalf("honest settle template refused at admission: %v", admitErr)
				}
			} else if admitErr == nil || !strings.Contains(admitErr.Error(), tc.admitSubstr) {
				t.Fatalf("admission: want refusal naming %q, got %v", tc.admitSubstr, admitErr)
			}

			// The settle verifier reads the same secret through the same parser.
			secret := crafted(0)
			accepted := a.st.recipientMatches(secret, au.SellerWallet)
			if tc.admitSubstr != "" && accepted {
				t.Fatal("settle verifier accepted a secret admission refuses")
			}
			if tc.admitSubstr == "" && !accepted {
				t.Fatal("settle verifier refused the honest wallet shape")
			}

			// The signing policy must not sign what admission refuses.
			signSecret := secret
			signAmount := int64(128)
			if tc.admitSubstr == "" {
				signSecret = p2pkOut(au.SellerWallet, strings.Repeat("ef", 32))
			}
			bidID := insertCraftedSettle(t, a, id, signSecret, signAmount)
			_, code, signErr := releasePolicy(a, SignReq{Bid: bidID, Outcome: "settle"})
			if tc.admitSubstr == "" {
				if code != 200 {
					t.Fatalf("signing policy refused the honest wallet shape: %d %v", code, signErr)
				}
			} else {
				if code == 200 || signErr == nil || !strings.Contains(signErr.Error(), tc.signSubstr) {
					t.Fatalf("signing: want refusal naming %q, got code %d err %v", tc.signSubstr, code, signErr)
				}
			}
		})
	}
}

// The denomination defect: an output amount that is not a key of the pinned
// keyset can never be signed by the mint. Admission and the signing policy
// both refuse it by name.
func TestStrictTemplateOutputNonDenominationRefused(t *testing.T) {
	a, m, id := liveApp(t)
	bob := newBidder(t)
	au := a.st.Auctions[id]
	// 150 = 128+16+4+2 at liveApp's denominations; move 2 sats from the 4-sat
	// output to the 2-sat one so the sum still conserves, but output 2 carries
	// a 3-sat amount the keyset cannot sign.
	ev := craftedBidEvent(t, a, m, id, bob, func(c *BidContent) {
		outs := c.OutcomeTemplates.Settle.Outputs
		for i := range outs {
			outs[i].Secret = p2pkOut(au.SellerWallet, fmt.Sprintf("%s%02x", strings.Repeat("ef", 31), i))
		}
		outs[2].Amount, outs[3].Amount = 3, 1
	})
	err := a.st.applyBid(ev, tNow, 11, true, true)
	if err == nil || !strings.Contains(err.Error(), "not a pinned keyset denomination") {
		t.Fatalf("non-denomination output not refused at admission by name: %v", err)
	}

	bidID := insertCraftedSettle(t, a, id, p2pkOut(au.SellerWallet, strings.Repeat("ab", 32)), 3)
	if _, code, signErr := releasePolicy(a, SignReq{Bid: bidID, Outcome: "settle"}); code == 200 ||
		signErr == nil || !strings.Contains(signErr.Error(), "not a pinned keyset denomination") {
		t.Fatalf("non-denomination output not refused before signing: code %d err %v", code, signErr)
	}
}

// A V4V split whose share cannot be decomposed into the pinned keyset's
// denominations can never settle at the mint, so admission refuses it by name.
func TestSigAllSplitShareNotRepresentableRefused(t *testing.T) {
	a, m, id := liveApp(t)
	au := a.st.Auctions[id]
	recipient := realPub(t, 21)
	split, err := validateV4VSplit([]SplitRow{splitRow("v4v", recipient, 5000)})
	if err != nil {
		t.Fatalf("fixture split invalid: %v", err)
	}
	au.V4VSplit = split
	// A 5000 bps share of a 150-sat bid is 75 = 64+8+2+1; without the 8-sat key
	// it cannot be represented. The bid's own proofs (128+16+4+2) still can.
	delete(a.st.Params.Keys, "8")
	bob := newBidder(t)
	ev, _ := m.bidTx(t, a, bob, id, 150, "d1")
	err = a.st.applyBid(ev, tNow, 11, true, true)
	if err == nil || !strings.Contains(err.Error(), "cannot be represented in the pinned keyset denominations") {
		t.Fatalf("an undecomposable split share was not refused by name at admission: %v", err)
	}
}

// The audit probe (verification-go-admission-test.go), permanently in the repo:
// data=committed seller key plus an extra pubkey with n_sigs=1. The mint would
// build the signer set as [seller, bidder] with one required signature, so the
// bidder alone could spend the seller's payout. It must be REFUSED everywhere.
func TestSigAllAdmissionRefusesAuditExtraPubkeyProbe(t *testing.T) {
	a, m, id := liveApp(t)
	bob := newBidder(t)
	au := a.st.Auctions[id]
	attacker := realPub(t, 21)
	secret := fmt.Sprintf(`["P2PK",{"nonce":"%s","data":"%s","tags":[["pubkeys","%s"],["n_sigs","1"],["sigflag","SIG_INPUTS"]]}]`,
		strings.Repeat("ef", 32), au.SellerWallet, attacker)

	ev := craftedBidEvent(t, a, m, id, bob, func(c *BidContent) {
		for i := range c.OutcomeTemplates.Settle.Outputs {
			c.OutcomeTemplates.Settle.Outputs[i].Secret = secret
		}
	})
	admitErr := a.st.applyBid(ev, tNow, 11, true, true)
	if admitErr == nil {
		t.Fatalf("admission ACCEPTED the extra-pubkey settle output (hole present); secret = %s", secret)
	}
	if !strings.Contains(admitErr.Error(), "must not name additional pubkeys") {
		t.Fatalf("admission refused for the wrong reason: %v", admitErr)
	}
	if a.st.recipientMatches(secret, au.SellerWallet) {
		t.Fatal("settle verifier accepted the extra-pubkey settle output")
	}
	bidID := insertCraftedSettle(t, a, id, secret, 128)
	if _, code, _ := releasePolicy(a, SignReq{Bid: bidID, Outcome: "settle"}); code == 200 {
		t.Fatal("signing policy agreed to sign the extra-pubkey settle output")
	}
}
