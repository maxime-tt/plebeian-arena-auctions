// Live probe for the ADDITIVE REBID CHAIN (R1-A, R2-A, R3, R4).
//
//   node tests/probe-additive-rebid.mjs [http://127.0.0.1:8090] [wss://relay.playday.it]
//
// One fixed lot, four facts, all read from the wire:
//
//  1. R2-A / R1-A — a two-leg rebid chain.  A bids, then A RAISES THEIR OWN
//     position.  Both are accepted, /api/state shows highAmount == the SUM of
//     the legs and BOTH legs `leading`, and the second leg's escrow is only the
//     delta it locked.
//  2. R4 — every admitted bid is published as a kind-1449 event with schema
//     arena.bidadmit.v1, carrying the leg's delta and the position it belongs
//     to, anchored (content AND tags) at that bid's OWN committed height, signed
//     by a validator on the kind-1448 roster.
//  3. R3 — a below-floor bid is REFUSED at admission: no commit, no bid in
//     /api/state, no entry in the lot's void list, and its escrow is untouched
//     at the mint (still unspent).
//  4. Settlement over EVERY leg — the lot closes, every `won` leg settles into
//     its own kind-1444 receipt, and the receipts sum to the position the lot
//     closed at.
//
// The probe leaves real state behind (a lot, a settled escrow, and one refused
// escrow left locked until its locktime). Run it against a demo root unless the
// point is to prove the DEPLOYMENT.

import { spawn } from 'node:child_process';
import { ArenaClient, newIdentity, idPub, walletPub } from '../web/src/lib/arena.js';

const BASE = process.argv[2] || 'http://127.0.0.1:8090';
const RELAY = process.argv[3] || process.env.ACCEPTANCE_RELAY || 'wss://relay.playday.it';
const NAK = process.env.NAK_BINARY || `${process.env.HOME || ''}/.local/bin/nak`;

const c = new ArenaClient(BASE);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let pass = 0, fail = 0;
const ok = (cond, msg) => { if (cond) { pass++; console.log('  PASS', msg); } else { fail++; console.log('  FAIL', msg); } };
async function until(f, ms, label) {
  const t0 = Date.now();
  let last;
  while (Date.now() - t0 < ms) {
    try { const v = await f(); if (v) return v; } catch (e) { last = e; }
    await sleep(1000);
  }
  throw new Error(`timed out waiting for ${label}${last ? ` (${last.message})` : ''}`);
}
const attempt = async (p, label) => { try { return await p; } catch (e) { ok(false, `${label} (${e.message})`); return null; } };
const finish = async () => {
  await c.close().catch(() => {});
  console.log(`\nprobe-additive-rebid: ${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
};
const tagOf = (ev, name) => { const t = (ev.tags || []).find((x) => x[0] === name); return t ? t[1] : undefined; };

function nakReq(kind) {
  return new Promise((resolve, reject) => {
    const p = spawn(NAK, ['req', '-k', String(kind), RELAY], { stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '', err = '', settled = false;
    const parse = () => {
      const evs = [];
      for (const line of out.split('\n')) {
        const s = line.trim();
        if (!s) continue;
        try { evs.push(JSON.parse(s)); } catch { /* a nak banner line */ }
      }
      return evs;
    };
    const timer = setTimeout(() => p.kill('SIGKILL'), 15000);
    const done = (why) => {
      if (settled) return;
      settled = true; clearTimeout(timer);
      if (why) reject(new Error(`nak req -k ${kind} ${why}: ${err.trim()}`)); else resolve(parse());
    };
    p.stdout.on('data', (d) => { out += d; });
    p.stderr.on('data', (d) => { err += d; });
    p.on('error', (e) => done(`could not run (${e.message})`));
    p.on('close', (code) => { if (parse().length || code === 0) done(null); else done(`exited ${code}`); });
  });
}

// The newest kind-1448 params event: the roster a published event's signer must
// be on, and the terms (min duration, increment) the probe builds its lot with.
async function relayParams() {
  const evs = await nakReq(1448);
  if (!evs.length) throw new Error('no kind-1448 params event on the relay');
  evs.sort((a, b) => (b.created_at - a.created_at) || (a.id < b.id ? 1 : -1));
  const content = JSON.parse(evs[0].content);
  return { content, roster: new Set((content.validators || []).map((v) => String(v.pubkey).toLowerCase())) };
}

// placeBid mirrors the suite's funded-bid path; force skips the client's floor
// check (used for the leader's own raise, which the SHIPPED client still refuses
// client-side -- the chain accepts it now: R2-A).
async function placeBid(u, coord, amount, force = false) {
  const st = await c.state();
  const au = st.auctions[coord];
  const { locked } = await c.lockForBid(u, amount, st.params, au, { force, state: st });
  const r = await c.bid(u, au, amount, locked);
  const bid = await until(async () => { const s2 = await c.state(); return s2.bids[r.id] && s2; }, 20000, 'the bid to commit');
  return { id: r.id, locked, state: bid };
}

const seller = newIdentity('Additive Rebid Seller');
const alice = newIdentity('Additive Rebid Alice');
const bob = newIdentity('Additive Rebid Bob');
const coordOf = (d) => `1440:${idPub(seller)}:${d}`;

const params0 = await relayParams();
const st0 = await c.state();
console.log(`\n[setup] chain height ${st0.height}, params_version ${params0.content.params_version}`);

const D = 'additive-rebid-' + Date.now();
const MIN_DUR = Number(st0.params.minDurationSec ?? 20);
const DUR = Math.max(40, MIN_DUR);
await c.createAuction(seller, {
  title: 'Additive rebid probe', description: 'probe', image: '', category: 'Art',
  startPrice: 100, minIncrement: 10, durationSec: DUR, mode: 'fixed',
}, D);
const coord = coordOf(D);
const created = await attempt(until(async () => { const st = await c.state(); return st.auctions[coord] && st; }, 20000, 'the create to commit'), 'the create to commit');
if (!created) { await finish(); }
const au0 = created.auctions[coord];
const openSec = au0.endAt - Math.floor(Date.now() / 1000);
ok(au0.mode === 'fixed' && au0.startPrice === 100 && au0.minIncrement === 10, `lot open: fixed, start 100, increment 10, ${openSec} s left`);
if (openSec < 20) { ok(false, `the lot has only ${openSec} s left: too short to bid twice`); await finish(); }

// ── 1. the two-leg rebid chain (R1-A, R2-A) ────────────────────────────────
console.log('\n[1] a two-leg rebid chain: bid, then the LEADER raises their own position');
for (const u of [alice, bob]) await c.faucet(u, 1000);
ok(await c.balance(alice) === 1000, 'Alice funded with 1000 fake sats');

const leg1 = await attempt(placeBid(alice, coord, 100), 'leg 1 (alice 100) to commit');
if (!leg1) { await finish(); }
ok(leg1.state.auctions[coord].highBid === leg1.id && leg1.state.auctions[coord].highAmount === 100,
  `leg 1 committed: highAmount ${leg1.state.auctions[coord].highAmount} (height ${leg1.state.height})`);

const leg2 = await attempt(placeBid(alice, coord, 5, true), 'leg 2 (the leader raising by 5) to commit');
if (!leg2) { await finish(); }
const au1 = leg2.state.auctions[coord];
console.log(`      RAW STATE highAmount=${au1.highAmount} highBid=${au1.highBid}`);
console.log(JSON.stringify({ auction: au1, bids: leg2.state.auctions[coord].bids.map((b) => leg2.state.bids[b]) }, null, 2).slice(0, 1800));
ok(au1.highAmount === 105, `the LEADER's own raise is accepted and the position is the SUM: highAmount ${au1.highAmount} == 100 + 5`);
ok(au1.highBid === leg2.id, 'highBid is the leg that set the position (the newest one)');
ok(leg1.state.bids[leg1.id].status === 'leading' || leg2.state.bids[leg1.id].status === 'leading', 'leg 1 is still a LEADING leg of the chain');
ok(leg2.state.bids[leg2.id].status === 'leading', 'leg 2 is leading');
ok(leg2.state.bids[leg2.id].amount === 5, `leg 2's escrow is only the DELTA it locked (${leg2.state.bids[leg2.id].amount} sats), not 105`);
const leadingSum = leg2.state.auctions[coord].bids.map((b) => leg2.state.bids[b]).filter((b) => b.status === 'leading').reduce((s, b) => s + b.amount, 0);
ok(leadingSum === au1.highAmount, `Σ leading legs ${leadingSum} == highAmount ${au1.highAmount} (the §2.4 invariant)`);

// ── 2. R4: the admission fact on the relay ─────────────────────────────────
console.log(`\n[2] the per-bid admission fact (kind 1449 ${'arena.bidadmit.v1'}) on ${RELAY}`);
const admissions = await attempt(until(async () => {
  const evs = (await nakReq(1449)).filter((ev) => tagOf(ev, 'auction') === coord
    && (() => { try { return JSON.parse(ev.content).schema === 'arena.bidadmit.v1'; } catch { return false; } })());
  return evs.length >= 2 ? evs : null;
}, 60000, 'the admission events'), 'the admission events');
if (!admissions) { await finish(); }
admissions.sort((a, b) => (Number(tagOf(a, 'height') || 0) - Number(tagOf(b, 'height') || 0)));
for (const ev of admissions) console.log(`      RAW ADMIT EVENT ${ev.id}\n${JSON.stringify(ev, null, 2)}`);
const byBid = new Map(admissions.map((ev) => [JSON.parse(ev.content).bid_id, { ev, ct: JSON.parse(ev.content) }]));
ok(byBid.size >= 2, `an admission fact per admitted bid (${byBid.size} events)`);

for (const [label, leg] of [['leg 1', leg1], ['leg 2', leg2]]) {
  const got = byBid.get(leg.id);
  if (!got) { ok(false, `${label}: no admission event names ${leg.id}`); continue; }
  const { ev, ct } = got;
  const bid = leg.state.bids[leg.id];
  ok(ct.schema === 'arena.bidadmit.v1', `${label}: schema arena.bidadmit.v1`);
  ok(ct.auction_id === coord && ct.bid_id === leg.id && ct.bidder === idPub(alice), `${label}: names the lot, the bid and the bidder`);
  ok(ct.amount === bid.amount, `${label}: amount ${ct.amount} == the leg's committed delta ${bid.amount}`);
  // The position carried is the position of the chain the bid BELONGS TO at
  // publication. Leg 2 is the newest leg, so it is 105 whatever the pass: the
  // leg that set the position names itself. Leg 1 was alone for a moment, so a
  // pass that ran between the two bids recorded 100 (its own resulting
  // position) -- both are truthful, and the app can tell them apart by
  // high_bid.
  const positions = label === 'leg 2' ? [105] : [100, 105];
  ok(positions.includes(ct.position), `${label}: position ${ct.position} is the position it belongs to (${positions.join(' or ')})`);
  ok(ct.position >= ct.amount, `${label}: the position is never below the leg's own delta`);
  const highBids = label === 'leg 2' ? [leg2.id] : [leg1.id, leg2.id];
  ok(highBids.includes(ct.high_bid), `${label}: high_bid names the leading leg (${String(ct.high_bid).slice(0, 12)}…)`);
  ok(ct.height === bid.height, `${label}: anchored at the bid's OWN committed height (${ct.height} == ${bid.height})`);
  ok(tagOf(ev, 'e') === ct.bid_id, `${label}: e tag == content bid_id`);
  ok(tagOf(ev, 'auction') === ct.auction_id, `${label}: auction tag == content auction_id`);
  ok(tagOf(ev, 'height') === String(ct.height), `${label}: height tag == content height`);
  ok(tagOf(ev, 'hash') === ct.block_hash && /^[0-9a-fA-F]{64}$/.test(String(ct.block_hash)), `${label}: hash tag == content block_hash`);
  ok(tagOf(ev, 'p') === ev.pubkey, `${label}: p tag names the signer`);
  ok(params0.roster.has(String(ev.pubkey).toLowerCase()), `${label}: signer is on the kind-1448 roster`);
}

// ── 3. R3: a below-floor bid is refused at admission ───────────────────────
console.log('\n[3] R3: a below-floor bid is REFUSED at admission (never committed, never void)');
const floor = au1.highAmount + au1.minIncrement; // 105 + 10
const stBefore = await c.state();
const small = Math.max(1, floor - 1);
const { locked: bobLocked } = await c.lockForBid(bob, small, stBefore.params, stBefore.auctions[coord], { force: true, state: stBefore });
await attempt(c.bid(bob, stBefore.auctions[coord], small, bobLocked)
  .then(() => { ok(false, `a ${small}-sat bid was ACCEPTED (the floor is ${floor})`); })
  .catch((e) => { ok(new RegExp(`at least ${floor}`).test(e.message), `the chain refused the ${small}-sat bid at admission -> "${e.message}"`); }),
  'the below-floor bid to be refused');
const stAfter = await c.state();
const bobBids = stAfter.auctions[coord].bids.map((b) => stAfter.bids[b]).filter((b) => b.bidder === idPub(bob));
ok(bobBids.length === 0, 'no bid of Bob\'s is in /api/state: the refusal was never committed');
ok((stAfter.auctions[coord].voids || []).length === 0, 'the lot\'s void list is still EMPTY: a refused bid is not a void');
ok(stAfter.auctions[coord].highAmount === 105 && stAfter.auctions[coord].highBid === leg2.id, 'the lot is untouched: Alice still leads at 105');
const spent = await c.spent(bobLocked);
console.log(`      INFO mint checkstate for the refused escrow: ${JSON.stringify(spent)}`);
ok(spent.length === bobLocked.length && spent.every((s) => s === false), `the refused escrow is untouched: all ${bobLocked.length} proofs are UNSPENT (reclaimable after locktime)`);
ok(await c.balance(bob) === 1000 - small, `Bob's wallet still shows the locked ${small} sats (he holds ${1000 - small})`);

// ── 4. settlement over every won leg ───────────────────────────────────────
console.log('\n[4] the lot closes and EVERY won leg settles');
const closed = await attempt(until(async () => { const st = await c.state(); return st.auctions[coord].status === 'closed' && st; }, 180000, 'the lot to close'), 'the lot to close');
if (!closed) { await finish(); }
const auC = closed.auctions[coord];
const wonLegs = auC.bids.map((b) => closed.bids[b]).filter((b) => b.status === 'won');
ok(auC.winner === leg2.id, `closed with the newest leg of the winning chain as winner (${String(auC.winner).slice(0, 12)}…)`);
ok(wonLegs.length === 2 && wonLegs.reduce((s, b) => s + b.amount, 0) === auC.highAmount,
  `EVERY leading leg is won (${wonLegs.length}) and they sum to the position ${auC.highAmount}`);

const bothSettled = await attempt(until(async () => {
  const st = await c.state();
  const legs = [leg1.id, leg2.id].map((id) => st.bids[id]);
  return legs.every((b) => b && b.settled === 'settle' && b.out && b.out.length) ? st : null;
}, 180000, 'both won legs to settle'), 'both won legs to settle');
if (!bothSettled) { await finish(); }
ok(true, 'both legs are recorded settled:settle');
// The money: take the per-leg settle outputs into the seller's wallet, exactly
// as a client does with the pushed ecash.
const settledProofs = [leg1.id, leg2.id].flatMap((id) => bothSettled.bids[id].out);
const sellerKey = walletPub(seller).toLowerCase();
ok(settledProofs.every((p) => String(JSON.parse(p.secret)[1].data).toLowerCase() === sellerKey),
  'every settled output is P2PK-locked to the committed seller wallet key');
await c.receive(seller, settledProofs);
const sellerBal = await c.balance(seller);
ok(sellerBal === auC.highAmount, `the seller received the SUM of the legs: ${sellerBal} == highAmount ${auC.highAmount}`);

console.log(`\n[4] the per-leg settle receipts (kind 1444) on ${RELAY}`);
let receiptTotal = 0, legReceipts = 0;
for (const id of [leg1.id, leg2.id]) {
  const evs = await attempt(until(async () => {
    const m = (await nakReq(1444)).filter((ev) => tagOf(ev, 'bid') === id);
    return m.length ? m : null;
  }, 60000, `a kind-1444 receipt for ${id}`), `a kind-1444 receipt for ${id}`);
  if (!evs) continue;
  evs.sort((a, b) => (a.created_at - b.created_at) || (a.id < b.id ? -1 : 1));
  const ev = evs[0], ct = JSON.parse(ev.content);
  console.log(`      RAW 1444 EVENT ${ev.id}\n${JSON.stringify(ev, null, 2)}`);
  ok(ct.schema === 'arena.settle.v1' && ct.auction_id === coord && ct.bid_id === id && ct.outcome === 'settle', `receipt for ${String(id).slice(0, 12)}… names the lot, that leg and the outcome`);
  ok(ct.out.every((o) => String(o.recipient).toLowerCase() === String(walletPub(seller)).toLowerCase()), 'its payout is locked to the committed seller wallet');
  ok(tagOf(ev, 'height') === String(ct.height) && tagOf(ev, 'hash') === ct.block_hash, 'its anchor agrees between tags and content');
  receiptTotal += ct.out.reduce((s, o) => s + Number(o.amount), 0);
  legReceipts++;
}
ok(legReceipts === 2, `one settle receipt per won leg (${legReceipts})`);
ok(receiptTotal === auC.highAmount, `Σ receipts ${receiptTotal} == the position the lot closed at ${auC.highAmount}`);

await finish();
