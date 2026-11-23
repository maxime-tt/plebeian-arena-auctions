package main

// Tests for the mint-capability gate (mintcaps.go, handover §3.7): a mint is
// trusted only if GET /v1/info advertises NUT-10 (P2PK) and NUT-11 (SIG_ALL).
// The gate runs against an httptest server, so no live mint is needed.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireMintCapabilities(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want string // "" = must pass; otherwise a substring of the error
	}{
		{"both nuts", 200, `{"version":"Nutshell/0.21","nuts":{"4":{},"5":{},"10":{},"11":{}}}`, ""},
		{"missing sig_all", 200, `{"nuts":{"10":{}}}`, "NUT-11"},
		{"missing p2pk", 200, `{"nuts":{"11":{}}}`, "NUT-10"},
		{"neither", 200, `{"nuts":{"4":{}}}`, "NUT-10 and NUT-11"},
		{"no nuts field", 200, `{"version":"x"}`, "NUT-10 and NUT-11"},
		{"bad json", 200, `{`, "bad /v1/info"},
		{"server error", 500, ``, "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/info" {
					t.Errorf("path = %q, want /v1/info", r.URL.Path)
				}
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := requireMintCapabilities(context.Background(), srv.URL)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want the mint accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}

	// An empty URL is refused without any network call.
	if err := requireMintCapabilities(context.Background(), "   "); err == nil {
		t.Fatal("an empty mint url must be refused")
	}
	// A trailing slash must not double up on the path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/info" {
			t.Errorf("path = %q, want /v1/info", r.URL.Path)
		}
		w.Write([]byte(`{"nuts":{"10":{},"11":{}}}`))
	}))
	defer srv.Close()
	if err := requireMintCapabilities(context.Background(), srv.URL+"/"); err != nil {
		t.Fatalf("a trailing slash must be normalised: %v", err)
	}
}
