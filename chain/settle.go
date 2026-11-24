package main

// Push settlement (ADR 0009, v0.5.0; 2-of-2 rework, spec §4/§5). The
// validators move escrows themselves the moment a block makes a move due,
// instead of waiting for the bidder's or seller's browser to ask:
//
//	refund  an outbid, lost or late (void) bid goes back to the bidder's
//	        wallet key, via the bidder's own pre-signed refund template;
//	settle  the winning bid of a closed lot goes to the seller's wallet key,
//	        via the bidder's own pre-signed settle template.
//
// There is no relock and no expiry: under the 2-of-2 lock a re-lock to the
// committee key would hand the committee a spendable output (spec §4), and a
// long lock freezes only the bidder's own funds (spec §2.3).
//
// The settler is an EXECUTOR, not an author (spec §5): the outputs are already
// fixed by the bidder's signature in the committed template, so the settler
// builds nothing and chooses nothing. Every validator re-derives the committed
// template's outputs from its own copy of the chain and signs that exact
// digest; the settler submits the template's own outputs with the bidder's
// pre-signature plus the collected committee signature. The mint only accepts
// the two transactions the bidder pre-signed, and the tx is recorded on chain
// so a diversion would be provable.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const settlePrefix = "settle:"

// settleTx records one outcome move: the escrow's committed proofs were spent
// at the mint and the outcome template's new proofs are recorded on chain.
type settleTx struct {
	Bid     string  `json:"bid"`
	Outcome string  `json:"outcome"` // settle | refund (the committed template submitted)
	Proofs  []Proof `json:"proofs"`  // the new ecash, with DLEQ (e, s, r)
	Sig     string  `json:"sig"`     // FROST committee signature on the swap (SIG_ALL)
}

func parseSettle(tx []byte) (*settleTx, bool) {
	if !bytes.HasPrefix(tx, []byte(settlePrefix)) || len(tx) > 256<<10 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(tx[len(settlePrefix):]))
	dec.DisallowUnknownFields()
	var st settleTx
	if dec.Decode(&st) != nil || dec.More() || !lowerHex(st.Bid, 64) || !lowerHex(st.Sig, 128) {
		return nil, false
	}
	if st.Outcome != "settle" && st.Outcome != "refund" {
		return nil, false
	}
	return &st, true
}

// settlementCarrier is the kind-1452 content: exactly the existing settleTx
// payload plus the schema string.  A courier publishes it; the chain turns it
// into the settle tx EventTransaction submits, so CheckTx/FinalizeBlock see the
// same parseSettle bytes as a direct settle tx.
type settlementCarrier struct {
	Schema  string  `json:"schema"`
	Bid     string  `json:"bid"`
	Outcome string  `json:"outcome"`
	Proofs  []Proof `json:"proofs"`
	Sig     string  `json:"sig"`
}

// parseSettlementCarrier validates a 1452 content string and returns the
// settleTx it carries.  The guards mirror parseSettle: lowercase ids, the two
// outcomes, 1..maxOutProofs(64) proofs, and the produced settle: bytes within
// 256 KiB.
func parseSettlementCarrier(content string) (*settleTx, error) {
	var c settlementCarrier
	dec := json.NewDecoder(strings.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSettlementCarrierBadPayload, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing JSON", ErrSettlementCarrierBadPayload)
	}
	if c.Schema != SettlementCarrierSchema {
		return nil, fmt.Errorf("%w: %q", ErrSettlementCarrierBadSchema, c.Schema)
	}
	if !lowerHex(c.Bid, 64) || !lowerHex(c.Sig, 128) {
		return nil, fmt.Errorf("%w: bid and sig must be lowercase hex", ErrSettlementCarrierBadPayload)
	}
	if c.Outcome != "settle" && c.Outcome != "refund" {
		return nil, fmt.Errorf("%w: outcome %q", ErrSettlementCarrierBadPayload, c.Outcome)
	}
	if len(c.Proofs) < 1 || len(c.Proofs) > 64 {
		return nil, fmt.Errorf("%w: proofs count %d", ErrSettlementCarrierBadPayload, len(c.Proofs))
	}
	st := &settleTx{Bid: c.Bid, Outcome: c.Outcome, Proofs: c.Proofs, Sig: c.Sig}
	if len(makeSettle(st)) > 256<<10 {
		return nil, fmt.Errorf("%w: settle tx exceeds 256 KiB", ErrSettlementCarrierBadPayload)
	}
	return st, nil
}

func makeSettle(st *settleTx) []byte {
	b, _ := json.Marshal(st)
	return append([]byte(settlePrefix), b...)
}

// outcomeDue says whether bid's committed outcome template may be submitted
// now, and to which key it pays. signing is true for the validators' signing
// policy (wall clock now), false for recording a settle tx that already
// carries the group signature: then the move is accepted whatever the bid's
// status became meanwhile, because the escrow has really moved and the chain
// must follow it.
//
//	refund  allowed once the bid is outbid, lost or void;
//	settle  allowed once the lot is closed and THIS leg won -- every leg of the
//	        winning position (plan §2.6), not only the leg au.Winner names.
func (s *State) outcomeDue(bid *Bid, outcome string, now int64, signing bool) (string, error) {
	if bid.Settled != "" {
		return "", errors.New("escrow already settled")
	}
	au := s.Auctions[bid.Auction]
	if au == nil {
		return "", errors.New("unknown auction")
	}
	switch outcome {
	case "refund":
		if bid.Status != "outbid" && bid.Status != "lost" && bid.Status != "void" {
			return "", fmt.Errorf("bid is %s, not refundable", bid.Status)
		}
		return bid.Wallet, nil
	case "settle":
		// Every leg of the winning position pays, not just the last one: a
		// winner's position is the sum of their leading legs, and each leg's
		// escrow is a separate SIG_ALL swap into ITS OWN committed template
		// (plan §2.6, §3.1). closeDue promotes exactly those legs to "won", so
		// the status IS the membership test -- au.Winner names only the leg
		// that set the top, and gating on it would strand the others.
		if au.Status != "closed" || bid.Status != "won" {
			return "", errors.New("only a winning leg of a closed auction pays out")
		}
		return au.SellerWallet, nil
	}
	return "", errors.New("outcome must be settle or refund")
}

// settlementDestinations derives, from committed chain state only, the
// destination of each output of a bid's outcome template: one recipient per
// committed V4V split row in order (the seller remainder last) for a settle
// with a split, otherwise the single committed key (the seller payout key for
// settle, the bidder's wallet key for refund). It is the single source the
// settle verifier and the signing policy both compare against.
func (s *State) settlementDestinations(bid *Bid, outcome string) ([]string, error) {
	au := s.Auctions[bid.Auction]
	if au == nil {
		return nil, errors.New("unknown auction")
	}
	switch outcome {
	case "refund":
		if bid.Wallet == "" {
			return nil, errors.New("bid committed no wallet spend key")
		}
		return []string{bid.Wallet}, nil
	case "settle":
		if len(au.V4VSplit) > 0 {
			rows, err := splitSats(au.SellerWallet, au.V4VSplit, bid.Amount, s.Params.Keys)
			if err != nil {
				return nil, fmt.Errorf("sigall settle template: %w", err)
			}
			out := make([]string, 0, len(rows))
			for _, row := range rows {
				out = append(out, row.Recipient)
			}
			return out, nil
		}
		if au.SellerWallet == "" {
			return nil, errors.New("auction committed no seller payout key")
		}
		return []string{au.SellerWallet}, nil
	}
	return nil, errors.New("outcome must be settle or refund")
}

// settlementOutputs turns a bid's committed outcome template into the SignOutput
// list the FROST message and the mint swap are built from: each output's B_ is
// recomputed from the committed secret and opening, so the committee signs
// exactly what the bidder pre-signed and never chooses an output of its own.
//
// Before it returns -- and therefore before any validator signs -- every output
// is held to the SAME strict parser admission uses: the canonical single-key
// P2PK shape, locked to the destination committed on chain for that index, at a
// denomination the mint can sign. A validator never signs a template it would
// not have admitted.
func (s *State) settlementOutputs(bid *Bid, outcome string) (sigAllSignedTemplate, []SignOutput, error) {
	template, err := sigAllTemplateNamed(bid.OutcomeTemplates, outcome)
	if err != nil {
		return sigAllSignedTemplate{}, nil, err
	}
	outs, err := sigAllTemplateSignOutputs(template)
	if err != nil {
		return sigAllSignedTemplate{}, nil, err
	}
	if len(outs) == 0 {
		return sigAllSignedTemplate{}, nil, errors.New("committed template has no outputs")
	}
	destinations, err := s.settlementDestinations(bid, outcome)
	if err != nil {
		return sigAllSignedTemplate{}, nil, err
	}
	if len(destinations) > 1 && len(destinations) != len(template.Outputs) {
		return sigAllSignedTemplate{}, nil, errors.New("committed v4v split does not match the committed template's outputs")
	}
	for index, out := range template.Outputs {
		if out.ID != s.Params.KeysetID {
			return sigAllSignedTemplate{}, nil, fmt.Errorf("sigall %s output %d names keyset %q, not the pinned %q", outcome, index, out.ID, s.Params.KeysetID)
		}
		if err := sigAllAmountPinned(out.Amount, s.Params); err != nil {
			return sigAllSignedTemplate{}, nil, fmt.Errorf("sigall %s output %d: %w", outcome, index, err)
		}
		dest := destinations[0]
		if len(destinations) > 1 {
			dest = destinations[index]
		}
		if err := strictTemplateOutput(out.Secret, dest); err != nil {
			return sigAllSignedTemplate{}, nil, fmt.Errorf("sigall %s output %d: %w", outcome, index, err)
		}
	}
	return template, outs, nil
}

// blinded is B_ = Y + r*G for a revealed secret and blinding factor.
func blinded(secret, rHex string) (string, error) {
	Y, err := hashToCurve([]byte(secret))
	if err != nil {
		return "", err
	}
	r, err := parseScalar(rHex)
	if err != nil {
		return "", errors.New("bad blinding factor")
	}
	return hex.EncodeToString(add(Y, mulG(r)).SerializeCompressed()), nil
}

type settleCheck struct {
	ys []string
}

// verifySettle checks a settle tx at block time now: the outcome is due, every
// new proof is a real mint signature (DLEQ) on an output the committed
// template names, and the group signature covers the old escrow and exactly
// those committed outputs. Nothing here reads a caller-supplied output: the
// outputs are re-derived from the committed template alone.
//
// Destination: a settle with a committed V4V split (spec §6) pays one output per
// committed row plus the seller remainder, so each output must be locked to ITS
// committed recipient; a settle with no split, and every refund, stays on the
// single committed destination from outcomeDue.
func (s *State) verifySettle(st *settleTx, now int64) (*settleCheck, error) {
	bid := s.Bids[st.Bid]
	if bid == nil {
		return nil, errors.New("unknown bid")
	}
	to, err := s.outcomeDue(bid, st.Outcome, now, false)
	if err != nil {
		return nil, err
	}
	if !isCompressedPub(to) {
		return nil, errors.New("outcome has no committed destination")
	}
	// expected[i] is the committed destination of output i, derived by the same
	// helper the signing policy uses: the single outcome destination, or one
	// recipient per committed split row.
	expected, err := s.settlementDestinations(bid, st.Outcome)
	if err != nil {
		return nil, err
	}
	template, outs, err := s.settlementOutputs(bid, st.Outcome)
	if err != nil {
		return nil, err
	}
	if len(expected) > 1 && len(expected) != len(outs) {
		return nil, errors.New("committed split does not match the committed template's outputs")
	}
	if len(st.Proofs) == 0 || len(st.Proofs) > maxOutProofs(s.Params) || len(st.Proofs) != len(outs) {
		return nil, errors.New("settle proofs do not match the committed template")
	}
	ys := make([]string, len(st.Proofs))
	var seen map[string]bool
	for i, pr := range st.Proofs {
		kh, ok := s.Params.Keys[fmt.Sprint(pr.Amount)]
		if !ok || pr.ID != s.Params.KeysetID {
			return nil, errors.New("proof from unpinned keyset or bad amount")
		}
		A, err := parsePub(kh)
		if err != nil {
			return nil, errors.New("bad keyset key")
		}
		if err := verifyDLEQ(pr, A); err != nil {
			return nil, err
		}
		// The new proof must be the committed template's own output: same
		// secret, and it must blind to the committed blinded point.
		if pr.Secret != template.Outputs[i].Secret {
			return nil, errors.New("outcome proof is not the committed template's output")
		}
		b, err := blinded(pr.Secret, pr.DLEQ.R)
		if err != nil {
			return nil, err
		}
		if b != outs[i].B {
			return nil, errors.New("outcome proof does not blind to the committed output")
		}
		// And it must be locked to the committed destination of THIS output: the
		// committed split row's recipient, or the single committed destination
		// (spec §4/§6).
		dest := expected[0]
		if len(expected) > 1 {
			dest = expected[i]
		}
		if !s.recipientMatches(pr.Secret, dest) {
			return nil, errors.New("outcome output is not locked to the committed destination")
		}
		y, err := proofY(pr.Secret)
		if err != nil {
			return nil, err
		}
		if seen == nil {
			seen = map[string]bool{}
		}
		if seen[y] {
			return nil, errors.New("duplicate outcome proof")
		}
		seen[y] = true
		if _, used := s.UsedY[y]; used {
			return nil, errors.New("proof already used")
		}
		ys[i] = y
	}
	h := sha256Sum(sigAllMessage(bid.Proofs, outs))
	if !schnorrVerifyHash(h[:], s.Params.GroupKey, st.Sig) {
		return nil, errors.New("group signature does not cover this settlement")
	}
	return &settleCheck{ys: ys}, nil
}

// recipientMatches reports whether a template output secret is the canonical
// single-key P2PK condition locked to key (x-only compared), through the SAME
// strictTemplateOutput parser admission and the signing policy use -- so a
// settle tx can never record ecash that pays anyone but the committed
// destination, in a shape those two would have accepted.
func (s *State) recipientMatches(secret, key string) bool {
	return strictTemplateOutput(secret, key) == nil
}

func (s *State) applySettle(st *settleTx, v *settleCheck) {
	bid := s.Bids[st.Bid]
	bid.Settled, bid.Out = st.Outcome, st.Proofs
	// A1b: record WHICH block moved the money.  FinalizeBlock sets
	// s.Height, s.Time = req.Height, req.Time.Unix() before it applies any tx, so
	// this is the block being finalized: deterministic, and identical on every
	// validator.
	bid.SettledHeight, bid.SettledAt = s.Height, s.Time
	for _, y := range v.ys {
		s.UsedY[y] = bid.ID
	}
}

// dueMove is one escrow the settler should move now: it names the committed
// outcome template to submit, never any output.
type dueMove struct {
	Bid     *Bid   `json:"bid"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
}

// settleWindowSec: escrows of lots closed longer ago than this are left to
// their owners (their pre-signed refund, or the last-resort locktime refund),
// so the due list stays short.
const settleWindowSec = 7 * 24 * 3600

// dueMoves lists every escrow that should move at wall time now, oldest first.
func (s *State) dueMoves(now int64) []dueMove {
	var out []dueMove
	for _, b := range s.Bids {
		if b.Settled != "" {
			continue
		}
		au := s.Auctions[b.Auction]
		if au == nil || (au.Status == "closed" && now-au.ClosedAt > settleWindowSec) {
			continue
		}
		for _, outcome := range []string{"refund", "settle"} {
			if to, err := s.outcomeDue(b, outcome, now, true); err == nil {
				out = append(out, dueMove{Bid: b, Outcome: outcome, To: to})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bid.Height != out[j].Bid.Height {
			return out[i].Bid.Height < out[j].Bid.Height
		}
		return out[i].Bid.ID < out[j].Bid.ID
	})
	return out
}

func sha256Sum(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// maxOutProofs bounds the committed template's output count a validator will
// accept, matching the bid's MaxProofs bound.
func maxOutProofs(params Params) int {
	n := params.MaxProofs
	if n <= 0 || n > 64 {
		n = 64
	}
	return n
}

// splitAmount writes n as a sum of the keyset's powers of two (largest first);
// nil when it cannot. The bidder's wallet uses this shape for its templates,
// and the tests use it to build them.
func splitAmount(n int64, keys map[string]string) []int64 {
	var out []int64
	for bit := int64(1) << 62; bit > 0; bit >>= 1 {
		if n&bit == 0 {
			continue
		}
		if _, ok := keys[fmt.Sprint(bit)]; !ok {
			return nil
		}
		out = append(out, bit)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) == 0 {
		return nil
	}
	return out
}
