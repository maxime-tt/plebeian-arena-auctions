package main

// The gateway's HTTP surface, as a pure function of its dependencies so it can
// be built and asserted on in a test (gateway_routes_test.go).
//
// Handover §6.1/§6.2: all public communication goes through Nostr relays, so a
// DEPLOYED gateway serves exactly ONE route -- POST /internal/settle, the
// coordinator's settlement push -- bound to the mesh listener.  The public
// Nostr-era surface (/api/state, /api/bid, /api/tx, /api/settle, /api/events,
// /api/chat, /api/network, /api/publish, /mint/ and /api/admin/) is removed, and
// no static file is served.  Clients submit events to relays and read facts from
// relays (kinds 1440-1450).
//
// The FIPS ACL is the peer firewall (handover §3.2): the mesh listener is
// reachable only from the other whitelisted validators, so there is deliberately
// no second auth scheme on these routes -- the transport is the boundary.
//
// dev=true keeps the old public routes and the static UI for the repository's
// own dev UI, the acceptance script and the probes.  It is never a deployed
// role: cmdGateway refuses to run it off loopback.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// chatLog is the gateway's in-memory lot chat (dev only; never consensus data).
type chatLog struct {
	mu   sync.Mutex
	msgs []chatMsg
}

// gatewayDeps is everything the gateway's HTTP surface needs.  It is a struct,
// not a set of closures, so the route table is a pure function of its inputs.
type gatewayDeps struct {
	sup      *supervisor
	pub      *relayPublisher
	hub      *eventHub
	rc       *releaseCoordinator
	chat     *chatLog
	meshHost string
	static   string
	mint     string
	// demoControls gates the unauthenticated validator stop/start route.  It is
	// a dev-only affordance and defaults off.
	demoControls bool
}

// gatewayMux builds the gateway's HTTP handler.  See the file comment for what
// dev changes.
func gatewayMux(d gatewayDeps, dev bool) http.Handler {
	mu := http.NewServeMux()

	// The coordinator's settlement push.  This is the existing rc.release body,
	// mesh-only, and it is the ONLY route a deployed gateway registers.
	settle := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "POST only"})
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if !json.Valid(body) {
			writeJSON(w, 400, map[string]string{"error": "bad request"})
			return
		}
		code, res := d.rc.release(body)
		writeJSON(w, code, res)
	}
	mu.HandleFunc("/internal/settle", settle)

	if !dev {
		// A deployed role: the mesh listener and nothing else.  No static files,
		// no public API.
		return gzipMiddleware(mu)
	}

	// ---- dev only: the retired public surface, kept for the dev UI ----
	//
	// The two settlement entry points may share one handler in dev: they are
	// the same operation, and dev is a single-host convenience.
	mu.HandleFunc("/api/settle", settle)

	mintURL, _ := url.Parse(d.mint)
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
		for i := range 4 {
			if !d.sup.running(i) {
				continue
			}
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, validatorURL(d.meshHost, i)+"/state?"+q.Encode(), nil)
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
	mu.Handle("/api/events", d.hub)

	// one bid with its proofs, for releasing it
	mu.HandleFunc("/api/bid", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !lowerHex(id, 64) {
			writeJSON(w, 400, map[string]string{"error": "id must be 64 lowercase hex"})
			return
		}
		for i := range 4 {
			if !d.sup.running(i) {
				continue
			}
			resp, err := client.Get(validatorURL(d.meshHost, i) + "/bid?id=" + id)
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
		for i := range 4 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := map[string]any{"name": names[i], "index": i + 1, "up": false, "process": d.sup.running(i)}
				if d.sup.running(i) {
					resp, err := client.Get(validatorURL(d.meshHost, i) + "/status")
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
	mu.HandleFunc("/api/publish", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 4)
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := map[string]any{"name": names[i], "index": i + 1, "up": false}
				if d.sup.running(i) {
					resp, err := client.Get(validatorURL(d.meshHost, i) + "/status")
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
			if d.sup.running(i) {
				validators = append(validators, validatorRPC{Name: names[i], URL: "http://" + net.JoinHostPort(d.meshHost, fmt.Sprint(rpcPort(i)))})
			}
		}
		// Keep a nil publisher a nil interface, so submitTx skips the relay
		// path entirely instead of logging "no relay configured".
		var pub txPublisher
		if d.pub != nil {
			pub = d.pub
		}
		status, out := submitTx(r.Context(), client, validators, event, compact.Bytes(), pub)
		writeJSON(w, status, out)
	})

	mu.HandleFunc("/api/admin/validator/", func(w http.ResponseWriter, r *http.Request) {
		if !d.demoControls {
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
			d.sup.stop(n - 1)
		case "start":
			if err := d.sup.start(n - 1); err != nil {
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
			d.chat.mu.Lock()
			d.chat.msgs = append(d.chat.msgs, chatMsg{ev, time.Now().Unix()})
			if len(d.chat.msgs) > 300 {
				d.chat.msgs = d.chat.msgs[len(d.chat.msgs)-300:]
			}
			d.chat.mu.Unlock()
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		d.chat.mu.Lock()
		defer d.chat.mu.Unlock()
		writeJSON(w, 200, d.chat.msgs)
	})

	// static front end with SPA fallback
	fsrv := http.FileServer(http.Dir(d.static))
	mu.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(d.static, filepath.Clean(r.URL.Path))
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

	return gzipMiddleware(mu)
}

// validatorURL is the internal mesh URL of validator i's HTTP listener.  The
// gateway reaches every validator there, so a deployed node whose mesh listener
// is its FIPS address is addressed correctly (a bare IPv6 literal is bracketed
// by net.JoinHostPort).
func validatorURL(meshHost string, i int) string {
	return "http://" + net.JoinHostPort(meshHost, fmt.Sprint(httpPort(i)))
}

// validateBindAddr rejects a listen address whose host is unspecified or
// broadcast.  An arena node's HTTP surface is mesh-only (handover §6.1/§6.2), so
// binding 0.0.0.0 (every interface, v4) or :: (every interface, v6) would expose
// the internal routes to anyone.  The address must name a host and a port.
func validateBindAddr(addr string) error {
	host, err := hostOfAddr(addr)
	if err != nil {
		return err
	}
	switch host {
	case "", "0.0.0.0", "::", "0:0:0:0:0:0:0:0":
		return fmt.Errorf("bind address %q is unspecified: the mesh listener must name one interface (the FIPS address on a deployed node, or 127.0.0.1 for a single host)", addr)
	}
	return nil
}

// hostOfAddr extracts the host from a host:port address, tolerating both the
// bracketed IPv6 form ([::1]:8080) and a bare IPv6 literal whose port is the
// last colon-separated field (::1:8080).
func hostOfAddr(addr string) (string, error) {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return strings.Trim(h, "[]"), nil
	}
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", fmt.Errorf("bind address %q needs a port", addr)
	}
	if h := addr[:i]; h != "" {
		return strings.Trim(h, "[]"), nil
	}
	return "", fmt.Errorf("bind address %q needs a host", addr)
}

// isLoopbackHost reports whether a bare host names the loopback interface.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// isLoopbackAddr reports whether a host:port address names the loopback
// interface.  Dev mode requires it: it serves the public UI, which must never be
// reachable off-host.
func isLoopbackAddr(addr string) bool {
	host, err := hostOfAddr(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}
