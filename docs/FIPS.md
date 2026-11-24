# FIPS — the validator mesh and its firewall

Each validator has its **own** FIPS identity (an npub) and whitelists the other
three. Only the FROST port is opened to peers; relay traffic stays ordinary
WebSocket/HTTPS, and there is **no inbound public HTTP** at all. FIPS is both the
mesh transport and the firewall. (Handover §3.1–§3.2.)

## Node and port map

From `chain/node.go`: `httpPort(i) = 7101 + i`, and the per-validator HTTP server
on that port serves the FROST endpoints `/frost/commit` and `/frost/sign`
(`chain/node.go:378-380`). CometBFT p2p and RPC are loopback.

| Validator | `-i` | HTTP / **FROST port** | p2p | RPC |
|---|---|---|---|---|
| Validator 1 | 1 | **7101** | 26656 | 26657 |
| Validator 2 | 2 | **7102** | 26666 | 26667 |
| Validator 3 | 3 | **7103** | 26676 | 26677 |
| Validator 4 | 4 | **7104** | 26686 | 26687 |

The mint listens on `127.0.0.1:3338`. Each validator's internal listener binds
to `-mesh-host` (its FIPS address on a deployed node, `127.0.0.1` on a
single-host run) and carries `/frost/commit`, `/frost/sign` plus the
coordinator-facing read routes (`/state`, `/bid`, `/height`, `/due`, `/status`).
The FIPS ACL is the only boundary on that listener: no route is public. The
`arena-frost.nft` drop-in below opens the listener's port to the other three
validators, which admits the read routes on that port to those peers as well;
they never leave the mesh. The settlement coordinator now lives INSIDE the
elected validator node (the smallest `Params.Validators` pubkey): each node
reaches the others' `/frost/commit` and `/frost/sign` on this same listener, and
a deployed node names its peers with `-mesh-peers` (default
`127.0.0.1:<httpPort(j)>`). The deployed gateway is only the validator
supervisor and starts no HTTP listener at all; ops visibility stays on the
validators' mesh `/status`. (Handover §8.6 / Q2.)

## Per-node configuration shape (handover §3.2)

Two root-owned files decide who can reach this node over the mesh:

- `/etc/fips/peers.allow` — one npub per line, the **other three** validators.
- `/etc/fips/fips.d/arena-frost.nft` — the `fips0` firewall drop-in: accept only
  the FROST port from the derived mesh addresses, default drop:

  ```
  ip6 saddr <derived-peer-1>/128 tcp dport <frost-port> accept
  ip6 saddr <derived-peer-2>/128 tcp dport <frost-port> accept
  ip6 saddr <derived-peer-3>/128 tcp dport <frost-port> accept
  ```

  `<derived>` is the peer's FIPS `NodeAddr` — the first 16 bytes of the SHA-256 of
  its x-only key, rendered `fd…`. **Never hand-compute it**: derive each one with

  ```bash
  fipsctl address <npub>
  ```

## The documented flow: `~/work/fips/fips-allow-peer`

`~/work/fips/README-fips-allow-peer.md` documents the helper that derives the
address, appends the ACL line and writes exactly one drop-in named after the peer.
Use it rather than editing the files by hand:

```bash
# look at the truth first — never guess
sudo -n /usr/local/sbin/fips-allow-peer --list

# a service: admit the key AND open the FROST port it must reach
sudo -n /usr/local/sbin/fips-allow-peer --dry-run <npub> <frost-port>
sudo -n /usr/local/sbin/fips-allow-peer <npub> <frost-port>          # apply

# a peering-only grant: admit the key, open nothing
sudo -n /usr/local/sbin/fips-allow-peer --peer-only <npub>

# undo, symmetrically
sudo -n /usr/local/sbin/fips-allow-peer --revoke <npub>
```

`--dry-run` derives and prints the address and the exact files it would write
without changing anything; it and `--list` run unprivileged. The helper validates
the npub through `fipsctl address` (so a typo can never reach a file) and appends
to the ACL rather than rewriting it.

## Verify the loaded state, not the file

The daemon re-reads `peers.allow` itself within ~2 s; `systemctl reload fips` is
**not applicable** on this unit (no `ExecReload`), so its failure does not mean the
ACL did not load.

```bash
fipsctl acl show | grep -F <npub>    # allow_entries must include the peer
fipsctl probe <npub>                 # reachability
```

If `fipsctl acl show` does not move, stop and investigate — do not restart the
daemon. (The arena's own code has no FIPS integration yet: this document is the
per-node configuration the deployment requires, per handover §3.2.)
