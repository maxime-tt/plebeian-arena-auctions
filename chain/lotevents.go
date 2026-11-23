package main

// The lot-state and refusal events: kind 1449 (the committed end of a lot) and
// kind 1450 (the chain's own reason for dropping a client event).
//
// KIND 1449 CARRIES THREE SCHEMAS, told apart by the content schema string:
// "arena.lot.v1" is the lot's committed end (and, for an extending bid, the bid
// that moved it); "arena.bidadmit.v1" is the chain's per-bid ADMISSION fact --
// it ACCEPTED this bid, and the position that resulted -- which is the
// confirmation an app shows instead of guessing (R4); "arena.createadmit.v1" is
// the chain's per-lot ACCEPTANCE fact -- it ACCEPTED this lot at a committed
// height, so every validator knows which auctions are being followed.  All
// three shapes are anchored at a committed height with that block's hash and
// all are signed under a validator's OWN key, so one kind stays one anchor
// discipline while the schema tells a reader which fact it is looking at.
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
	// BidAdmitSchema is the content schema of a kind-1449 BID-ADMISSION event:
	// the per-bid admission fact the chain publishes when it ACCEPTS a bid, so
	// an app can confirm "the chain accepted this bid" from the wire instead of
	// inferring it (R4). It rides the lot-state kind 1449 -- the one the chain
	// already publishes about a lot's changing state -- and the schema string
	// says which of the two shapes the event carries.
	BidAdmitSchema = "arena.bidadmit.v1"
	// CreateAdmitSchema is the content schema of a kind-1449 CREATE-ADMISSION
	// event: the chain's own record that it ACCEPTED a lot -- the root create
	// it committed, at the height it committed it -- so every validator knows
	// which auctions it is following and which it is not. It rides the same
	// lot-state kind 1449 and the schema string tells the three shapes apart.
	CreateAdmitSchema = "arena.createadmit.v1"
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

	ErrBidAdmitKind         = errors.New("not_a_bid_admit_event")
	ErrBidAdmitBadSchema    = errors.New("bad_bid_admit_schema")
	ErrBidAdmitBadSignature = errors.New("bad_bid_admit_signature")
	ErrBidAdmitUnanchored   = errors.New("unanchored_bid_admit")
	ErrBidAdmitNotInRoster  = errors.New("bid_admit_not_in_roster")

	ErrCreateAdmitKind         = errors.New("not_a_create_admit_event")
	ErrCreateAdmitBadSchema    = errors.New("bad_create_admit_schema")
	ErrCreateAdmitBadSignature = errors.New("bad_create_admit_signature")
	ErrCreateAdmitUnanchored   = errors.New("unanchored_create_admit")
	ErrCreateAdmitNotInRoster  = errors.New("create_admit_not_in_roster")
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
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrLotNotInRoster, ev.Pubkey)
	}
	return &c, nil
}

// BidAdmitContent is the exact content JSON of a kind-1449 BID-ADMISSION event:
// the chain's own record that it ACCEPTED one bid (R4).  An app that published
// a bid can read this instead of guessing from silence.
//
// amount is THIS leg's delta -- what the leg locked, unchanged semantics.
// position is the position the bid belongs to: the sum of every leading leg of
// its bidder at admission, i.e. au.HighAmount while the bid leads and the
// settlement total once the lot closes; it is the number the floor was compared
// against.  high_bid is the leg that established that position -- the same id
// closeDue promotes to Winner.  bidder is the leg's bidder.  height/block_hash
// are the MANDATORY anchor: the bid's OWN committed height, not the publishing
// pass's.
type BidAdmitContent struct {
	Schema    string `json:"schema"`
	AuctionID string `json:"auction_id"`
	BidID     string `json:"bid_id"`
	Bidder    string `json:"bidder"`
	Amount    int64  `json:"amount"`
	Position  int64  `json:"position"`
	HighBid   string `json:"high_bid"`
	Height    int64  `json:"height"`
	BlockHash string `json:"block_hash"`
}

// check enforces the field rules the builder and the parser share: an
// unanchorable admission is neither buildable nor acceptable, and an admission
// whose position is below the bid's own delta would contradict the floor the
// chain applied.
func (c *BidAdmitContent) check() error {
	switch {
	case c.AuctionID == "" || c.BidID == "" || c.Bidder == "":
		return fmt.Errorf("%w: admission needs an auction, a bid and a bidder", ErrBidAdmitBadSchema)
	case c.Amount <= 0:
		return fmt.Errorf("%w: a bid's amount is a positive delta", ErrBidAdmitBadSchema)
	case c.Position < c.Amount:
		return fmt.Errorf("%w: position %d is below the bid's own amount %d", ErrBidAdmitBadSchema, c.Position, c.Amount)
	case c.HighBid == "":
		return fmt.Errorf("%w: admission needs the leading leg", ErrBidAdmitBadSchema)
	case c.Height <= 0 || c.BlockHash == "":
		return ErrBidAdmitUnanchored
	}
	return nil
}

// BuildBidAdmitEvent builds a signed kind-1449 bid-admission event.  The schema
// is fixed here and the anchor is MANDATORY, exactly as on a lot event; it is
// signed under sk, the validator's OWN key.
func BuildBidAdmitEvent(sk *btcec.PrivateKey, c BidAdmitContent, at int64) (*Event, error) {
	c.Schema = BidAdmitSchema
	if err := c.check(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	// e names the admitted bid (indexable, as NIP-01 intends); auction/height/
	// hash are the same anchor identity a lot event carries, and p is the
	// validator identity the roster check indexes.
	tags := [][]string{
		{"e", c.BidID},
		{"auction", c.AuctionID},
		{"height", strconv.FormatInt(c.Height, 10)},
		{"hash", c.BlockHash},
		{"p", NostrPubkeyHex(sk)},
	}
	return SignEvent(sk, KindLot, string(body), tags, at)
}

// ParseBidAdmitEvent reads a kind-1449 bid-admission event as a client would:
// it verifies the kind, the signature, the schema, the field rules and the
// MANDATORY anchor, cross-checking the e and anchor tags against the content.
// roster, when non-nil, additionally checks membership.  A pure lot event (or
// any other schema) is refused by name, so a reader can tell the two shapes of
// kind 1449 apart.
func ParseBidAdmitEvent(ev *Event, roster map[string]ValidatorKey) (*BidAdmitContent, error) {
	if ev == nil || ev.Kind != KindLot {
		return nil, ErrBidAdmitKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBidAdmitBadSignature, err)
	}
	var c BidAdmitContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrBidAdmitBadSchema
	}
	if c.Schema != BidAdmitSchema {
		return nil, fmt.Errorf("%w: %q", ErrBidAdmitBadSchema, c.Schema)
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	tagEvent, okE := tagValue(ev.Tags, "e")
	tagAuction, okA := tagValue(ev.Tags, "auction")
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okE || !okA || !okH || !okHash {
		return nil, ErrBidAdmitUnanchored
	}
	if tagEvent != c.BidID {
		return nil, fmt.Errorf("%w: e tag names %s, content names %s", ErrBidAdmitBadSchema, tagEvent, c.BidID)
	}
	if tagAuction != c.AuctionID {
		return nil, fmt.Errorf("%w: tags name a different lot", ErrBidAdmitBadSchema)
	}
	if tagHeight != strconv.FormatInt(c.Height, 10) || tagHash != c.BlockHash {
		return nil, ErrBidAdmitUnanchored
	}
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrBidAdmitNotInRoster, ev.Pubkey)
	}
	return &c, nil
}

// CreateAdmitContent is the exact content JSON of a kind-1449 CREATE-ADMISSION
// event: the chain's own record that it ACCEPTED one lot -- the root create it
// committed -- so a validator (and any reader) knows which auctions are being
// followed instead of inferring one from silence.  This is the fact the fee
// announcements and a max-concurrent-lots bound later hang off.
//
// seller is the root create's author (its pubkey in the coordinator's hex),
// root_event_id the id of the FIRST 1440 event for (seller, d) -- the lot's
// stable root, which bids and supersessions both bind to.  accepted_height is
// the committed height the root create was applied at; block_hash is THAT
// block's hash.  They are the MANDATORY anchor, and they NEVER move for an
// accepted lot: a supersession changes a lot's display fields, not when it was
// accepted.  end_time is the lot's end as it stands at publication and
// original_end_time the end the create implied (Auction.OriginalEnd), so a
// reader can tell a later anti-snipe extension from the schedule.  mode is the
// lot's committed mode ("fixed" | "antisnipe"), so a reader knows whether an
// extension is possible at all.
type CreateAdmitContent struct {
	Schema          string `json:"schema"`
	AuctionID       string `json:"auction_id"`
	Seller          string `json:"seller"`
	RootEventID     string `json:"root_event_id"`
	AcceptedHeight  int64  `json:"accepted_height"`
	BlockHash       string `json:"block_hash"`
	EndTime         int64  `json:"end_time"`
	OriginalEndTime int64  `json:"original_end_time"`
	Mode            string `json:"mode"`
}

// check enforces the field rules the builder and the parser share: an
// unanchorable acceptance is neither buildable nor acceptable, an acceptance
// without a lot, a seller or a root says nothing a client can index, and a mode
// outside the two the chain commits would describe a lot that cannot exist.
func (c *CreateAdmitContent) check() error {
	switch {
	case c.AuctionID == "" || c.Seller == "" || c.RootEventID == "":
		return fmt.Errorf("%w: acceptance needs an auction, a seller and a root", ErrCreateAdmitBadSchema)
	case c.Mode != "fixed" && c.Mode != "antisnipe":
		return fmt.Errorf("%w: mode %q is neither fixed nor antisnipe", ErrCreateAdmitBadSchema, c.Mode)
	case c.EndTime <= 0 || c.OriginalEndTime <= 0:
		return fmt.Errorf("%w: acceptance needs a positive end and original end", ErrCreateAdmitBadSchema)
	case c.AcceptedHeight <= 0 || c.BlockHash == "":
		return ErrCreateAdmitUnanchored
	}
	return nil
}

// BuildCreateAdmitEvent builds a signed kind-1449 create-admission event.  The
// schema is fixed here and the anchor is MANDATORY, exactly as on a lot or a
// bid-admission event; it is signed under sk, the validator's OWN key.
func BuildCreateAdmitEvent(sk *btcec.PrivateKey, c CreateAdmitContent, at int64) (*Event, error) {
	c.Schema = CreateAdmitSchema
	if err := c.check(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	// e names the lot's ROOT (the same anchor a bid carries, so a reader can
	// join acceptance to bids by root); auction/height/hash are the same anchor
	// identity the other two shapes carry, and p is the validator identity the
	// roster check indexes.
	tags := [][]string{
		{"e", c.RootEventID},
		{"auction", c.AuctionID},
		{"height", strconv.FormatInt(c.AcceptedHeight, 10)},
		{"hash", c.BlockHash},
		{"p", NostrPubkeyHex(sk)},
	}
	return SignEvent(sk, KindLot, string(body), tags, at)
}

// ParseCreateAdmitEvent reads a kind-1449 create-admission event as a client
// would: it verifies the kind, the signature, the schema, the field rules and
// the MANDATORY anchor, cross-checking the e and anchor tags against the
// content so a relay that re-framed one cannot hide the other.  roster, when
// non-nil, additionally checks membership.  A lot event or a bid-admission
// event (or any other schema) is refused by name, so a reader can tell the
// three shapes of kind 1449 apart.
func ParseCreateAdmitEvent(ev *Event, roster map[string]ValidatorKey) (*CreateAdmitContent, error) {
	if ev == nil || ev.Kind != KindLot {
		return nil, ErrCreateAdmitKind
	}
	if err := ev.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCreateAdmitBadSignature, err)
	}
	var c CreateAdmitContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return nil, ErrCreateAdmitBadSchema
	}
	if c.Schema != CreateAdmitSchema {
		return nil, fmt.Errorf("%w: %q", ErrCreateAdmitBadSchema, c.Schema)
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	tagEvent, okE := tagValue(ev.Tags, "e")
	tagAuction, okA := tagValue(ev.Tags, "auction")
	tagHeight, okH := tagValue(ev.Tags, "height")
	tagHash, okHash := tagValue(ev.Tags, "hash")
	if !okE || !okA || !okH || !okHash {
		return nil, ErrCreateAdmitUnanchored
	}
	if tagEvent != c.RootEventID {
		return nil, fmt.Errorf("%w: e tag names %s, content names %s", ErrCreateAdmitBadSchema, tagEvent, c.RootEventID)
	}
	if tagAuction != c.AuctionID {
		return nil, fmt.Errorf("%w: tags name a different lot", ErrCreateAdmitBadSchema)
	}
	if tagHeight != strconv.FormatInt(c.AcceptedHeight, 10) || tagHash != c.BlockHash {
		return nil, ErrCreateAdmitUnanchored
	}
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrCreateAdmitNotInRoster, ev.Pubkey)
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
	if !factSignerAllowed(ev, roster) {
		return nil, fmt.Errorf("%w: %s", ErrRefusedNotInRoster, ev.Pubkey)
	}
	return &c, nil
}
