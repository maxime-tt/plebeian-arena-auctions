/**
 * The facilitator's run loop (handover §5): watch the relay, assemble a swap
 * from public facts, POST it to the mint, publish the result.
 *
 * Outbound only — the relay and the mint. There is no inbound listener (the
 * carrier is a regular kind the chain ingests; nothing calls back), so it is
 * safe on the market VPS. It holds no custody key: the courier key that signs
 * the 1452 authors nothing authoritative.
 */

import { SimplePool, generateSecretKey, nip19 } from "nostr-tools";
import { hexToBytes } from "@noble/hashes/utils.js";
import { pointFromHex, verifyDLEQProof } from "@cashu/cashu-ts";
import { loadConfig, type Config } from "./config.ts";
import { anchorRefusal, subscribeCommittee, subscribeCommitments, verifyCommitteeSignature } from "./relay.ts";
import { planSwap, buildSwapBody, matchSignatures } from "./swap.ts";
import { isAlreadySpent, getMintInfo, getMintKeys, postSwap } from "./mint.ts";
import { buildCarrier, buildCarrierEvent, unblindToReceipts, SettlementRefusal } from "./settle.ts";
import { chainTimeFromCommitments, deadlineRefusal, effectiveNow, mintTimeFromInfo } from "./clock.ts";
import type {
  BidContent,
  CommitteeSignatureContent,
  CreateContent,
  NostrEvent,
  SettlementCarrierContent,
  StateCommitmentContent,
} from "./types.ts";

const log = (...parts: unknown[]) => console.log(new Date().toISOString(), ...parts);

function parseAuctionCoordinate(coordinate: string): { seller: string; d: string } | null {
  const m = /^1440:([0-9a-f]{64}):(.+)$/.exec(coordinate);
  if (!m) return null;
  return { seller: m[1] as string, d: m[2] as string };
}

async function fetchBid(pool: SimplePool, relays: string[], bidId: string): Promise<BidContent | null> {
  if (!/^[0-9a-f]{64}$/.test(bidId)) return null;
  const ev = await pool.get(relays, { ids: [bidId] });
  if (!ev || ev.kind !== 1441) return null;
  try {
    return JSON.parse(ev.content) as BidContent;
  } catch {
    return null;
  }
}

async function fetchCreate(
  pool: SimplePool,
  relays: string[],
  coordinate: string,
): Promise<CreateContent | null> {
  const parsed = parseAuctionCoordinate(coordinate);
  if (!parsed) return null;
  const ev = await pool.get(relays, { kinds: [1440], authors: [parsed.seller], "#d": [parsed.d], limit: 1 });
  if (!ev || ev.kind !== 1440) return null;
  try {
    return JSON.parse(ev.content) as CreateContent;
  } catch {
    return null;
  }
}

/** Resolve the courier's own secret key: env override, else ephemeral. */
function courierSecretKey(): Uint8Array {
  const nsec = process.env.SF_COURIER_NSEC;
  if (nsec && nsec.trim() !== "") {
    const decoded = nip19.decode(nsec.trim());
    if (decoded.type !== "nsec") throw new Error("SF_COURIER_NSEC is not an nsec");
    return decoded.data;
  }
  return generateSecretKey();
}

class Facilitator {
  private readonly commitments: StateCommitmentContent[] = [];
  private readonly completed = new Map<string, SettlementCarrierContent>();
  private readonly keysCache = new Map<string, Record<string, string>>();
  private readonly processing = new Set<string>();
  /**
   * 1451s whose anchor is not yet provable from the 1447 commitments we have seen. The
   * coordinator anchors at its OWN committed height, so a fact can arrive one or two blocks
   * ahead of our view; dropping it would stall a settlement nobody else retries. Each new
   * commitment re-checks them.
   */
  private readonly pendingAnchors = new Map<string, NostrEvent>();

  constructor(
    private readonly cfg: Config,
    private readonly pool: SimplePool,
    private readonly sk: Uint8Array,
    private readonly fetchImpl: typeof fetch = fetch,
  ) {}

  start(): { close(): void } {
    const committeeSub = subscribeCommittee(this.pool, this.cfg.relays, (ev) => {
      void this.onCommittee(ev).catch((err) => log("committee handler error:", (err as Error).message));
    });
    const commitmentSub = subscribeCommitments(this.pool, this.cfg.relays, (ev) => {
      this.onCommitment(ev);
    });
    log("facilitator watching", this.cfg.relays.join(","), "mint", this.cfg.mintUrl);
    return {
      close: () => {
        committeeSub.close();
        commitmentSub.close();
      },
    };
  }

  private onCommitment(ev: NostrEvent): void {
    try {
      const c = JSON.parse(ev.content) as StateCommitmentContent;
      if (typeof c?.height === "number" && typeof c?.time === "number") this.commitments.push(c);
    } catch {
      /* ignore a malformed commitment; it is only a clock/anchor hint */
    }
    // A new committed height may make a stashed 1451 provable; re-dispatch each pending fact
    // (it is re-stashed if it still is not).
    if (this.pendingAnchors.size > 0) {
      const pending = [...this.pendingAnchors.entries()];
      this.pendingAnchors.clear();
      for (const [, event] of pending) {
        void this.onCommittee(event).catch((err) => log("pending 1451 handler error:", (err as Error).message));
      }
    }
  }

  private async onCommittee(ev: NostrEvent): Promise<void> {
    const verified = verifyCommitteeSignature(ev, {
      committeeGroupPubkey: this.cfg.committeeGroupPubkey,
      validatorRoster: this.cfg.validatorRoster,
    });
    if (!verified.ok) {
      log("drop 1451:", verified.reason);
      return;
    }
    const content = verified.value.content;
    const key = `${content.bid}|${content.outcome}`;
    const anchor = anchorRefusal(content, this.commitments);
    if (anchor !== null) {
      if (anchor.startsWith("anchor: 1451 height")) {
        // Not WRONG, just not yet provable: our own committed view lags the coordinator's by a
        // block or so. Keep it and re-check on the next commitment.
        this.pendingAnchors.set(`${content.bid}|${content.outcome}`, ev);
        return;
      }
      log("drop 1451:", anchor);
      return;
    }
    if (this.processing.has(key)) return;

    const cached = this.completed.get(key);
    if (cached) {
      await this.publishCarrier(cached, "repeat");
      return;
    }

    this.processing.add(key);
    try {
      await this.settle(content);
    } finally {
      this.processing.delete(key);
    }
  }

  private async settle(committee: CommitteeSignatureContent): Promise<void> {
    const bid = await fetchBid(this.pool, this.cfg.relays, committee.bid);
    if (!bid) {
      log(`no bid ${committee.bid} on the relay; skipping`);
      return;
    }
    const create = await fetchCreate(this.pool, this.cfg.relays, committee.auction);
    if (!create) {
      log(`no create ${committee.auction} on the relay; skipping`);
      return;
    }

    const planned = planSwap({
      committee,
      bid,
      create,
      committeeGroupPubkey: this.cfg.committeeGroupPubkey,
    });
    if (!planned.ok) {
      log(`decline ${committee.bid} ${committee.outcome}:`, planned.reason);
      return;
    }
    const plan = planned.plan;

    const refusal = await this.deadlineRefusalFor(plan.locktime);
    if (refusal) {
      log(`decline ${committee.bid} ${committee.outcome}:`, refusal);
      return;
    }

    let signatures;
    try {
      signatures = await postSwap(this.cfg.mintUrl, buildSwapBody(plan), {
        fetchImpl: this.fetchImpl,
        timeoutMs: this.cfg.requestTimeoutMs,
      });
    } catch (err) {
      if (isAlreadySpent(err)) {
        // A spent input is success: someone (another facilitator, the
        // recipient) already executed. Nothing to publish from here; the
        // chain's own receipt is the primary source.
        log(`${committee.bid} ${committee.outcome}: input already spent; nothing to do`);
        return;
      }
      log(`swap failed for ${committee.bid} ${committee.outcome}:`, (err as Error).message);
      return;
    }

    const keys = await this.keysFor(plan.outputs.map((o) => o.id));
    const matched = matchSignatures(plan.outputs, signatures, (index, sig) => {
      // Disambiguate outputs sharing (amount, id) by their DLEQ.
      try {
        const out = plan.outputs[index]!;
        const keyHex = keys[String(out.amount)];
        if (!keyHex || !sig.dleq?.e || !sig.dleq?.s) return false;
        return verifyDLEQProof(
          { e: hexToBytes(sig.dleq.e), s: hexToBytes(sig.dleq.s) },
          pointFromHex(out.B_),
          pointFromHex(sig.C_),
          pointFromHex(keyHex),
        );
      } catch {
        return false;
      }
    });

    let carrier: SettlementCarrierContent;
    try {
      const proofs = unblindToReceipts(plan, matched, keys);
      carrier = buildCarrier(committee, proofs);
    } catch (err) {
      if (err instanceof SettlementRefusal) log(`decline ${committee.bid}: ${err.message}`);
      else log(`unblind error for ${committee.bid}:`, (err as Error).message);
      return;
    }

    this.completed.set(`${committee.bid}|${committee.outcome}`, carrier);
    await this.publishCarrier(carrier, "settle");
  }

  private async deadlineRefusalFor(locktime: number): Promise<string | null> {
    let mintTime: number | null = null;
    try {
      mintTime = mintTimeFromInfo(
        await getMintInfo(this.cfg.mintUrl, {
          fetchImpl: this.fetchImpl,
          timeoutMs: this.cfg.requestTimeoutMs,
        }),
      );
    } catch (err) {
      log("mint /v1/info unavailable:", (err as Error).message);
    }
    const chain = chainTimeFromCommitments(this.commitments);
    const reading = effectiveNow({
      chainTime: chain ? chain.time : null,
      mintTime,
      wallClockSec: Math.floor(Date.now() / 1000),
      fallbackMarginSec: this.cfg.fallbackMarginSec,
    });
    const refusal = deadlineRefusal(reading.now, locktime, this.cfg.clockToleranceSec);
    return refusal === null ? null : `${refusal} (clock ${reading.source})`;
  }

  private async keysFor(ids: string[]): Promise<Record<string, string>> {
    const id = ids[0];
    if (id === undefined) throw new SettlementRefusal("plan has no outputs");
    const cached = this.keysCache.get(id);
    if (cached) return cached;
    const keys = await getMintKeys(this.cfg.mintUrl, id, {
      fetchImpl: this.fetchImpl,
      timeoutMs: this.cfg.requestTimeoutMs,
    });
    this.keysCache.set(id, keys);
    return keys;
  }

  private async publishCarrier(carrier: SettlementCarrierContent, why: string): Promise<void> {
    const event = buildCarrierEvent(this.sk, carrier, Math.floor(Date.now() / 1000));
    try {
      const results = await Promise.allSettled(this.pool.publish(this.cfg.relays, event));
      const ok = results.filter((r) => r.status === "fulfilled").length;
      log(`${why}: published 1452 ${event.id} for ${carrier.bid} to ${ok}/${this.cfg.relays.length} relays`);
    } catch (err) {
      log(`1452 publish failed for ${carrier.bid}:`, (err as Error).message);
    }
  }
}

async function main(): Promise<void> {
  const configPath = process.argv[2] ?? process.env.SF_CONFIG ?? "./config.json";
  const cfg = await loadConfig(configPath);
  const pool = new SimplePool();
  const sk = courierSecretKey();
  const facilitator = new Facilitator(cfg, pool, sk);
  const handle = facilitator.start();
  if (process.env.SF_ONCE === "1") {
    setTimeout(() => {
      handle.close();
      pool.close([]);
      process.exit(0);
    }, 2_000);
  }
  const stop = () => {
    log("shutting down");
    handle.close();
    pool.close([]);
    process.exit(0);
  };
  process.on("SIGINT", stop);
  process.on("SIGTERM", stop);
}

if (import.meta.main) {
  main().catch((err) => {
    log("fatal:", (err as Error).message);
    process.exit(1);
  });
}

export { Facilitator };
