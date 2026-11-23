package main

// Tests for the create's mint and relay announcements (spec §3.6/§3.7,
// handover §6.4): a ROOT create must name the mint the chain trusts, a
// supersession may omit it but must not name another, relays are validated when
// present, and NONE of it is stored on the Auction (the state encoding does not
// change).

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestRootCreateMustNameTheTrustedMint: the deterministic admission rule.  A
// root create that names no mint, or a mint the chain does not trust, is
// refused by name and nothing is stored; the trusted mint is accepted, and a
// trailing slash is the only normalisation.
func TestRootCreateMustNameTheTrustedMint(t *testing.T) {
	t.Run("no mint", func(t *testing.T) {
		s := createRuleState()
		seller := newBidder(t)
		c := boundaryCreate()
		c.MintURL = ""
		err := s.applyCreate(createAtLimit(t, seller.sk, c, "mint-lot"), tNow, 1, true)
		if err == nil || err.Error() != "create must name a mint (mintUrl)" {
			t.Fatalf("want the missing-mint refusal, got %v", err)
		}
		if len(s.Auctions) != 0 {
			t.Fatal("a refused create must not be stored")
		}
	})

	t.Run("untrusted mint", func(t *testing.T) {
		s := createRuleState()
		seller := newBidder(t)
		c := boundaryCreate()
		c.MintURL = "http://127.0.0.1:9999"
		err := s.applyCreate(createAtLimit(t, seller.sk, c, "mint-lot"), tNow, 1, true)
		if err == nil || !strings.Contains(err.Error(), "create names an untrusted mint") {
			t.Fatalf("want the untrusted-mint refusal, got %v", err)
		}
		if len(s.Auctions) != 0 {
			t.Fatal("a refused create must not be stored")
		}
	})

	t.Run("trusted mint", func(t *testing.T) {
		for _, mint := range []string{testMintAddr, testMintAddr + "/"} {
			s := createRuleState()
			seller := newBidder(t)
			c := boundaryCreate()
			c.MintURL = mint
			if err := s.applyCreate(createAtLimit(t, seller.sk, c, "mint-lot"), tNow, 1, true); err != nil {
				t.Fatalf("mint %q must be accepted: %v", mint, err)
			}
			if s.Auctions[coordOf(seller.pub, "mint-lot")] == nil {
				t.Fatalf("mint %q: the lot was not stored", mint)
			}
		}
	})
}

// TestSupersessionMintRules: a supersession may omit the mint (the root already
// announced it) but must not name a different one.
func TestSupersessionMintRules(t *testing.T) {
	s := createRuleState()
	seller := newBidder(t)
	root := createAtLimit(t, seller.sk, boundaryCreate(), "mint-lot")
	if err := s.applyCreate(root, tNow, 1, true); err != nil {
		t.Fatalf("root create: %v", err)
	}
	coord := coordOf(seller.pub, "mint-lot")

	// A supersession naming ANOTHER mint is refused by name and changes nothing.
	bad := boundaryCreate()
	bad.Title = "edited"
	bad.MintURL = "http://127.0.0.1:9999"
	badEv := createEv(t, seller.sk, "mint-lot", bad, tNow+10, [2]string{"e", root.ID})
	if err := s.applyCreate(badEv, tNow+10, 2, true); err == nil || !strings.Contains(err.Error(), "create names an untrusted mint") {
		t.Fatalf("want the untrusted-mint refusal, got %v", err)
	}
	if au := s.Auctions[coord]; au.Title != "lot" {
		t.Fatalf("a refused supersession must not change the lot, title = %q", au.Title)
	}

	// A supersession that OMITS the mint is accepted.
	omitted := boundaryCreate()
	omitted.Title = "edited"
	omitted.MintURL = ""
	omitEv := createEv(t, seller.sk, "mint-lot", omitted, tNow+10, [2]string{"e", root.ID})
	if err := s.applyCreate(omitEv, tNow+10, 2, true); err != nil {
		t.Fatalf("a supersession omitting the mint must be accepted: %v", err)
	}
	if au := s.Auctions[coord]; au.Title != "edited" {
		t.Fatalf("the accepted supersession did not take: title = %q", au.Title)
	}

	// A supersession naming the SAME mint (trailing slash) is accepted too.
	same := boundaryCreate()
	same.Title = "edited again"
	same.MintURL = testMintAddr + "/"
	sameEv := createEv(t, seller.sk, "mint-lot", same, tNow+20, [2]string{"e", root.ID})
	if err := s.applyCreate(sameEv, tNow+20, 3, true); err != nil {
		t.Fatalf("a supersession naming the trusted mint must be accepted: %v", err)
	}
	if au := s.Auctions[coord]; au.Title != "edited again" {
		t.Fatalf("the accepted supersession did not take: title = %q", au.Title)
	}
}

// TestCreateRelayAnnouncements: ws:// or wss:// URLs with a host, at most 8, no
// duplicates -- and the list never enters committed state.
func TestCreateRelayAnnouncements(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		s := createRuleState()
		seller := newBidder(t)
		c := boundaryCreate()
		c.Relays = []string{"wss://relay.example", "ws://127.0.0.1:10547"}
		if err := s.applyCreate(createAtLimit(t, seller.sk, c, "relay-lot"), tNow, 1, true); err != nil {
			t.Fatalf("valid relays were refused: %v", err)
		}
		au := s.Auctions[coordOf(seller.pub, "relay-lot")]
		if au == nil {
			t.Fatal("the lot was not stored")
		}
		// The announcement is NOT state: an Auction has no relay field, so its
		// canonical encoding must not mention the announced URL.
		enc, err := json.Marshal(au)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(enc), "relay.example") {
			t.Fatalf("the relay announcement leaked into committed state: %s", enc)
		}
	})

	tooMany := make([]string, 0, MaxCreateRelays+1)
	for i := 0; i <= MaxCreateRelays; i++ {
		tooMany = append(tooMany, fmt.Sprintf("wss://relay%d.example", i))
	}

	refused := []struct {
		name   string
		relays []string
	}{
		{"http url", []string{"https://relay.example"}},
		{"bare scheme", []string{"ws://"}},
		{"duplicate", []string{"wss://a.example", "wss://a.example"}},
		{"too many", tooMany},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			s := createRuleState()
			seller := newBidder(t)
			c := boundaryCreate()
			c.Relays = tc.relays
			err := s.applyCreate(createAtLimit(t, seller.sk, c, "relay-lot"), tNow, 1, true)
			if err == nil || !strings.Contains(err.Error(), "relay") {
				t.Fatalf("relays %v must be refused by name, got %v", tc.relays, err)
			}
			if len(s.Auctions) != 0 {
				t.Fatal("a refused create must not be stored")
			}
		})
	}
}
