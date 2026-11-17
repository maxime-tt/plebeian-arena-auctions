package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Submitter turns an observed signed event into a committed chain transaction.
//
// It is deliberately an interface with one method: the observer loop has no
// business knowing whether the transaction reaches the chain through a local
// CometBFT RPC endpoint, a remote one, or a fake in a test.  That is what lets
// one observer run per validator without the loop changing.
type Submitter interface {
	SubmitEvent(ctx context.Context, event Event) (SubmitResult, error)
}

// SubmitFailure is why a submission was refused, and whether the refusal is
// worth another attempt.  An untyped error forces every caller to guess: the
// observer must retry "endpoint is closed while node is catching up" (the node
// is simply not ready) but must never retry "CheckTx rejected" (the app has
// answered, and the answer is no).
type SubmitFailure struct {
	// Transient is true when the same submission may succeed later: a
	// transport refusal, an RPC-layer error object (CometBFT reports "endpoint
	// is closed while node is catching up" there), a body that did not decode,
	// or a broadcast that returned no result.  It is false when the chain
	// itself rejected the transaction and would reject it again.
	Transient bool
	// Message is the human-readable reason, unchanged from the text this
	// package reported before it was typed.
	Message string
}

func (failure *SubmitFailure) Error() string { return failure.Message }

// appRejectionCode returns the application-level code carried in a CometBFT
// broadcast error message, or 0 when the message carries none (a transport or
// readiness refusal).
//
// CometBFT's mempool formats a CheckTx rejection as
// "tx %X is invalid: code=%d, data=%X, log='%s', codespace='%s'"
// (mempool/errors.go) and the RPC layer wraps it as
// "broadcast error on transaction validation: %w" (rpc/core/mempool.go).  A
// non-zero code is the application's ANSWER, and retrying it can never change
// it, so it must be terminal.  This is why the split is read off the MESSAGE
// and not off the RPC error object: that object carries both readiness problems
// ("endpoint is closed while node is catching up") and answers, and only the
// text tells them apart.
func appRejectionCode(message string) uint32 {
	const marker = "is invalid: code="
	start := strings.Index(message, marker)
	if start < 0 {
		return 0
	}
	digits := message[start+len(marker):]
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	code, err := strconv.ParseUint(digits[:end], 10, 32)
	if err != nil {
		return 0
	}
	return uint32(code)
}

// IsTransientSubmit reports whether err carries a *SubmitFailure that says the
// same submission may be attempted again.  It walks the whole wrap chain, so a
// caller may wrap the failure with context (as the observer does) without
// hiding the classification.
func IsTransientSubmit(err error) bool {
	for err != nil {
		if failure, ok := err.(*SubmitFailure); ok && failure.Transient {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// SubmitResult mirrors what the HTTP /api/tx path already reports back: the
// height the transaction landed at and the application code.  A caller can log
// it and a test can assert on it.
type SubmitResult struct {
	Height int64
	Code   uint32
	TxHash string
}

// EventTransaction builds the exact transaction bytes the gateway's /api/tx
// path submits: the compact JSON of the Nostr event, which is what decodeTx
// unmarshals.  Both transports must agree byte for byte, so there is one
// function and not two.
func EventTransaction(event Event) ([]byte, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	// Prove the shape now rather than at submit time: a round-trip through the
	// app's own decoder catches a malformed event here, not three hops later.
	if _, err := decodeTx(compact.Bytes()); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

// RPCSubmitter submits events through CometBFT RPC endpoints'
// broadcast_tx_sync.  It speaks plain HTTP/JSON so the ingest links no extra
// client, and it fans out to every validator's RPC.
//
// Fan-out is the point (handover §3, workstream C): blocks are produced on
// demand, and CometBFT's handleTxsAvailable fires on the LOCAL TxsAvailable
// channel, so a tx sitting in a non-proposer's mempool waits for that
// validator's turn — up to CreateEmptyBlocksInterval.  Submitting to every
// validator's RPC guarantees the current proposer has the tx in its own
// mempool, and then the block lands within TimeoutCommit (300 ms) plus one
// round: <= 2 s, which is the bound a client may rely on.
type RPCSubmitter struct {
	// RPCURL is a single validator RPC base, e.g. http://127.0.0.1:26657.  It is
	// kept for callers that deliberately submit to one node; URLs, when set,
	// takes precedence.
	RPCURL string
	// URLs are the validator RPC bases to fan out to.  Success is "at least one
	// accepted (or already had it in cache)"; the rest only matter when none
	// accepted.
	URLs []string
	// Timeout bounds one endpoint's submission.  Zero means 20s.
	Timeout time.Duration
	// Client overrides the HTTP client; nil uses one with Timeout.
	Client *http.Client
}

// endpoints is the deduplicated fan-out list, with RPCURL as the fallback.
func (submitter RPCSubmitter) endpoints() []string {
	seen := map[string]struct{}{}
	var endpoints []string
	for _, url := range append(append([]string(nil), submitter.URLs...), submitter.RPCURL) {
		if url == "" {
			continue
		}
		if _, duplicate := seen[url]; duplicate {
			continue
		}
		seen[url] = struct{}{}
		endpoints = append(endpoints, url)
	}
	return endpoints
}

func (submitter RPCSubmitter) SubmitEvent(ctx context.Context, event Event) (SubmitResult, error) {
	endpoints := submitter.endpoints()
	if len(endpoints) == 0 {
		return SubmitResult{}, &SubmitFailure{Message: "submitter requires an RPC URL"}
	}
	tx, err := EventTransaction(event)
	if err != nil {
		// A malformed event can never commit; retrying it is pointless.
		return SubmitResult{}, &SubmitFailure{Message: err.Error()}
	}

	var (
		accepted  SubmitResult
		anyOK     bool
		firstErr  error
		anyOpen   bool
		anyFailed bool
	)
	for _, endpoint := range endpoints {
		result, err := submitter.submitOne(ctx, endpoint, tx)
		if err == nil {
			if !anyOK {
				accepted, anyOK = result, true
			}
			continue
		}
		anyFailed = true
		if firstErr == nil {
			firstErr = err
		}
		if IsTransientSubmit(err) {
			anyOpen = true
		}
	}
	if anyOK {
		// One endpoint accepting is enough: the tx is in a mempool (or the node
		// already had it) and will be proposed when that validator's turn comes.
		return accepted, nil
	}
	if !anyFailed {
		return SubmitResult{}, &SubmitFailure{Transient: true, Message: "no validator accepted the transaction"}
	}
	if anyOpen {
		// At least one refusal was transient, so the same submission may still
		// succeed later; report it as transient regardless of the first error.
		return SubmitResult{}, &SubmitFailure{Transient: true, Message: firstErr.Error()}
	}
	return SubmitResult{}, firstErr
}

// submitOne submits tx to a single endpoint.  It classifies the outcome: an
// accepted tx or one already in the node's cache is success (nil), a CometBFT
// RPC-layer error object, a transport error, an undecodable body, or a missing
// result is transient, and an application-level CheckTx rejection is terminal.
func (submitter RPCSubmitter) submitOne(ctx context.Context, endpoint string, tx []byte) (SubmitResult, error) {
	timeout := submitter.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("%s/broadcast_tx_sync?tx=0x%s", endpoint, hex.EncodeToString(tx))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SubmitResult{}, &SubmitFailure{Message: err.Error()}
	}
	client := submitter.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	response, err := client.Do(request)
	if err != nil {
		return SubmitResult{}, &SubmitFailure{Transient: true, Message: err.Error()}
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))

	var decoded struct {
		Result *struct {
			Code uint32 `json:"code"`
			Log  string `json:"log"`
			Hash string `json:"hash"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return SubmitResult{}, &SubmitFailure{Transient: true, Message: fmt.Sprintf("decode broadcast result: %v", err)}
	}
	if decoded.Error != nil {
		message := decoded.Error.Data
		if message == "" {
			message = decoded.Error.Message
		}
		// A duplicate the node already has in its mempool is success, not a
		// failure: it is the same transaction, and it will commit once.
		if strings.Contains(message, "already exists in cache") {
			return SubmitResult{}, nil
		}
		// The error object carries both the application's rejections and
		// readiness problems, so read the split off the message: a CheckTx
		// code is an answer (terminal), anything else (e.g. "endpoint is closed
		// while node is catching up") is transient.
		if appRejectionCode(message) != 0 {
			return SubmitResult{}, &SubmitFailure{Message: fmt.Sprintf("broadcast rejected: %s", message)}
		}
		return SubmitResult{}, &SubmitFailure{Transient: true, Message: fmt.Sprintf("broadcast rejected: %s", message)}
	}
	if decoded.Result == nil {
		return SubmitResult{}, &SubmitFailure{Transient: true, Message: "broadcast returned no result"}
	}
	if decoded.Result.Code != 0 {
		// The application answered: the chain will not accept this tx later.
		return SubmitResult{}, &SubmitFailure{Message: fmt.Sprintf("CheckTx rejected: %s", decoded.Result.Log)}
	}
	return SubmitResult{Code: decoded.Result.Code, TxHash: decoded.Result.Hash}, nil
}

// MemorySubmitter records submissions in memory.  It exists so the observer loop
// and its reconnect behaviour can be tested without a chain, and so a validator
// process that only wants to observe can be started without a broadcaster.
type MemorySubmitter struct {
	mu          sync.Mutex
	submissions []Event
	failures    map[string]error
	always      map[string]error
	attempts    map[string]int
}

func NewMemorySubmitter() *MemorySubmitter {
	return &MemorySubmitter{
		failures: map[string]error{},
		always:   map[string]error{},
		attempts: map[string]int{},
	}
}

// FailEvery makes the submitter return err for the named event, once.  A later
// attempt at the same event succeeds, so it exercises "retry once and commit".
func (submitter *MemorySubmitter) FailEvery(eventID string, err error) {
	submitter.mu.Lock()
	defer submitter.mu.Unlock()
	submitter.failures[eventID] = err
}

// FailAlways makes the submitter return err for the named event on every
// attempt, until ClearFailures removes it.  It exercises a retry that has to
// back off and try again several times before it can commit.
func (submitter *MemorySubmitter) FailAlways(eventID string, err error) {
	submitter.mu.Lock()
	defer submitter.mu.Unlock()
	submitter.always[eventID] = err
}

// ClearFailures drops every one-shot and persistent failure for the named event.
func (submitter *MemorySubmitter) ClearFailures(eventID string) {
	submitter.mu.Lock()
	defer submitter.mu.Unlock()
	delete(submitter.failures, eventID)
	delete(submitter.always, eventID)
}

// Attempts is how many times SubmitEvent was called for the named event,
// successful or not; a test uses it to prove a retry actually happened.
func (submitter *MemorySubmitter) Attempts(eventID string) int {
	submitter.mu.Lock()
	defer submitter.mu.Unlock()
	return submitter.attempts[eventID]
}

func (submitter *MemorySubmitter) SubmitEvent(_ context.Context, event Event) (SubmitResult, error) {
	submitter.mu.Lock()
	defer submitter.mu.Unlock()
	submitter.attempts[event.ID]++
	if err, ok := submitter.always[event.ID]; ok {
		return SubmitResult{}, err
	}
	if err, ok := submitter.failures[event.ID]; ok {
		delete(submitter.failures, event.ID)
		return SubmitResult{}, err
	}
	submitter.submissions = append(submitter.submissions, event)
	return SubmitResult{Height: int64(len(submitter.submissions)), TxHash: event.ID}, nil
}

// Submissions returns the events accepted so far, oldest first.  The slice is a
// copy, so a test cannot accidentally mutate the record.
func (submitter *MemorySubmitter) Submissions() []Event {
	submitter.mu.Lock()
	defer submitter.mu.Unlock()
	out := make([]Event, len(submitter.submissions))
	copy(out, submitter.submissions)
	return out
}
