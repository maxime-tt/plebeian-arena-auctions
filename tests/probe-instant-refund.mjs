// Live probe for HANDOVER-2026-11-18 §4.1: measure the instant refund.
//
//   node tests/probe-instant-refund.mjs [http://127.0.0.1:8095]
//
// A funds and bids at the start price; B outbids at a higher amount; the lot
// stays OPEN (600 s of anti-snipe time), so a refund that lands here is an
// instant, leader-change effect and NOT a close-time one. We watch /api/state
// until A's bid shows settled:"refund" with a non-empty out, then ask the mint
// (NUT-07) whether A's escrow proofs are SPENT.
//
// The chain must already be up with -demo-controls (the suite's own flag):
// scripts/acceptance-validator-workstreams.sh wires this probe in.
//
// This file asserts the MONEY facts only. The kind-1443 receipt assertion is a
// separate increment (the chain change that publishes it is not in this tree),
// so it is kept as one function, checkReceipt(), called once at the end.
import { ArenaClient, newIdentity, idPub } from '../web/src/lib/arena.js';

const BASE = process.argv[2] || 'http://127.0.0.1:8080';
const c = new ArenaClient(BASE);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let pass = 0, fail = 0;
const ok = (cond, msg) => { if (cond) { pass++; console.log('  PASS', msg); } else { fail++; console.log('  FAIL', msg); } };
async function until(f, ms, label) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) { const v = await f().catch(() => null); if (v) return v; await sleep(250); }
  throw new Error('timeout waiting for ' + label);
}
// The probe holds Coco wallets (from faucet); dispose them and exit explicitly
// so a lingering handle can never wedge the suite's pipeline.
const finish = async () => {
  await c.close().catch(() => {});
  console.log(`\nprobe: ${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
};
// A timed-out `until` is a FAIL with the label printed, never a crash (spec).
const attempt = async (p, label) => { try { return await p; } catch (e) { ok(false, `${label} (${e.message})`); return null; } };

// The suite's funded-bid path (tests/e2e.mjs placeBid): lockForBid() then
// bid(), and the locked escrow proofs are kept by the caller.
async function placeBid(u, coord, amount) {
  const st = await c.state();
  const au = st.auctions[coord];
  const { locked } = await c.lockForBid(u, amount, st.params, au, { state: st });
  const r = await c.bid(u, au, amount, locked);
  const bid = await until(async () => { const s2 = await c.state(); return s2.bids[r.id] && s2; }, 15000, 'the bid to commit');
  return { id: r.id, locked, state: bid };
}

// The kind-1443 (refund) / 1444 (settle) receipt assertions land here in the
// next increment. Nothing to assert in this tree yet: the publishing hook is
// not written, so a receipt would be a false negative, not a finding.
async function checkReceipt() { /* receipt step: separate increment */ }

const seller = newIdentity('Instant Refund Seller');
const alice = newIdentity('Instant Refund Alice');
const bob = newIdentity('Instant Refund Bob');
const coordOf = (d) => `1440:${idPub(seller)}:${d}`;

console.log('\n[§4.1] the instant refund: A is outbid on a lot that is still open');

const d = 'instant-refund-' + Date.now();
const lot = {
  title: 'Instant refund probe', description: 'probe', image: '', category: 'Art',
  startPrice: 100, minIncrement: 10,
  durationSec: 600,          // 10 minutes: the lot stays open through the whole measurement
  mode: 'antisnipe', windowSec: 60,
};
await c.createAuction(seller, lot, d);
const coord = coordOf(d);
const created = await attempt(until(async () => { const st = await c.state(); return st.auctions[coord] && st; }, 15000, 'the create to commit'), 'the create to commit');
if (!created) { await finish(); }
const au0 = created.auctions[coord];
ok(au0.mode === 'antisnipe' && au0.windowSec === 60, `lot open: mode=${au0.mode} windowSec=${au0.windowSec} durationSec=${au0.durationSec}`);
ok(created.height > 0, `committed at height ${created.height}, ends at ${au0.endAt} (${Math.round(au0.endAt - created.time)} s from the block time)`);

for (const u of [alice, bob]) await c.faucet(u, 1000);

// Bid A (alice) at the start price.
const A = await attempt(placeBid(alice, coord, au0.startPrice), 'bid A (alice) to commit');
if (!A) { await finish(); }
const escrowA = A.locked;    // A's escrow proofs, kept for the mint check
ok(A.state.auctions[coord].highBid === A.id, `bid A committed at ${A.state.bids[A.id].amount} sats (alice leads)`);

// Bid B (bob) at a higher amount -- this is the outbid.
const B = await attempt(placeBid(bob, coord, au0.startPrice + au0.minIncrement), 'bid B (bob) to commit');
if (!B) { await finish(); }
const tOutbid = Date.now();
ok(B.state.bids[A.id].status === 'outbid', `bid B committed at ${B.state.bids[B.id].amount} sats; A is now outbid (height ${B.state.height})`);
ok(B.state.auctions[coord].status === 'open', 'the lot is still open at the outbid');

// Poll /api/state until A's refund is recorded with its outputs.
const refund = await attempt(until(async () => {
  const st = await c.state();
  const b = st.bids[A.id];
  if (b && b.settled === 'refund' && b.out && b.out.length > 0) return { st, bid: b };
  return null;
}, 20000, 'the refund to appear while the lot is open'), 'the refund to appear while the lot is open');
const tRefund = Date.now();

if (!refund) {
  // The refund did not fire: that is the finding (spec §4.1). Print what the
  // state and the mint actually say, then fail.
  const st = await c.state().catch(() => null);
  if (st) {
    const b = st.bids[A.id] || {};
    console.log(`      INFO lot status ${st.auctions[coord]?.status}; bids[A].status ${b.status}; settled ${JSON.stringify(b.settled ?? null)}; out ${b.out?.length ?? 0}; height ${st.height}`);
  }
  const spent0 = await c.spent(escrowA).catch(() => null);
  console.log(`      INFO mint checkstate for A's escrow: ${spent0 ? JSON.stringify(spent0) : 'unavailable'}`);
  await finish();
}

const { st, bid } = refund;
const deltaMs = tRefund - tOutbid;

// --- the measured numbers ---
console.log(`      INFO outbid -> refund recorded: ${deltaMs} ms`);
console.log(`      INFO committed height when the refund appeared: ${st.height}`);
console.log(`      INFO bids[A].settled = ${JSON.stringify(bid.settled)}; A's bid committed at height ${bid.height} (block ${bid.time})`);
console.log(`      INFO bids[A].out = ${bid.out.length} proof entr${bid.out.length === 1 ? 'y' : 'ies'}, locked to ${bid.out.map((p) => JSON.parse(p.secret)[1].data.slice(0, 12) + '…').join(', ')}`);
for (const k of ['settledHeight', 'settledAt']) {
  const v = k in bid ? bid[k] : undefined;
  console.log(`      INFO bids[A].${k}: ${v == null ? 'absent/null (the chain change that adds it is not in this tree)' : v}`);
}
const spent = await c.spent(escrowA);
console.log(`      INFO mint checkstate for A's ${escrowA.length} escrow proofs: ${JSON.stringify(spent)}`);

// --- the assertions ---
ok(st.auctions[coord].status === 'open', `the lot is still OPEN when the refund is recorded (status=${st.auctions[coord].status}) -- the refund is instant, not a close-time effect`);
ok(deltaMs <= 10000, `the refund was recorded within 10 s of the outbid (measured ${deltaMs} ms)`);
ok(bid.out.length > 0, `A's refund carries a non-empty out (${bid.out.length} proof entries)`);
ok(spent.length === escrowA.length && spent.every(Boolean), `every one of A's ${escrowA.length} escrow proofs is SPENT at the mint`);
ok(st.auctions[coord].highBid === B.id && st.bids[A.id].status === 'outbid', 'bob is still the leading bid and A is outbid: the refund did not disturb the auction');

await checkReceipt();
await finish();
