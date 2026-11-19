package main

// The auction state machine. Every validator runs this identically; the
// AppHash (sha256 of the canonical JSON state) proves they agree.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
)

type Params struct {
	MintURL        string            `json:"mintUrl"`
	KeysetID       string            `json:"keysetId"`
	Keys           map[string]string `json:"keys"`
	GroupKey       string            `json:"groupKey"`    // FROST 3-of-4 escrow key (compressed hex); no single validator holds it
	FrostPublic    json.RawMessage   `json:"frostPublic"` // FROST public key package: group key + each validator's verifying share
	ValidatorNames []string          `json:"validatorNames"`
	MinWindowSec   int64             `json:"minWindowSec"` // anti-snipe window bounds (ADR 0009)
	MaxWindowSec   int64             `json:"maxWindowSec"`
	// SettlementSafetySec is how long after the end a bid itself establishes
	// the settlement must land, before the bidder's unilateral refund branch
	// opens (spec §2.1). It replaces LockMarginSec as the lock driver.
	SettlementSafetySec int64 `json:"settlementSafetySec"`
	// BidToConsensusLatencySec covers the gap between the bidder's clock and
	// the block that admits the bid (spec §2.1). It replaces lockGraceSec.
	BidToConsensusLatencySec int64 `json:"bidToConsensusLatencySec"`
	// LockMarginSec is retired as a lock driver: SettlementSafetySec is the
	// lock rule now (spec §10). It is kept only so an older genesis still
	// parses; it drives nothing.
	LockMarginSec  int64 `json:"lockMarginSec,omitempty"`
	MinDurationSec int64 `json:"minDurationSec"`
	MaxDurationSec int64 `json:"maxDurationSec"`
	MaxProofs      int   `json:"maxProofs"`
	Threshold      int   `json:"threshold"`
	// Validators is the verdict-publish roster (spec §9, REPLY §3.4): each
	// validator's own Nostr pubkey, moniker and index.  It lives in Params, so
	// it is inside the AppHash -- a state commitment (kind 1447) commits to the
	// roster at that height, which is why a client needs no separate roster
	// verification in this increment.  A verdict is signed under the validator's
	// OWN key from this list, never a shared publisher key.
	Validators []ValidatorKey `json:"validators,omitempty"`
}

type Auction struct {
	ID           string   `json:"id"` // coordinate a = 1440:<seller>:<d>; the auction's stable identity
	Seller       string   `json:"seller"`
	SellerWallet string   `json:"sellerWallet"`
	SellerName   string   `json:"sellerName"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Image        string   `json:"image"`
	Category     string   `json:"category"`
	StartPrice   int64    `json:"startPrice"`
	MinIncrement int64    `json:"minIncrement"`
	CreatedAt    int64    `json:"createdAt"`
	CreatedH     int64    `json:"createdHeight"`
	EndAt        int64    `json:"endAt"`
	Mode         string   `json:"mode"`      // fixed | antisnipe (ADR 0009)
	WindowSec    int64    `json:"windowSec"` // antisnipe: a bid with less than this left resets the clock to it
	Status       string   `json:"status"`    // open | closed
	HighBid      string   `json:"highBid"`
	HighAmount   int64    `json:"highAmount"`
	Bids         []string `json:"bids"`
	Extensions   int      `json:"extensions"`
	Winner       string   `json:"winner"`          // bid id
	Voids        []string `json:"voids,omitempty"` // late bids, refundable at once (ADR 0007)
	ClosedAt     int64    `json:"closedAt"`
	ClosedH      int64    `json:"closedHeight"`
	// Root pinning and supersession (owner decision 2026-11-12, spec §8). The
	// auction's identity is (seller, d); the FIRST 1440 event for it is the
	// root and its id is auction_root_event_id. Later 1440 events are
	// supersessions gated by the immutable allow-list, and this struct always
	// holds the latest ACCEPTED version — the one resolution a regular kind
	// does not get from the relay.
	D                string `json:"d"`                // the d tag = auction_id
	RootID           string `json:"rootId"`           // auction_root_event_id
	VersionID        string `json:"versionId"`        // event id of the canonical (latest accepted) version
	VersionCreatedAt int64  `json:"versionCreatedAt"` // created_at of that version; gates supersession order
	DurationSec      int64  `json:"durationSec"`      // frozen duration; EndAt is its materialisation
	// V4VSplit is the committed value-for-value payout schedule (spec §6). It is
	// set once, at the create, and is an IMMUTABLE money field (spec §8): no
	// later event may change it. Each row names a payout recipient and an
	// integer basis-point share; the seller keeps 10000-sum(allocationBps). The
	// bidder's pre-signed SETTLE template must carry these exact outputs, so the
	// money can only reach these recipients (see verifySigAllBidTemplates).
	V4VSplit []CommittedSplitRow `json:"v4vSplit,omitempty"`
}

// OriginalEnd is the end time the CREATE implies: CreatedAt + DurationSec.
// Neither field ever moves -- applyCreate sets EndAt = now + c.DurationSec and
// CreatedAt = now, and a supersession never recomputes either -- so this is the
// lot's end as committed, whatever later anti-snipe bids did to EndAt.  A
// kind-1449 lot event carries it beside the CURRENT end so a reader can tell an
// extension from the original schedule.
func (au *Auction) OriginalEnd() int64 { return au.CreatedAt + au.DurationSec }

type Bid struct {
	ID       string  `json:"id"`
	Auction  string  `json:"auction"`
	Bidder   string  `json:"bidder"`
	Name     string  `json:"name"`
	Wallet   string  `json:"wallet"`
	Amount   int64   `json:"amount"`
	Proofs   []Proof `json:"proofs"`
	Locktime int64   `json:"locktime"`
	Height   int64   `json:"height"`
	Time     int64   `json:"time"`
	Status   string  `json:"status"` // leading | outbid | won | lost | void
	Extended bool    `json:"extended"`
	// The bidder's two pre-signed outcome transactions, carried verbatim in
	// the committed bid (spec §1.2/§2). Every validator re-derives each
	// template's digest and re-checks the bidder's signature at admission, so
	// the chain verifies the only two spends the escrow may ever make. A
	// template that re-locks to the committee key is forbidden (spec §4).
	OutcomeTemplates *sigAllOutcomeTemplates `json:"outcome_templates,omitempty"`
	// Push settlement (ADR 0009): the validators moved the escrow themselves.
	Settled string  `json:"settled,omitempty"` // refund | settle: the committed outcome template applied (parseSettle's bound)
	Out     []Proof `json:"out,omitempty"`     // that ecash, P2PK-locked to the recipient
	// The WHEN of the settle (A1b): the committed height the settle tx landed at
	// and that block's time.  omitempty is MANDATORY, not style: snapshot.go's
	// load path refuses any state whose canonical encoding does not re-encode
	// byte-identically, so a field without it would make every existing
	// data/app-state/*.json unreadable and stop the live root restarting.  Both
	// zero on every bid that has not settled, so an old snapshot loads as-is.
	SettledHeight int64 `json:"settledHeight,omitempty"` // the committed height the settle tx landed at
	SettledAt     int64 `json:"settledAt,omitempty"`     // that block's time (unix seconds)
	// The per-bid lot state (kind 1449): the end THIS bid committed by
	// extending the lot and the lot's extension count right after that move.
	// Both are needed because a later bid moves au.EndAt again, so the current
	// EndAt cannot say what an earlier bid established.  omitempty is
	// MANDATORY for the same reason as the settle when above: snapshot.go's
	// load path refuses a state whose canonical encoding does not re-encode
	// byte-identically, so a field without it would make every existing
	// data/app-state/*.json unreadable.  Both are zero on every bid that did
	// not extend, so an old snapshot loads as-is.
	EstablishedEnd   int64 `json:"establishedEnd,omitempty"`   // the end this bid committed by extending the lot
	ExtensionOrdinal int   `json:"extensionOrdinal,omitempty"` // the lot's extension count after this bid's move (1 for the first)
}

type Rejection struct {
	Height int64  `json:"height"`
	Time   int64  `json:"time"`
	Kind   int    `json:"kind"`
	Pubkey string `json:"pubkey"`
	Event  string `json:"event"`
	Reason string `json:"reason"`
}

// Height and Time are kept in memory and published, but hashed as zero
// (canonical): an empty block then leaves the app hash unchanged, so
// CometBFT does not need a block to prove it (ADR 0003 §1). They must stay
// the first two fields: publish() rewrites that prefix.
type State struct {
	Height     int64               `json:"height"`
	Time       int64               `json:"time"`
	Params     Params              `json:"params"`
	Auctions   map[string]*Auction `json:"auctions"`
	Order      []string            `json:"order"`
	Bids       map[string]*Bid     `json:"bids"`
	UsedY      map[string]string   `json:"usedY"`
	Rejections []Rejection         `json:"rejections"`
	TxCount    int64               `json:"txCount"`
}

func newState() *State {
	return &State{Auctions: map[string]*Auction{}, Bids: map[string]*Bid{}, UsedY: map[string]string{}, Rejections: []Rejection{}, Order: []string{}}
}

// canonical is the hashed encoding: the state with Height and Time zeroed.
// Go sorts map keys, so it is deterministic.
func (s *State) canonical() []byte {
	c := *s
	c.Height, c.Time = 0, 0
	b, _ := json.Marshal(&c)
	return b
}

func (s *State) hash() []byte {
	h := sha256.Sum256(s.canonical())
	return h[:]
}

const canonPrefix = `{"height":0,"time":0,`

// publish turns canonical bytes into the published state by filling in the
// height and time.
func publish(canon []byte, h, t int64) []byte {
	if !bytes.HasPrefix(canon, []byte(canonPrefix)) {
		return canon
	}
	out := make([]byte, 0, len(canon)+24)
	out = append(out, `{"height":`...)
	out = strconv.AppendInt(out, h, 10)
	out = append(out, `,"time":`...)
	out = strconv.AppendInt(out, t, 10)
	out = append(out, ',')
	return append(out, canon[len(canonPrefix):]...)
}

type App struct {
	abci.BaseApplication
	mu         sync.RWMutex
	st         *State
	committed  []byte  // canonical JSON of the last committed state (Height, Time zero)
	committedH int64   // height of that snapshot
	committedT int64   // its block time
	pending    []byte  // snapshot made in FinalizeBlock, published at Commit
	ledger     *ledger // per-block tx archive (ADR 0003 §4); nil in tests
	retain     int64   // keep this many blocks below the newest snapshot; 0 keeps all
	pendLines  []ledgerLine
	appHash    []byte
	// hash of committed; Info reports it with committedH (ADR 0003 §2)
	committedHash []byte
	snap          *snapshotter  // nil: no snapshots (tests)
	commitCh      chan struct{} // closed and replaced at every Commit (for /height?after=)
}

// waitCommit blocks until the committed height is above after, the context
// ends or max passes; it returns the committed height then.
func (a *App) waitCommit(ctx context.Context, after int64, max time.Duration) int64 {
	t := time.NewTimer(max)
	defer t.Stop()
	for {
		a.mu.Lock()
		h := a.committedH
		if a.commitCh == nil {
			a.commitCh = make(chan struct{})
		}
		ch := a.commitCh
		a.mu.Unlock()
		if h > after {
			return h
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return h
		case <-t.C:
			return h
		}
	}
}

func NewApp() *App { return &App{st: newState()} }

// Snapshot is the published committed state (with its height and time).
func (a *App) Snapshot() []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return publish(a.committed, a.committedH, a.committedT)
}

func (a *App) Read(f func(s *State)) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	f(a.st)
}

func (a *App) Info(context.Context, *abci.InfoRequest) (*abci.InfoResponse, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	// The committed height and hash: 0 on a fresh start (CometBFT replays
	// from genesis), or a restored snapshot's (it replays only what follows).
	return &abci.InfoResponse{Data: "arena-auctions", Version: "0.5.2", AppVersion: 1,
		LastBlockHeight: a.committedH, LastBlockAppHash: a.committedHash}, nil
}

func (a *App) InitChain(_ context.Context, req *abci.InitChainRequest) (*abci.InitChainResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.st = newState()
	if err := json.Unmarshal(req.AppStateBytes, &a.st.Params); err != nil {
		return nil, fmt.Errorf("bad app_state: %w", err)
	}
	a.committed = a.st.canonical()
	return &abci.InitChainResponse{}, nil
}

type CreateContent struct {
	Title        string `json:"title"`
	Description  string `json:"description"`
	Image        string `json:"image"`
	Category     string `json:"category"`
	StartPrice   int64  `json:"startPrice"`
	MinIncrement int64  `json:"minIncrement"`
	DurationSec  int64  `json:"durationSec"`
	Mode         string `json:"mode"`
	WindowSec    int64  `json:"windowSec"`
	SellerWallet string `json:"sellerWallet"`
	Name         string `json:"name"`
	// V4VSplit is the OPTIONAL committed value-for-value payout schedule (spec
	// §6). Each row is {role, recipientPubkey, allocationBps}; the seller keeps
	// the remainder. It is validated by validateV4VSplit, stored immutably on
	// the auction, and reproduced exactly by the bidder's pre-signed SETTLE
	// template.
	V4VSplit []SplitRow `json:"v4vSplit,omitempty"`
}

type BidContent struct {
	Auction string  `json:"auction"`
	Amount  int64   `json:"amount"`
	Wallet  string  `json:"wallet"`
	Name    string  `json:"name"`
	Proofs  []Proof `json:"proofs"`
	// The bidder's pre-signed settle and refund transactions (spec §1.2). The
	// chain refuses a bid without them, and refuses any template not locked to
	// the committed seller (settle) or the bidder (refund) keys.
	OutcomeTemplates *sigAllOutcomeTemplates `json:"outcome_templates,omitempty"`
}

// closeTx is the validators' own "close what is due" transaction (ADR 0003
// §1): close:<auction id>:<endAt>:<unix seconds sent>. It is not a Nostr
// event, so the gateway's /api/tx (JSON only) cannot submit one.
const closePrefix = "close:"

type closeTx struct {
	Auction string
	EndAt   int64
	Sent    int64
}

func parseClose(tx []byte) (*closeTx, bool) {
	if !bytes.HasPrefix(tx, []byte(closePrefix)) || len(tx) > 128 {
		return nil, false
	}
	parts := strings.Split(string(tx[len(closePrefix):]), ":")
	if len(parts) != 3 || !lowerHex(parts[0], 64) {
		return nil, false
	}
	end, e1 := strconv.ParseInt(parts[1], 10, 64)
	sent, e2 := strconv.ParseInt(parts[2], 10, 64)
	if e1 != nil || e2 != nil {
		return nil, false
	}
	return &closeTx{parts[0], end, sent}, true
}

func makeClose(id string, endAt, now int64) []byte {
	return []byte(fmt.Sprintf("%s%s:%d:%d", closePrefix, id, endAt, now))
}

// lateBid is a bid that is well formed and fully locked to the escrow key
// but lost a race (too low, already leading, lot ended). The chain records
// it as void so the validators can refund it at once (ADR 0007).
type lateBid struct{ reason string }

func (e *lateBid) Error() string { return e.reason }

// maxVoids caps the late bids one auction keeps; past it they are refused
// (the bidder reclaims after locktime, as before).
const maxVoids = 256

func decodeTx(tx []byte) (*Event, error) {
	var ev Event
	if err := json.Unmarshal(tx, &ev); err != nil {
		return nil, errors.New("tx is not a Nostr event")
	}
	if ev.Kind != KindCreate && ev.Kind != KindBid {
		return nil, fmt.Errorf("unsupported kind %d", ev.Kind)
	}
	if err := ev.Verify(); err != nil {
		return nil, err
	}
	return &ev, nil
}

// safeImage allows an empty image, a same-site path or an https URL made of
// URL-safe characters only, so it can never break out of an HTML attribute
// (internal audit A5).
func safeImage(s string) bool {
	if s == "" {
		return true
	}
	if !(strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//")) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~:/?#[]@!$&()*+,;=%", c) >= 0) {
			return false
		}
	}
	return true
}

func isPubHex(s string) bool {
	if len(s) != 66 || !(strings.HasPrefix(s, "02") || strings.HasPrefix(s, "03")) {
		return false
	}
	_, err := parsePub(s)
	return err == nil
}

// validD: the d tag (auction_id). NIP-01 addressable/parametrised-replaceable
// charset — [A-Za-z0-9_-], 1..64 chars. An auction whose d fails this is not
// an auction: the coordinate a = 1440:<seller>:<d> would not be parseable.
func validD(d string) bool {
	if len(d) < 1 || len(d) > 64 {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// coordOf is OUR coordinate convention for a regular kind: a =
// 1440:<seller_pubkey>:<d>. This is not NIP-01 addressability — 1440 is
// regular, so the relay replaces nothing and the latest accepted version must
// be resolved explicitly (spec §7, §8). The coordinate is the auction's stable
// identity: bids and the auction map are keyed by it, not by a mutable version
// event id.
func coordOf(seller, d string) string {
	return strconv.Itoa(KindCreate) + ":" + seller + ":" + d
}

// tagValue returns the first value of the named tag.
func tagValue(tags [][]string, name string) (string, bool) {
	for _, t := range tags {
		if len(t) >= 2 && t[0] == name {
			return t[1], true
		}
	}
	return "", false
}

// immutableFields is the allow-list's immutable half, remapped onto his current
// create payload (owner decision 2026-11-12, spec §8). The principle is
// display is mutable; money, time and cryptography are not:
//
//   - StartPrice        money  — the absolute floor every bid is checked against
//   - MinIncrement      money  — the step rule
//   - Mode              time   — fixed | antisnipe decides whether EndAt can move
//   - WindowSec         time   — the anti-snipe reset amount; EndAt derives from it
//   - DurationSec       time   — when a fixed lot closes
//   - SellerWallet      crypto — where settlement pays; the lock's refund/data key
//
// The d value and the seller are immutable structurally: they are the
// coordinate (seller, d), so changing either is a different auction, never an
// edit. Title, Description and Image are display and stay editable. Category
// and SellerName are judgement calls, both mutable (see the report): a
// category is metadata, and a display name is a display field with no bearing
// on money, time or the lock.
func immutableChanged(cur *Auction, c *CreateContent, mode string) string {
	switch {
	case c.StartPrice != cur.StartPrice:
		return "StartPrice"
	case c.MinIncrement != cur.MinIncrement:
		return "MinIncrement"
	case mode != cur.Mode:
		return "Mode"
	case c.WindowSec != cur.WindowSec:
		return "WindowSec"
	case c.DurationSec != cur.DurationSec:
		return "DurationSec"
	case strings.ToLower(c.SellerWallet) != cur.SellerWallet:
		return "SellerWallet"
	case splitImmutableChanged(cur, c.V4VSplit) != "":
		// The split is a money field (spec §8). It is checked here, in the one
		// immutability mechanism, so a supersession that changes who gets paid
		// is refused by name against the canonical version rather than being
		// silently accepted and then tripping the bid template check.
		return "V4VSplit"
	}
	return ""
}

// canonicalAuction resolves (seller, d) to the canonical auction: the latest
// 1440 event that passed the immutable check. Because 1440 is a regular kind,
// no relay performed this replacement for us, so the persisted state IS the
// resolution — a root that never existed, or an update that failed the check,
// simply never overwrote it, and the entity holds the last accepted version.
func (s *State) canonicalAuction(seller, d string) *Auction {
	return s.Auctions[coordOf(seller, d)]
}

// byRoot finds the auction whose root event id is rootID — or, as a
// convenience for callers that name the auction by its coordinate, the auction
// stored under that coordinate key. The validators' own close tx names an
// auction by its stable root id, not by a mutable version.
func (s *State) byRoot(rootID string) *Auction {
	if au := s.Auctions[rootID]; au != nil {
		return au
	}
	for _, au := range s.Auctions {
		if au.RootID == rootID {
			return au
		}
	}
	return nil
}

// applyCreate validates and (if apply) executes a 1440 create event at block
// time now.
//
// The auction's identity is (seller, d), keyed by the coordinate
// a = 1440:<seller>:<d>. The FIRST event for a coordinate is the ROOT; its
// event id is auction_root_event_id. A later event for the same coordinate is a
// SUPERSESSION: it must carry e = <auction_root_event_id> and may change only
// the allow-listed display fields. A change to any immutable field (money,
// time, cryptography) is refused with "immutable_field_changed <field>" and
// ignored — the state keeps the last accepted version.
func (s *State) applyCreate(ev *Event, now, height int64, apply bool) error {
	d, ok := tagValue(ev.Tags, "d")
	if !ok || !validD(d) {
		return errors.New("create needs a valid d tag")
	}
	coord := coordOf(ev.Pubkey, d)
	root, hasRoot := tagValue(ev.Tags, "e")

	var c CreateContent
	if err := json.Unmarshal([]byte(ev.Content), &c); err != nil {
		return errors.New("bad create content")
	}
	p := s.Params
	switch {
	case len(strings.TrimSpace(c.Title)) == 0 || len(c.Title) > MaxTitleChars:
		// The bound is formatted from the constant: the reason text a refusal
		// carries must never advertise a limit the check does not enforce.
		return fmt.Errorf("title required (max %d chars)", MaxTitleChars)
	case len(c.Description) > MaxDescriptionChars || len(c.Image) > MaxImageChars || len(c.Category) > MaxCategoryChars || len(c.Name) > MaxNameChars:
		return errors.New("field too long")
	case !safeImage(c.Image):
		return errors.New("image must be a same-site path or an https URL")
	case c.StartPrice < MinStartPrice || c.StartPrice > MaxStartPrice:
		return errors.New("start price out of range")
	case c.MinIncrement < MinIncrement || c.MinIncrement > MaxIncrement:
		return errors.New("increment out of range")
	case c.DurationSec < p.MinDurationSec || c.DurationSec > p.MaxDurationSec:
		return fmt.Errorf("duration must be %d-%d seconds", p.MinDurationSec, p.MaxDurationSec)
	case !isPubHex(strings.ToLower(c.SellerWallet)):
		return errors.New("seller wallet pubkey invalid")
	case c.Mode != "" && c.Mode != "fixed" && c.Mode != "antisnipe":
		return errors.New("mode must be fixed or antisnipe")
	case c.Mode == "antisnipe" && (c.WindowSec < p.MinWindowSec || c.WindowSec > p.MaxWindowSec):
		return fmt.Errorf("anti-snipe window must be %d-%d seconds", p.MinWindowSec, p.MaxWindowSec)
	case c.Mode != "antisnipe" && c.WindowSec != 0:
		return errors.New("a fixed-time lot has no anti-snipe window")
	}
	// The committed V4V split is optional but, when present, strictly
	// validated; a malformed split is refused by name (validateV4VSplit).
	split, err := validateV4VSplit(c.V4VSplit)
	if err != nil {
		return err
	}
	mode := c.Mode
	if mode == "" {
		mode = "fixed"
	}

	cur := s.Auctions[coord]
	if cur == nil {
		// The root: the first 1440 for this (seller, d). It must not claim a
		// root it cannot know about — only a supersession carries e.
		if hasRoot {
			return errors.New("first create for this (seller, d) must not carry an e tag")
		}
		if !apply {
			return nil
		}
		end := now + c.DurationSec
		s.Auctions[coord] = &Auction{
			ID: coord, D: d, RootID: ev.ID, VersionID: ev.ID, VersionCreatedAt: ev.CreatedAt, DurationSec: c.DurationSec,
			Seller: ev.Pubkey, SellerWallet: strings.ToLower(c.SellerWallet), SellerName: c.Name,
			Title: c.Title, Description: c.Description, Image: c.Image, Category: c.Category,
			StartPrice: c.StartPrice, MinIncrement: c.MinIncrement, CreatedAt: now, CreatedH: height,
			EndAt: end, Mode: mode, WindowSec: c.WindowSec, Status: "open", Bids: []string{},
			V4VSplit: split,
		}
		s.Order = append(s.Order, coord)
		return nil
	}

	// A supersession. It must point at the ROOT — not at whatever version it
	// last saw. That is what keeps bids (which also point at the root)
	// attached across an edit.
	if !hasRoot || root != cur.RootID {
		return errors.New("update must reference the auction root (e = auction_root_event_id)")
	}
	// Immutable check against the canonical (latest accepted) version. A
	// refused supersession is ignored, never applied: the state below stays
	// untouched, so the canonical auction remains the last accepted one.
	if f := immutableChanged(cur, &c, mode); f != "" {
		return fmt.Errorf("immutable_field_changed %s", f)
	}
	// Latest by created_at that passes the check: an edit no newer than the
	// canonical version does not win, and a replay is ignored.
	if ev.CreatedAt <= cur.VersionCreatedAt {
		return errors.New("stale update ignored")
	}
	if !apply {
		return nil
	}
	// Only display fields change; money, time and cryptography stay put, and
	// EndAt is never recomputed.
	cur.Title, cur.Description, cur.Image, cur.Category, cur.SellerName = c.Title, c.Description, c.Image, c.Category, c.Name
	cur.VersionID, cur.VersionCreatedAt, cur.CreatedH = ev.ID, ev.CreatedAt, height
	return nil
}

// establishedEnd is the end time a bid establishes, given whether it extends.
// For an antisnipe bid that arrives with less than WindowSec left, the lot's
// end becomes now+WindowSec (see applyBid), so THAT is the end the bid's
// locktime must clear. For a fixed lot, and for an antisnipe bid that does not
// extend, it is the lot's current EndAt.
func (s *State) establishedEnd(au *Auction, now int64, extends bool) int64 {
	if extends {
		return now + au.WindowSec
	}
	if au.Mode == "antisnipe" && au.EndAt < now+au.WindowSec {
		// The bid arrives inside the window: it wins the extension, so treat
		// the new end as established even before applyBid commits it.
		return now + au.WindowSec
	}
	return au.EndAt
}

// minLocktime is the earliest refund locktime a bid on au may carry at block
// time now, derived from the end THIS bid establishes (spec §2.1/§2.2):
//
//	minLocktime = max(establishedEnd, now + WindowSec)
//	            + SettlementSafetySec + BidToConsensusLatencySec
//
// It MUST be evaluated against the post-extension end: checking before the
// extension would validate an extending bid against the OLD end and collapse
// the post-end safety margin from SettlementSafetySec to as little as
// BidToConsensusLatencySec (spec §2.2).
func (s *State) minLocktime(au *Auction, now int64) int64 {
	end := s.establishedEnd(au, now, au.Mode == "antisnipe" && au.EndAt-now < au.WindowSec)
	floor := end
	// The now+WindowSec term guarantees an anti-snipe bid carries at least a
	// full window of lock after the moment it extends the end. A fixed lot has
	// no window (WindowSec is 0 and its end never moves), so the term is
	// vacuous and the floor is the lot's own end.
	if au.Mode == "antisnipe" && now+au.WindowSec > floor {
		floor = now + au.WindowSec
	}
	return floor + s.Params.SettlementSafetySec + s.Params.BidToConsensusLatencySec
}

// checkLock enforces the 2-of-2 [bidder, committee] SIG_ALL escrow lock (spec
// §1): the bidder's wallet key is the first main key, the committee (FROST
// group) key is the pubkeys entry, n_sigs is 2, SIG_ALL, refund to the bidder
// alone, exactly five canonical tags, and the locktime clears minLock -- which
// minLocktime derives from the end this bid establishes, not the old one.
func (s *State) checkLock(proofs []Proof, wallet string, minLock int64) (int64, error) {
	if len(proofs) == 0 {
		return 0, errors.New("all proofs must share one lock")
	}
	return checkSigAllLock(proofs, wallet, s.Params.GroupKey, minLock)
}

func lower(a []string) []string {
	out := make([]string, len(a))
	for i, v := range a {
		out[i] = strings.ToLower(v)
	}
	return out
}

func (s *State) applyBid(ev *Event, now, height int64, apply, timed bool) error {
	if _, ok := s.Bids[ev.ID]; ok {
		return errors.New("bid already recorded")
	}
	var b BidContent
	if err := json.Unmarshal([]byte(ev.Content), &b); err != nil {
		return errors.New("bad bid content")
	}
	au, ok := s.Auctions[b.Auction]
	if !ok {
		return errors.New("unknown auction")
	}
	// 0. The bid binds to the auction ROOT, not to the latest version: it must
	// carry e = <auction_root_event_id>. This check lives HERE, in the
	// deterministic transition, not only in CheckTx: admission is a mempool
	// filter and a Byzantine proposer can include a transaction that never
	// passed it (spec §8).
	if e, ok := tagValue(ev.Tags, "e"); !ok || e != au.RootID {
		return errors.New("bid must reference the auction root (auction_root_event_id)")
	}
	// The coordinate convention a = 1440:<seller>:<d>, if present, must match.
	if a, ok := tagValue(ev.Tags, "a"); ok && a != au.ID {
		return errors.New("bid's a coordinate does not match the auction")
	}
	// 1. Is it a real, fully locked escrow? If not, refuse it outright.
	wallet := strings.ToLower(b.Wallet)
	if !isPubHex(wallet) {
		return errors.New("wallet pubkey invalid")
	}
	if len(b.Proofs) == 0 || len(b.Proofs) > s.Params.MaxProofs {
		return errors.New("bad proof count")
	}
	var sum int64
	seen := map[string]bool{}
	ys := make([]string, 0, len(b.Proofs))
	for _, pr := range b.Proofs {
		if pr.ID != s.Params.KeysetID {
			return errors.New("proof from unpinned keyset")
		}
		kh, ok := s.Params.Keys[fmt.Sprint(pr.Amount)]
		if !ok || pr.Amount <= 0 {
			return errors.New("bad proof amount")
		}
		A, err := parsePub(kh)
		if err != nil {
			return errors.New("bad keyset key")
		}
		if err := verifyDLEQ(pr, A); err != nil {
			return err
		}
		y, err := proofY(pr.Secret)
		if err != nil {
			return err
		}
		if seen[y] {
			return errors.New("duplicate proof")
		}
		if _, used := s.UsedY[y]; used {
			return errors.New("proof already used in another bid")
		}
		seen[y] = true
		ys = append(ys, y)
		if sum > math.MaxInt64-pr.Amount {
			return errors.New("proof total overflows")
		}
		sum += pr.Amount
	}
	if sum != b.Amount {
		return fmt.Errorf("proofs total %d but bid is %d", sum, b.Amount)
	}
	lt, err := s.checkLock(b.Proofs, wallet, s.minLocktime(au, now))
	if err != nil {
		return err
	}
	// The bidder's pre-signed outcome templates: consensus verifies the only
	// two spends the escrow may ever make (spec §1.2). A bid without them, or
	// with a template not locked to the committed seller/bidder keys, is
	// refused by name -- there is no fallback and no server-side synthesis.
	if err := verifySigAllBidTemplates(au, &b, s.Params); err != nil {
		return err
	}
	// 2. Is it economically admissible? A bid's Amount is a DELTA added to the
	// bidder's POSITION, never a resulting total (app doctrine:
	// ui/auction/arena/bid-rules.ts:155-165; the escrow IS this delta, see the
	// proof-sum check above). The top is the leader's position, and every other
	// bidder's position on this lot is ZERO: their earlier legs were outbid and
	// are refundable, so the money they still hold here is nothing (plan §2.1).
	//
	// That makes the floor a POSITION floor -- the resulting position
	// own+Amount must reach top+minIncrement -- and it makes a leader's own
	// raise legal (R2-A): with additive deltas every positive Amount strictly
	// raises the top, so no rule needs to forbid it. The old "you are already
	// the highest bidder" case is therefore DELETED, not relaxed.
	//
	// INVARIANT (asserted in tests, and the chain's form of ADR-0003 S4.9):
	// after any accepted bid au.HighAmount == Σ { bid.Amount : bid.Status ==
	// "leading" } and, after close, == Σ { bid.Amount : bid.Status == "won" }.
	own := int64(0)
	if au.HighBid != "" && s.Bids[au.HighBid].Bidder == ev.Pubkey {
		own = au.HighAmount // the leader's position IS the top
	}
	min := au.StartPrice // a first bid's position must reach the opening price
	if au.HighBid != "" {
		min = au.HighAmount + au.MinIncrement
		if own > 0 {
			min = own // the leader improving their own standing need only reach the top
		}
	}
	var late error
	switch {
	case au.Status != "open" || (timed && now >= au.EndAt):
		late = &lateBid{"auction closed"}
	case ev.Pubkey == au.Seller:
		late = &lateBid{"seller cannot bid on own lot"}
	case b.Amount > math.MaxInt64-own:
		// Byzantine guard: Amount is bounded by the proof-sum check above, but
		// own accumulates over the leader's legs, so the SUM must not wrap.
		return errors.New("position overflows")
	case own+b.Amount < min:
		late = &lateBid{fmt.Sprintf("bid must add at least %d sats: your position would reach %d, the floor is %d",
			min-own, own+b.Amount, min)}
	}
	if late != nil && len(au.Voids) >= maxVoids {
		return errors.New(late.Error() + " (late-bid limit reached for this lot)")
	}
	if !apply {
		return late
	}
	bid := &Bid{ID: ev.ID, Auction: au.ID, Bidder: ev.Pubkey, Name: b.Name, Wallet: wallet, Amount: b.Amount,
		Proofs: b.Proofs, Locktime: lt, Height: height, Time: now, Status: "leading",
		OutcomeTemplates: b.OutcomeTemplates}
	s.Bids[ev.ID] = bid
	for _, y := range ys {
		s.UsedY[y] = ev.ID
	}
	if late != nil {
		bid.Status = "void"
		au.Voids = append(au.Voids, ev.ID)
		return late
	}
	if au.HighBid != "" && s.Bids[au.HighBid].Bidder != ev.Pubkey {
		// The lead CHANGED HANDS: the dethroned bidder's whole position is
		// outbid -- every leg of it, because each leg's escrow is a separate
		// delta that must be given back. A leader RAISING their own position
		// keeps every leg leading: those legs ARE the position, and settlement
		// walks each one (plan §2.3). This runs before the new bid is appended,
		// so it can never touch it.
		for _, id := range au.Bids {
			if s.Bids[id].Status == "leading" {
				s.Bids[id].Status = "outbid"
			}
		}
	}
	// Anti-snipe (ADR 0009): a bid with less than the window left puts the
	// clock back to the window. No cap: the lot ends when the bidding stops.
	// Fixed lots end at their set time, whatever happens.
	if au.Mode == "antisnipe" && au.EndAt-now < au.WindowSec {
		au.EndAt = now + au.WindowSec
		au.Extensions++
		bid.Extended = true
		// The committed end and ordinal belong to THIS bid: a later bid moves
		// au.EndAt again, so the lot's current end cannot say what this one
		// established (kind 1449 publishes exactly this pair).
		bid.EstablishedEnd, bid.ExtensionOrdinal = au.EndAt, au.Extensions
	}
	// HighAmount is the leader's POSITION (the sum of every leading leg), not
	// the last delta; HighBid is the LEG that established it -- the same id
	// closeDue promotes to Winner and DeriveVerdict names (plan §2.4). The caps
	// and the anti-snipe extension above are unchanged by this.
	au.HighBid, au.HighAmount = ev.ID, own+b.Amount
	au.Bids = append(au.Bids, ev.ID)
	return nil
}

// closeDue closes every open lot whose end has passed. There is no no-sale
// path and no expiry: an anti-snipe lot ends when the bidding stops, and a
// lock may be arbitrarily long -- it freezes only the bidder's own funds
// (spec §2.3), and the last-resort refund simply arrives later.
func (s *State) closeDue(now, height int64) {
	ids := append([]string{}, s.Order...)
	sort.Strings(ids)
	for _, id := range ids {
		au := s.Auctions[id]
		if au.Status == "open" && now >= au.EndAt {
			au.Status, au.ClosedAt, au.ClosedH = "closed", now, height
			if au.HighBid != "" {
				au.Winner = au.HighBid
			}
			// Every LEADING leg is part of the winning position and is paid;
			// every other accepted leg is refundable. Keyed on the STATUS, not
			// the bidder (plan §2.5): a bidder's outbid legs, from a chain a
			// later challenger defeated, are refundable and must never be
			// promoted to won. Void bids are not in au.Bids at all.
			//
			// INVARIANT: HighAmount == Σ { bid.Amount : bid.Status == "won" }.
			for _, id := range au.Bids {
				if s.Bids[id].Status == "leading" {
					s.Bids[id].Status = "won"
				} else {
					s.Bids[id].Status = "lost"
				}
			}
		}
	}
}

// positionOf returns the position a bid BELONGS TO, read from committed state:
// the sum of its bidder's live legs, which is au.HighAmount while the bid leads
// and the winning total after close (the §2.4 invariant).  ok is false for a
// bid that is neither leading nor won: an outbid/lost leg's position is no
// longer a committed fact (its money is on its way back), so nothing may be
// stated about it -- this is the one place the guarantee is written down, and
// the publisher (R4) keys on it.
func (s *State) positionOf(bid *Bid) (int64, bool) {
	if bid == nil || (bid.Status != "leading" && bid.Status != "won") {
		return 0, false
	}
	au := s.Auctions[bid.Auction]
	if au == nil || au.HighBid == "" {
		return 0, false
	}
	return au.HighAmount, true
}

func (a *App) CheckTx(_ context.Context, req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {
	if c, ok := parseClose(req.Tx); ok {
		return a.checkClose(c, time.Now().Unix()), nil
	}
	if st, ok := parseSettle(req.Tx); ok {
		a.mu.RLock()
		defer a.mu.RUnlock()
		if _, err := a.st.verifySettle(st, a.st.Time); err != nil {
			return &abci.CheckTxResponse{Code: 4, Log: err.Error()}, nil
		}
		return &abci.CheckTxResponse{Code: 0}, nil
	}
	ev, err := decodeTx(req.Tx)
	if err != nil {
		return &abci.CheckTxResponse{Code: 1, Log: err.Error()}, nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch ev.Kind {
	case KindCreate:
		err = a.st.applyCreate(ev, a.st.Time, a.st.Height, false)
		if err != nil {
			// A create refused HERE never reaches a block, so FinalizeBlock's
			// rejection path never runs, State.Rejections never gains it and a
			// relay-only client never learns the reason (kind 1450 exists for
			// exactly that).  So admit the tx and let delivery decide: the
			// decision is deterministic and is made again there, where
			// applyCreate(..., apply=true) refuses it identically, appends the
			// Rejection with THAT block's height and the publisher anchors the
			// kind-1450 to it.  The dry run above mutates nothing (apply=false
			// returns before every write), so admitting costs no state change
			// and creates are one per lot, so the block-space cost is bounded.
			// The BID branch deliberately stays strict: applyBid verifies
			// escrow cryptography (DLEQ, SIG_ALL), and admitting unverified
			// bids would multiply that work across all four validators. It
			// also REFUSES a bid that is economically late (R3), because such
			// a bid can never become valid -- see the comment below.
			return &abci.CheckTxResponse{Code: 0, Log: "will be decided at delivery: " + err.Error()}, nil
		}
	case KindBid:
		// The BID branch leaves the cryptography where it is and decides the
		// ECONOMY here (R3). applyBid still verifies the whole escrow (DLEQ per
		// proof, one shared SIG_ALL lock, the bidder's two pre-signed outcome
		// templates) before it looks at the floor, so an unverified bid is
		// still refused outright; what changes is what happens to a bid that is
		// verified but LATE on arrival.
		//
		// A bid that is late NOW can never become valid: the position floor
		// only rises, a closed lot stays closed, and the seller never changes.
		// So it is refused at admission and never committed -- no state entry,
		// no void list entry, no escrow touched.  The reason goes back on the
		// submit (`/api/tx`), NOT as a later kind-1450: a mempool refusal never
		// reaches a block, so the chain has no committed height to anchor a
		// refusal event to (exactly the reasoning the create branch records
		// above).
		//
		// The wall clock and the lot's clock are used (as checkClose does): a
		// lot whose end has passed cannot be won by a bid the next block will
		// close first.  This can only make CheckTx STRICTER than delivery, never
		// looser: minLocktime is monotone in `now`, and a block's time is never
		// earlier than the wall clock this validator saw.
		//
		// `void` still exists and is still what a genuine race produces: a bid
		// that passed this filter and was overtaken before its block (another
		// bid took the lead and raised the floor), or a bid only a Byzantine
		// proposer could have included.  Those are decided at delivery, where
		// applyBid marks them void and refundable.
		err = a.st.applyBid(ev, time.Now().Unix(), a.st.Height, false, true)
		var late *lateBid
		if errors.As(err, &late) {
			return &abci.CheckTxResponse{Code: 2, Log: late.reason}, nil
		}
	}
	if err != nil {
		return &abci.CheckTxResponse{Code: 2, Log: err.Error()}, nil
	}
	return &abci.CheckTxResponse{Code: 0}, nil
}

// checkClose admits a close transaction only while that auction is open,
// still ends at endAt, and endAt has passed on this validator's clock; and
// only if it was made in the last 30 s. Anything else is refused, so it is
// no use for spam.
func (a *App) checkClose(c *closeTx, now int64) *abci.CheckTxResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()
	// The close tx names the auction by its stable ROOT id, never by a mutable
	// version event id: an edit must not be able to orphan an in-flight close.
	au := a.st.byRoot(c.Auction)
	switch {
	case au == nil || au.Status != "open" || au.EndAt != c.EndAt:
		return &abci.CheckTxResponse{Code: 3, Log: "nothing to close"}
	case c.EndAt > now:
		return &abci.CheckTxResponse{Code: 3, Log: "not due yet"}
	case c.Sent < now-30 || c.Sent > now+5:
		return &abci.CheckTxResponse{Code: 3, Log: "stale close"}
	}
	return &abci.CheckTxResponse{Code: 0}
}

func (a *App) FinalizeBlock(_ context.Context, req *abci.FinalizeBlockRequest) (*abci.FinalizeBlockResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.st
	now := req.Time.Unix()
	s.Height, s.Time = req.Height, now
	s.closeDue(now, req.Height)
	res := make([]*abci.ExecTxResult, len(req.Txs))
	lines := make([]ledgerLine, 0, len(req.Txs))
	for i, tx := range req.Txs {
		if _, ok := parseClose(tx); ok {
			// closeDue above already did the work; the tx only made the block
			res[i] = &abci.ExecTxResult{Code: 0}
			continue
		}
		if st, ok := parseSettle(tx); ok {
			v, err := s.verifySettle(st, now)
			if err != nil {
				// not a rejection a user caused: just not recorded
				res[i] = &abci.ExecTxResult{Code: 4, Log: err.Error()}
				continue
			}
			s.applySettle(st, v)
			s.TxCount++
			res[i] = &abci.ExecTxResult{Code: 0}
			lines = append(lines, ledgerLine{Height: req.Height, Time: now, Event: st.Bid, Result: "settled", Reason: st.Outcome, Tx: json.RawMessage(tx[len(settlePrefix):])})
			continue
		}
		ev, err := decodeTx(tx)
		if err == nil {
			switch ev.Kind {
			case KindCreate:
				err = s.applyCreate(ev, now, req.Height, true)
			case KindBid:
				err = s.applyBid(ev, now, req.Height, true, true)
			}
		}
		var late *lateBid
		if errors.As(err, &late) {
			s.TxCount++
			res[i] = &abci.ExecTxResult{Code: 0, Log: "void: " + late.reason}
			lines = append(lines, ledgerLine{Height: req.Height, Time: now, Kind: ev.Kind, Event: ev.ID, Result: "void", Reason: late.reason})
			continue
		}
		if err != nil {
			r := Rejection{Height: req.Height, Time: now, Reason: err.Error()}
			if ev != nil {
				r.Kind, r.Pubkey, r.Event = ev.Kind, ev.Pubkey, ev.ID
			}
			s.Rejections = append(s.Rejections, r)
			if len(s.Rejections) > 50 {
				s.Rejections = s.Rejections[len(s.Rejections)-50:]
			}
			res[i] = &abci.ExecTxResult{Code: 2, Log: err.Error()}
			ll := ledgerLine{Height: req.Height, Time: now, Result: "rejected", Reason: err.Error()}
			if ev != nil {
				ll.Kind, ll.Event = ev.Kind, ev.ID
			}
			lines = append(lines, ll)
			continue
		}
		s.TxCount++
		res[i] = &abci.ExecTxResult{Code: 0}
		lines = append(lines, ledgerLine{Height: req.Height, Time: now, Kind: ev.Kind, Event: ev.ID, Result: "ok", Tx: json.RawMessage(tx)})
	}
	a.pendLines = lines
	// One serialisation per block: the app hash is the SHA-256 of the same
	// canonical JSON that Commit publishes (identical to s.hash()).
	b := s.canonical()
	h := sha256.Sum256(b)
	a.appHash, a.pending = h[:], b
	return &abci.FinalizeBlockResponse{TxResults: res, AppHash: a.appHash}, nil
}

func (a *App) Commit(context.Context, *abci.CommitRequest) (*abci.CommitResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending != nil {
		a.committed, a.pending = a.pending, nil
		a.committedHash = a.appHash
	} else {
		a.committed = a.st.canonical()
		h := sha256.Sum256(a.committed)
		a.committedHash = h[:]
	}
	a.committedH, a.committedT = a.st.Height, a.st.Time
	if a.commitCh != nil {
		close(a.commitCh)
		a.commitCh = nil
	}
	// the ledger line goes to disk before any block it covers can be pruned
	if a.ledger != nil {
		if err := a.ledger.append(a.committedH, a.pendLines); err != nil {
			log.Printf("ledger %d: %v", a.committedH, err)
		}
	}
	a.pendLines = nil
	var retain int64
	if a.retain > 0 && a.snap != nil {
		if r := a.snap.lastHeight() - a.retain; r > 0 {
			retain = r
		}
	}
	if a.snap != nil && a.snap.every > 0 && a.committedH%a.snap.every == 0 {
		h, t, hash, raw := a.committedH, a.committedT, a.committedHash, a.committed
		go func() {
			if err := a.snap.write(h, t, hash, raw); err != nil {
				log.Printf("snapshot %d: %v", h, err)
			}
		}()
	}
	return &abci.CommitResponse{RetainHeight: retain}, nil
}
