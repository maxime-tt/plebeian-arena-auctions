package main

// Tests for the create-content rules (createrules.go) and their publication in
// the kind-1448 params event.
//
// The point of these constants is that the chain's enforcement and what it
// TELLS a relay-only client are the same numbers, so the tests here meet both
// sides at every boundary: applyCreate must accept a create exactly AT each
// limit and refuse boundary+1 with the named reason, and BuildParamsEvent must
// publish exactly those limits.  A client that pre-validates from params alone
// must reach the same verdict the chain would.
//
// paramsVersion digests Params (committed state), not ParamsContent, so these
// rules must NOT move it: TestParamsVersionUnchangedByCreateRules pins the
// digest of a fixed Params literal, and a future edit that slips the rules into
// Params (or reorders the digest input) fails there instead of silently
// changing every params_version the chain publishes.

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// createRuleState is a state that accepts a create on the non-boundary fields,
// so a case can vary exactly one keep-out dimension.
func createRuleState() *State {
	s := newState()
	s.Params = Params{MinDurationSec: 20, MaxDurationSec: 259200, MinWindowSec: 60, MaxWindowSec: 900, MintURL: testMintAddr}
	return s
}

// createAtLimit signs a create whose content is a valid lot, with tags = [d].
func createAtLimit(t *testing.T, sk *btcec.PrivateKey, c CreateContent, d string) *Event {
	t.Helper()
	return signEventTags(t, sk, KindCreate, string(must(json.Marshal(c))), tNow, [][]string{{"d", d}})
}

// boundaryCreate is baseCreate with a long enough d-less content; the caller
// adjusts one field to the value under test.
func boundaryCreate() CreateContent {
	return CreateContent{
		Title: "lot", Description: "d", Image: "/a.png", Category: "art",
		Name: "seller", StartPrice: 100, MinIncrement: 10, DurationSec: 100,
		Mode: "fixed", SellerWallet: sellerWalletKey(), MintURL: testMintAddr,
	}
}

// TestCreateRulesPublishedInParams checks that the params event carries every
// rule, that each published value is the constant, and that image_rule names
// the chain's rule.
func TestCreateRulesPublishedInParams(t *testing.T) {
	// The constants themselves are pinned by value: the wire carries these
	// numbers to a client that must pre-validate offline, so a change here is a
	// protocol change, not a tuning knob.
	pinned := []struct {
		name string
		got  int64
		want int64
	}{
		{"MaxTitleChars", MaxTitleChars, 120},
		{"MaxDescriptionChars", MaxDescriptionChars, 2000},
		{"MaxImageChars", MaxImageChars, 500},
		{"MaxCategoryChars", MaxCategoryChars, 40},
		{"MaxNameChars", MaxNameChars, 40},
		{"MinStartPrice", MinStartPrice, 1},
		{"MaxStartPrice", MaxStartPrice, 10_000_000},
		{"MinIncrement", MinIncrement, 1},
		{"MaxIncrement", MaxIncrement, 1_000_000},
	}
	for _, c := range pinned {
		if c.got != c.want {
			t.Fatalf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if ImageRule != "same-site-or-https" {
		t.Fatalf("ImageRule = %q, want %q", ImageRule, "same-site-or-https")
	}

	p := paramsForRulesTest()
	ev, err := BuildParamsEvent(newBidder(t).sk, p, 77, strings.Repeat("cd", 32), tNow)
	if err != nil {
		t.Fatalf("BuildParamsEvent: %v", err)
	}
	c, err := ParseParamsEvent(ev, nil)
	if err != nil {
		t.Fatalf("ParseParamsEvent: %v", err)
	}
	published := []struct {
		name string
		got  int64
		want int64
	}{
		{"max_title_chars", int64(c.MaxTitleChars), MaxTitleChars},
		{"max_description_chars", int64(c.MaxDescriptionChars), MaxDescriptionChars},
		{"max_image_chars", int64(c.MaxImageChars), MaxImageChars},
		{"max_category_chars", int64(c.MaxCategoryChars), MaxCategoryChars},
		{"max_name_chars", int64(c.MaxNameChars), MaxNameChars},
		{"start_price_min", c.StartPriceMin, MinStartPrice},
		{"start_price_max", c.StartPriceMax, MaxStartPrice},
		{"increment_min", c.IncrementMin, MinIncrement},
		{"increment_max", c.IncrementMax, MaxIncrement},
	}
	for _, r := range published {
		if r.got != r.want {
			t.Fatalf("%s published as %d, want the enforced constant %d", r.name, r.got, r.want)
		}
	}
	if c.ImageRule != ImageRule {
		t.Fatalf("image_rule = %q, want %q", c.ImageRule, ImageRule)
	}
}

// TestCreateRulesEnforcedAtBoundary drives applyCreate to the edge of every
// numeric limit: exactly at the limit it must be accepted, one past it refused
// by name.  Each accepted case also checks that the limit the params event
// PUBLISHES is the limit the enforcement accepted — the two sides can never
// drift apart without this failing.
func TestCreateRulesEnforcedAtBoundary(t *testing.T) {
	published, err := BuildParamsEvent(newBidder(t).sk, paramsForRulesTest(), 1, strings.Repeat("ab", 32), tNow)
	if err != nil {
		t.Fatalf("BuildParamsEvent: %v", err)
	}
	wire, err := ParseParamsEvent(published, nil)
	if err != nil {
		t.Fatalf("ParseParamsEvent: %v", err)
	}

	// long returns a string that is valid wherever the field's own rule is
	// satisfied: ASCII letters only, and for the image an https prefix.
	long := func(n int) string { return strings.Repeat("a", n) }

	type boundary struct {
		name      string                     // what the case varies
		set       func(*CreateContent, int)  // set the field to n (the candidate length/value)
		atLimit   int                        // the value that must be accepted
		past      int                        // the value that must be refused
		wantErr   string                     // refusal reason for `past`
		published func(*ParamsContent) int64 // the rule the client reads off params
	}

	cases := []boundary{
		{
			name:    "title",
			set:     func(c *CreateContent, n int) { c.Title = long(n) },
			atLimit: MaxTitleChars, past: MaxTitleChars + 1,
			wantErr:   "title required (max 120 chars)",
			published: func(c *ParamsContent) int64 { return int64(c.MaxTitleChars) },
		},
		{
			name:    "description",
			set:     func(c *CreateContent, n int) { c.Description = long(n) },
			atLimit: MaxDescriptionChars, past: MaxDescriptionChars + 1,
			wantErr:   "field too long",
			published: func(c *ParamsContent) int64 { return int64(c.MaxDescriptionChars) },
		},
		{
			name:    "image",
			set:     func(c *CreateContent, n int) { c.Image = "https://" + long(n-8) },
			atLimit: MaxImageChars, past: MaxImageChars + 1,
			wantErr:   "field too long",
			published: func(c *ParamsContent) int64 { return int64(c.MaxImageChars) },
		},
		{
			name:    "category",
			set:     func(c *CreateContent, n int) { c.Category = long(n) },
			atLimit: MaxCategoryChars, past: MaxCategoryChars + 1,
			wantErr:   "field too long",
			published: func(c *ParamsContent) int64 { return int64(c.MaxCategoryChars) },
		},
		{
			name:    "name",
			set:     func(c *CreateContent, n int) { c.Name = long(n) },
			atLimit: MaxNameChars, past: MaxNameChars + 1,
			wantErr:   "field too long",
			published: func(c *ParamsContent) int64 { return int64(c.MaxNameChars) },
		},
		{
			name:    "start+",
			set:     func(c *CreateContent, n int) { c.StartPrice = int64(n) },
			atLimit: int(MaxStartPrice), past: int(MaxStartPrice) + 1,
			wantErr:   "start price out of range",
			published: func(c *ParamsContent) int64 { return c.StartPriceMax },
		},
		{
			name:    "start-",
			set:     func(c *CreateContent, n int) { c.StartPrice = int64(n) },
			atLimit: int(MinStartPrice), past: int(MinStartPrice) - 1,
			wantErr:   "start price out of range",
			published: func(c *ParamsContent) int64 { return c.StartPriceMin },
		},
		{
			name:    "increment+",
			set:     func(c *CreateContent, n int) { c.MinIncrement = int64(n) },
			atLimit: int(MaxIncrement), past: int(MaxIncrement) + 1,
			wantErr:   "increment out of range",
			published: func(c *ParamsContent) int64 { return c.IncrementMax },
		},
		{
			name:    "increment-",
			set:     func(c *CreateContent, n int) { c.MinIncrement = int64(n) },
			atLimit: int(MinIncrement), past: int(MinIncrement) - 1,
			wantErr:   "increment out of range",
			published: func(c *ParamsContent) int64 { return c.IncrementMin },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/at-limit", func(t *testing.T) {
			s := createRuleState()
			seller := newBidder(t)
			c := boundaryCreate()
			tc.set(&c, tc.atLimit)
			ev := createAtLimit(t, seller.sk, c, "rule-lot")
			if err := s.applyCreate(ev, tNow, 1, true); err != nil {
				t.Fatalf("%s at its limit %d was refused: %v", tc.name, tc.atLimit, err)
			}
			au := s.Auctions[coordOf(ev.Pubkey, "rule-lot")]
			if au == nil {
				t.Fatalf("%s at its limit was not stored as an auction", tc.name)
			}
			var stored int64
			switch tc.name {
			case "title":
				stored = int64(len(au.Title))
			case "description":
				stored = int64(len(au.Description))
			case "image":
				stored = int64(len(au.Image))
			case "category":
				stored = int64(len(au.Category))
			case "name":
				stored = int64(len(au.SellerName))
			case "start+", "start-":
				stored = au.StartPrice
			case "increment+", "increment-":
				stored = au.MinIncrement
			}
			if stored != int64(tc.atLimit) {
				t.Fatalf("%s: stored %d, want the boundary %d", tc.name, stored, tc.atLimit)
			}
			// The published rule must be exactly the value the chain took.
			if want := tc.published(wire); stored != want {
				t.Fatalf("%s: chain accepted %d but params publishes %d", tc.name, stored, want)
			}
		})

		t.Run(tc.name+"/past-limit", func(t *testing.T) {
			s := createRuleState()
			seller := newBidder(t)
			c := boundaryCreate()
			tc.set(&c, tc.past)
			ev := createAtLimit(t, seller.sk, c, "rule-lot")
			err := s.applyCreate(ev, tNow, 1, true)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("%s past its limit %d: want %q, got %v", tc.name, tc.past, tc.wantErr, err)
			}
			// The refusal must name the same boundary the client reads off params.
			if want := tc.published(wire); want != int64(tc.atLimit) {
				t.Fatalf("%s: refused at %d but params publishes %d", tc.name, tc.atLimit, want)
			}
			if len(s.Auctions) != 0 {
				t.Fatal("a refused create must not be stored")
			}
		})
	}
}

// TestCreateImageRule checks the image rule the params event names, byte for
// byte against safeImage: https and same-site paths pass, protocol-relative,
// plain http, and anything outside the URI-safe set do not.
func TestCreateImageRule(t *testing.T) {
	accepted := []string{
		"",
		"https://example.com/a.png",
		"https://example.com/a.png?x=1&y=2#z",
		"/same-site.png",
		"./same-site.png",
	}
	for _, img := range accepted {
		if !safeImage(img) {
			t.Fatalf("image %q must satisfy the %s rule", img, ImageRule)
		}
		s := createRuleState()
		seller := newBidder(t)
		c := boundaryCreate()
		c.Image = img
		ev := createAtLimit(t, seller.sk, c, "rule-lot")
		if err := s.applyCreate(ev, tNow, 1, true); err != nil {
			t.Fatalf("image %q was refused by applyCreate: %v", img, err)
		}
	}

	refused := []string{
		"http://example.com/a.png",
		"//host/x.png",
		"https://example.com/a b.png",
		"https://example.com/<script>.png",
		"https://example.com/a\".png",
		"javascript:alert(1)",
	}
	for _, img := range refused {
		if safeImage(img) {
			t.Fatalf("image %q must be refused by the %s rule", img, ImageRule)
		}
		s := createRuleState()
		seller := newBidder(t)
		c := boundaryCreate()
		c.Image = img
		ev := createAtLimit(t, seller.sk, c, "rule-lot")
		err := s.applyCreate(ev, tNow, 1, true)
		if err == nil || err.Error() != "image must be a same-site path or an https URL" {
			t.Fatalf("image %q: want the image reason, got %v", img, err)
		}
	}
}

// TestCreateTitleBlankRefused keeps the emptiness rule (not a length rule)
// pinned to the same reason the refusal carries.
func TestCreateTitleBlankRefused(t *testing.T) {
	s := createRuleState()
	seller := newBidder(t)
	c := boundaryCreate()
	c.Title = "   "
	ev := createAtLimit(t, seller.sk, c, "rule-lot")
	err := s.applyCreate(ev, tNow, 1, true)
	if err == nil || err.Error() != "title required (max "+strconv.Itoa(MaxTitleChars)+" chars)" {
		t.Fatalf("a blank title must be refused by name, got %v", err)
	}
}

// paramsVersionDigest is the sha256 of the canonical JSON of paramsForRulesTest.
// It is PINNED: params_version is the wire identity of the committed terms, and
// the create-content rules are code (createrules.go), not state.  If a future
// edit adds these rules to Params, reorders Params, or changes the digest
// encoding, this digest moves and every params_version the chain publishes
// changes with it — which would break every client that compared a params
// event against a receipt.  That must fail here, loudly, instead.
const paramsVersionDigest = "59c82a68c2bf8437f4259422adeb107f373e62ac5e5da41c582e79045b532aeb"

// paramsForRulesTest is a fixed Params literal: every digest-relevant field is
// spelled out, so the pinned digest means something stable.
func paramsForRulesTest() Params {
	return Params{
		MintURL:      testMintAddr,
		KeysetID:     "0102b930f4dc2d6a",
		Keys:         map[string]string{"1": "02aa", "2": "02bb", "64": "02cc"},
		GroupKey:     "03be47f12cc26f63cedb617ecc5dae10485aab91a63bd7ba808ae7c4f23e853da4",
		MinWindowSec: 60, MaxWindowSec: 900,
		SettlementSafetySec: 900, BidToConsensusLatencySec: 15,
		MinDurationSec: 20, MaxDurationSec: 100000, MaxProofs: 64, Threshold: 3,
		Validators: []ValidatorKey{
			{Pubkey: "aa00000000000000000000000000000000000000000000000000000000000001", Name: "v1", Index: 1},
			{Pubkey: "bb00000000000000000000000000000000000000000000000000000000000002", Name: "v2", Index: 2},
		},
	}
}

// TestParamsVersionUnchangedByCreateRules pins paramsVersion to a fixed digest
// and checks that the params event built from that Params reports it — the
// published rules ride alongside the digest without entering it.
func TestParamsVersionUnchangedByCreateRules(t *testing.T) {
	p := paramsForRulesTest()
	if got := paramsVersion(p); got != paramsVersionDigest {
		t.Fatalf("paramsVersion = %q, want the pinned %q: the committed terms (or the digest encoding) moved, and every published params_version with them", got, paramsVersionDigest)
	}
	ev, err := BuildParamsEvent(newBidder(t).sk, p, 9, strings.Repeat("ef", 32), tNow)
	if err != nil {
		t.Fatalf("BuildParamsEvent: %v", err)
	}
	c, err := ParseParamsEvent(ev, nil)
	if err != nil {
		t.Fatalf("ParseParamsEvent: %v", err)
	}
	if c.ParamsVersion != paramsVersionDigest {
		t.Fatalf("params_version = %q, want the pinned digest", c.ParamsVersion)
	}
}
