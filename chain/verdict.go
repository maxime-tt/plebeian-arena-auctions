package main

// The verdict-publish path: the WRITE half of the Nostr transport.  It
// publishes a validator's verdicts (kind 1445), the periodic state commitment
// (kind 1447), the ephemeral state pulse (kind 21447), the escrow TERMS
// (kind 1448), and the money RECEIPTS (kind 1443 refund / 1444 settle).  See
// notes/REPLY-nostr-verdict-contract.md and MERGED-PROTOCOL-SPEC.md §9, which
// fix this contract.
//
// THE RECEIPTS ARE THE MONEY FACTS.  A verdict CLAIMS an outcome; a receipt
// CARRIES the minted outputs the claim implies, so a client can claim the ecash
// from the relay alone (handover §3).  The out list MAY be published: every
// proof in it is P2PK-locked to the committed recipient named beside it, and
// spending needs that key, so the secret is not a bearer instrument here.  A
// receipt is anchored to the SETTLE's committed height and that block's time --
// the moment money moved -- not to the publishing pass's height.
//
// THE RULE THAT KEEPS IT HONEST: THE RELAY NOTIFIES, THE CHAIN DECIDES, THE
// ANCHOR PROVES.  The chain stays the source of truth for ordering, time and
// validity.  A verdict published here is a signed CLAIM about ALREADY-COMMITTED
// chain state; it is not a query result and it is not authoritative.  Three
// rules shape everything in this file, and they are non-negotiable:
//
//  1. PUBLISH ONLY AFTER COMMIT, ANCHORED TO THAT BLOCK.  A verdict carries the
//     committed height and the block hash of a COMMITTED block.  A claim a reorg
//     can contradict is worse than no claim, so we never publish for a pending
//     or uncommitted height.  The publisher (verdictpublish.go) enforces this by
//     reading ONLY the app's committed snapshot and refusing to emit a verdict
//     whose committed block hash it cannot yet read.
//
//  2. THE ANCHOR IS MANDATORY.  Every verdict and every state commitment
//     carries (height, hash).  A client MUST ignore an unanchored verdict --
//     ParseVerdict refuses it by name (ErrVerdictUnanchored), so the refusal is
//     assertable in a test.
//
//  3. VERDICTS ARE APPEND-ONLY.  Kind 1445 is a REGULAR kind (NIP-01 1000 <= n
//     < 10000): a relay stores every event and replaces nothing.  We do NOT
//     make 1445 parametrised-replaceable.  A lifecycle change (placed -> won ->
//     settled) is a NEW verdict event, and a client resolves latest-per-validator
//     for display.  This is what preserves the audit trail the whole security
//     argument rests on.
//
// Regular kinds, never the NIP-01 ephemeral range (20000-29999): khatru
// (`nak serve`) refuses an ephemeral EVENT with no matching live subscription
// ("mute: no one was listening for this") and does not buffer it.  A verdict
// that only exists while somebody happens to be subscribed is not a verdict.
//
// THE STATE PULSE (kind 21447) IS THE ONE EXCEPTION, and it is deliberate: it
// is a heartbeat, not a verdict, so its loss costs nothing and it is ephemeral
// by design (owner ruling, handover §8.1).  It never carries a verdict or a bid
// confirmation; the stored anchor stays the 1447 commitment.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

const (
	// VerdictSchema is the content schema of a kind-1445 verdict event.
	VerdictSchema = "arena.verdict.v1"
	// StateCommitSchema is the content schema of a kind-1447 state commitment.
	StateCommitSchema = "arena.statecommit.v1"
	// StatePulseSchema is the content schema of a kind-21447 state pulse.
	StatePulseSchema = "arena.statepulse.v1"
	// ParamsSchema is the content schema of a kind-1448 params event.
	ParamsSchema = "arena.params.v1"
	// RefundSchema is the content schema of a kind-1443 refund receipt.
	RefundSchema = "arena.refund.v1"
	// SettleSchema is the content schema of a kind-1444 settle receipt.
	SettleSchema = "arena.settle.v1"
	// DerivedFromCommittedState states the determinism claim ON THE WIRE: the
	// outcome is a pure function of committed state, not a validator opinion.
	DerivedFromCommittedState = "committed_state"
	// StateCommitMaxInterval is the default maximum WALL-CLOCK gap between two
	// stored state commitments.  It is the heartbeat bound: blocks are produced
	// on demand, so a block-height cadence has no wall-time bound at all and an
	// idle chain could leave its committed clock stale indefinitely (handover
	// §1).  The publisher (verdictpublish.go) also commits whenever the height
	// moves, which is the more frequent case.
	StateCommitMaxInterval = 10 * time.Second
	// StateCommitEvery is the default cadence, in blocks, for re-publishing the
	// PARAMS event (kind 1448) so a long-lived chain without a terms change
	// still refreshes it.  It is NO LONGER the state-commitment gate: the
	// commitment is gated on a verdict, the first pass, a height move or the
	// StateCommitMaxInterval heartbeat (handover §1.4).  Publisher.every is
	// kept only for this params cadence.
	StateCommitEvery int64 = 100
)

// The four outcomes the state machine can decide.  Kept as plain strings so the
// value on the wire and the value in code cannot drift.
const (
	OutcomeWinner        = "winner"
	OutcomeReserveNotMet = "reserve_not_met"
	OutcomeCancelled     = "cancelled"
	OutcomeAborted       = "aborted"
)

// Named refusals.  A client ignores an unanchored verdict; naming the refusal
// lets a test assert exactly which rule fired.
var (
	ErrVerdictKind                  = errors.New("not_a_verdict_event")
	ErrVerdictBadSchema             = errors.New("bad_verdict_schema")
	ErrVerdictUnanchored            = errors.New("unanchored_verdict")
	ErrVerdictBadSignature          = errors.New("bad_verdict_signature")
	ErrVerdictNotInRoster           = errors.New("verdict_not_in_roster")
	ErrCommitmentKind               = errors.New("not_a_state_commitment")
	ErrCommitmentBadSchema          = errors.New("bad_state_commitment_schema")
	ErrCommitmentBadSignature       = errors.New("bad_state_commitment_signature")
	ErrCommitmentUnanchored         = errors.New("unanchored_state_commitment")
	ErrPulseKind                    = errors.New("not_a_state_pulse")
	ErrPulseBadSchema               = errors.New("bad_state_pulse_schema")
	ErrPulseBadSignature            = errors.New("bad_state_pulse_signature")
	ErrPulseUnanchored              = errors.New("unanchored_state_pulse")
	ErrPulseNotInRoster             = errors.New("state_pulse_not_in_roster")
	ErrParamsKind                   = errors.New("not_a_params_event")
	ErrParamsBadSchema              = errors.New("bad_params_schema")
	ErrParamsBadSignature           = errors.New("bad_params_signature")
	ErrParamsUnanchored             = errors.New("unanchored_params_event")
	ErrParamsNotInRoster            = errors.New("params_not_in_roster")
	ErrReceiptKind                  = errors.New("not_a_receipt_event")
	ErrReceiptBadSchema             = errors.New("bad_receipt_schema")
	ErrReceiptBadSignature          = errors.New("bad_receipt_signature")
	ErrReceiptUnanchored            = errors.New("unanchored_receipt")
	ErrReceiptNotInRoster           = errors.New("receipt_not_in_roster")
	ErrReceiptBadOutcome            = errors.New("bad_receipt_outcome")
	ErrReceiptNoOutputs             = errors.New("receipt_has_no_outputs")
	ErrReceiptBadOutput             = errors.New("bad_receipt_output")
	ErrReceiptParamsVersionMismatch = errors.New("receipt_params_version_mismatch")
)

// ValidatorKey is one roster entry: a validator's Nostr pubkey, its moniker and
// its 1-based index.  The roster lives in Params, so it is inside the AppHash:
// a state commitment (kind 1447) commits to the roster at that height, which is
// why a client needs no separate roster-verification pass in this increment
// (REPLY §3.4).
type ValidatorKey struct {
	Pubkey string `json:"pubkey"` // x-only Nostr pubkey (64 lowercase hex)
	Name   string `json:"name"`
	Index  int    `json:"index"` // 1..N
}

// Roster returns the validator pubkeys as a set, for the client-side check.
func (p Params) Roster() map[string]ValidatorKey {
	out := make(map[string]ValidatorKey, len(p.Validators))
	for _, v := range p.Validators {
		out[v.Pubkey] = v
	}
	return out
}

// IsValidator reports whether pubkey is on the roster.
func (p Params) IsValidator(pubkey string) bool {
	_, ok := p.Roster()[pubkey]
	return ok
}

// Verdict is what committed state decides about one auction.  It is derived
// ONLY from committed state -- no clocks, no node-local data -- so two honest
// validators given the same state derive the same verdict.  That is the
// determinism property that makes a quorum of verdicts meaningful.
type Verdict struct {
	AuctionID string
	BidID     string // "" when the auction has no winning bid
	Outcome   string
}

// DeriveVerdict returns the verdict the committed state implies for auctionID,
// or ok=false when the auction has not closed (no verdict exists yet, so
// nothing should be published).  It reads only s, and it is deliberately total
// and side-effect free:
//
//   - a closed auction with a winning bid  -> winner (bid_id = the winning bid)
//   - a closed auction with no winning bid -> reserve_not_met (no bid_id)
//
// cancelled/aborted are reserved for the funding-abort path (kind 1446); they
// are not reachable from closeDue today, so this function never invents them.
// Because it is a pure function of committed state, two validators that agree on
// the state agree on the verdict -- the property the whole publish path rests
// on, and the first thing a reviewer should ask for.
func DeriveVerdict(s *State, auctionID string) (Verdict, bool) {
	au := s.Auctions[auctionID]
	if au == nil || au.Status != "closed" {
		return Verdict{}, false
	}
	if au.Winner != "" {
		return Verdict{AuctionID: au.ID, BidID: au.Winner, Outcome: OutcomeWinner}, true
	}
	return Verdict{AuctionID: au.ID, Outcome: OutcomeReserveNotMet}, true
}

// VerdictContent is the exact content JSON of a kind-1445 verdict event.
type VerdictContent struct {
	Schema            string  `json:"schema"`
	AuctionID         string  `json:"auction_id"`
	BidID             string  `json:"bid_id"`
	Outcome           string  `json:"outcome"`
	PayoutTokenSHA256 *string `json:"payout_token_sha256"` // null when no payout is claimed
	Height            int64   `json:"height"`
	BlockHash         string  `json:"block_hash"`
	DerivedFrom       string  `json:"derived_from"`
}

// ReceiptOut is one committed destination's ecash in a receipt: the proof the
// mint signed, plus the recipient key its P2PK lock names.  dleq has NO
// omitempty: the key must always be present on the wire, null when the mint
// gave none.  Routing the field through Proof would drop it, because Proof.DLEQ
// carries omitempty.
type ReceiptOut struct {
	Recipient string `json:"recipient"`
	Amount    int64  `json:"amount"`
	ID        string `json:"id"`
	Secret    string `json:"secret"`
	C         string `json:"C"`
	DLEQ      *DLEQ  `json:"dleq"` // null when the mint gave none
}

// ReceiptContent is the exact content JSON of a kind-1443 refund / kind-1444
// settle receipt: the MONEY FACT a validator publishes once an escrow has
// actually moved.  A verdict CLAIMS an outcome; a receipt CARRIES the minted
// outputs the claim implies, so a relay-only client can claim them without any
// HTTP path back to a validator (handover §3).
//
// height/settled_at are the SETTLE's committed height and that block's TIME,
// not the publishing pass's: they say WHEN the money moved, which is what makes
// a receipt a fact rather than a heartbeat.
type ReceiptContent struct {
	Schema        string       `json:"schema"`
	AuctionID     string       `json:"auction_id"`
	BidID         string       `json:"bid_id"`
	Outcome       string       `json:"outcome"` // refund | settle
	Height        int64        `json:"height"`
	BlockHash     string       `json:"block_hash"`
	SettledAt     int64        `json:"settled_at"`
	ParamsVersion string       `json:"params_version"`
	Out           []ReceiptOut `json:"out"`
}

// CheckParamsVersion refuses when the receipt's params_version differs from the
// digest of the terms the caller trusts: a payout is money under the TERMS in
// force, and a client must not accept one computed under other terms (handover
// §6).  It is the comparison between the terms the app trusts and the receipt
// it holds, and it must be testable.
func (c *ReceiptContent) CheckParamsVersion(p Params) error {
	if c.ParamsVersion != paramsVersion(p) {
		return ErrReceiptParamsVersionMismatch
	}
	return nil
}

// StateCommitmentContent is the exact content JSON of a kind-1447 state commit.
type StateCommitmentContent struct {
	Schema  string `json:"schema"`
	Height  int64  `json:"height"`
	AppHash string `json:"app_hash"`
	Time    int64  `json:"time"`
}

// StatePulseContent is the exact content JSON of a kind-21447 state pulse.  The
// anchor (height + app hash) is mandatory, exactly as on the commitment it
// twins; only the kind and the schema differ.  Time is the wall clock at which
// the pulse was signed and is what advances between two pulses at ONE height.
type StatePulseContent struct {
	Schema  string `json:"schema"`
	Height  int64  `json:"height"`
	AppHash string `json:"app_hash"`
	Time    int64  `json:"time"`
}

// ParamsContent is the exact content JSON of a kind-1448 params event: the
// escrow's TERMS, not a hash of them.  The market app reads every term from the
// relay alone (handover §4), so this event is the relay source for the FROST
// escrow key, the mint and keyset, the window and duration bounds, the proof
// cap, the threshold and the validator roster.  The anchor (height, app_hash)
// is the SAME one the commitment published in the same pass carries, so a
// client that reads params + height can tell they belong together: the terms
// here are the terms committed in THAT app hash (handover §4 consistency rule).
type ParamsContent struct {
	Schema        string            `json:"schema"`
	Height        int64             `json:"height"`
	AppHash       string            `json:"app_hash"`
	ParamsVersion string            `json:"params_version"`
	GroupKey      string            `json:"group_key"`
	MintURL       string            `json:"mint_url"`
	KeysetID      string            `json:"keyset_id"`
	Keys          map[string]string `json:"keys"`
	// SettlementSafetySec is how long after the end a bid itself establishes
	// the settlement must land; BidToConsensusLatencySec covers the bidder's
	// clock to the admitting block (spec §2.1).
	SettlementSafetySec      int64 `json:"settlement_safety_sec"`
	BidToConsensusLatencySec int64 `json:"bid_to_consensus_latency_sec"`
	// MinIncrementBps is the minimum raise over the current top, in basis
	// points of that top: admission requires max(1 sat, ceil(top * bps /
	// 10000)).  It is a committed term (it rides in the app hash with Params),
	// published here as the EFFECTIVE value, so a client reads 200 for the
	// default rather than the 0 that means "unset" inside Params.
	MinIncrementBps int64          `json:"min_increment_bps"`
	MinWindowSec    int64          `json:"min_window_sec"`
	MaxWindowSec    int64          `json:"max_window_sec"`
	MinDurationSec  int64          `json:"min_duration_sec"`
	MaxDurationSec  int64          `json:"max_duration_sec"`
	MaxProofs       int            `json:"max_proofs"`
	Threshold       int            `json:"threshold"`
	Validators      []ValidatorKey `json:"validators"`

	// The create-content rules below are CODE, not committed terms: they are
	// the limits applyCreate enforces on every kind-1440 create, published
	// here so a relay-only client can pre-validate a create against the same
	// numbers the chain enforces instead of discovering a refusal afterwards.
	// They are built from the constants in createrules.go, so a client reads
	// them as the rules THIS validator builds its params events with.
	//
	// They are deliberately NOT in Params: paramsVersion digests Params, and
	// these rules are not part of the app hash, so publishing them must leave
	// every params_version unchanged.  A client that wants the committed terms
	// reads params_version; these tell it what the chain will accept next.
	MaxTitleChars       int    `json:"max_title_chars"`
	MaxDescriptionChars int    `json:"max_description_chars"`
	MaxImageChars       int    `json:"max_image_chars"`
	MaxCategoryChars    int    `json:"max_category_chars"`
	MaxNameChars        int    `json:"max_name_chars"`
	ImageRule           string `json:"image_rule"`
	StartPriceMin       int64  `json:"start_price_min"`
	StartPriceMax       int64  `json:"start_price_max"`
	IncrementMin        int64  `json:"increment_min"`
	IncrementMax        int64  `json:"increment_max"`
}

// paramsVersion is the CONTENT DIGEST of a Params value: the sha256 hex of its
// canonical JSON.  It is NOT a counter -- there is no version number to bump,
// because the terms are simply whatever the committed state says they are at
// the height the event names.  A client compares the params it TRUSTS against
// the commitment it trusts by this value: the same digest means the terms it
// holds are the terms in that app hash, and a differing digest means they are
// stale.  json.Marshal is deterministic here (map keys sorted, struct fields in
// declaration order), so every validator computes the same digest for the same
// Params -- which is the whole point of putting it on the wire.
func paramsVersion(p Params) string {
	// Params cannot fail to marshal for committed state: it is strings, ints,
	// string maps and a json.RawMessage that already came from JSON.
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NostrPubkeyHex is the x-only Nostr pubkey (64 lowercase hex) for a secp256k1
// key.  This is the identity a validator signs its verdicts under, and the key
// a client roster-checks.
func NostrPubkeyHex(sk *btcec.PrivateKey) string {
	return hex.EncodeToString(schnorr.SerializePubKey(sk.PubKey()))
}

// verdictKeyFile is a validator's own verdict-signing key, written by
// `arena init` to that validator's home.  Like the FROST share it is per
// validator: a verdict signed under one key must stay distinguishable from a
// verdict signed under another, so there is never a shared publisher key.  One
// shared publisher would make a forged verdict indistinguishable from a real
// one, and the quorum argument would collapse.
type verdictKeyFile struct {
	Pubkey string `json:"pubkey"` // x-only Nostr pubkey (64 lowercase hex)
	Secret string `json:"secret"` // secp256k1 secret scalar (32-byte hex)
}

// loadVerdictKey reads a verdict-signing key from disk.
func loadVerdictKey(path string) (*btcec.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f verdictKeyFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(f.Secret)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("verdict key: secret must be 32-byte hex")
	}
	sk, _ := btcec.PrivKeyFromBytes(raw)
	if NostrPubkeyHex(sk) != f.Pubkey {
		return nil, errors.New("verdict key: pubkey does not match the secret")
	}
	return sk, nil
}

// SignEvent builds and BIP-340 signs a Nostr event with this module's canonical
// serializer, so the event id and signature are exactly what Event.Verify
// accepts.  It is the one signer both the verdict and the commitment use.
func SignEvent(sk *btcec.PrivateKey, kind int, content string, tags [][]string, at int64) (*Event, error) {
	ev := &Event{Pubkey: NostrPubkeyHex(sk), CreatedAt: at, Kind: kind, Tags: tags, Content: content}
	h := sha256.Sum256([]byte(ev.Serialize()))
	ev.ID = hex.EncodeToString(h[:])
	sig, err := schnorr.Sign(sk, h[:])
	if err != nil {
		return nil, err
	}
	ev.Sig = hex.EncodeToString(sig.Serialize())
	return ev, nil
}

// BuildVerdictEvent builds a signed kind-1445 verdict event.  The height and
// block hash are MANDATORY: an empty block hash or a non-positive height is an
// error here, so an unanchored verdict cannot be built at all.  The event is
// signed under sk -- the validator's OWN key.  There is never one shared
// publisher key: a forged verdict must stay distinguishable from a real one.
func BuildVerdictEvent(sk *btcec.PrivateKey, v Verdict, height int64, blockHash string, payoutTokenSHA256 *string, at int64) (*Event, error) {
	if height <= 0 || blockHash == "" {
		return nil, ErrVerdictUnanchored
	}
	if v.AuctionID == "" || v.Outcome == "" {
		return nil, fmt.Errorf("verdict needs an auction id and an outcome")
	}
	c := VerdictContent{
		Schema:            VerdictSchema,
		AuctionID:         v.AuctionID,
		BidID:             v.BidID,
		Outcome:           v.Outcome,
		PayoutTokenSHA256: payoutTokenSHA256,
		Height:            height,
		BlockHash:         blockHash,
		DerivedFrom:       DerivedFromCommittedState,
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	tags := [][]string{
		{"auction", v.AuctionID},
	}
	// The bid tag is omitted for a no-bid / aborted auction (REPLY §3.2).
	if v.BidID != "" {
		tags = append(tags, []string{"bid", v.BidID})
	}
	tags = append(tags,
		[]string{"height", strconv.FormatInt(height, 10)},
		[]string{"hash", blockHash},
		[]string{"outcome", v.Outcome},
		[]string{"p", NostrPubkeyHex(sk)},
	)
	return SignEvent(sk, KindOutcome, string(body), tags, at)
}

// ParseVerdict reads a kind-1445 event as a client would: it verifies the
// signature, the schema, and the MANDATORY anchor, and refuses by name when the
// anchor is missing.  roster, when non-nil, additionally checks membership.
func ParseVerdict(ev *Event, roster map[string]ValidatorKey) (*VerdictContent, error) {
	if ev == nil || ev.Kind != KindOutcome {
		return nil, ErrVerdictKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVerdictBadSignature, err)
	}
	var c VerdictContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrVerdictBadSchema
	}
	if c.Schema != VerdictSchema {
		return nil, fmt.Errorf("%w: %q", ErrVerdictBadSchema, c.Schema)
	}
	// The anchor is read from the TAGS (indexable) and cross-checked against the
	// content, so a relay that re-framed one cannot hide the other.
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okH || !okHash || tagHeight == "" || tagHash == "" || c.Height <= 0 || c.BlockHash == "" {
		return nil, ErrVerdictUnanchored
	}
	if tagHeight != strconv.FormatInt(c.Height, 10) || tagHash != c.BlockHash {
		return nil, ErrVerdictUnanchored
	}
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrVerdictNotInRoster, ev.Pubkey)
	}
	return &c, nil
}

// BuildStateCommitmentEvent builds a signed kind-1447 state commitment.  The
// anchor (height + app hash) is mandatory, as on a verdict.
func BuildStateCommitmentEvent(sk *btcec.PrivateKey, height int64, appHashHex string, at int64) (*Event, error) {
	if height <= 0 || appHashHex == "" {
		return nil, ErrCommitmentUnanchored
	}
	c := StateCommitmentContent{Schema: StateCommitSchema, Height: height, AppHash: appHashHex, Time: at}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	tags := [][]string{
		{"height", strconv.FormatInt(height, 10)},
		{"hash", appHashHex},
		{"p", NostrPubkeyHex(sk)},
	}
	return SignEvent(sk, KindStateCommit, string(body), tags, at)
}

// ParseStateCommitment reads a kind-1447 event, verifying signature, schema and
// the mandatory anchor.
func ParseStateCommitment(ev *Event, roster map[string]ValidatorKey) (*StateCommitmentContent, error) {
	if ev == nil || ev.Kind != KindStateCommit {
		return nil, ErrCommitmentKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCommitmentBadSignature, err)
	}
	var c StateCommitmentContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrCommitmentBadSchema
	}
	if c.Schema != StateCommitSchema {
		return nil, fmt.Errorf("%w: %q", ErrCommitmentBadSchema, c.Schema)
	}
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okH || !okHash || tagHeight == "" || tagHash == "" || c.Height <= 0 || c.AppHash == "" {
		return nil, ErrCommitmentUnanchored
	}
	if tagHeight != strconv.FormatInt(c.Height, 10) || tagHash != c.AppHash {
		return nil, ErrCommitmentUnanchored
	}
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrVerdictNotInRoster, ev.Pubkey)
	}
	return &c, nil
}

// BuildStatePulseEvent builds a signed kind-21447 state pulse.  The anchor
// (height + app hash) is mandatory, exactly as on a state commitment, and the
// tags are the same height/hash/p triple; only the kind and the schema differ.
// The event is EPHEMERAL: the relay forwards it to live subscribers and stores
// nothing, so a pulse dropped for having no listener is a non-event.
func BuildStatePulseEvent(sk *btcec.PrivateKey, height int64, appHashHex string, at int64) (*Event, error) {
	if height <= 0 || appHashHex == "" {
		return nil, ErrPulseUnanchored
	}
	c := StatePulseContent{Schema: StatePulseSchema, Height: height, AppHash: appHashHex, Time: at}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	tags := [][]string{
		{"height", strconv.FormatInt(height, 10)},
		{"hash", appHashHex},
		{"p", NostrPubkeyHex(sk)},
	}
	return SignEvent(sk, KindStatePulse, string(body), tags, at)
}

// ParseStatePulse reads a kind-21447 event, verifying signature, schema and the
// mandatory anchor -- the same gate as ParseStateCommitment.
func ParseStatePulse(ev *Event, roster map[string]ValidatorKey) (*StatePulseContent, error) {
	if ev == nil || ev.Kind != KindStatePulse {
		return nil, ErrPulseKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPulseBadSignature, err)
	}
	var c StatePulseContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrPulseBadSchema
	}
	if c.Schema != StatePulseSchema {
		return nil, fmt.Errorf("%w: %q", ErrPulseBadSchema, c.Schema)
	}
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okH || !okHash || tagHeight == "" || tagHash == "" || c.Height <= 0 || c.AppHash == "" {
		return nil, ErrPulseUnanchored
	}
	if tagHeight != strconv.FormatInt(c.Height, 10) || tagHash != c.AppHash {
		return nil, ErrPulseUnanchored
	}
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrPulseNotInRoster, ev.Pubkey)
	}
	return &c, nil
}

// BuildParamsEvent builds a signed kind-1448 params event carrying the escrow's
// terms.  The anchor (height + app hash) is mandatory, exactly as on a state
// commitment, and it must be the same height and app hash as the commitment
// published in the same pass, so a client can tell the terms and the commitment
// belong together (handover §4).  The event is a STORED (regular) kind: a client
// that was offline still finds the current terms.
func BuildParamsEvent(sk *btcec.PrivateKey, p Params, height int64, appHashHex string, at int64) (*Event, error) {
	if height <= 0 || appHashHex == "" {
		return nil, ErrParamsUnanchored
	}
	c := ParamsContent{
		Schema: ParamsSchema, Height: height, AppHash: appHashHex, ParamsVersion: paramsVersion(p),
		GroupKey: p.GroupKey, MintURL: p.MintURL, KeysetID: p.KeysetID, Keys: p.Keys,
		SettlementSafetySec: p.SettlementSafetySec, BidToConsensusLatencySec: p.BidToConsensusLatencySec,
		MinIncrementBps: effectiveMinIncrementBps(p),
		MinWindowSec:    p.MinWindowSec, MaxWindowSec: p.MaxWindowSec,
		MinDurationSec: p.MinDurationSec, MaxDurationSec: p.MaxDurationSec,
		MaxProofs: p.MaxProofs, Threshold: p.Threshold, Validators: p.Validators,
		// From createrules.go: the same constants applyCreate enforces, never a
		// second copy of the numbers.
		MaxTitleChars: MaxTitleChars, MaxDescriptionChars: MaxDescriptionChars,
		MaxImageChars: MaxImageChars, MaxCategoryChars: MaxCategoryChars, MaxNameChars: MaxNameChars,
		ImageRule:     ImageRule,
		StartPriceMin: MinStartPrice, StartPriceMax: MaxStartPrice,
		IncrementMin: MinIncrement, IncrementMax: MaxIncrement,
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	tags := [][]string{
		{"height", strconv.FormatInt(height, 10)},
		{"hash", appHashHex},
		{"p", NostrPubkeyHex(sk)},
	}
	return SignEvent(sk, KindParams, string(body), tags, at)
}

// ParseParamsEvent reads a kind-1448 event as a client would: it verifies the
// signature and the schema, and cross-checks the tag anchor against the content
// anchor, refusing by name when they disagree -- so a relay that re-framed one
// cannot hide the other.  roster, when non-nil, additionally checks membership.
func ParseParamsEvent(ev *Event, roster map[string]ValidatorKey) (*ParamsContent, error) {
	if ev == nil || ev.Kind != KindParams {
		return nil, ErrParamsKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParamsBadSignature, err)
	}
	var c ParamsContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrParamsBadSchema
	}
	if c.Schema != ParamsSchema {
		return nil, fmt.Errorf("%w: %q", ErrParamsBadSchema, c.Schema)
	}
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okH || !okHash || tagHeight == "" || tagHash == "" || c.Height <= 0 || c.AppHash == "" {
		return nil, ErrParamsUnanchored
	}
	if tagHeight != strconv.FormatInt(c.Height, 10) || tagHash != c.AppHash {
		return nil, ErrParamsUnanchored
	}
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrParamsNotInRoster, ev.Pubkey)
	}
	return &c, nil
}

// receiptOutputs pairs each committed proof with the recipient key its P2PK
// lock names, via parseP2PK(secret).Data VERBATIM -- the same compressed form
// bid.Wallet carries, so a client can compare the two directly.  Only a plain
// P2PK is accepted: verifySettle's recipientMatches already refused any other
// shape at admission, so a failure here is defensive, not reachable on
// committed state.
func receiptOutputs(proofs []Proof) ([]ReceiptOut, error) {
	out := make([]ReceiptOut, len(proofs))
	for i, p := range proofs {
		l, err := parseP2PK(p.Secret)
		if err != nil || l.Locktime != 0 || len(l.Refund) != 0 || l.NSigs != 1 || len(l.Pubkeys) != 0 {
			return nil, fmt.Errorf("%w: output %d is not a plain P2PK", ErrReceiptBadOutput, i)
		}
		out[i] = ReceiptOut{Recipient: l.Data, Amount: p.Amount, ID: p.ID, Secret: p.Secret, C: p.C, DLEQ: p.DLEQ}
	}
	return out, nil
}

// BuildRefundEvent builds a signed kind-1443 refund receipt (schema
// arena.refund.v1, outcome refund).  The anchor is MANDATORY: the committed
// height the settle landed at, that block's hash and its TIME.  A non-positive
// height, an empty block hash or a non-positive settled_at refuses as
// ErrReceiptUnanchored, so an unanchored receipt cannot be built at all.  The
// event is signed under sk -- the validator's OWN key, never a shared publisher
// key, exactly like a verdict.
func BuildRefundEvent(sk *btcec.PrivateKey, rc ReceiptContent, at int64) (*Event, error) {
	return buildReceiptEvent(sk, rc, KindRefund, RefundSchema, "refund", at)
}

// BuildSettleEvent builds a signed kind-1444 settle receipt (schema
// arena.settle.v1, outcome settle).
func BuildSettleEvent(sk *btcec.PrivateKey, rc ReceiptContent, at int64) (*Event, error) {
	return buildReceiptEvent(sk, rc, KindSettle, SettleSchema, "settle", at)
}

// buildReceiptEvent is the one builder both receipt kinds share: the
// kind/schema/outcome triple is fixed by the caller, so a receipt can never be
// built with the other kind's schema or the other outcome's value.
func buildReceiptEvent(sk *btcec.PrivateKey, rc ReceiptContent, kind int, schema, outcome string, at int64) (*Event, error) {
	if rc.Height <= 0 || rc.BlockHash == "" || rc.SettledAt <= 0 {
		return nil, ErrReceiptUnanchored
	}
	if rc.AuctionID == "" || rc.BidID == "" {
		return nil, fmt.Errorf("%w: receipt needs an auction id and a bid id", ErrReceiptBadSchema)
	}
	if rc.Outcome != outcome {
		return nil, fmt.Errorf("%w: %q on a %s receipt", ErrReceiptBadOutcome, rc.Outcome, outcome)
	}
	if len(rc.Out) == 0 {
		return nil, ErrReceiptNoOutputs
	}
	if rc.ParamsVersion == "" {
		return nil, ErrReceiptParamsVersionMismatch
	}
	rc.Schema = schema
	body, err := json.Marshal(rc)
	if err != nil {
		return nil, err
	}
	// auction/bid/height/hash are the tags §3 fixes as what the app relies on;
	// outcome and p are the same two additions a verdict carries, and p is what
	// makes the roster check indexable.
	tags := [][]string{
		{"auction", rc.AuctionID},
		{"bid", rc.BidID},
		{"height", strconv.FormatInt(rc.Height, 10)},
		{"hash", rc.BlockHash},
		{"outcome", rc.Outcome},
		{"p", NostrPubkeyHex(sk)},
	}
	return SignEvent(sk, kind, string(body), tags, at)
}

// ParseRefundEvent reads a kind-1443 refund receipt as a client would.
func ParseRefundEvent(ev *Event, roster map[string]ValidatorKey) (*ReceiptContent, error) {
	return parseReceiptEvent(ev, KindRefund, RefundSchema, "refund", roster)
}

// ParseSettleEvent reads a kind-1444 settle receipt as a client would.
func ParseSettleEvent(ev *Event, roster map[string]ValidatorKey) (*ReceiptContent, error) {
	return parseReceiptEvent(ev, KindSettle, SettleSchema, "settle", roster)
}

// parseReceiptEvent is the one parser both receipt kinds share: it verifies the
// kind, the signature, the schema, the outcome-vs-kind pairing and the MANDATORY
// anchor, cross-checking the TAGS against the content so a relay that re-framed
// one cannot hide the other.  Each fault is refused by name.  roster, when
// non-nil, additionally checks membership.
func parseReceiptEvent(ev *Event, kind int, schema, outcome string, roster map[string]ValidatorKey) (*ReceiptContent, error) {
	if ev == nil || ev.Kind != kind {
		return nil, ErrReceiptKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReceiptBadSignature, err)
	}
	var c ReceiptContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrReceiptBadSchema
	}
	if c.Schema != schema {
		return nil, fmt.Errorf("%w: %q", ErrReceiptBadSchema, c.Schema)
	}
	if c.Outcome != outcome {
		return nil, fmt.Errorf("%w: %q on a %s receipt", ErrReceiptBadOutcome, c.Outcome, outcome)
	}
	if c.AuctionID == "" || c.BidID == "" {
		return nil, fmt.Errorf("%w: receipt needs an auction id and a bid id", ErrReceiptBadSchema)
	}
	// The anchor is read from the TAGS (indexable) and cross-checked against the
	// content, so a relay that re-framed one cannot hide the other.
	tagAuction, okA := tagValue(ev.Tags, "auction")
	tagBid, okB := tagValue(ev.Tags, "bid")
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okA || !okB || !okH || !okHash {
		return nil, ErrReceiptUnanchored
	}
	if tagAuction != c.AuctionID || tagBid != c.BidID {
		return nil, fmt.Errorf("%w: tags name a different bid", ErrReceiptBadSchema)
	}
	if tagHeight != strconv.FormatInt(c.Height, 10) || tagHash != c.BlockHash {
		return nil, ErrReceiptUnanchored
	}
	if c.Height <= 0 || c.BlockHash == "" || c.SettledAt <= 0 {
		return nil, ErrReceiptUnanchored
	}
	if len(c.Out) == 0 {
		return nil, ErrReceiptNoOutputs
	}
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrReceiptNotInRoster, ev.Pubkey)
	}
	return &c, nil
}

// ClientOutcome is what a relay-only client reconstructs from verdict events
// alone: the resolved outcome for one auction, and the height it was anchored
// to.  latest-per-validator is resolved here (append-only: the newest anchored
// verdict per validator wins for DISPLAY).
type ClientOutcome struct {
	AuctionID string
	Outcome   string
	BidID     string
	Height    int64
	BlockHash string
	Signers   []string
}

// verdictClaim is one validator's newest anchored verdict for an auction.
type verdictClaim struct {
	content *VerdictContent
	at      int64
	id      string
}

// ResolveOutcome folds a stream of relay events into one auction's outcome,
// using ONLY data read from the relay.  It ignores anything that is not a
// well-formed, anchored, roster-signed verdict for that auction (ParseVerdict
// is the gate).  latest-per-validator wins; ties break on created_at then id,
// so the fold is deterministic.
func ResolveOutcome(auctionID string, events []*Event, roster map[string]ValidatorKey) (ClientOutcome, bool) {
	perValidator := map[string]verdictClaim{}
	for _, ev := range events {
		if ev == nil || ev.Kind != KindOutcome {
			continue
		}
		if a, ok := tagValue(ev.Tags, "auction"); !ok || a != auctionID {
			continue
		}
		c, err := ParseVerdict(ev, roster)
		if err != nil {
			continue // unanchored / off-roster / malformed: ignore
		}
		prev, seen := perValidator[ev.Pubkey]
		if !seen || ev.CreatedAt > prev.at || (ev.CreatedAt == prev.at && ev.ID > prev.id) {
			perValidator[ev.Pubkey] = verdictClaim{content: c, at: ev.CreatedAt, id: ev.ID}
		}
	}
	if len(perValidator) == 0 {
		return ClientOutcome{}, false
	}
	signers := make([]string, 0, len(perValidator))
	for pubkey := range perValidator {
		signers = append(signers, pubkey)
	}
	sort.Strings(signers)

	// The quorum view: the outcome the most validators signed, ties broken by
	// the lexicographically smallest outcome for determinism.  A demo client
	// trusts the quorum (see REPLY §2); this is the same security model as the
	// chain's, relocated to the transport.
	counts := map[string]int{}
	for _, pubkey := range signers {
		counts[perValidator[pubkey].content.Outcome]++
	}
	chosen, bestCount := "", -1
	for _, outcome := range sortedKeys(counts, func(k string) string { return k }) {
		if counts[outcome] > bestCount {
			chosen, bestCount = outcome, counts[outcome]
		}
	}
	// The newest anchored verdict among the validators that signed that view.
	var best verdictClaim
	for _, pubkey := range signers {
		cl := perValidator[pubkey]
		if cl.content.Outcome != chosen {
			continue
		}
		if best.content == nil || cl.at > best.at || (cl.at == best.at && cl.id > best.id) {
			best = cl
		}
	}
	out := ClientOutcome{AuctionID: best.content.AuctionID, Outcome: best.content.Outcome, BidID: best.content.BidID,
		Height: best.content.Height, BlockHash: best.content.BlockHash, Signers: signers}
	return out, true
}

// sortedKeys returns the keys of m sorted by key(k); it exists so every fold
// over a map in this file is deterministic.
func sortedKeys[V any](m map[string]V, key func(string) string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}
