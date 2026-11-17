package main

// /api/tx must mean "accepted and pollable" (handover §3, workstream C).  These
// tests drive submitTx with httptest validator RPCs and a fake relay publisher:
// no real network, no real relay, no chain.

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cometbft/cometbft/types"
	"github.com/nbd-wtf/go-nostr"
)

// fakeRPCServer answers every broadcast_tx_sync with body and counts the hits.
func fakeRPCServer(t *testing.T, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// fakePublisher stands in for relayPublisher so no relay is contacted.
type fakePublisher struct {
	err       error
	published []Event
}

func (publisher *fakePublisher) Publish(_ context.Context, event Event) error {
	publisher.published = append(publisher.published, event)
	return publisher.err
}

// apiTxFixture builds a valid signed create and its tx bytes.
func apiTxFixture(t *testing.T, title string) (Event, []byte) {
	t.Helper()
	secret := nostr.GeneratePrivateKey()
	event := signedCreate(t, secret, title, 1_800_000_400)
	wire := toChainEvent(&event)
	tx, err := EventTransaction(wire)
	if err != nil {
		t.Fatalf("EventTransaction: %v", err)
	}
	return wire, tx
}

// TestApiTxRelayAndValidatorsAccept: a relay is configured and the validators
// accept.  The answer must be ok WITH a hash (the relay publish alone is not the
// answer), and the relay must have been used too.
func TestApiTxRelayAndValidatorsAccept(t *testing.T) {
	accept, hits := fakeRPCServer(t, `{"result":{"code":0,"log":"","hash":"DEADBEEF"}}`)
	event, tx := apiTxFixture(t, "ApiTx relay ok")
	publisher := &fakePublisher{}

	status, out := submitTx(context.Background(), http.DefaultClient,
		[]validatorRPC{{Name: "Validator 1", URL: accept.URL}}, event, tx, publisher)

	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("status=%d out=%v, want 200 ok", status, out)
	}
	if out["hash"] != "DEADBEEF" {
		t.Fatalf("hash = %v, want the RPC hash DEADBEEF", out["hash"])
	}
	if out["via"] != "Validator 1" {
		t.Fatalf("via = %v, want Validator 1", out["via"])
	}
	if *hits != 1 {
		t.Fatalf("validator hits = %d, want 1", *hits)
	}
	if len(publisher.published) != 1 || publisher.published[0].ID != event.ID {
		t.Fatalf("relay published = %v, want the event", publisher.published)
	}
}

// TestApiTxRelayPublishFailsStillOK: a relay failure is best effort.  If a
// validator accepts, the request is still ok and pollable.
func TestApiTxRelayPublishFailsStillOK(t *testing.T) {
	accept, _ := fakeRPCServer(t, `{"result":{"code":0,"log":"","hash":"F00D"}}`)
	event, tx := apiTxFixture(t, "ApiTx relay down")
	publisher := &fakePublisher{err: context.DeadlineExceeded}

	status, out := submitTx(context.Background(), http.DefaultClient,
		[]validatorRPC{{Name: "Validator 2", URL: accept.URL}}, event, tx, publisher)

	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("status=%d out=%v, want 200 ok despite the relay failure", status, out)
	}
	if out["hash"] != "F00D" {
		t.Fatalf("hash = %v, want F00D", out["hash"])
	}
	if len(publisher.published) != 1 {
		t.Fatalf("relay publish attempts = %d, want 1 (attempted, then ignored)", len(publisher.published))
	}
}

// TestApiTxAllValidatorsDown: with every RPC unreachable there is no acceptor,
// so the request must NOT be ok.
func TestApiTxAllValidatorsDown(t *testing.T) {
	first, _ := fakeRPCServer(t, `{"result":{"code":0,"log":"","hash":"x"}}`)
	second, _ := fakeRPCServer(t, `{"result":{"code":0,"log":"","hash":"y"}}`)
	first.Close()
	second.Close()

	event, tx := apiTxFixture(t, "ApiTx all down")
	publisher := &fakePublisher{}

	status, out := submitTx(context.Background(), http.DefaultClient,
		[]validatorRPC{{Name: "Validator 1", URL: first.URL}, {Name: "Validator 2", URL: second.URL}},
		event, tx, publisher)

	if out["ok"] == true {
		t.Fatalf("status=%d out=%v, want NOT ok when no validator accepted", status, out)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (transient, retryable)", status)
	}
	if out["hash"] != nil {
		t.Fatalf("out = %v, want no hash on a failed submit", out)
	}
}

// TestApiTxAlreadyInCache: a node that already has the tx answers with an error
// object carrying "already exists in cache".  That is success, and the hash must
// still be returned so the caller can poll.
func TestApiTxAlreadyInCache(t *testing.T) {
	cached, _ := fakeRPCServer(t, `{"error":{"message":"tx already exists in cache"}}`)
	event, tx := apiTxFixture(t, "ApiTx cached")

	status, out := submitTx(context.Background(), http.DefaultClient,
		[]validatorRPC{{Name: "Validator 3", URL: cached.URL}}, event, tx, nil)

	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("status=%d out=%v, want 200 ok", status, out)
	}
	if out["hash"] != txHash(tx) {
		t.Fatalf("hash = %v, want the locally computed %s", out["hash"], txHash(tx))
	}
	if out["note"] != "already submitted" {
		t.Fatalf("note = %v, want already submitted", out["note"])
	}
}

// TestApiTxTerminalRejection: an application-level rejection from every
// validator is a 400, and the message survives.
func TestApiTxTerminalRejection(t *testing.T) {
	rejected, _ := fakeRPCServer(t, `{"result":{"code":1,"log":"invalid signature"}}`)
	event, tx := apiTxFixture(t, "ApiTx rejected")

	status, out := submitTx(context.Background(), http.DefaultClient,
		[]validatorRPC{{Name: "Validator 4", URL: rejected.URL}}, event, tx, nil)

	if status != http.StatusBadRequest || out["ok"] == true {
		t.Fatalf("status=%d out=%v, want 400 not-ok", status, out)
	}
	if out["error"] != "invalid signature" {
		t.Fatalf("error = %v, want the CheckTx log", out["error"])
	}
}

// TestApiTxLateBidKeepsLateField: a late bid commits as void; the `late: ` log
// must still be surfaced, and the hash returned.
func TestApiTxLateBidKeepsLateField(t *testing.T) {
	late, _ := fakeRPCServer(t, `{"result":{"code":0,"log":"late: snipe window open","hash":"1234"}}`)
	event, tx := apiTxFixture(t, "ApiTx late")

	status, out := submitTx(context.Background(), http.DefaultClient,
		[]validatorRPC{{Name: "Validator 1", URL: late.URL}}, event, tx, nil)

	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("status=%d out=%v, want 200 ok", status, out)
	}
	if out["late"] != "snipe window open" {
		t.Fatalf("late = %v, want the trimmed log", out["late"])
	}
	if out["hash"] != "1234" {
		t.Fatalf("hash = %v, want 1234", out["hash"])
	}
}

// TestApiTxHashIsCometBFTTxHash pins the local hash to CometBFT's own:
// types.Tx.Hash() is sha256 over the raw tx bytes, and the RPC reports it as
// uppercase hex.  A client polling by this hash must match what the node saw.
func TestApiTxHashIsCometBFTTxHash(t *testing.T) {
	_, tx := apiTxFixture(t, "ApiTx hash")
	want := strings.ToUpper(hex.EncodeToString(types.Tx(tx).Hash()))
	if got := txHash(tx); got != want {
		t.Fatalf("txHash = %s, want CometBFT's %s", got, want)
	}
	if len(want) != 64 {
		t.Fatalf("hash length = %d, want 64", len(want))
	}
}
