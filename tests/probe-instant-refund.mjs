// Live probe for HANDOVER-2026-11-18 §4.1: measure the instant refund.
//
//   node tests/probe-instant-refund.mjs [http://127.0.0.1:8095] [ws://127.0.0.1:10547]
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
// Then the RECEIPTS (handover-2026-11-18 §3/§6): the same refund must have a
// kind-1443 event on the relay, anchored to the settle's committed height and
// carrying the minted outputs, and a second short fixed lot is closed to get the
// kind-1444 settle receipt for the seller's payout. Both are read back from the
// relay with nak and re-checked against /api/state and the params roster, and
// both are printed verbatim under RAW 1443 EVENT / RAW 1444 EVENT.
import { spawn } from 'node:child_process';
import { ArenaClient, newIdentity, idPub, walletPub } from '../web/src/lib/arena.js';

const BASE = process.argv[2] || 'http://127.0.0.1:8080';
// The relay under test: the 4th argv slot (the acceptance script passes its
// $RELAY), the environment, or the script's default local port.
const RELAY = process.argv[3] || process.env.ACCEPTANCE_RELAY || 'ws://127.0.0.1:10547';
const NAK = process.env.NAK_BINARY || `${process.env.HOME || ''}/.local/bin/nak`;
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

// ── the receipts (handover §3, the wire contract) ───────────────────────────
// A receipt is read from the RELAY alone, exactly as the app would: nak prints
// the stored events as NDJSON, one per line. Every assertion below re-checks
// what ParseRefundEvent/ParseSettleEvent enforce on the chain side, plus the one
// thing only the relay can prove: that out[] IS the ecash /api/state committed.
const tagOf = (ev, name) => { const t = (ev.tags || []).find((x) => x[0] === name); return t ? t[1] : undefined; };

function nakReq(kind) {
  return new Promise((resolve, reject) => {
    // spawn, not execFile: nak reads a filter from STDIN when it is a pipe, so
    // an inherited open stdin makes it wait forever. stdin must be /dev/null.
    const p = spawn(NAK, ['req', '-k', String(kind), RELAY], { stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '', err = '', settled = false;
    const parse = () => {
      const evs = [];
      for (const line of out.split('\n')) {
        const s = line.trim();
        if (!s) continue;
        try { evs.push(JSON.parse(s)); } catch { /* a nak banner line, not an event */ }
      }
      return evs;
    };
    const timer = setTimeout(() => { p.kill('SIGKILL'); }, 15000);
    const finish = (why) => {
      if (settled) return;
      settled = true; clearTimeout(timer);
      if (why) reject(new Error(`nak req -k ${kind} ${why}: ${err.trim()}`));
      else resolve(parse());
    };
    p.stdout.on('data', (d) => { out += d; });
    p.stderr.on('data', (d) => { err += d; });
    p.on('error', (e) => finish(`could not run (${e.message})`));
    p.on('close', (code) => { if (parse().length || code === 0) finish(null); else finish(`exited ${code}`); });
  });
}

// The newest kind-1448 params event on the relay: the terms in force (hence the
// digest a receipt must carry) and the roster its signer must be on.
async function relayParams() {
  const evs = await nakReq(1448);
  if (!evs.length) throw new Error('no kind-1448 params event on the relay');
  evs.sort((a, b) => (b.created_at - a.created_at) || (a.id < b.id ? 1 : -1));
  const content = JSON.parse(evs[0].content);
  return {
    version: String(content.params_version),
    roster: new Set((content.validators || []).map((v) => String(v.pubkey).toLowerCase())),
  };
}

// checkReceipt asserts one receipt kind end to end. It waits for the relay to
// carry an event naming the bid, prints it verbatim under the RAW marker, then
// re-checks the contract: schema/ids/outcome, the anchor (content AND tags, the
// cross-check ParseVerdict enforces), the roster, the per-destination recipient,
// and that out[].secret/C equal the proofs /api/state committed for that bid.
// §3 allows several validators to publish their own receipt for one move, so
// every event naming the bid is admitted and the earliest-anchored one -- what a
// client keeps after the (bid_id, outcome) dedupe -- carries the deep checks.
async function checkReceipt(kind, schema, outcome, { coord, bidId, stateBid, recipients, params }) {
  const label = `kind ${kind} ${outcome}`;
  let matches;
  try {
    matches = await until(async () => {
      const m = (await nakReq(kind)).filter((ev) => tagOf(ev, 'bid') === bidId);
      return m.length ? m : null;
    }, 30000, `${label} naming the bid on ${RELAY}`);
  } catch {
    ok(false, `${label}: no receipt naming the bid on ${RELAY} within 30 s`);
    return;
  }
  const raw = matches.map((ev) => JSON.parse(ev.content));
  matches.sort((a, b) => (a.created_at - b.created_at) || (a.id < b.id ? -1 : 1));
  const ev = matches[0], c = JSON.parse(ev.content);

  console.log(`      RAW ${kind} EVENT ${ev.id}`);
  console.log(JSON.stringify(ev, null, 2));

  ok(matches.length >= 1, `${label}: a receipt names the bid on ${RELAY} (${matches.length} event${matches.length === 1 ? '' : 's'}, ${new Set(matches.map((m) => m.pubkey)).size} signer(s))`);
  ok(raw.every((x) => x.auction_id === coord && x.bid_id === bidId && x.outcome === outcome),
    `${label}: every event naming the bid agrees on auction/bid/outcome (the §3 dedupe rule)`);

  ok(c.schema === schema, `${label}: content schema == "${schema}" (got ${JSON.stringify(c.schema)})`);
  ok(c.auction_id === coord, `${label}: content auction_id == coord (${coord})`);
  ok(c.bid_id === bidId, `${label}: content bid_id == the bid`);
  ok(c.outcome === outcome, `${label}: content outcome == "${outcome}"`);
  ok(c.height === stateBid.settledHeight, `${label}: content height == bids.settledHeight (${stateBid.settledHeight})`);
  ok(/^[0-9a-fA-F]{64}$/.test(String(c.block_hash)), `${label}: block_hash is the committed 64-hex block hash`);
  if (!/^[0-9a-f]{64}$/.test(String(c.block_hash))) {
    // §3 writes "<64 lowercase hex>", but the chain carries the block hash
    // verbatim from CometBFT's HexBytes.String(), which is UPPERCASE -- the same
    // string the shipped kind-1445 verdicts carry in their "hash" tag. The
    // substantive checks are the tag/content agreement below and that the height
    // is the settle's; this line flags the wording mismatch, it does not fail.
    console.log(`      INFO ${label}: block_hash is UPPERCASE hex (${c.block_hash.slice(0, 8)}…), as CometBFT's HexBytes.String() and the live 1445 verdicts report it; §3 says "64 lowercase hex" -- a doc/wire wording mismatch, reported`);
  }
  ok(c.settled_at === stateBid.settledAt, `${label}: content settled_at == bids.settledAt (${stateBid.settledAt})`);
  ok(c.params_version === params.version, `${label}: params_version == the newest kind-1448 digest`);
  ok(Array.isArray(c.out) && c.out.length > 0, `${label}: out is non-empty (${(c.out || []).length})`);

  // the tag anchor must agree with the content, field for field
  ok(tagOf(ev, 'auction') === coord, `${label}: tag auction == coord`);
  ok(tagOf(ev, 'bid') === bidId, `${label}: tag bid == the bid`);
  ok(tagOf(ev, 'height') === String(c.height), `${label}: tag height == strconv(height)`);
  ok(tagOf(ev, 'hash') === c.block_hash, `${label}: tag hash == content block_hash`);

  // the signer must be on the roster the params event carries
  ok(params.roster.has(String(ev.pubkey).toLowerCase()),
    `${label}: signer ${String(ev.pubkey).slice(0, 12)}… is on the kind-1448 roster of ${params.roster.size}`);
  if (matches.length > 1) {
    console.log(`      INFO ${matches.length} receipts for this bid, one per publishing validator: ${matches.map((m) => m.pubkey.slice(0, 8)).join(', ')}`);
  }

  // the money: one output per committed destination, each locked to it, and the
  // exact proofs /api/state recorded for the bid
  const want = recipients.map((r) => r.toLowerCase());
  ok(c.out.every((o) => want.includes(String(o.recipient).toLowerCase())),
    `${label}: every out[].recipient == the committed recipient (${want.map((r) => r.slice(0, 10) + '…').join(', ')})`);
  const committed = Array.isArray(stateBid.out) ? stateBid.out : [];
  ok(c.out.length === committed.length && c.out.every((o, i) => o.secret === committed[i].secret && o.C === committed[i].C),
    `${label}: out[].secret/C match /api/state's bids.out entry for entry (${c.out.length})`);
}

const seller = newIdentity('Instant Refund Seller');
const alice = newIdentity('Instant Refund Alice');
const bob = newIdentity('Instant Refund Bob');
const carol = newIdentity('Instant Refund Carol');
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

// --- the kind-1443 refund receipt, read off the relay ---
console.log(`\n[§6] the refund receipt (kind 1443) on ${RELAY}`);
await checkReceipt(1443, 'arena.refund.v1', 'refund', {
  coord, bidId: A.id, stateBid: bid, recipients: [walletPub(alice)], params: await relayParams(),
});

// --- a second, short fixed lot: bid it, close it, read the kind-1444 settle ---
console.log('\n[§6] the settle receipt (kind 1444): a short fixed lot, closed and paid');
const db = 'instant-settle-' + Date.now();
const lotB = {
  title: 'Instant settle probe', description: 'probe', image: '', category: 'Art',
  startPrice: 100, minIncrement: 10,
  durationSec: 25, mode: 'fixed',
};
await c.createAuction(seller, lotB, db);
const coordB = coordOf(db);
const createdB = await attempt(until(async () => { const st = await c.state(); return st.auctions[coordB] && st; }, 15000, 'the second create to commit'), 'the second create to commit');
if (!createdB) { await finish(); }
ok(createdB.auctions[coordB].mode === 'fixed' && createdB.auctions[coordB].durationSec === 25,
  `second lot open: mode=${createdB.auctions[coordB].mode} durationSec=${createdB.auctions[coordB].durationSec}`);

await c.faucet(carol, 1000);
const CB = await attempt(placeBid(carol, coordB, createdB.auctions[coordB].startPrice), "carol's bid to commit");
if (!CB) { await finish(); }
ok(CB.state.auctions[coordB].highBid === CB.id, `carol leads the second lot at ${CB.state.bids[CB.id].amount} sats`);

// A fixed lot closes when a block lands after its end (the chain's closer does
// it); the settle then rides the next blocks. Both are given generous time: the
// empty-block interval can delay either.
const endB = CB.state.auctions[coordB].endAt;
const closedB = await attempt(until(async () => { const st = await c.state(); return st.auctions[coordB].status === 'closed' && st; }, 90000, 'the second lot to close'), 'the second lot to close');
if (!closedB) { await finish(); }
const tCloseB = Date.now();
ok(closedB.auctions[coordB].winner === CB.id, 'second lot closed with carol winning at 100');
console.log(`      INFO second lot closed ${(tCloseB / 1000 - endB).toFixed(1)} s after its end`);

const settledB = await attempt(until(async () => {
  const st = await c.state();
  const b = st.bids[CB.id];
  if (b && b.settled === 'settle' && b.out && b.out.length > 0) return { st, bid: b };
  return null;
}, 90000, 'the settle to be recorded'), 'the settle to be recorded');
if (!settledB) { await finish(); }
const settleMs = Date.now() - tCloseB;
console.log(`      INFO close -> settle recorded: ${settleMs} ms (${(settleMs / 1000).toFixed(1)} s)`);
console.log(`      INFO bids[carol].settled = settle; committed when: height ${settledB.bid.settledHeight}, at ${settledB.bid.settledAt}`);
ok(settleMs <= 90000, `the settle was recorded within 90 s of the close (measured ${settleMs} ms)`);
ok(settledB.bid.settledHeight > 0 && settledB.bid.settledAt > 0, `the settle carries a committed when: height ${settledB.bid.settledHeight}, at ${settledB.bid.settledAt}`);

console.log(`\n[§6] the settle receipt (kind 1444) on ${RELAY}`);
await checkReceipt(1444, 'arena.settle.v1', 'settle', {
  coord: coordB, bidId: CB.id, stateBid: settledB.bid, recipients: [walletPub(seller)], params: await relayParams(),
});

await finish();
