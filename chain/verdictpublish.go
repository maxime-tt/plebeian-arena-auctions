package main

// The publish driver.  It reads the validator's COMMITTED snapshot and writes
// four event classes back to the relay:
//
//   - a state pulse (kind 21447, EPHEMERAL) on EVERY pass, so a live subscriber
//     can place the chain's clock even while nothing happens.  A pulse the
//     relay drops for having no listener is a non-event (owner ruling, handover
//     §8.1): it is not retried, not logged as an error and gates nothing;
//   - a state commitment (kind 1447, STORED) whenever a verdict is published, on
//     the first pass, whenever the committed HEIGHT MOVES, and on the
//     StateCommitMaxInterval heartbeat when the chain is otherwise idle -- so
//     the committed clock can never go stale by more than that wall-clock gap
//     (handover §1).  This is the anchor that proves a chain of time;
//   - a params event (kind 1448, STORED) when the escrow terms change or on the
//     Publisher.every block cadence, anchored to the SAME (height, app_hash) as
//     the commitment published in the same pass (handover §4);
//   - a verdict (kind 1445) for each auction the chain has CLOSED.
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
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
)

// PublishRecord is one action the publisher took, in order.  Tests assert the
// ordering on it: every verdict record carries the committed height it acted
// at, so "published after commit" is checkable rather than merely claimed.
type PublishRecord struct {
	Op              string // "state_commitment" | "state_pulse" | "params" | "verdict" | "refused"
	CommittedHeight int64  // the app's committed height when Step ran
	Height          int64  // the anchor height on the event
	Hash            string // the anchor hash on the event
	AuctionID       string
	Outcome         string
	Reason          string // for "refused"
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
	lastCommitHeight int64           // committed height of the last commitment published
	lastCommitTime   int64           // unix seconds (p.now) of the last commitment published
	lastParams       string          // params digest of the last params event published
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

	// 1. Verdicts first: each one is a "meaningful change".
	publishedVerdict := p.publishVerdicts(ctx, state, height)

	appHashHex := hexOrEmpty(appHash)
	now := p.now()

	// 2. Pulse: UNCONDITIONAL, on every pass, so a live subscriber always has
	//    the chain's clock.  It is ephemeral and gates nothing.
	if appHashHex != "" {
		p.publishPulse(ctx, height, appHashHex, now)
	}

	// 3. Params: publish when the terms CHANGE, and on the block cadence so a
	//    long-lived chain refreshes them even without a change (handover §4).
	digest := paramsVersion(state.Params)
	p.mu.Lock()
	needParams := p.lastParams == "" || digest != p.lastParams || height%p.every == 0
	// 4. Stored commitment gate: a verdict went out, the first ever publish, the
	//    committed height moved, the heartbeat elapsed, or params need an anchor
	//    to ride.  The heartbeat is the point of the change: blocks are produced
	//    on demand, so a block-height cadence alone has no wall-time bound.
	needCommit := publishedVerdict || p.lastCommitTime == 0 || height != p.lastCommitHeight ||
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
