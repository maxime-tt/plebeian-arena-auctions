package main

// The supervisor starts the four validators, and the demo control can stop and
// start one again.  Both paths must build the SAME command line: a restarted
// validator that came back without its -relay flags would have no observer (so
// no ingest), no verdict publisher, no state commitments and no params event --
// a second-class node that only p2p-syncs.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSupervisorRestartKeepsRelayFlags pins the argv a demo-control restart uses
// against the argv the gateway's initial start uses.  It asserts on the builder
// rather than exec'ing anything, so it spawns no nodes.
func TestSupervisorRestartKeepsRelayFlags(t *testing.T) {
	relays := []string{"wss://relay.example", "wss://relay2.example"}
	const commitMax = 10 * time.Second
	sup := &supervisor{root: "run/chain", relays: relays, commitMax: commitMax, meshHost: "10.0.0.1", meshPeers: "10.0.0.1:9001,10.0.0.2:9001,10.0.0.3:9001,10.0.0.4:9001"}

	for i := range 4 {
		args := sup.argsFor(i) // exactly what start(i) execs
		joined := strings.Join(args, " ")
		if args[0] != "node" {
			t.Fatalf("validator %d argv = %v, want it to run the node command", i+1, args)
		}
		if !strings.Contains(joined, fmt.Sprintf("-i %d", i+1)) {
			t.Fatalf("validator %d argv = %v, want its own -i", i+1, args)
		}
		for _, relay := range relays {
			if !strings.Contains(joined, "-relay "+relay) {
				t.Fatalf("validator %d argv = %v, lost -relay %s", i+1, args, relay)
			}
		}
		if !strings.Contains(joined, "-commit-max-interval=10s") {
			t.Fatalf("validator %d argv = %v, lost the commit interval", i+1, args)
		}
		if !strings.Contains(joined, "-mesh-host 10.0.0.1") {
			t.Fatalf("validator %d argv = %v, lost the mesh host", i+1, args)
		}
		// The peers' mesh addresses are what the elected coordinator dials for a
		// FROST round.  A node left on the loopback default cannot reach a single
		// peer on a deployed mesh, so no kind-1451 would ever be published.
		if !strings.Contains(joined, "-mesh-peers 10.0.0.1:9001,10.0.0.2:9001,10.0.0.3:9001,10.0.0.4:9001") {
			t.Fatalf("validator %d argv = %v, lost the mesh peers", i+1, args)
		}
	}

	// The initial start must produce the same flags as a restart, and must not
	// be the bare `node -root ... -i N` that caused the defect.
	initial := nodeArgs(sup.root, 3, relays, commitMax, sup.meshHost, sup.meshPeers)
	restart := sup.argsFor(3)
	if strings.Join(initial, " ") != strings.Join(restart, " ") {
		t.Fatalf("initial start argv %v differs from restart argv %v", initial, restart)
	}
	bare := []string{"node", "-root", sup.root, "-i", "4"}
	if strings.Join(restart, " ") == strings.Join(bare, " ") {
		t.Fatalf("restart argv is the bare node command: %v", restart)
	}
}

// TestNodeArgsOmitsTheIntervalWhenUnset: a supervisor built without a commit
// interval (a test, or an older caller) must not emit "-commit-max-interval=0s".
func TestNodeArgsOmitsTheIntervalWhenUnset(t *testing.T) {
	args := nodeArgs("run/chain", 0, []string{"wss://relay.example"}, 0, "127.0.0.1", "")
	if strings.Contains(strings.Join(args, " "), "-mesh-peers") {
		t.Fatalf("argv = %v, want no mesh-peers flag when the list is empty", args)
	}
	if strings.Contains(strings.Join(args, " "), "-commit-max-interval") {
		t.Fatalf("argv = %v, want no commit-interval flag when unset", args)
	}
	if !strings.Contains(strings.Join(args, " "), "-relay wss://relay.example") {
		t.Fatalf("argv = %v, want the relay flag", args)
	}
}
