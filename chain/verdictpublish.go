package main

// The publish driver.  It reads the validator's COMMITTED snapshot and writes
// eight event classes back to the relay:
//
//   - a state pulse (kind 21447, EPHEMERAL) on EVERY pass, so a live subscriber
//     can place the chain's clock even while nothing happens.  A pulse the
//     relay drops for having no listener is a non-event (owner ruling, handover
//     §8.1): it is not retried, not logged as an error and gates nothing;
//   - a state commitment (kind 1447, STORED) whenever a claim (a verdict or a
//     receipt) is published, on the first pass, whenever the committed HEIGHT
//     MOVES, and on the StateCommitMaxInterval heartbeat when the chain is
//     otherwise idle -- so the committed clock can never go stale by more than
//     that wall-clock gap (handover §1).  This is the anchor that proves a chain
//     of time;
//   - a params event (kind 1448, STORED) when the escrow terms change or on the
//     Publisher.every block cadence, anchored to the SAME (height, app_hash) as
//     the commitment published in the same pass (handover §4);
//   - a verdict (kind 1445) for each auction the chain has CLOSED;
//   - a receipt (kind 1443 refund / 1444 settle, STORED) for each escrow the
//     chain has MOVED, anchored to the SETTLE's committed height and time -- the
//     money fact a relay-only client needs to claim the minted ecash;
//   - the lot's committed END (kind 1449, STORED) for each bid that extended a
//     lot, anchored at THAT bid's height, and once for each closed lot, anchored
//     at ClosedH -- so a late reader learns where the lot ended;
//   - the chain's ACCEPTANCE of each lot it follows (kind 1449, STORED, schema
//     arena.createadmit.v1) for each auction the chain has committed, anchored
//     at the lot's ACCEPTED height -- so a reader learns from the chain itself
//     which auctions are being followed, instead of inferring it from silence;
//   - the chain's own refusal (kind 1450, STORED) for each committed rejection
//     of a create or bid, carrying the reason VERBATIM and anchored at the
//     rejecting block's height.
//
// THE ONE RULE THAT SHAPES THIS FILE: a claim is published only AFTER the block
// that decides it is COMMITTED, anchored to a COMMITTED block's hash.  Step
// reads the committed height, time and app hash from the app (never a pending
// view), and it refuses to emit a verdict until it can read the committed block
// hash at that height from the chain.  A verdict the chain cannot yet anchor is
// not published -- it is retried on the next Step, and the refusal is recorded
// so a test can assert it.  A claim a reorg can contradict is worse than no
// claim.
//
// WHAT THE ANCHOR PROVES, EXACTLY (audit 2026-11-13, REPLY §8.1): the anchor is
// the committed height at the moment Step ran, so it proves "the committed state
// at height H produced this outcome" -- the property determinism needs, and it
// is reorg-safe at H.  It does NOT prove that the verdict was published after
// the auction's DECIDING block; a client cannot derive that from (height, hash)
// alone.  The behaviour is defensible -- the outcome genuinely is a function of
// that committed state -- but the claim is stated here precisely, not loosely.
//
// The publisher is deliberately split from the App: the App (ABCI) knows its
// app hash but not the CometBFT block hash, so the block hash is injected as a
// function.  In production that function reads the node's BlockStore; in tests
// it returns a deterministic stand-in.

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
)

// PublishRecord is one action the publisher took, in order.  Tests assert the
// ordering on it: every verdict record carries the committed height it acted
// at, so "published after commit" is checkable rather than merely claimed.
type PublishRecord struct {
	Op              string // "state_commitment" | "state_pulse" | "params" | "verdict" | "receipt" | "lot" | "bid_admit" | "create_admit" | "refusal" | "refused"
	CommittedHeight int64  // the app's committed height when Step ran
	Height          int64  // the anchor height on the event
	Hash            string // the anchor hash on the event
	AuctionID       string
	Outcome         string
	Reason          string // "refused": why nothing was published; "refusal": the verbatim chain reason on the event
	Error           string // set when the relay rejected the publish
}

// PublisherConfig configures a Publisher.  Key, Relay, App and BlockHash are
// required.
type PublisherConfig struct {
	// Key is THIS validator's Nostr signing key (verdicts and commitments are
	// signed under it, never a shared publisher key).
	Key *btcec.PrivateKey
	// Relay is the transport.
	Relay RelayWriter
	// App is the validator's application; the publisher reads its committed
	// snapshot only.
	App *App
	// BlockHash returns the CometBFT block hash (lowercase hex) at a committed
	// height.  ok=false means that height's hash is not readable yet -- the
	// publisher then refuses to anchor anything to it and retries later.
	BlockHash func(height int64) (hash string, ok bool)
	// Every is the PARAMS-event cadence in blocks (kind 1448 is re-published
	// every N blocks even without a change).  Zero means StateCommitEvery.  It
	// is no longer the state-commitment gate: the commitment is gated on a
	// verdict, the first pass, a height move or the MaxInterval heartbeat.
	Every int64
	// MaxInterval is the maximum WALL-CLOCK gap between two stored state
	// commitments.  Blocks are produced on demand, so a block-height cadence has
	// no wall-time bound; this bounds how stale the committed clock can get on an
	// idle chain.  Zero or negative means StateCommitMaxInterval.
	MaxInterval time.Duration
	// Now is the event timestamp source.  Nil means time.Now.
	Now func() int64
	// Logger receives progress; nil means log.Default.
	Logger *log.Logger
}

// Publisher is the write half of the Nostr transport for one validator.
type Publisher struct {
	key         *btcec.PrivateKey
	relay       RelayWriter
	app         *App
	blockHash   func(height int64) (string, bool)
	every       int64
	maxInterval time.Duration
	now         func() int64
	log         *log.Logger

	mu               sync.Mutex
	published        map[string]bool // auction id -> a verdict has been published
	receipted        map[string]bool // bidID + "|" + outcome -> a receipt has been published (or can never be)
	lots             map[string]bool // auctionID + "|" + end_time -> a lot event has been published
	admits           map[string]bool // bid id -> that bid's admission fact has been published (R4)
	accepts          map[string]bool // auction id -> that lot's create-admission fact has been published
	refusals         map[string]bool // event id + "|" + reason -> a refusal event has been published
	lastCommitHeight int64           // committed height of the last commitment published
	lastCommitTime   int64           // unix seconds (p.now) of the last commitment published
	lastParams       string          // params digest of the last params event published
	lastParamsHeight int64           // committed height of the last params event published
	records          []PublishRecord
}

// NewPublisher validates config and returns a publisher.
func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if cfg.Key == nil {
		return nil, errPublisher("publisher requires a validator signing key")
	}
	if cfg.Relay == nil {
		return nil, errPublisher("publisher requires a relay writer")
	}
	if cfg.App == nil {
		return nil, errPublisher("publisher requires an app")
	}
	if cfg.BlockHash == nil {
		return nil, errPublisher("publisher requires a block-hash source")
	}
	every := cfg.Every
	if every <= 0 {
		every = StateCommitEvery
	}
	maxInterval := cfg.MaxInterval
	if maxInterval <= 0 {
		maxInterval = StateCommitMaxInterval
	}
	now := cfg.Now
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Publisher{
		key: cfg.Key, relay: cfg.Relay, app: cfg.App, blockHash: cfg.BlockHash,
		every: every, maxInterval: maxInterval, now: now, log: logger,
		published: map[string]bool{},
		receipted: map[string]bool{},
		lots:      map[string]bool{},
		admits:    map[string]bool{},
		accepts:   map[string]bool{},
		refusals:  map[string]bool{},
	}, nil
}

type publisherError string

func (e publisherError) Error() string { return string(e) }

func errPublisher(msg string) error { return publisherError(msg) }

// Run calls Step on a ticker until ctx is cancelled.  Step is also exported so
// a caller can drive it explicitly (and a test can hold the clock).
func (p *Publisher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Step(ctx)
		}
	}
}

// Step performs one publish pass against the current COMMITTED state.
func (p *Publisher) Step(ctx context.Context) {
	height, blockTime, appHash, raw := p.app.committedSnapshot()
	if height == 0 || len(raw) == 0 {
		return // nothing committed yet; never publish for an uncommitted view
	}
	state := newState()
	if json.Unmarshal(raw, state) != nil {
		p.record(PublishRecord{Op: "refused", CommittedHeight: height, Reason: "committed_state_unreadable"})
		return
	}
	state.Height, state.Time = height, blockTime

	// 1. Verdicts, receipts, lot state, acceptances and refusals first: each
	//    one is a "meaningful change", and each result joins the commitment
	//    gate below.  All five run on every pass; none may be skipped just
	//    because another published something.
	publishedVerdict := p.publishVerdicts(ctx, state, height)
	publishedReceipt := p.publishReceipts(ctx, state, height)
	publishedLots := p.publishLots(ctx, state, height)
	publishedAccepts := p.publishAccepts(ctx, state, height)
	publishedRefusals := p.publishRefusals(ctx, state, height)
	publishedClaim := publishedVerdict || publishedReceipt || publishedLots || publishedAccepts || publishedRefusals

	appHashHex := hexOrEmpty(appHash)
	now := p.now()

	// 2. Pulse: UNCONDITIONAL, on every pass, so a live subscriber always has
	//    the chain's clock.  It is ephemeral and gates nothing.
	if appHashHex != "" {
		p.publishPulse(ctx, height, appHashHex, now)
	}

	// 3. Params: publish when the terms CHANGE, and on the block cadence so a
	//    long-lived chain refreshes them even without a change (handover §4).
	//    The cadence is EDGE-triggered on the height: blocks are produced on
	//    demand, so a chain can sit at one height for a long time, and a bare
	//    `height % every == 0` would re-publish the same event on every pass
	//    while it does (measured live: 61 events per validator at one height).
	digest := paramsVersion(state.Params)
	p.mu.Lock()
	needParams := p.lastParams == "" || digest != p.lastParams ||
		(height%p.every == 0 && height != p.lastParamsHeight)
	// 4. Stored commitment gate: a verdict went out, the first ever publish, the
	//    committed height moved, the heartbeat elapsed, or params need an anchor
	//    to ride.  The heartbeat is the point of the change: blocks are produced
	//    on demand, so a block-height cadence alone has no wall-time bound.
	needCommit := publishedClaim || p.lastCommitTime == 0 || height != p.lastCommitHeight ||
		now-p.lastCommitTime >= int64(p.maxInterval/time.Second) || needParams
	p.mu.Unlock()

	if (needCommit || needParams) && appHashHex == "" {
		p.record(PublishRecord{Op: "refused", CommittedHeight: height, Reason: "app_hash_empty"})
		return
	}

	if needCommit {
		ev, err := BuildStateCommitmentEvent(p.key, height, appHashHex, now)
		if err != nil {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Reason: err.Error()})
			return
		}
		rec := PublishRecord{Op: "state_commitment", CommittedHeight: height, Height: height, Hash: appHashHex}
		if err := p.relay.Publish(ctx, ev); err != nil {
			rec.Error = err.Error()
		} else {
			p.mu.Lock()
			p.lastCommitHeight = height
			p.lastCommitTime = now
			p.mu.Unlock()
		}
		p.record(rec)
	}

	// 5. Params ride the SAME (height, app_hash) as the commitment just
	//    published in this pass, so a client can tell they belong together.
	//    A failed publish leaves lastParams unchanged, so it retries.
	if needParams {
		ev, err := BuildParamsEvent(p.key, state.Params, height, appHashHex, now)
		if err != nil {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Reason: err.Error()})
			return
		}
		rec := PublishRecord{Op: "params", CommittedHeight: height, Height: height, Hash: appHashHex}
		if err := p.relay.Publish(ctx, ev); err != nil {
			rec.Error = err.Error()
		} else {
			p.mu.Lock()
			p.lastParams = digest
			p.lastParamsHeight = height
			p.mu.Unlock()
		}
		p.record(rec)
	}
}

// publishPulse publishes this pass's ephemeral heartbeat (kind 21447).  It is
// best-effort by design: the event is EPHEMERAL, so the relay drops it when
// nobody is subscribed, and a pulse nobody could have received is a non-event,
// not a failure.  That "mute: no one was listening for this" answer is the
// relay's implementation choice for an ephemeral event with no listener
// (handover §8.1 correction 3): it is swallowed here -- no record, no retry, no
// error log -- and gates nothing.  Any OTHER failure is recorded so a real
// transport problem stays visible.
func (p *Publisher) publishPulse(ctx context.Context, height int64, appHashHex string, now int64) {
	ev, err := BuildStatePulseEvent(p.key, height, appHashHex, now)
	if err != nil {
		p.record(PublishRecord{Op: "refused", CommittedHeight: height, Reason: err.Error()})
		return
	}
	rec := PublishRecord{Op: "state_pulse", CommittedHeight: height, Height: height, Hash: appHashHex}
	if err := p.relay.Publish(ctx, ev); err != nil {
		if noListenerError(err) {
			return
		}
		rec.Error = err.Error()
	}
	p.record(rec)
}

// noListenerError reports whether a publish failure is the relay's "nobody is
// subscribed" answer to an ephemeral event rather than a transport failure.
// The deployment's relay (khatru) answers an ephemeral event with no live
// subscription with "mute: no one was listening for this"; a pulse lost that
// way costs nothing, so it must not be retried or alarmed on.
func noListenerError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no one was listening") || strings.Contains(msg, "mute")
}

// publishVerdicts publishes one verdict per newly-closed auction, anchored to
// the committed height's block hash.  It returns whether anything was
// published, so the caller can publish a commitment to accompany the change.
func (p *Publisher) publishVerdicts(ctx context.Context, state *State, height int64) bool {
	closed := make([]string, 0, len(state.Auctions))
	for id, au := range state.Auctions {
		if au != nil && au.Status == "closed" {
			closed = append(closed, id)
		}
	}
	sort.Strings(closed) // deterministic order across validators

	any := false
	for _, id := range closed {
		p.mu.Lock()
		done := p.published[id]
		p.mu.Unlock()
		if done {
			continue
		}
		verdict, ok := DeriveVerdict(state, id)
		if !ok {
			continue
		}
		// THE ORDERING RULE: the anchor needs the committed block hash at this
		// height.  If it is not readable, refuse -- do not publish unanchored.
		blockHash, ok := p.blockHash(height)
		if !ok || blockHash == "" {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, AuctionID: id, Reason: "committed_block_hash_unknown"})
			continue
		}
		// payout_token_sha256 stays null: a verdict is a CLAIM that a payout
		// exists, never a receipt.  The mint is the authority on money moved
		// (REPLY §4.2).
		ev, err := BuildVerdictEvent(p.key, verdict, height, blockHash, nil, p.now())
		if err != nil {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, AuctionID: id, Reason: err.Error()})
			continue
		}
		rec := PublishRecord{Op: "verdict", CommittedHeight: height, Height: height, Hash: blockHash,
			AuctionID: id, Outcome: verdict.Outcome}
		if err := p.relay.Publish(ctx, ev); err != nil {
			// Do NOT mark it published: a transient relay failure retries.
			rec.Error = err.Error()
			p.record(rec)
			continue
		}
		p.mu.Lock()
		p.published[id] = true
		p.mu.Unlock()
		p.record(rec)
		any = true
	}
	return any
}

// publishReceipts publishes one receipt per settled escrow, anchored to the
// SETTLE's committed height and that block's time -- the moment money moved,
// not the publishing pass's height.  It returns whether anything was published,
// so the caller can publish a commitment to accompany the change.  A receipt is
// a receipt, not a heartbeat: it is published once per (bid, outcome), never on
// a cadence.
func (p *Publisher) publishReceipts(ctx context.Context, state *State, height int64) bool {
	ids := make([]string, 0, len(state.Bids))
	for id := range state.Bids {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic order across validators

	any := false
	for _, id := range ids {
		bid := state.Bids[id]
		if bid == nil || bid.Settled == "" {
			continue
		}
		key := bid.ID + "|" + bid.Settled
		p.mu.Lock()
		done := p.receipted[key]
		p.mu.Unlock()
		if done {
			continue
		}
		// A settle with no committed WHEN -- recorded before settledHeight
		// existed, as every settle on the live root is -- can never be anchored:
		// there is no height whose block hash proves it.  Both fields are checked
		// because a record that names a height but no time could not be built
		// either, and this branch is the one place a receipt is marked
		// unpublishable, so it must not leave a shape that retries forever.
		// Record ONE refusal and mark it: the alternative is a record per pass,
		// for as long as the process lives.
		if bid.SettledHeight <= 0 || bid.SettledAt <= 0 {
			p.mu.Lock()
			p.receipted[key] = true
			p.mu.Unlock()
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, AuctionID: bid.Auction,
				Outcome: bid.Settled, Reason: "settle_has_no_block_when"})
			continue
		}
		// THE ORDERING RULE: the anchor is the SETTLE's committed height.  The
		// production BlockHash reads the CometBFT BlockStore, which answers any
		// retained past height.  If it is not readable, refuse and retry later.
		blockHash, ok := p.blockHash(bid.SettledHeight)
		if !ok || blockHash == "" {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: bid.SettledHeight,
				AuctionID: bid.Auction, Outcome: bid.Settled, Reason: "committed_block_hash_unknown"})
			continue
		}
		var kind int
		var schema string
		switch bid.Settled {
		case "refund":
			kind, schema = KindRefund, RefundSchema
		case "settle":
			kind, schema = KindSettle, SettleSchema
		default:
			// An unknown outcome can never be published: mark it so it is not
			// retried forever.
			p.mu.Lock()
			p.receipted[key] = true
			p.mu.Unlock()
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: bid.SettledHeight,
				AuctionID: bid.Auction, Outcome: bid.Settled, Reason: "unknown_settled_outcome"})
			continue
		}
		out, err := receiptOutputs(bid.Out)
		if err != nil {
			p.mu.Lock()
			p.receipted[key] = true
			p.mu.Unlock()
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: bid.SettledHeight,
				AuctionID: bid.Auction, Outcome: bid.Settled, Reason: err.Error()})
			continue
		}
		rc := ReceiptContent{
			Schema: schema, AuctionID: bid.Auction, BidID: bid.ID, Outcome: bid.Settled,
			Height: bid.SettledHeight, BlockHash: blockHash, SettledAt: bid.SettledAt,
			ParamsVersion: paramsVersion(state.Params), Out: out,
		}
		var ev *Event
		if kind == KindRefund {
			ev, err = BuildRefundEvent(p.key, rc, p.now())
		} else {
			ev, err = BuildSettleEvent(p.key, rc, p.now())
		}
		rec := PublishRecord{Op: "receipt", CommittedHeight: height, Height: bid.SettledHeight,
			Hash: blockHash, AuctionID: bid.Auction, Outcome: bid.Settled}
		if err != nil {
			rec.Error = err.Error()
			p.record(rec)
			continue
		}
		if err := p.relay.Publish(ctx, ev); err != nil {
			// Do NOT mark it receipted: a transient relay failure retries.
			rec.Error = err.Error()
			p.record(rec)
			continue
		}
		p.mu.Lock()
		p.receipted[key] = true
		p.mu.Unlock()
		p.record(rec)
		any = true
	}
	return any
}

// publishLots publishes the lot's state (kind 1449) in three shapes: once for
// every bid that actually extended the lot, anchored at THAT bid's committed
// height and that block's hash; once for every ADMITTED bid that still leads or
// won, carrying the resulting position (the arena.bidadmit.v1 shape, R4); and
// once for every closed lot, anchored at ClosedH.
//
// The three shapes answer three different questions.  A bid event says "this bid
// moved the end to E at height H"; the per-bid pair is read from the bid
// (EstablishedEnd/ExtensionOrdinal) because a later bid moves au.EndAt again --
// the lot's CURRENT end cannot say what an earlier bid established.  A closed
// lot event says "this is where the lot ended", so a late reader learns the
// final end; a lot that never extended therefore publishes exactly one event at
// close, with end_time == original_end_time and extensions == 0.  An admission
// event says "the chain accepted this bid, and its position is P", which is what
// lets a client confirm its own bid from the wire.
//
// Dedupe is per validator by (auction_id, end_time) for lot events and by bid id
// for admission events, and an event is marked only AFTER a successful relay
// publish, so a transient relay failure retries.  If the anchor block's hash is
// not readable yet, nothing is published: the refusal is recorded and retried
// next pass -- a claim the chain cannot anchor is not published.
func (p *Publisher) publishLots(ctx context.Context, state *State, height int64) bool {
	bidIDs := make([]string, 0, len(state.Bids))
	for id := range state.Bids {
		bidIDs = append(bidIDs, id)
	}
	sort.Strings(bidIDs) // deterministic order across validators

	any := false
	// 1. Every bid that extended the lot: its own committed end and ordinal.
	for _, id := range bidIDs {
		bid := state.Bids[id]
		if bid == nil || bid.EstablishedEnd <= 0 {
			continue
		}
		au := state.Auctions[bid.Auction]
		if au == nil {
			// The bid names a lot the committed state does not hold; there is
			// no original end to publish beside it, and inventing one would be
			// a claim.  Skip: this cannot become publishable by retrying.
			continue
		}
		key := bid.Auction + "|" + strconv.FormatInt(bid.EstablishedEnd, 10)
		p.mu.Lock()
		done := p.lots[key]
		p.mu.Unlock()
		if done {
			continue
		}
		// THE ORDERING RULE: the anchor is the BID's committed height, not this
		// pass's.  If the block hash is not readable, refuse and retry later.
		blockHash, ok := p.blockHash(bid.Height)
		if !ok || blockHash == "" {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: bid.Height,
				AuctionID: bid.Auction, Reason: "committed_block_hash_unknown"})
			continue
		}
		lc := LotContent{
			AuctionID: bid.Auction, EndTime: bid.EstablishedEnd,
			OriginalEndTime: au.OriginalEnd(), Extensions: bid.ExtensionOrdinal,
			Height: bid.Height, BlockHash: blockHash,
		}
		ev, err := BuildLotEvent(p.key, lc, p.now())
		if err != nil {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: bid.Height,
				AuctionID: bid.Auction, Reason: err.Error()})
			continue
		}
		rec := PublishRecord{Op: "lot", CommittedHeight: height, Height: bid.Height,
			Hash: blockHash, AuctionID: bid.Auction}
		if err := p.relay.Publish(ctx, ev); err != nil {
			// Do NOT mark it published: a transient relay failure retries.
			rec.Error = err.Error()
			p.record(rec)
			continue
		}
		p.mu.Lock()
		p.lots[key] = true
		p.mu.Unlock()
		p.record(rec)
		any = true
	}

	// 2. Every ADMITTED bid: the chain's own confirmation that it ACCEPTED the
	// bid, carrying the position that resulted (R4) -- so an app that published
	// a bid reads an acceptance instead of inferring one from state.  Only a
	// bid that still leads or won can carry its position truthfully (an
	// outbid/lost leg's position is not a committed fact any more; see
	// State.positionOf), so those are the shapes published.  The anchor is the
	// BID's OWN committed height, and dedupe is per bid, so a bid is announced
	// once.  A transient relay failure does not mark, so it retries.
	for _, id := range bidIDs {
		bid := state.Bids[id]
		if bid == nil {
			continue
		}
		position, ok := state.positionOf(bid)
		if !ok {
			continue
		}
		au := state.Auctions[bid.Auction]
		if au == nil {
			continue // no committed lot to name: not publishable, not retryable
		}
		p.mu.Lock()
		done := p.admits[bid.ID]
		p.mu.Unlock()
		if done {
			continue
		}
		blockHash, ok := p.blockHash(bid.Height)
		if !ok || blockHash == "" {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: bid.Height,
				AuctionID: bid.Auction, Reason: "committed_block_hash_unknown"})
			continue
		}
		ac := BidAdmitContent{
			AuctionID: bid.Auction, BidID: bid.ID, Bidder: bid.Bidder, Amount: bid.Amount,
			Position: position, HighBid: au.HighBid, Height: bid.Height, BlockHash: blockHash,
		}
		ev, err := BuildBidAdmitEvent(p.key, ac, p.now())
		if err != nil {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: bid.Height,
				AuctionID: bid.Auction, Reason: err.Error()})
			continue
		}
		rec := PublishRecord{Op: "bid_admit", CommittedHeight: height, Height: bid.Height,
			Hash: blockHash, AuctionID: bid.Auction}
		if err := p.relay.Publish(ctx, ev); err != nil {
			// Do NOT mark it published: a transient relay failure retries.
			rec.Error = err.Error()
			p.record(rec)
			continue
		}
		p.mu.Lock()
		p.admits[bid.ID] = true
		p.mu.Unlock()
		p.record(rec)
		any = true
	}

	// 3. Every closed lot: its FINAL end, anchored at the block that closed it.
	auctionIDs := make([]string, 0, len(state.Auctions))
	for id := range state.Auctions {
		auctionIDs = append(auctionIDs, id)
	}
	sort.Strings(auctionIDs)
	for _, id := range auctionIDs {
		au := state.Auctions[id]
		if au == nil || au.Status != "closed" || au.ClosedH <= 0 {
			continue
		}
		key := au.ID + "|" + strconv.FormatInt(au.EndAt, 10)
		p.mu.Lock()
		done := p.lots[key]
		p.mu.Unlock()
		if done {
			continue
		}
		blockHash, ok := p.blockHash(au.ClosedH)
		if !ok || blockHash == "" {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: au.ClosedH,
				AuctionID: au.ID, Reason: "committed_block_hash_unknown"})
			continue
		}
		lc := LotContent{
			AuctionID: au.ID, EndTime: au.EndAt, OriginalEndTime: au.OriginalEnd(),
			Extensions: au.Extensions, Height: au.ClosedH, BlockHash: blockHash,
		}
		ev, err := BuildLotEvent(p.key, lc, p.now())
		if err != nil {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: au.ClosedH,
				AuctionID: au.ID, Reason: err.Error()})
			continue
		}
		rec := PublishRecord{Op: "lot", CommittedHeight: height, Height: au.ClosedH,
			Hash: blockHash, AuctionID: au.ID}
		if err := p.relay.Publish(ctx, ev); err != nil {
			rec.Error = err.Error()
			p.record(rec)
			continue
		}
		p.mu.Lock()
		p.lots[key] = true
		p.mu.Unlock()
		p.record(rec)
		any = true
	}
	return any
}

// publishAccepts publishes the chain's ACCEPTANCE of each lot it follows (kind
// 1449, schema arena.createadmit.v1), anchored at the height the root create was
// committed.  It is the positive counterpart of a refusal: instead of a reader
// inferring which auctions count from the absence of a refusal, the chain states
// it, so at any point every validator knows which auctions are going on.  This is
// the fact a later fee announcement or a max-concurrent-lots bound hangs off
// (maintainer's requirement).
//
// The anchor is the lot's OWN accepted height -- Auction.AcceptedH, which
// applyCreate sets once at the root and a supersession never moves -- so the fact
// is stable for the life of an accepted lot however many times it is edited.
// Dedupe is per validator by auction id, and an event is marked only AFTER a
// successful relay publish, so a transient relay failure retries.  If the anchor
// block's hash is not readable yet, nothing is published: the refusal is recorded
// and retried next pass -- a claim the chain cannot anchor is not published.
//
// LEGACY LOTS.  A lot already in state when AcceptedH first shipped has
// AcceptedH == 0: it was accepted before the chain began recording the height.
// Those, and only those, anchor at the lot's current CreatedH, so the deploy does
// not silently orphan every lot the chain has been running.  The consequence,
// stated so nobody is surprised: a legacy lot SUPERSEDED between two validator
// restarts re-emits its acceptance at a moved anchor, because a supersession
// updates CreatedH.  The two events are identical in content -- the anchor is the
// only difference -- and a client dedupes at the earliest anchored height; a lot
// created after this change always has AcceptedH > 0 and never moves.
func (p *Publisher) publishAccepts(ctx context.Context, state *State, height int64) bool {
	ids := make([]string, 0, len(state.Auctions))
	for id := range state.Auctions {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic order across validators

	any := false
	for _, id := range ids {
		au := state.Auctions[id]
		if au == nil {
			continue
		}
		p.mu.Lock()
		done := p.accepts[au.ID]
		p.mu.Unlock()
		if done {
			continue
		}
		anchor := au.AcceptedH
		if anchor <= 0 {
			// Legacy lot (see the comment above): accepted before AcceptedH
			// existed, so its current CreatedH is the best anchor it has.
			anchor = au.CreatedH
		}
		if anchor <= 0 {
			// No committed height at all to anchor to.  Do NOT mark it: a
			// supersession updates CreatedH, so this can become publishable; a
			// permanent mark would suppress a legitimately acceptable lot.
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, AuctionID: au.ID,
				Reason: "create_has_no_committed_height"})
			continue
		}
		blockHash, ok := p.blockHash(anchor)
		if !ok || blockHash == "" {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: anchor,
				AuctionID: au.ID, Reason: "committed_block_hash_unknown"})
			continue
		}
		ac := CreateAdmitContent{
			AuctionID: au.ID, Seller: au.Seller, RootEventID: au.RootID,
			AcceptedHeight: anchor, BlockHash: blockHash,
			EndTime: au.EndAt, OriginalEndTime: au.OriginalEnd(), Mode: au.Mode,
		}
		ev, err := BuildCreateAdmitEvent(p.key, ac, p.now())
		if err != nil {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: anchor,
				AuctionID: au.ID, Reason: err.Error()})
			continue
		}
		rec := PublishRecord{Op: "create_admit", CommittedHeight: height, Height: anchor,
			Hash: blockHash, AuctionID: au.ID}
		if err := p.relay.Publish(ctx, ev); err != nil {
			// Do NOT mark it published: a transient relay failure retries.
			rec.Error = err.Error()
			p.record(rec)
			continue
		}
		p.mu.Lock()
		p.accepts[au.ID] = true
		p.mu.Unlock()
		p.record(rec)
		any = true
	}
	return any
}

// publishRefusals publishes the chain's OWN reason (kind 1450) for every
// committed rejection of an auction event -- create (1440) or bid (1441) -- so
// a client whose event was dropped can read why, carrying the reason VERBATIM
// and anchored at the rejection's own committed height and that block's hash.
//
// state.Rejections is capped at 50 entries by the chain itself, so this pass is
// bounded.  Dedupe is per validator by (event id, reason); the map is
// in-memory, exactly like the verdict map above, so a process restart
// republishes the current rejections once -- the bound on that re-publish is
// the chain's own 50-entry cap.  A relay failure does not mark, so it retries;
// an unreadable anchor hash records a refusal and retries next pass.
func (p *Publisher) publishRefusals(ctx context.Context, state *State, height int64) bool {
	any := false
	for _, rej := range state.Rejections {
		if (rej.Kind != KindCreate && rej.Kind != KindBid) || rej.Event == "" || rej.Reason == "" {
			continue // only auction traffic the client acts on, with a reason to state
		}
		if rej.Height <= 0 {
			continue // no block to anchor to: nothing can ever be published
		}
		key := rej.Event + "|" + rej.Reason
		p.mu.Lock()
		done := p.refusals[key]
		p.mu.Unlock()
		if done {
			continue
		}
		blockHash, ok := p.blockHash(rej.Height)
		if !ok || blockHash == "" {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: rej.Height,
				Reason: "committed_block_hash_unknown"})
			continue
		}
		rc := RefusedContent{EventID: rej.Event, Kind: rej.Kind, Reason: rej.Reason,
			Height: rej.Height, BlockHash: blockHash}
		ev, err := BuildRefusedEvent(p.key, rc, p.now())
		if err != nil {
			p.record(PublishRecord{Op: "refused", CommittedHeight: height, Height: rej.Height,
				Reason: err.Error()})
			continue
		}
		rec := PublishRecord{Op: "refusal", CommittedHeight: height, Height: rej.Height,
			Hash: blockHash, Reason: rej.Reason}
		if err := p.relay.Publish(ctx, ev); err != nil {
			// Do NOT mark it published: a transient relay failure retries.
			rec.Error = err.Error()
			p.record(rec)
			continue
		}
		p.mu.Lock()
		p.refusals[key] = true
		p.mu.Unlock()
		p.record(rec)
		any = true
	}
	return any
}

func (p *Publisher) record(r PublishRecord) {
	p.mu.Lock()
	p.records = append(p.records, r)
	p.mu.Unlock()
}

// Records returns a copy of the actions taken, in order.
func (p *Publisher) Records() []PublishRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PublishRecord(nil), p.records...)
}

// RelayHealth reports, per relay, the state of the publish transport: whether
// it is connected, how many attempts failed, and the last error.  A publish
// failure used to be invisible outside the process -- recorded on a
// PublishRecord and printed nowhere -- so an operator found it only by noticing
// that no new events existed (incident 2026-11-19: 1h45m).  This is what the
// validator's /status and the gateway's /api/publish serve.
func (p *Publisher) RelayHealth() []PublishHealth {
	if h, ok := p.relay.(interface{ Health() []PublishHealth }); ok {
		return h.Health()
	}
	return nil
}

func hexOrEmpty(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0x0f])
	}
	return string(out)
}
