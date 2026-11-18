// Live probe for HANDOVER-2026-11-18-chain-state-half: the two new state events.
//
//   node tests/probe-state-half.mjs [http://127.0.0.1:8090] [wss://relay.playday.it]
//
// PART 1 — kind 1449 (the lot's committed end).  Create an anti-snipe lot whose
// window is almost the whole duration, wait until the lot is INSIDE its window,
// then bid.  That bid moves the end, so the validator that committed it must
// publish a 1449 while the lot is still OPEN.  The probe asserts, from the relay
// alone plus /api/state, that end_time > original_end_time, extensions >= 1,
// height == the bid's committed height, the tags agree with the content, the
// signer is on the roster, and the lot is still open at the moment of capture.
//
// PART 2 — kind 1450 (a refusal).  Publish a kind-1440 with a 200-character
// title.  The chain refuses it (title required (max 120 chars)); the refusal has
// to appear on the relay naming that event id with the chain's own reason,
// anchored to the height the rejection was recorded at.
//
// Both events are printed verbatim under RAW 1449 EVENT / RAW 1450 EVENT so the
// report can paste relay bytes, never a reconstruction.
//
// The probe leaves real state behind on whatever root it runs against: the lot
// it extends will close and settle, and the bidder's escrow moves to its wallet
// (which this process discards at exit).  Run it against a demo root unless the
// point is to prove the DEPLOYMENT's publisher; that is a deliberate cost.

import { spawn } from 'node:child_process';
import { ArenaClient, newIdentity, idPub } from '../web/src/lib/arena.js';

const BASE = process.argv[2] || 'http://127.0.0.1:8090';
const RELAY = process.argv[3] || process.env.ACCEPTANCE_RELAY || 'wss://relay.playday.it';
const NAK = process.env.NAK_BINARY || `${process.env.HOME || ''}/.local/bin/nak`;

const c = new ArenaClient(BASE);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let pass = 0, fail = 0;
const ok = (cond, msg) => {
  if (cond) { pass++; console.log('  PASS', msg); }
  else { fail++; console.log('  FAIL', msg); }
};
async function until(f, ms, label) {
  const end = Date.now() + ms;
  let last;
  while (Date.now() < end) {
    try { const v = await f(); if (v) return v; } catch (e) { last = e; }
    await sleep(1500);
  }
  throw new Error(`timed out waiting for ${label}${last ? ` (${last.message})` : ''}`);
}
const attempt = async (p, label) => {
  try { return await p; } catch (e) { ok(false, `${label} (${e.message})`); return null; }
};

const finish = async () => {
  await c.close().catch(() => {});
  console.log(`\nprobe-state-half: ${pass} passed, ${fail} failed`);
  process.exit(fail ? 1 : 0);
};

const tagOf = (ev, name) => {
  const t = (ev.tags || []).find((x) => x[0] === name);
  return t ? t[1] : undefined;
};

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
    const timer = setTimeout(() => { p.kill('SIGKILL'); }, 20000);
    const done = (why) => {
      if (settled) return;
      settled = true; clearTimeout(timer);
      if (why) reject(new Error(`nak req -k ${kind} ${why}: ${err.trim()}`));
      else resolve(parse());
    };
    p.stdout.on('data', (d) => { out += d; });
    p.stderr.on('data', (d) => { err += d; });
    p.on('error', (e) => done(`could not run (${e.message})`));
    p.on('close', (code) => { if (parse().length || code === 0) done(null); else done(`exited ${code}`); });
  });
}

// The newest kind-1448 params event on the relay: the terms in force and the
// roster a new event's signer must be on.
async function relayParams() {
  const evs = await nakReq(1448);
  if (!evs.length) throw new Error('no kind-1448 params event on the relay');
  evs.sort((a, b) => (b.created_at - a.created_at) || (a.id < b.id ? 1 : -1));
  const content = JSON.parse(evs[0].content);
  return {
    content,
    roster: new Set((content.validators || []).map((v) => String(v.pubkey).toLowerCase())),
  };
}

const state = () => c.state();

// PROBE_PART2_ONLY=1 skips the lot/bid half: re-running the refusal check must
// not strand another lot's escrow on the live root.
const PART2_ONLY = process.env.PROBE_PART2_ONLY === '1';

async function main() {
  const { content: params, roster } = await relayParams();
  ok(params.min_duration_sec > 0 && params.max_duration_sec >= params.min_duration_sec, 'params carry the duration bounds');

  // ── PART 1: kind 1449, the moved end ───────────────────────────────────────
  if (!PART2_ONLY) {
  const seller = newIdentity('lot1449-seller');
  const bidder = newIdentity('lot1449-bidder');

  // window almost the whole duration: the lot reaches its window quickly, so
  // the bid below lands inside it and MUST extend the end.
  const durationSec = Math.max(params.min_duration_sec + 10, 120);
  const windowSec = Math.min(params.max_window_sec, Math.max(params.min_window_sec, durationSec - 20));
  const d = 'lot1449-' + Date.now();
  const lot = {
    title: 'state-half 1449 probe', description: 'proves the moved end is published while open',
    image: '', category: 'misc', startPrice: 100, minIncrement: 10,
    durationSec, mode: 'antisnipe', windowSec,
  };
  const created = await attempt(c.createAuction(seller, lot, d), 'create the anti-snipe lot');
  if (!created) return finish();
  const coord = created.coord;
  console.log(`lot ${coord} (duration ${durationSec}s, window ${windowSec}s)`);

  const au0 = await attempt(until(async () => (await state()).auctions[coord], 30000, 'the lot to commit'),
    'the lot commits');
  if (!au0) return finish();
  const originalEnd = au0.createdAt + au0.durationSec;
  ok(au0.endAt === originalEnd && au0.extensions === 0, `fresh lot: endAt == original end ${originalEnd}`);

  // Wait until less than the window remains: any bid now extends the lot.
  await attempt(until(async () => {
    const s = await state();
    const au = s.auctions[coord];
    return au && au.endAt - (Date.now() / 1000) <= windowSec - 10 ? au : null;
  }, (durationSec + 60) * 1000, 'the lot to enter its anti-snipe window'), 'the lot enters its window');

  const st = await state();
  const auNow = st.auctions[coord];
  await attempt(c.faucet(bidder, 200), 'fund the bidder');
  const locked = await attempt(c.lockForBid(bidder, 100, st.params, auNow, { state: st }), 'lock the bid escrow');
  if (!locked) return finish();
  const bidRes = await attempt(c.bid(bidder, auNow, 100, locked.locked), 'submit the extending bid');
  if (!bidRes) return finish();

  const committedBid = await attempt(until(async () => (await state()).bids[bidRes.id], 30000, 'the bid to commit'),
    'the bid commits');
  if (!committedBid) return finish();
  ok(committedBid.extended === true, 'the bid committed as EXTENDING (bid.extended == true)');
  ok(committedBid.height > 0, `the bid has a committed height (${committedBid.height})`);

  const ev449 = await attempt(until(async () => {
    const evs = (await nakReq(1449)).filter((ev) => tagOf(ev, 'auction') === coord);
    return evs.length ? evs : null;
  }, 90000, 'a kind-1449 for the lot'), 'kind 1449 appears');
  if (!ev449) return finish();

  // Capture the lot's status at the moment the event is observable.
  const live = await state();
  const auLive = live.auctions[coord];
  ok(auLive && auLive.status === 'open', 'the lot is STILL OPEN when the 1449 is readable');

  ev449.sort((a, b) => (Number(tagOf(a, 'height') || 0) - Number(tagOf(b, 'height') || 0)));
  const ev = ev449[0];
  const ct = JSON.parse(ev.content);
  console.log(`\n      RAW 1449 EVENT ${ev.id}`);
  console.log(JSON.stringify(ev, null, 2));

  ok(ev.kind === 1449, 'kind 1449');
  ok(ct.schema === 'arena.lot.v1', `schema arena.lot.v1 (${ct.schema})`);
  ok(ct.auction_id === coord, 'content names the lot');
  ok(ct.end_time > ct.original_end_time, `end_time ${ct.end_time} > original ${ct.original_end_time}`);
  ok(ct.end_time > originalEnd, `end_time is past the ORIGINAL end ${originalEnd}`);
  ok(ct.extensions >= 1, `extensions >= 1 (${ct.extensions})`);
  ok(ct.height === committedBid.height, `anchored at the bid's committed height (${ct.height})`);
  ok(tagOf(ev, 'height') === String(ct.height), 'height tag agrees with content');
  ok(tagOf(ev, 'hash') === ct.block_hash && ct.block_hash !== '', 'hash tag agrees with content');
  ok(roster.has(ev.pubkey.toLowerCase()), 'signer is on the roster');
  ok(tagOf(ev, 'p') === ev.pubkey, 'p tag names the signer');
  } else {
    console.log('  (part 1 skipped: PROBE_PART2_ONLY)');
  }

  // ── PART 2: kind 1450, a refusal ───────────────────────────────────────────
  const throwaway = newIdentity('bad-create');
  const badD = 'bad1440-' + Date.now();
  const badContent = {
    title: 'x'.repeat(200), // the chain's limit is 120
    description: '', image: '', category: 'misc', name: 'refused create',
    startPrice: 100, minIncrement: 10, durationSec: Math.max(params.min_duration_sec, 60),
    mode: 'fixed', windowSec: 0,
  };
  const badEv = await c.sign(throwaway, 1440, badContent, [['d', badD]]);
  // Two answers are legitimate at the SUBMIT boundary, and only one of them can
  // help a relay-only client: a mempool refusal never reaches a block, so the
  // chain cannot record it and cannot publish a 1450 for it. The chain therefore
  // admits the create and decides at delivery. Both paths are recorded here;
  // what the probe insists on below is the 1450 and the committed rejection.
  try {
    await c.submit(badEv);
    ok(true, 'the chain admitted the create so the refusal can be recorded at delivery');
  } catch (e) {
    ok(/title required \(max 120 chars\)/.test(e.message),
      `the chain answered at admission with the reason: "${e.message}"`);
  }

  const ev450 = await attempt(until(async () => {
    const evs = (await nakReq(1450)).filter((ev) => tagOf(ev, 'e') === badEv.id);
    return evs.length ? evs : null;
  }, 90000, 'a kind-1450 for the refused create'), 'kind 1450 appears');
  if (!ev450) return finish();

  ev450.sort((a, b) => (Number(tagOf(a, 'height') || 0) - Number(tagOf(b, 'height') || 0)));
  const rfe = ev450[0];
  const rct = JSON.parse(rfe.content);
  console.log(`\n      RAW 1450 EVENT ${rfe.id}`);
  console.log(JSON.stringify(rfe, null, 2));

  ok(rfe.kind === 1450, 'kind 1450');
  ok(rct.schema === 'arena.refused.v1', `schema arena.refused.v1 (${rct.schema})`);
  ok(rct.event_id === badEv.id, 'content names the refused event id');
  ok(rct.kind === 1440, `content records the refused kind (${rct.kind})`);
  ok(rct.reason === 'title required (max 120 chars)', `reason is the chain's own words: "${rct.reason}"`);
  ok(tagOf(rfe, 'e') === rct.event_id, 'e tag agrees with content');
  ok(tagOf(rfe, 'height') === String(rct.height), 'height tag agrees with content');
  ok(tagOf(rfe, 'hash') === rct.block_hash && rct.block_hash !== '', 'hash tag agrees with content');
  ok(roster.has(rfe.pubkey.toLowerCase()), 'signer is on the roster');
  ok(tagOf(rfe, 'p') === rfe.pubkey, 'p tag names the signer');

  // The refusal is a fact about committed state: the chain recorded it at that
  // height for that event.
  const after = await state();
  const rec = (after.rejections || []).find((r) => r.event === badEv.id);
  ok(rec && rec.reason === rct.reason, 'committed state records the same event id and reason');
  ok(rec && rec.height === rct.height, `the anchor height is the rejection's height (${rct.height})`);
  ok(!after.auctions[`1440:${idPub(throwaway)}:${badD}`], 'the refused create left NO auction on the chain');
  ok(rfe.created_at >= Date.now() / 1000 - 3600, 'published recently, not a historical event');
  await finish();
}

main().catch((e) => { ok(false, `probe crashed: ${e.message}`); finish(); });