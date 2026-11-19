package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
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
}

// nodeArgs builds the argv for validator i (0-based).  It is the ONE place the
// node command line is built, so the gateway's initial start and a demo-control
// restart cannot drift: a restarted validator that lost its -relay flag comes
// back with no relay observer (so no ingest), no verdict publisher, no state
// commitments and no params event -- a second-class node that only p2p-syncs.
func nodeArgs(root string, i int, relays []string, commitMax time.Duration) []string {
	args := []string{"node", "-root", root, "-i", fmt.Sprint(i + 1)}
	for _, relay := range relays {
		args = append(args, "-relay", relay)
	}
	if commitMax > 0 {
		args = append(args, "-commit-max-interval="+commitMax.String())
	}
	return args
}

// argsFor is the argv a start of validator i uses.  Both the initial start and
// the demo-control restart go through here.
func (s *supervisor) argsFor(i int) []string {
	return nodeArgs(s.root, i, s.relays, s.commitMax)
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
	static := fs.String("static", "web/dist", "front end dir")
	addr := fs.String("addr", "127.0.0.1:8080", "listen addr")
	mint := fs.String("mint", "http://127.0.0.1:3338", "mint")
	demo := fs.Bool("demo-controls", false, "expose the unauthenticated validator stop/start route (demo only, never on a real-sats deployment)")
	commitMaxInterval := fs.Duration("commit-max-interval", StateCommitMaxInterval, "maximum wall-clock gap between state commitments")
	var relays relayURLs
	fs.Var(&relays, "relay", "relay websocket URL clients publish to (repeatable); the relay is the primary transport")
	fs.Parse(args)
	bin, _ := os.Executable()
	sup := &supervisor{root: *root, bin: bin, logs: filepath.Dir(*root), relays: relays, commitMax: *commitMaxInterval}
	for i := range 4 {
		check(sup.start(i))
	}
	publisher := newRelayPublisher(relays)

	var chatMu sync.Mutex
	chat := []chatMsg{}

	mu := http.NewServeMux()
	mintURL, _ := url.Parse(*mint)
	mp := httputil.NewSingleHostReverseProxy(mintURL)
	mu.Handle("/mint/", http.StripPrefix("/mint", mintHandler(mp)))

	// state from the first healthy validator. ?view=client is the slim view
	// the page polls (see view.go); it answers 304 when nothing changed.
	mu.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		q := url.Values{}
		for _, k := range []string{"view", "bidders", "closed"} {
			if v := r.URL.Query().Get(k); v != "" && len(v) <= 2048 {
				q.Set(k, v)
			}
		}
		for i := 0; i < 4; i++ {
			if !sup.running(i) {
				continue
			}
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/state?%s", httpPort(i), q.Encode()), nil)
			if inm := r.Header.Get("If-None-Match"); inm != "" && len(inm) <= 256 {
				req.Header.Set("If-None-Match", inm)
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if tag := resp.Header.Get("ETag"); tag != "" {
				w.Header().Set("ETag", tag)
				w.Header().Set("Cache-Control", "no-cache")
			}
			w.Header().Set("X-Served-By", names[i])
			if resp.StatusCode == http.StatusNotModified {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			if resp.StatusCode == 200 && len(b) > 2 {
				w.Header().Set("Content-Type", "application/json")
				w.Write(b)
				return
			}
		}
		writeJSON(w, 503, map[string]string{"error": "no validator reachable"})
	})

	// push: "a block was committed" as Server-Sent Events (events.go)
	// the validator long-polls (/height?after=N waits up to 20 s for a block)
	evCl := &http.Client{Timeout: 25 * time.Second}
	hub := newEventHub(func(after int64) (int64, bool) {
		for i := 0; i < 4; i++ {
			if sup.running(i) {
				if h, ok := heightFrom(evCl, fmt.Sprintf("http://127.0.0.1:%d/height?after=%d", httpPort(i), after)); ok {
					return h, true
				}
			}
		}
		return 0, false
	})
	mu.Handle("/api/events", hub)

	// one bid with its proofs, for releasing it
	mu.HandleFunc("/api/bid", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !lowerHex(id, 64) {
			writeJSON(w, 400, map[string]string{"error": "id must be 64 lowercase hex"})
			return
		}
		for i := 0; i < 4; i++ {
			if !sup.running(i) {
				continue
			}
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/bid?id=%s", httpPort(i), id))
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 404 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				w.Write(b)
				return
			}
		}
		writeJSON(w, 503, map[string]string{"error": "no validator reachable"})
	})

	mu.HandleFunc("/api/network", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 4)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := map[string]any{"name": names[i], "index": i + 1, "up": false, "process": sup.running(i)}
				if sup.running(i) {
					resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/status", httpPort(i)))
					if err == nil {
						json.NewDecoder(resp.Body).Decode(&m)
						resp.Body.Close()
						m["up"] = true
						m["process"] = true
					}
				}
				out[i] = m
			}(i)
		}
		wg.Wait()
		writeJSON(w, 200, out)
	})

	// Publish transport health.  The chain's outbound publisher froze for
	// 1h45m on 2026-11-19 while the chain kept advancing, and nothing outside
	// the process could see it: the relay just stopped receiving kinds 1443-
	// 1450.  Each validator now reports, per relay, whether it is connected,
	// how many attempts failed and the last error; this route aggregates them.
	// curl 127.0.0.1:8090/api/publish is the outside check.
	mu.HandleFunc("/api/publish", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 4)
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := map[string]any{"name": names[i], "index": i + 1, "up": false}
				if sup.running(i) {
					resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/status", httpPort(i)))
					if err == nil {
						var status map[string]any
						json.NewDecoder(resp.Body).Decode(&status)
						resp.Body.Close()
						m["up"] = true
						if publish, ok := status["publish"]; ok {
							m["relays"] = publish
						} else {
							m["relays"] = []PublishHealth{}
							m["publishing"] = false
						}
					}
				}
				out[i] = m
			}(i)
		}
		wg.Wait()
		writeJSON(w, 200, out)
	})

	// Publish a signed Nostr event.  The relay is the transport and the
	// discovery mechanism, but it is NOT enough on its own: the event is always
	// also submitted directly to every validator's RPC, so the tx reaches the
	// current proposer's own mempool (see RPCSubmitter for the <= 2 s bound) and
	// a restart of the observer hop cannot lose it.  A relay publish is best
	// effort and never decides the answer.  ok:true means the tx was accepted
	// into at least one mempool and is pollable by the hash that is always
	// returned.
	mu.HandleFunc("/api/tx", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || r.Method != http.MethodPost {
			writeJSON(w, 400, map[string]string{"error": "POST a signed event"})
			return
		}
		var compact bytes.Buffer
		if json.Compact(&compact, body) != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}
		var event Event
		if json.Unmarshal(compact.Bytes(), &event) != nil || event.Verify() != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid signed event"})
			return
		}
		validators := make([]validatorRPC, 0, 4)
		for i := range 4 {
			if sup.running(i) {
				validators = append(validators, validatorRPC{Name: names[i], URL: fmt.Sprintf("http://127.0.0.1:%d", rpcPort(i))})
			}
		}
		// Keep a nil publisher a nil interface, so submitTx skips the relay
		// path entirely instead of logging "no relay configured".
		var pub txPublisher
		if publisher != nil {
			pub = publisher
		}
		status, out := submitTx(r.Context(), client, validators, event, compact.Bytes(), pub)
		writeJSON(w, status, out)
	})

	// relay a release request to every validator's signer; the browser assembles 3 of 4
	// escrow release: one FROST signing round across the validators
	rc, err := loadCoordinator(*root, func(i int, path string, body any, out any) (int, error) {
		b, _ := json.Marshal(body)
		resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d%s", httpPort(i), path), "application/json", bytes.NewReader(b))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
		return resp.StatusCode, nil
	})
	check(err)
	// push settlement (ADR 0009, spec §5): refunds and settles the moment they
	// are due, over the bid's committed outcome templates. The settler is an
	// executor: it names the bid and the outcome and submits the bidder's own
	// pre-signed template.
	if os.Getenv("ARENA_PUSH") != "0" {
		go newSettler(rc, *mint, sup.running, hub, filepath.Join(filepath.Dir(*root), "settle-journal.jsonl")).run()
	}

	// The committee's half of a release, for the operations view and for any
	// page that outlives the settler: run one FROST round over the bid's
	// COMMITTED outcome template and return the aggregate signature. The caller
	// names the bid and the outcome and nothing else -- there are no
	// caller-chosen outputs and no auth field (spec §5), because under the
	// 2-of-2 the destination is already fixed by the bidder's own signature.
	//
	// This is NOT the product path: the settler pushes refunds and the payout
	// automatically off committed blocks. It exists so a page or an operator
	// can still finish a move when the settler is behind.
	mu.HandleFunc("/api/settle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "POST only"})
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if !json.Valid(body) {
			writeJSON(w, 400, map[string]string{"error": "bad request"})
			return
		}
		code, res := rc.release(body)
		writeJSON(w, code, res)
	})

	mu.HandleFunc("/api/admin/validator/", func(w http.ResponseWriter, r *http.Request) {
		if !*demo {
			writeJSON(w, 404, map[string]string{"error": "demo controls are off on this deployment"})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "POST only"})
			return
		}
		var n int
		var action string
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/api/admin/validator/"), "%d/%s", &n, &action)
		if n < 1 || n > 4 {
			writeJSON(w, 400, map[string]string{"error": "validator 1-4"})
			return
		}
		switch action {
		case "stop":
			sup.stop(n - 1)
		case "start":
			if err := sup.start(n - 1); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		default:
			writeJSON(w, 400, map[string]string{"error": "stop or start"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "validator": n, "action": action})
	})

	// lot chat: signed Nostr events, kept in memory (not consensus data)
	mu.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var ev Event
			if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&ev) != nil || ev.Kind != KindChat || ev.Verify() != nil || len(ev.Content) > 280 {
				writeJSON(w, 400, map[string]string{"error": "invalid chat event"})
				return
			}
			chatMu.Lock()
			chat = append(chat, chatMsg{ev, time.Now().Unix()})
			if len(chat) > 300 {
				chat = chat[len(chat)-300:]
			}
			chatMu.Unlock()
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		chatMu.Lock()
		defer chatMu.Unlock()
		writeJSON(w, 200, chat)
	})

	// static front end with SPA fallback
	fsrv := http.FileServer(http.Dir(*static))
	mu.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(*static, filepath.Clean(r.URL.Path))
		if st, err := os.Stat(p); err != nil || st.IsDir() && r.URL.Path != "/" {
			r.URL.Path = "/"
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fsrv.ServeHTTP(w, r)
	})

	srv := &http.Server{Addr: *addr, Handler: gzipMiddleware(mu),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	log.Printf("gateway on %s", *addr)
	go func() {
		sigc := make(chan os.Signal, 1)
		signalNotify(sigc)
		<-sigc
		// stop all four at once: each may take a few seconds to write its
		// shutdown snapshot, and systemd's TimeoutStopSec is 20 s
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) { defer wg.Done(); sup.stop(i) }(i)
		}
		wg.Wait()
		os.Exit(0)
	}()
	log.Fatal(srv.ListenAndServe())
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
		// a late bid: on chain as void, refundable as soon as it lands (ADR 0007)
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
