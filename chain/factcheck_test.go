package main

// Tests for the shared fact gate (factcheck.go, handover §6.3/§6.5).
//
// Every fact a validator publishes -- verdict, lot, bid-admission,
// create-admission, refusal, params, commitment, pulse and receipt -- is signed
// under the validator's OWN key from Params.Validators and anchored to a
// committed height.  These tests build a REAL signed event with the repo's own
// builders and assert the two gates on every one of them:
//
//   (a) a signer that is not on the roster is refused by that parser's OWN named
//       Err*NotInRoster (the shared predicate must not flatten the names);
//   (b) a wrong block hash at the fact's height is refused by BindFactAnchor;
//   (c) with no lookup wired, an anchor cannot be bound at all
//       (ErrAnchorLookupUnavailable) -- the §8.7 transport is an open item;
//   (d) a commitment at another height (or no commitment) is refused.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// parseErr adapts one fact parser to the table's error-only shape.
func parseErr[T any](f func(*Event, map[string]ValidatorKey) (T, error)) func(*Event, map[string]ValidatorKey) error {
	return func(ev *Event, roster map[string]ValidatorKey) error {
		_, err := f(ev, roster)
		return err
	}
}

// factCase is one fact shape under test.
type factCase struct {
	name string
	// build signs a real event of this shape at height with blockHash.
	build func(t *testing.T, sk *btcec.PrivateKey, height int64, blockHash string) *Event
	// parse is the reader's gate, returning only its error.
	parse func(ev *Event, roster map[string]ValidatorKey) error
	// wantErr is the parser's OWN named roster refusal.
	wantErr error
}

// factCases builds one case per fact shape.  height/blockHash are the anchor the
// events carry; the same values the fake lookup is wired to return.
func factCases(height int64, blockHash string) []factCase {
	auctionID := "1440:" + hx(9) + ":lot"
	return []factCase{
		{
			name: "verdict",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				ev, err := BuildVerdictEvent(sk, Verdict{AuctionID: auctionID, BidID: hx(1), Outcome: OutcomeWinner}, h, bh, nil, tNow)
				if err != nil {
					t.Fatalf("BuildVerdictEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseVerdict),
			wantErr: ErrVerdictNotInRoster,
		},
		{
			name: "lot",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				ev, err := BuildLotEvent(sk, LotContent{AuctionID: auctionID, EndTime: 100, OriginalEndTime: 100, Height: h, BlockHash: bh}, tNow)
				if err != nil {
					t.Fatalf("BuildLotEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseLotEvent),
			wantErr: ErrLotNotInRoster,
		},
		{
			name: "bid-admit",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				ev, err := BuildBidAdmitEvent(sk, BidAdmitContent{AuctionID: auctionID, BidID: hx(1), Bidder: hx(2), Amount: 10, Position: 10, HighBid: hx(1), Height: h, BlockHash: bh}, tNow)
				if err != nil {
					t.Fatalf("BuildBidAdmitEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseBidAdmitEvent),
			wantErr: ErrBidAdmitNotInRoster,
		},
		{
			name: "create-admit",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				ev, err := BuildCreateAdmitEvent(sk, CreateAdmitContent{AuctionID: auctionID, Seller: hx(9), RootEventID: hx(1), AcceptedHeight: h, BlockHash: bh, EndTime: 100, OriginalEndTime: 100, Mode: "fixed"}, tNow)
				if err != nil {
					t.Fatalf("BuildCreateAdmitEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseCreateAdmitEvent),
			wantErr: ErrCreateAdmitNotInRoster,
		},
		{
			name: "refused",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				ev, err := BuildRefusedEvent(sk, RefusedContent{EventID: hx(4), Kind: KindCreate, Reason: "title required", Height: h, BlockHash: bh}, tNow)
				if err != nil {
					t.Fatalf("BuildRefusedEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseRefusedEvent),
			wantErr: ErrRefusedNotInRoster,
		},
		{
			name: "params",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				ev, err := BuildParamsEvent(sk, Params{MintURL: testMintAddr}, h, bh, tNow)
				if err != nil {
					t.Fatalf("BuildParamsEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseParamsEvent),
			wantErr: ErrParamsNotInRoster,
		},
		{
			name: "commitment",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				ev, err := BuildStateCommitmentEvent(sk, h, bh, tNow)
				if err != nil {
					t.Fatalf("BuildStateCommitmentEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseStateCommitment),
			wantErr: ErrVerdictNotInRoster, // the commitment parser's own name, unchanged
		},
		{
			name: "pulse",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				ev, err := BuildStatePulseEvent(sk, h, bh, tNow)
				if err != nil {
					t.Fatalf("BuildStatePulseEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseStatePulse),
			wantErr: ErrPulseNotInRoster,
		},
		{
			name: "receipt",
			build: func(t *testing.T, sk *btcec.PrivateKey, h int64, bh string) *Event {
				rc := ReceiptContent{
					AuctionID: auctionID, BidID: hx(1), Outcome: "refund",
					Height: h, BlockHash: bh, SettledAt: tNow, ParamsVersion: paramsVersion(Params{MintURL: testMintAddr}),
					Out: []ReceiptOut{{Recipient: hx(7), Amount: 1, ID: "00", Secret: "s", C: "02" + hx(8), DLEQ: nil}},
				}
				ev, err := BuildRefundEvent(sk, rc, tNow)
				if err != nil {
					t.Fatalf("BuildRefundEvent: %v", err)
				}
				return ev
			},
			parse:   parseErr(ParseRefundEvent),
			wantErr: ErrReceiptNotInRoster,
		},
	}
}

// TestFactSignerRosterAndAnchor drives every fact shape through the shared
// signer predicate and the one anchor seam.
func TestFactSignerRosterAndAnchor(t *testing.T) {
	const height = 77
	blockHash := testBlockHash(height)

	for _, tc := range factCases(height, blockHash) {
		t.Run(tc.name, func(t *testing.T) {
			signer := newBidder(t).sk
			ev := tc.build(t, signer, height, blockHash)

			// (a) a non-roster signer is refused by the parser's own name.
			other := newBidder(t)
			offRoster := map[string]ValidatorKey{other.pub: {Pubkey: other.pub, Name: "v1", Index: 1}}
			if err := tc.parse(ev, offRoster); !errors.Is(err, tc.wantErr) {
				t.Fatalf("off-roster signer: want %v, got %v", tc.wantErr, err)
			}
			// A roster holding the signer accepts it, and a nil roster keeps the
			// old behaviour (no membership check).
			onRoster := map[string]ValidatorKey{ev.Pubkey: {Pubkey: ev.Pubkey, Name: "v1", Index: 1}}
			if err := tc.parse(ev, onRoster); err != nil {
				t.Fatalf("roster signer refused: %v", err)
			}
			if err := tc.parse(ev, nil); err != nil {
				t.Fatalf("nil roster must preserve the old behaviour: %v", err)
			}

			// (b) a wrong hash at the fact's height does not bind; the right one
			// does.
			withLookup(t, func(h int64) (string, error) {
				if h != height {
					return "", fmt.Errorf("no block at %d", h)
				}
				return blockHash, nil
			})
			if err := BindFactAnchor(height, testBlockHash(height+1)); !errors.Is(err, ErrFactAnchorMismatch) {
				t.Fatalf("wrong anchor hash: want ErrFactAnchorMismatch, got %v", err)
			}
			if err := BindFactAnchor(height, blockHash); err != nil {
				t.Fatalf("the fact's own anchor must bind: %v", err)
			}

			// (d) a commitment at another height -- or none -- is refused; the
			// commitment at the fact's height binds it.
			if err := CheckFactAtCommitment(height, blockHash, &StateCommitmentContent{Height: height + 1, AppHash: blockHash}); !errors.Is(err, ErrFactAnchorMismatch) {
				t.Fatalf("commitment at another height: want ErrFactAnchorMismatch, got %v", err)
			}
			if err := CheckFactAtCommitment(height, blockHash, nil); !errors.Is(err, ErrFactAnchorMismatch) {
				t.Fatalf("no commitment: want ErrFactAnchorMismatch, got %v", err)
			}
			if err := CheckFactAtCommitment(height, blockHash, &StateCommitmentContent{Height: height, AppHash: blockHash}); err != nil {
				t.Fatalf("commitment at the fact height must bind: %v", err)
			}
		})
	}
}

// TestBindFactAnchorWithoutALookup: with the default seam -- no transport wired
// for handover §8.7 -- nothing can be bound, and the reader is told exactly
// that rather than given a silent pass.
func TestBindFactAnchorWithoutALookup(t *testing.T) {
	if err := BindFactAnchor(5, testBlockHash(5)); !errors.Is(err, ErrAnchorLookupUnavailable) {
		t.Fatalf("default lookup: want ErrAnchorLookupUnavailable, got %v", err)
	}
	// The fact's own anchor validity is checked first.
	if err := BindFactAnchor(0, testBlockHash(5)); !errors.Is(err, ErrFactUnanchored) {
		t.Fatalf("height 0: want ErrFactUnanchored, got %v", err)
	}
	if err := BindFactAnchor(5, ""); !errors.Is(err, ErrFactUnanchored) {
		t.Fatalf("empty hash: want ErrFactUnanchored, got %v", err)
	}
	// A lookup that succeeds but names no block is also "cannot bind".
	withLookup(t, func(int64) (string, error) { return "", nil })
	if err := BindFactAnchor(5, testBlockHash(5)); !errors.Is(err, ErrAnchorLookupUnavailable) {
		t.Fatalf("empty lookup result: want ErrAnchorLookupUnavailable, got %v", err)
	}
}

// TestFactSignerAllowedNilEvent: the predicate never panics on a nil event.
func TestFactSignerAllowedNilEvent(t *testing.T) {
	if !factSignerAllowed(nil, nil) {
		t.Fatal("a nil roster must allow (no membership check)")
	}
	if factSignerAllowed(nil, map[string]ValidatorKey{}) {
		t.Fatal("a nil event is not on any roster")
	}
}

// withLookup wires the one anchor seam for the duration of the test and restores
// the default afterwards.
func withLookup(t *testing.T, lookup BlockHashLookup) {
	t.Helper()
	prev := blockHashLookup
	blockHashLookup = lookup
	t.Cleanup(func() { blockHashLookup = prev })
}
