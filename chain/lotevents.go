package main

// The lot-state and refusal events: kind 1449 (the committed end of a lot) and
// kind 1450 (the chain's own reason for dropping a client event).
//
// THE RECEIPT FAMILY IS THE TEMPLATE (verdict.go, "BuildRefundEvent" /
// "parseReceiptEvent").  Both kinds here are STORED, regular kinds signed under
// the validator's OWN key, carrying a MANDATORY anchor (height + that block's
// hash) in the tags AND in the content, cross-checked by the parser so a relay
// that re-framed one cannot hide the other.  Each fault is refused by name, so
// a test asserts exactly which rule fired.
//
// WHY THE ANCHOR IS PER-BID.  A lot's end moves: an anti-snipe bid resets it,
// and a later bid moves it again.  So "the lot's end" is not one fact at one
// height -- every extending bid COMMITTED an end at its own height.  Kind 1449
// therefore publishes, for each extending bid, that bid's established end
// (Bid.EstablishedEnd) and extension ordinal (Bid.ExtensionOrdinal), anchored at
// that bid's OWN committed height; and once per closed lot it publishes the
// final end, anchored at ClosedH, so a late reader learns where the lot ended
// even when no bid ever moved it.
//
// WHY KIND 1450 EXISTS.  A client whose 1440/1441 event the chain dropped needs
// the chain's OWN reason, not a silent absence: the reason string is published
// VERBATIM (for example "title required (max 120 chars)"), anchored at the
// height of the block that recorded the rejection.  Chat (1442) and every other
// kind are deliberately not covered -- a refusal event is for auction traffic.
//
// Both kinds are REGULAR (1000 <= n < 10000), never the ephemeral range khatru
// drops: a client that missed the live stream must still be able to read them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/btcsuite/btcd/btcec/v2"
)

const (
	// LotSchema is the content schema of a kind-1449 lot event.
	LotSchema = "arena.lot.v1"
	// RefusedSchema is the content schema of a kind-1450 refusal event.
	RefusedSchema = "arena.refused.v1"
)

// Named refusals, in the receipt style: a client ignores an unanchored event;
// naming the refusal lets a test assert exactly which rule fired.
var (
	ErrLotKind         = errors.New("not_a_lot_event")
	ErrLotBadSchema    = errors.New("bad_lot_schema")
	ErrLotBadSignature = errors.New("bad_lot_signature")
	ErrLotUnanchored   = errors.New("unanchored_lot")
	ErrLotNotInRoster  = errors.New("lot_not_in_roster")

	ErrRefusedKind         = errors.New("not_a_refused_event")
	ErrRefusedBadSchema    = errors.New("bad_refused_schema")
	ErrRefusedBadSignature = errors.New("bad_refused_signature")
	ErrRefusedUnanchored   = errors.New("unanchored_refused")
	ErrRefusedNotInRoster  = errors.New("refused_not_in_roster")
	ErrRefusedBadTarget    = errors.New("bad_refused_target_kind")
	ErrRefusedBadEvent     = errors.New("refused_names_a_different_event")
)

// LotContent is the exact content JSON of a kind-1449 lot event: the committed
// end of one lot, and -- for an extending bid -- the bid that moved it.
//
// end_time is the end in force at the anchor height: the bid's own established
// end when this event is about an extending bid, the lot's final EndAt when it
// is about a closed lot.  original_end_time is the end the CREATE implied
// (Auction.OriginalEnd), so a client can tell an extension from the schedule:
// the two differ exactly when the lot was extended.  extensions is the lot's
// extension count at the anchor.
type LotContent struct {
	Schema          string `json:"schema"`
	AuctionID       string `json:"auction_id"`
	EndTime         int64  `json:"end_time"`
	OriginalEndTime int64  `json:"original_end_time"`
	Extensions      int    `json:"extensions"`
	Height          int64  `json:"height"`
	BlockHash       string `json:"block_hash"`
}

// check enforces the field rules the builder and the parser share, refusing by
// name: an unanchorable event is not buildable and not acceptable, and a lot
// event without an auction, a positive end or a non-negative extension count
// says nothing a client can use.
func (c *LotContent) check() error {
	switch {
	case c.AuctionID == "":
		return fmt.Errorf("%w: lot needs an auction id", ErrLotBadSchema)
	case c.EndTime <= 0 || c.OriginalEndTime <= 0:
		return fmt.Errorf("%w: lot needs a positive end and original end", ErrLotBadSchema)
	case c.Extensions < 0:
		return fmt.Errorf("%w: extensions must not be negative", ErrLotBadSchema)
	case c.Height <= 0 || c.BlockHash == "":
		return ErrLotUnanchored
	}
	return nil
}

// RefusedContent is the exact content JSON of a kind-1450 refusal event: the
// chain's own reason for dropping one client event, VERBATIM.  kind is the
// dropped event's kind and is limited to auction traffic (create 1440, bid
// 1441); the anchor is the height of the block whose rejection record named it.
type RefusedContent struct {
	Schema    string `json:"schema"`
	EventID   string `json:"event_id"`
	Kind      int    `json:"kind"`
	Reason    string `json:"reason"`
	Height    int64  `json:"height"`
	BlockHash string `json:"block_hash"`
}

// check enforces the field rules the builder and the parser share.  Only the
// two auction kinds a client acts on are covered: a chat refusal is not an
// answer anyone needs on the relay.
func (c *RefusedContent) check() error {
	switch {
	case c.EventID == "":
		return fmt.Errorf("%w: refusal needs an event id", ErrRefusedBadSchema)
	case c.Kind != KindCreate && c.Kind != KindBid:
		return fmt.Errorf("%w: %d", ErrRefusedBadTarget, c.Kind)
	case c.Reason == "":
		return fmt.Errorf("%w: refusal needs a reason", ErrRefusedBadSchema)
	case c.Height <= 0 || c.BlockHash == "":
		return ErrRefusedUnanchored
	}
	return nil
}

// BuildLotEvent builds a signed kind-1449 lot event.  The anchor is MANDATORY
// and the schema is fixed here, so a lot event can never be built with another
// schema or without its block.  It is signed under sk -- the validator's OWN
// key, never a shared publisher key, exactly like a verdict or a receipt.
func BuildLotEvent(sk *btcec.PrivateKey, c LotContent, at int64) (*Event, error) {
	c.Schema = LotSchema
	if err := c.check(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	// auction/height/hash are the anchor and the lot's name; p is the
	// validator identity, which is what makes the roster check indexable.
	tags := [][]string{
		{"auction", c.AuctionID},
		{"height", strconv.FormatInt(c.Height, 10)},
		{"hash", c.BlockHash},
		{"p", NostrPubkeyHex(sk)},
	}
	return SignEvent(sk, KindLot, string(body), tags, at)
}

// ParseLotEvent reads a kind-1449 event as a client would: it verifies the
// kind, the signature, the schema, the field rules and the MANDATORY anchor,
// cross-checking the TAGS against the content so a relay that re-framed one
// cannot hide the other.  roster, when non-nil, additionally checks membership.
func ParseLotEvent(ev *Event, roster map[string]ValidatorKey) (*LotContent, error) {
	if ev == nil || ev.Kind != KindLot {
		return nil, ErrLotKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLotBadSignature, err)
	}
	var c LotContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrLotBadSchema
	}
	if c.Schema != LotSchema {
		return nil, fmt.Errorf("%w: %q", ErrLotBadSchema, c.Schema)
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	// The anchor is read from the TAGS (indexable) and cross-checked against the
	// content, so a relay that re-framed one cannot hide the other.
	tagAuction, okA := tagValue(ev.Tags, "auction")
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okA || !okH || !okHash {
		return nil, ErrLotUnanchored
	}
	if tagAuction != c.AuctionID {
		return nil, fmt.Errorf("%w: tags name a different lot", ErrLotBadSchema)
	}
	if tagHeight != strconv.FormatInt(c.Height, 10) || tagHash != c.BlockHash {
		return nil, ErrLotUnanchored
	}
	if roster != nil {
		if _, ok := roster[ev.Pubkey]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrLotNotInRoster, ev.Pubkey)
		}
	}
	return &c, nil
}

// BuildRefusedEvent builds a signed kind-1450 refusal event.  The reason is
// carried VERBATIM; the anchor is MANDATORY and the schema is fixed here.
func BuildRefusedEvent(sk *btcec.PrivateKey, c RefusedContent, at int64) (*Event, error) {
	c.Schema = RefusedSchema
	if err := c.check(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	// e names the dropped event (indexable, as NIP-01 intends), and
	// height/hash/p are the same anchor identity a lot event carries.
	tags := [][]string{
		{"e", c.EventID},
		{"height", strconv.FormatInt(c.Height, 10)},
		{"hash", c.BlockHash},
		{"p", NostrPubkeyHex(sk)},
	}
	return SignEvent(sk, KindRefused, string(body), tags, at)
}

// ParseRefusedEvent reads a kind-1450 event as a client would: it verifies the
// kind, the signature, the schema, the field rules and the MANDATORY anchor,
// cross-checking the e tag against event_id and the anchor tags against the
// content.  roster, when non-nil, additionally checks membership.
func ParseRefusedEvent(ev *Event, roster map[string]ValidatorKey) (*RefusedContent, error) {
	if ev == nil || ev.Kind != KindRefused {
		return nil, ErrRefusedKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRefusedBadSignature, err)
	}
	var c RefusedContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrRefusedBadSchema
	}
	if c.Schema != RefusedSchema {
		return nil, fmt.Errorf("%w: %q", ErrRefusedBadSchema, c.Schema)
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	tagEvent, okE := tagValue(ev.Tags, "e")
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okE || !okH || !okHash {
		return nil, ErrRefusedUnanchored
	}
	if tagEvent != c.EventID {
		return nil, fmt.Errorf("%w: e tag names %s, content names %s", ErrRefusedBadEvent, tagEvent, c.EventID)
	}
	if tagHeight != strconv.FormatInt(c.Height, 10) || tagHash != c.BlockHash {
		return nil, ErrRefusedUnanchored
	}
	if roster != nil {
		if _, ok := roster[ev.Pubkey]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrRefusedNotInRoster, ev.Pubkey)
		}
	}
	return &c, nil
}
