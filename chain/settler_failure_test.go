package main

// A mint swap that fails must be legible. Before this, swap() kept only the
// mint's `detail` -- for a non-denomination output the whole body IS the bare
// amount ("80") -- so the error and the log line named no rule and no status,
// and the journal recorded no error at all. These tests pin the four
// properties the change must hold:
//
//   - a refusal (non-2xx with a body): the error AND the journal record carry
//     the status and the body verbatim, beside the settlement's own identifiers;
//   - a healthy swap is unchanged (no failure record, same signatures);
//   - a transport failure (nothing listening) is distinguishable from a
//     refusal: it has no HTTP status and must not masquerade as one;
//   - the appended failure record carries no outputs, so crash recovery
//     (recover) cannot mistake it for a swapped attempt whose signature was lost.
//
// The journal keys of an attempt are unchanged (append, never rename): the
// first record's keys are asserted verbatim, so today's journal stays readable.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// swapFixture: one committed outbid escrow (so its refund move is due) with the
// exact material settle() has in hand at the mint swap -- the committed
// template's outputs, openings and bidder signature -- plus a temp journal and
// the escrow's own proofs. mint/hc are left for each test to point at a stub.
func swapFixture(t *testing.T) (*settler, *settleAttempt, []Proof, *testMint) {
	t.Helper()
	a, m, id := liveApp(t)
	alice, bob := newBidder(t), newBidder(t)
	ev1, tx1 := m.bidTx(t, a, alice, id, 150, "aa")
	_, tx2 := m.bidTx(t, a, bob, id, 200, "bb")
	finalize(t, a, 11, tNow+1, tx1, tx2)
	bid := a.st.Bids[ev1.ID]
	if bid.Status != "outbid" {
		t.Fatalf("setup: the first escrow must be outbid, got %q", bid.Status)
	}
	s := testSettler(t, a)
	outs, reveal, bidderSig, err := s.templateMaterial(bid, "refund")
	if err != nil {
		t.Fatal(err)
	}
	at := &settleAttempt{Bid: bid.ID, Outcome: "refund", To: alice.wallet, Auction: bid.Auction,
		Outputs: outs, Reveal: reveal, BidderSig: bidderSig, Sig: "00" /* placeholder committee half */, At: tNow}
	return s, at, bid.Proofs, m
}

// journalRaw returns the journal's lines, in order, as written.
func journalRaw(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// TestFailedSwapNamesStatusAndBody: the mint answers 400 with a body, and that
// status and body reach both the error and a second journal record.
func TestFailedSwapNamesStatusAndBody(t *testing.T) {
	s, at, inputs, _ := swapFixture(t)
	const body = `{"detail":"80","code":0}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	s.mint, s.hc = srv.URL, &http.Client{}

	// Production order (settle, chain/settler.go): journal the attempt, then
	// swap; a failure appends a second record beside it.
	if err := s.journalSubmission(at); err != nil {
		t.Fatal(err)
	}
	_, err := s.swapAndJournal(at, inputs)
	if err == nil {
		t.Fatal("a 400 from the mint must fail the swap")
	}

	// The ERROR names the mint, the status and the body.
	var mse *mintSwapError
	if !errors.As(err, &mse) {
		t.Fatalf("a refusal must carry the status and the body, got %T: %v", err, err)
	}
	if mse.Status != http.StatusBadRequest || mse.Body != body {
		t.Fatalf("captured status %d body %q, want 400 %q", mse.Status, mse.Body, body)
	}
	for _, want := range []string{srv.URL, "400", `"detail":"80"`, `"code":0`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the failure must name %q, got %q", want, err.Error())
		}
	}
	// What an operator reads in run/chain.log for this attempt (sweep, chain/settler.go).
	t.Logf("an operator would read: settle %s %s: %v", at.Outcome, at.Bid[:12], err)

	// The JOURNAL: the attempt's own record is untouched, and a second record
	// carries the status, the body and the attempt's identifiers.
	lines := journalRaw(t, s.journal)
	if len(lines) != 2 {
		t.Fatalf("want the attempt and one failure record, got %d lines", len(lines))
	}
	var submission map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &submission); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"bid", "outcome", "to", "auction", "outputs", "reveal", "bidder_sig", "sig", "at"} {
		if _, ok := submission[k]; !ok {
			t.Fatalf("the existing journal key %q must stay: %s", k, lines[0])
		}
	}
	for _, k := range []string{"http_status", "error"} {
		if _, ok := submission[k]; ok {
			t.Fatalf("the submission record must not carry %q: %s", k, lines[0])
		}
	}
	if strings.Contains(lines[1], `"outputs"`) {
		t.Fatalf("a failure record carries no outputs: %s", lines[1])
	}
	var fail settleAttempt
	if err := json.Unmarshal([]byte(lines[1]), &fail); err != nil {
		t.Fatal(err)
	}
	if fail.Bid != at.Bid || fail.Outcome != "refund" || fail.To != at.To || fail.At != at.At {
		t.Fatalf("the failure record must name the settlement: %+v", fail)
	}
	if fail.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("the failure record must carry the status, got %d", fail.HTTPStatus)
	}
	for _, want := range []string{srv.URL, "400", body} {
		if !strings.Contains(fail.Error, want) {
			t.Fatalf("the failure record must carry %q, got %q", want, fail.Error)
		}
	}
	if len(s.tries[at.Bid]) != 2 {
		t.Fatalf("in memory the failure record must sit beside the attempt, got %d", len(s.tries[at.Bid]))
	}
}

// TestHealthySwapIsUnchanged: a 200 with the signatures still returns them and
// writes no failure record.
func TestHealthySwapIsUnchanged(t *testing.T) {
	s, at, inputs, m := swapFixture(t)
	sigs := make([]blindSig, len(at.Outputs))
	for i, o := range at.Outputs {
		sigs[i] = m.blindSign(t, o)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"signatures": sigs})
	}))
	defer srv.Close()
	s.mint, s.hc = srv.URL, &http.Client{}

	if err := s.journalSubmission(at); err != nil {
		t.Fatal(err)
	}
	got, err := s.swapAndJournal(at, inputs)
	if err != nil {
		t.Fatalf("a healthy swap must succeed: %v", err)
	}
	if len(got) != len(at.Outputs) {
		t.Fatalf("the mint's %d signatures must come back whole, got %d", len(at.Outputs), len(got))
	}
	if lines := journalRaw(t, s.journal); len(lines) != 1 {
		t.Fatalf("a healthy swap writes no failure record, got %d lines", len(lines))
	}
	if len(s.tries[at.Bid]) != 1 {
		t.Fatalf("a healthy swap appends nothing, got %d records", len(s.tries[at.Bid]))
	}
}

// TestTransportFailureIsNotARefusal: nothing listens at the mint URL, so no
// HTTP status exists; the error and the journal record must say so rather than
// report a status and a body.
func TestTransportFailureIsNotARefusal(t *testing.T) {
	s, at, inputs, _ := swapFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close() // nothing listens now
	s.mint, s.hc = dead, &http.Client{Timeout: 2 * time.Second}

	if err := s.journalSubmission(at); err != nil {
		t.Fatal(err)
	}
	_, err := s.swapAndJournal(at, inputs)
	if err == nil {
		t.Fatal("a dead mint must fail the swap")
	}
	var mse *mintSwapError
	if errors.As(err, &mse) {
		t.Fatalf("a transport failure has no HTTP status and must not look like a refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "mint unreachable") || !strings.Contains(err.Error(), dead) {
		t.Fatalf("the error must say the mint was unreachable and name it: %v", err)
	}
	if strings.Contains(err.Error(), "HTTP ") {
		t.Fatalf("a transport failure must not claim an HTTP status: %v", err)
	}
	t.Logf("an operator would read: settle %s %s: %v", at.Outcome, at.Bid[:12], err)
	lines := journalRaw(t, s.journal)
	var fail settleAttempt
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &fail); err != nil {
		t.Fatal(err)
	}
	if fail.HTTPStatus != 0 || !strings.Contains(fail.Error, "mint unreachable") {
		t.Fatalf("the journal record must distinguish the transport failure: %+v", fail)
	}
}

// TestFailureRecordDoesNotMaskRecovery: a failure record has no outputs, so
// recover must skip it. With the mint answering restore (200, empty), the old
// code would have read it as an attempt whose signature was not journalled and
// stopped; the correct answer is that no attempt matches.
func TestFailureRecordDoesNotMaskRecovery(t *testing.T) {
	failure := &settleAttempt{Bid: hx(9), Outcome: "refund", To: hx(10), At: tNow,
		HTTPStatus: 400, Error: `mint swap at http://mint refused: HTTP 400: {"detail":"80","code":0}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"outputs":[],"signatures":[]}`)
	}))
	defer srv.Close()
	s := &settler{mint: srv.URL, hc: &http.Client{}, done: map[string]*settleAttempt{},
		tries: map[string][]*settleAttempt{failure.Bid: {failure}}}

	err := s.recover(dueMove{Bid: &Bid{ID: failure.Bid}, Outcome: "refund"})
	if err == nil || !strings.Contains(err.Error(), "no journalled attempt matches") {
		t.Fatalf("a failure record is not an attempt to restore: %v", err)
	}
	if strings.Contains(err.Error(), "signature was not journalled") {
		t.Fatalf("the failure record must not be mistaken for an attempt: %v", err)
	}
}
