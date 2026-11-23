package main

// The mint-capability gate (handover §3.7, §6.4).
//
// A mint is trusted only if its GET /v1/info advertises NUT-10 (P2PK) and
// NUT-11 (SIG_ALL) support.  NUT-11 is explicit: without it, "Proofs may be
// treated as regular anyone-can-spend Proofs", so a validator escrowing a
// bidder's proofs against such a mint is signing for a lock that does not hold.
//
// The check is cheap and it is RECORDED: `arena init` runs it before it pins the
// keyset, and every node runs it against the committed Params.MintURL before it
// starts.  It is deliberately NOT called from applyCreate or the consensus
// path -- that would be a network call inside the deterministic transition.  The
// deterministic rule there is the equality check (a root create must name
// exactly the mint the chain trusts, see applyCreate); this gate is what makes
// the trusted mint a CAPABLE one in the first place.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// nutP2PK is NUT-10: the P2PK lock the escrow's proofs carry.
	nutP2PK = 10
	// nutSigAll is NUT-11: SIG_ALL, which makes the 2-of-2 [bidder, committee]
	// lock bind every input at once.  Without it the mint treats the proofs as
	// anyone-can-spend.
	nutSigAll = 11
	// mintInfoTimeout bounds the whole GET /v1/info round trip.  It is a
	// context deadline (not a global http.Client timeout), so it cannot leak
	// into any other caller.
	mintInfoTimeout = 5 * time.Second
)

// mintInfo is the subset of GET <mint>/v1/info this gate reads.  `nuts` is keyed
// by the NUT number as a decimal string (NUT-06).
type mintInfo struct {
	Nuts map[string]json.RawMessage `json:"nuts"`
}

// requireMintCapabilities does GET <mintURL>/v1/info with a 5 s timeout and
// requires BOTH NUT-10 and NUT-11 to be present in `nuts`.  It returns an error
// naming exactly what is missing (or what failed), so the caller can log it and
// exit; the caller decides whether that is fatal.
func requireMintCapabilities(ctx context.Context, mintURL string) error {
	if strings.TrimSpace(mintURL) == "" {
		return errors.New("no mint url to check")
	}
	ctx, cancel := context.WithTimeout(ctx, mintInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(mintURL, "/")+"/v1/info", nil)
	if err != nil {
		return fmt.Errorf("mint %s: %w", mintURL, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mint %s: /v1/info unreachable: %w", mintURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mint %s: /v1/info returned %s", mintURL, resp.Status)
	}
	var info mintInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return fmt.Errorf("mint %s: bad /v1/info: %w", mintURL, err)
	}
	var missing []string
	for _, nut := range []int{nutP2PK, nutSigAll} {
		if _, ok := info.Nuts[strconv.Itoa(nut)]; !ok {
			missing = append(missing, "NUT-"+strconv.Itoa(nut))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("mint %s does not advertise %s: the escrow needs a P2PK+SIG_ALL mint (NUT-10 and NUT-11)", mintURL, strings.Join(missing, " and "))
	}
	return nil
}

// genesisParams reads the committed app state (Params) out of a validator's
// genesis.json.  A node checks its committed mint before it starts, and the
// genesis file is the one place that value is available before the ABCI app has
// run InitChain.
func genesisParams(path string) (Params, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Params{}, err
	}
	var g struct {
		AppState Params `json:"app_state"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return Params{}, err
	}
	return g.AppState, nil
}
