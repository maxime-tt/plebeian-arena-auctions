package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: arena init|node|gateway [flags]")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "node":
		cmdNode(os.Args[2:])
	case "gateway":
		cmdGateway(os.Args[2:])
	default:
		log.Fatal("unknown command")
	}
}

type supervisor struct {
	mu    sync.Mutex
	root  string
	bin   string
	logs  string
	procs [4]*exec.Cmd
	want  [4]bool
	// relays and commitMax are the gateway's per-node flags, kept so a validator
	// restarted through the demo control comes back with the SAME command line
	// (see nodeArgs).
	relays    []string
	commitMax time.Duration
	// meshHost is the address every validator's mesh listener binds to and is
	// reached at, so a restart rebuilds the same -mesh-host the initial start
	// used (see nodeArgs).
	meshHost string
}

// nodeArgs builds the argv for validator i (0-based).  It is the ONE place the
// node command line is built, so the gateway's initial start and a demo-control
// restart cannot drift: a restarted validator that lost its -relay flag comes
// back with no relay observer (so no ingest), no verdict publisher, no state
// commitments and no params event -- a second-class node that only p2p-syncs.
func nodeArgs(root string, i int, relays []string, commitMax time.Duration, meshHost string) []string {
	args := []string{"node", "-root", root, "-i", fmt.Sprint(i + 1)}
	for _, relay := range relays {
		args = append(args, "-relay", relay)
	}
	if commitMax > 0 {
		args = append(args, "-commit-max-interval="+commitMax.String())
	}
	// The mesh host is what the node binds its internal HTTP listener to, so a
	// restart must carry it too: a validator that came back on loopback while
	// its peers reach it on the FIPS address would be unreachable.
	if meshHost != "" {
		args = append(args, "-mesh-host", meshHost)
	}
	return args
}

// argsFor is the argv a start of validator i uses.  Both the initial start and
// the demo-control restart go through here.
func (s *supervisor) argsFor(i int) []string {
	return nodeArgs(s.root, i, s.relays, s.commitMax, s.meshHost)
}

func (s *supervisor) start(i int) error {
	return s.startWith(i, s.argsFor(i))
}

// startWith starts validator i with explicit arguments; start(i) builds them via
// argsFor.  It stays a separate entry point for tests.
func (s *supervisor) startWith(i int, args []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.procs[i] != nil {
		return nil
	}
	lf, err := os.OpenFile(filepath.Join(s.logs, fmt.Sprintf("v%d.log", i+1)), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(s.bin, args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.procs[i], s.want[i] = cmd, true
	go func() {
		cmd.Wait()
		lf.Close()
		s.mu.Lock()
		if s.procs[i] == cmd {
			s.procs[i] = nil
		}
		s.mu.Unlock()
	}()
	return nil
}

func (s *supervisor) stop(i int) {
	s.mu.Lock()
	cmd := s.procs[i]
	s.want[i] = false
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			s.mu.Lock()
			gone := s.procs[i] != cmd
			s.mu.Unlock()
			if gone {
				close(done)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		cmd.Process.Kill()
	}
}

func (s *supervisor) running(i int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[i] != nil
}

var client = &http.Client{Timeout: 4 * time.Second}

type chatMsg struct {
	Event
	Received int64 `json:"received"`
}

func cmdGateway(args []string) {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	root := fs.String("root", "run/chain", "chain root")
	static := fs.String("static", "web/dist", "front end dir (dev mode only: a deployed role serves no static UI)")
	addr := fs.String("addr", "127.0.0.1:8080", "listen addr")
	mint := fs.String("mint", "http://127.0.0.1:3338", "mint")
	dev := fs.Bool("dev", false, "DEV ONLY: serve the repository's public UI/probe routes (/api/*, /mint/) and the static UI. A deployed gateway serves none of them and no static files; dev mode is never a deployed role.")
	meshHost := fs.String("mesh-host", "127.0.0.1", "address the internal validator mesh listeners are reached at (the node's FIPS address on a deployed node, loopback on a single host)")
	demo := fs.Bool("demo-controls", false, "expose the unauthenticated validator stop/start route (dev/demo only, never on a real-sats deployment)")
	commitMaxInterval := fs.Duration("commit-max-interval", StateCommitMaxInterval, "maximum wall-clock gap between state commitments")
	var relays relayURLs
	fs.Var(&relays, "relay", "relay websocket URL clients publish to (repeatable); the relay is the primary transport")
	fs.Parse(args)

	// A deployed gateway is the validator supervisor only: it starts no HTTP
	// listener and registers no route.  Dev mode keeps the public UI/probe
	// surface and therefore needs a loopback bind and mesh host.
	if *dev {
		if err := validateBindAddr(*addr); err != nil {
			log.Fatalf("gateway: %v", err)
		}
		if !isLoopbackAddr(*addr) {
			log.Fatalf("gateway: -dev requires a loopback -addr (got %s): dev mode serves the public UI and is never a deployed role", *addr)
		}
		if !isLoopbackHost(*meshHost) {
			// Dev is a single-host run: the validators' mesh listeners and their
			// CometBFT RPC listeners (which nodeConfig binds to 127.0.0.1) must be
			// reachable from the same host, so a non-loopback -mesh-host would make
			// the dev UI talk to nothing.
			log.Fatalf("gateway: -dev requires a loopback -mesh-host (got %s): dev mode is a single-host run", *meshHost)
		}
		log.Printf("gateway: DEV MODE on %s: serving the public /api/* and /mint/ routes and the static UI; this is NOT a deployed role", *addr)
	}

	bin, _ := os.Executable()
	sup := &supervisor{root: *root, bin: bin, logs: filepath.Dir(*root), relays: relays, commitMax: *commitMaxInterval, meshHost: *meshHost}
	for i := range 4 {
		check(sup.start(i))
	}

	// A deployed role: supervise the validators and stop them on SIGTERM/SIGINT.
	// There is no HTTP server and no route to register.
	if !*dev {
		log.Printf("gateway: deployed role: supervising validators only (no HTTP listener)")
		stop := make(chan os.Signal, 1)
		signalNotify(stop)
		<-stop
		stopSupervisor(sup)
		return
	}

	publisher := newRelayPublisher(relays)

	// push: "a block was committed" as Server-Sent Events (events.go)
	// the validator long-polls (/height?after=N waits up to 20 s for a block).
	evCl := &http.Client{Timeout: 25 * time.Second}
	hub := newEventHub(func(after int64) (int64, bool) {
		for i := range 4 {
			if sup.running(i) {
				if h, ok := heightFrom(evCl, validatorURL(*meshHost, i)+fmt.Sprintf("/height?after=%d", after)); ok {
					return h, true
				}
			}
		}
		return 0, false
	})

	// The route table is a pure function of these dependencies (see gateway.go).
	// Dev keeps the retired public surface for the dev UI.
	deps := gatewayDeps{sup: sup, pub: publisher, hub: hub, chat: &chatLog{}, meshHost: *meshHost, static: *static, mint: *mint, demoControls: *demo}
	handler := gatewayMux(deps, true)

	srv := gatewayServer(true, *addr, handler)
	log.Printf("gateway on %s (dev=true)", *addr)
	go func() {
		sigc := make(chan os.Signal, 1)
		signalNotify(sigc)
		<-sigc
		stopSupervisor(sup)
		os.Exit(0)
	}()
	log.Fatal(srv.ListenAndServe())
}

// stopSupervisor stops all four validators at once: each may take a few seconds
// to write its shutdown snapshot, and systemd's TimeoutStopSec is 20 s.
func stopSupervisor(sup *supervisor) {
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) { defer wg.Done(); sup.stop(i) }(i)
	}
	wg.Wait()
}

// validatorRPC names one validator's CometBFT RPC endpoint for the /api/tx
// fan-out.
type validatorRPC struct {
	Name string
	URL  string
}

// txHash is the CometBFT transaction hash of tx: sha256 over the raw tx bytes,
// uppercase hex.  That is exactly what types.Tx(tx).Hash() reports and what a
// client polls a tx by, so /api/tx can always return a hash even when no
// validator's response carried one.
func txHash(tx []byte) string {
	sum := sha256.Sum256(tx)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// broadcastOutcome is one validator's answer to broadcast_tx_sync.
type broadcastOutcome struct {
	kind    broadcastOutcomeKind
	hash    string
	note    string
	late    string
	message string
}

type broadcastOutcomeKind int

const (
	// broadcastAccepted: the tx is in this validator's mempool (or was already
	// cached there).  A hash may be present.
	broadcastAccepted broadcastOutcomeKind = iota
	// broadcastTerminal: the application rejected the tx; no validator will
	// accept it later.
	broadcastTerminal
	// broadcastTransient: a transport error, an RPC-layer error object (the
	// node may be catching up), or an unreadable answer; the same submit may
	// succeed later.
	broadcastTransient
)

// broadcastTx submits tx to one validator's CometBFT RPC and classifies the
// answer, mirroring RPCSubmitter's classification so the two transports agree.
func broadcastTx(ctx context.Context, client *http.Client, rpcURL string, tx []byte) broadcastOutcome {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/broadcast_tx_sync?tx=0x%s", rpcURL, hex.EncodeToString(tx)), nil)
	if err != nil {
		return broadcastOutcome{kind: broadcastTransient, message: err.Error()}
	}
	response, err := client.Do(request)
	if err != nil {
		return broadcastOutcome{kind: broadcastTransient, message: err.Error()}
	}
	defer response.Body.Close()

	var rr struct {
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
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&rr); err != nil {
		return broadcastOutcome{kind: broadcastTransient, message: err.Error()}
	}
	if rr.Error != nil {
		msg := rr.Error.Data
		if msg == "" {
			msg = rr.Error.Message
		}
		if strings.Contains(msg, "already exists in cache") {
			return broadcastOutcome{kind: broadcastAccepted, note: "already submitted"}
		}
		// Same split as RPCSubmitter: a CheckTx code in the message is the
		// application's answer (terminal), anything else is a readiness or
		// transport refusal the caller may retry.
		if appRejectionCode(msg) != 0 {
			return broadcastOutcome{kind: broadcastTerminal, message: msg}
		}
		return broadcastOutcome{kind: broadcastTransient, message: msg}
	}
	if rr.Result == nil {
		return broadcastOutcome{kind: broadcastTransient, message: "broadcast returned no result"}
	}
	if rr.Result.Code != 0 {
		return broadcastOutcome{kind: broadcastTerminal, message: rr.Result.Log}
	}
	out := broadcastOutcome{kind: broadcastAccepted, hash: rr.Result.Hash}
	if strings.HasPrefix(rr.Result.Log, "late: ") {
		// A late bid: on chain as void, refundable as soon as it lands (ADR
		// 0007).  Since R3 the chain REFUSES an economically late bid in
		// CheckTx instead (code 2, handled as terminal above), so no current
		// validator answers this way -- it is kept because a mixed-version set
		// during the swap still can, and a client must not be told "rejected"
		// about a bid an OLD validator really did commit as void.
		out.late = strings.TrimPrefix(rr.Result.Log, "late: ")
	}
	return out
}

// txPublisher is the relay side of /api/tx: *relayPublisher in production, a
// fake in a test.  Keeping it an interface is what lets submitTx be exercised
// with no relay and no network.
type txPublisher interface {
	Publish(ctx context.Context, event Event) error
}

// submitTx is the /api/tx body, extracted so it is unit-testable.  It publishes
// the event to the relay when one is configured (best effort) and submits the tx
// to EVERY validator's RPC, then reports the HTTP status and the JSON body.
//
// ok:true means "accepted and pollable": at least one validator took the tx into
// its mempool, and the tx hash is always returned so the caller can poll it.  A
// relay publish on its own is never ok.  With no acceptor, a terminal rejection
// is a 400 and anything else (all transports down, nodes catching up) is a 503.
func submitTx(ctx context.Context, client *http.Client, validators []validatorRPC, event Event, tx []byte, publisher txPublisher) (int, map[string]any) {
	// A settlement carrier is not a chain event: its content IS the settleTx
	// payload.  Convert it here so the direct /api/tx path and the relay ingest
	// both submit the same bytes decodeTx/parseSettle already accepts.
	if event.Kind == KindSettlementCarrier {
		st, err := parseSettlementCarrier(event.Content)
		if err != nil {
			return http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()}
		}
		tx = makeSettle(st)
	}
	hash := txHash(tx)

	// The relay is transport and discovery: every validator's observer picks the
	// event up from there.  A relay failure is logged and ignored -- the direct
	// fan-out below is what decides the answer.
	if publisher != nil {
		if err := publisher.Publish(ctx, event); err != nil {
			log.Printf("relay publish failed: %v", err)
		}
	}

	var (
		accepted    []string
		acceptedRPC string
		note        string
		late        string
		terminal    string
		terminalVia string
		transient   string
	)
	for _, validator := range validators {
		outcome := broadcastTx(ctx, client, validator.URL, tx)
		switch outcome.kind {
		case broadcastAccepted:
			accepted = append(accepted, validator.Name)
			if acceptedRPC == "" {
				acceptedRPC = outcome.hash
			}
			if note == "" {
				note = outcome.note
			}
			if late == "" {
				late = outcome.late
			}
		case broadcastTerminal:
			if terminal == "" {
				terminal, terminalVia = outcome.message, validator.Name
			}
		case broadcastTransient:
			if transient == "" {
				transient = outcome.message
			}
		}
	}

	if len(accepted) > 0 {
		if acceptedRPC != "" {
			hash = acceptedRPC
		}
		out := map[string]any{"ok": true, "hash": hash, "via": strings.Join(accepted, ", ")}
		if note != "" {
			out["note"] = note
		}
		if late != "" {
			out["late"] = late
		}
		return http.StatusOK, out
	}
	if terminal != "" {
		return http.StatusBadRequest, map[string]any{"ok": false, "error": terminal, "via": terminalVia}
	}
	if transient == "" {
		transient = "no validator reachable"
	}
	return http.StatusServiceUnavailable, map[string]any{"ok": false, "error": transient}
}
