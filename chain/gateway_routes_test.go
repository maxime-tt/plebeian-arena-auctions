package main

// The gateway's route table is a pure function of its dependencies
// (gatewayMux), so the whole "retire the public HTTP surface" change
// (handover §6.1/§6.2) is assertable without starting a process.
//
// The rules under test:
//   - a DEPLOYED gateway (dev=false) registers NO route and starts NO listener;
//     every retired public path is absent because there is no handler at all;
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

// TestDeployedGatewayStartsNoListenerAndRegistersNoRoute: the deployed role is
// the validator supervisor only.  gatewayMux returns nil, so cmdGateway builds
// no http.Server and there is no route to serve.
func TestDeployedGatewayStartsNoListenerAndRegistersNoRoute(t *testing.T) {
	if h := gatewayMux(gatewayTestDeps(), false); h != nil {
		t.Fatalf("a deployed gateway must register no route and start no listener, got %T", h)
	}
	if srv := gatewayServer(false, "127.0.0.1:8080", nil); srv != nil {
		t.Fatalf("a deployed gateway must not construct an http.Server, got %+v", srv)
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
