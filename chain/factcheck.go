package main

// The shared fact gate: ONE signer predicate and ONE anchor seam for every
// fact a validator publishes under its OWN key from Params.Validators
// (handover §6.3, §6.5).
//
// WHY IT IS SHARED.  The kinds 1443-1450 and 21447 are all "facts": an event
// signed by one validator, anchored to the committed height it refers to.  Each
// parser used to repeat the same three lines of roster membership, and the
// anchor was never bound to anything -- a reader could not tell a fact about
// block H from a fact that merely claimed H.  Both halves now live here, so a
// reader the repo owns has exactly one place to get them right.
//
// THE ANCHOR IS A SEAM, NOT A TRANSPORT (handover §8.7).  How a reader obtains
// the block hash for a height is an OPEN item: the owner has not chosen a
// transport (relay event, RPC, block store).  So the lookup is a package
// variable with a default that REFUSES, and no caller picks a transport.  When
// §8.7 is decided, one assignment fills it in and every caller gains the check
// without changing.

import (
	"errors"
	"fmt"
)

var (
	// ErrFactUnanchored: the fact's anchor is missing or nonsense -- a
	// non-positive height or an empty block hash says nothing to bind.
	ErrFactUnanchored = errors.New("fact_unanchored")
	// ErrFactAnchorMismatch: the block hash the lookup returned for the fact's
	// height is not the one the fact carries, so the fact is not bound to that
	// committed block.
	ErrFactAnchorMismatch = errors.New("fact_anchor_mismatch")
	// ErrAnchorLookupUnavailable: no block-hash lookup is wired (the transport
	// is an open item, handover §8.7), so nothing can be bound yet.
	ErrAnchorLookupUnavailable = errors.New("anchor_lookup_unavailable")
)

// BlockHashLookup returns the committed block hash at a height.  It is the
// contract behind the one anchor seam (handover §8.7).
type BlockHashLookup func(height int64) (string, error)

// blockHashLookup is the wired lookup.  It is a package variable, not a call to
// a concrete transport, so §8.7 can be filled in without touching a caller.
// (Go forbids a type and a variable sharing one name, so the seam is unexported
// while the type above carries the contract.)  The default refuses with
// ErrAnchorLookupUnavailable: with no transport wired, a fact cannot be bound,
// and a reader must say so rather than pretend.
var blockHashLookup BlockHashLookup = func(int64) (string, error) {
	return "", ErrAnchorLookupUnavailable
}

// factSignerAllowed is THE one roster predicate every fact reader shares
// (handover §6.3): a fact is published by each validator under its OWN key from
// Params.Validators.  A nil roster preserves the previous behaviour (no
// membership check) so a parser called without a roster still works; a non-nil
// roster refuses a signer that is not in it.  The caller supplies its own named
// Err*NotInRoster, so each parser keeps the refusal name it has always had.
func factSignerAllowed(ev *Event, roster map[string]ValidatorKey) bool {
	if roster == nil {
		return true
	}
	if ev == nil {
		return false
	}
	_, ok := roster[ev.Pubkey]
	return ok
}

// BindFactAnchor binds a fact to the committed block at its height: it refuses
// a missing anchor (ErrFactUnanchored), asks the wired BlockHashLookup for that
// height's block hash, and refuses a fact whose hash is not that block's
// (ErrFactAnchorMismatch).  A lookup error -- including the default
// ErrAnchorLookupUnavailable -- is propagated untouched, so a reader can tell
// "cannot bind yet" from "bound to the wrong block".
func BindFactAnchor(height int64, blockHash string) error {
	if height <= 0 || blockHash == "" {
		return fmt.Errorf("%w: height %d hash %q", ErrFactUnanchored, height, blockHash)
	}
	got, err := blockHashLookup(height)
	if err != nil {
		return err
	}
	if got == "" {
		return fmt.Errorf("%w: height %d", ErrAnchorLookupUnavailable, height)
	}
	if got != blockHash {
		return fmt.Errorf("%w: fact names %s at height %d, that block is %s", ErrFactAnchorMismatch, blockHash, height, got)
	}
	return nil
}

// CheckFactAtCommitment binds a fact with a kind-1447 state commitment the
// reader already holds (handover §6.3, §8.7): the commitment proves the height
// is a committed one, and the block hash comes from the SAME single lookup
// BindFactAnchor uses.  A nil commitment or one at another height is
// ErrFactAnchorMismatch -- the fact is not about that commitment's block.  The
// transport that OBTAINS the commitment stays outside: this function is the
// last step of a reader that already has both the fact and the commitment.
func CheckFactAtCommitment(height int64, blockHash string, commitment *StateCommitmentContent) error {
	if commitment == nil || commitment.Height != height {
		return fmt.Errorf("%w: fact at height %d has no commitment at that height", ErrFactAnchorMismatch, height)
	}
	return BindFactAnchor(height, blockHash)
}
