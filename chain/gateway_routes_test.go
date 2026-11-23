package main

// The gateway's route table is a pure function of its dependencies
// (gatewayMux), so the whole "retire the public HTTP surface" change
// (handover §6.1/§6.2) is assertable without starting a process.
//
// The rules under test:
//   - a DEPLOYED gateway (dev=false) registers ONLY POST /internal/settle; every
//     retired public path is 404 and no static file is served;
//   - dev mode still registers the public routes the repository's dev UI,
//     acceptance script and probes use;
//   - a bind address must name one interface: 0.0.0.0/:: are refused, loopback
//     addresses are not.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// gatewayTestDeps is a gatewayDeps with no live process behind it.  Every
// handler that would reach a validator finds `running(i)` false and answers
// without a network call.
func gatewayTestDeps() gatewayDeps {
	hub := newEventHub(func(int64) (int64, bool) { return 0, false })
	return gatewayDeps{
		sup:      &supervisor{},
		hub:      hub,
		rc:       &releaseCoordinator{},
		chat:     &chatLog{},
		meshHost: "127.0.0.1",
		static:   "web/dist",
		mint:     "http://127.0.0.1:3338",
	}
}

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestDeployedGatewayServesOnlyInternalSettle: no retired public route answers,
// and nothing static is served.
func TestDeployedGatewayServesOnlyInternalSettle(t *testing.T) {
	h := gatewayMux(gatewayTestDeps(), false)

	removed := []string{
		"/api/state", "/api/bid", "/api/tx", "/api/settle", "/api/events",
		"/api/chat", "/api/network", "/api/publish", "/mint/v1/info",
		"/api/admin/validator/1/stop",
	}
	for _, path := range removed {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if rec := serve(h, method, path); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404: a deployed gateway must not serve this route", method, path, rec.Code)
			}
		}
	}

	// No static file server either: the SPA fallback would have answered 200 (or
	// a directory listing) on "/" and "/index.html".
	for _, path := range []string{"/", "/index.html", "/assets/app.js"} {
		if rec := serve(h, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: a deployed gateway serves no static UI", path, rec.Code)
		}
	}

	// The coordinator's endpoint IS registered: a wrong method or a bad body is
	// answered by the handler, never by the mux's 404.
	if rec := serve(h, http.MethodGet, "/internal/settle"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /internal/settle = %d, want 405 (registered)", rec.Code)
	}
	if rec := serve(h, http.MethodPost, "/internal/settle"); rec.Code != http.StatusBadRequest {
		t.Errorf("POST /internal/settle with no body = %d, want 400 (registered)", rec.Code)
	}
}

// TestDevGatewayKeepsThePublicRoutes: dev is the repository's dev/demo entry
// point, so the routes the UI and the probes use stay.
func TestDevGatewayKeepsThePublicRoutes(t *testing.T) {
	// A stub mint, so the /mint/ proxy probe never dials anything real.
	mintSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"nuts":{"10":{},"11":{}}}`))
	}))
	defer mintSrv.Close()
	deps := gatewayTestDeps()
	deps.mint = mintSrv.URL
	h := gatewayMux(deps, true)

	// The two settlement entry points share one handler in dev.
	if rec := serve(h, http.MethodGet, "/api/settle"); rec.Code == http.StatusNotFound {
		t.Error("dev mode must register /api/settle")
	}
	if rec := serve(h, http.MethodGet, "/internal/settle"); rec.Code == http.StatusNotFound {
		t.Error("dev mode must register /internal/settle")
	}
	// /api/state answers 503 with no validator running -- that is the handler,
	// not the mux's 404.
	if rec := serve(h, http.MethodGet, "/api/state"); rec.Code == http.StatusNotFound {
		t.Error("dev mode must register /api/state")
	}
	// The rest of the retired surface is back in dev.
	for _, path := range []string{"/api/bid?id=" + hx(0), "/api/network", "/api/publish", "/api/chat", "/mint/v1/info"} {
		if rec := serve(h, http.MethodGet, path); rec.Code == http.StatusNotFound {
			t.Errorf("dev mode must register %s", path)
		}
	}
	// /api/events is an SSE stream: it would block until the request context is
	// done, so drive it with an already-cancelled context and assert the stream
	// handler (200), not the mux's 404.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx))
	if rec.Code == http.StatusNotFound {
		t.Error("dev mode must register /api/events")
	}
}

// TestValidateBindAddr: a mesh listener must name one interface.
func TestValidateBindAddr(t *testing.T) {
	bad := []string{"0.0.0.0:8080", ":::8080", "[::]:8080", ":8080", "8080"}
	for _, addr := range bad {
		if err := validateBindAddr(addr); err == nil {
			t.Errorf("validateBindAddr(%q) = nil, want an error", addr)
		}
	}
	good := []string{"127.0.0.1:8080", "::1:8080", "[::1]:8080", "localhost:8080", "fd00::1:7101"}
	for _, addr := range good {
		if err := validateBindAddr(addr); err != nil {
			t.Errorf("validateBindAddr(%q) = %v, want nil", addr, err)
		}
	}
}

// TestIsLoopbackAddr: dev mode is refused off loopback.
func TestIsLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		if !isLoopbackAddr(addr) {
			t.Errorf("isLoopbackAddr(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0:8080", "10.0.0.5:8080", "fd00::1:8080", "example.com:8080"} {
		if isLoopbackAddr(addr) {
			t.Errorf("isLoopbackAddr(%q) = true, want false", addr)
		}
	}
}

// TestValidatorURLBracketsIPv6: the mesh host reaches every validator's internal
// listener, so a bare IPv6 literal must be bracketed to be a URL.
func TestValidatorURLBracketsIPv6(t *testing.T) {
	if got, want := validatorURL("127.0.0.1", 0), "http://127.0.0.1:7101"; got != want {
		t.Errorf("validatorURL = %q, want %q", got, want)
	}
	if got, want := validatorURL("fd00::1", 3), "http://[fd00::1]:7104"; got != want {
		t.Errorf("validatorURL = %q, want %q", got, want)
	}
}
