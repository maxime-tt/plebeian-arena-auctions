// Live probes for HANDOVER-2026-11-17-validator-workstreams, workstreams B and C.
//
//   probe 1 (C, §3): a tx handed to /api/tx commits within the documented bound.
//   probe 2 (B, §2): an event published to the relay while every validator is
//                    down is committed after the restart, with no client retry.
//
// The chain must already be up with -demo-controls (the suite's own flag).
// Usage: node tests/probe-restart-ingest.mjs [http://127.0.0.1:8080]
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

const seller = newIdentity('Restart Seller');
const lot = (title) => ({ title, description: 'probe', image: '', category: 'Art', startPrice: 100, minIncrement: 10, durationSec: 3600, mode: 'fixed' });
const upCount = async () => (await c.network()).filter((v) => v.up).length;
const stopAll = async () => { for (let i = 1; i <= 4; i++) await c.j(`/api/admin/validator/${i}/stop`, { method: 'POST' }); };
const startAll = async () => { for (let i = 1; i <= 4; i++) await c.j(`/api/admin/validator/${i}/start`, { method: 'POST' }); };
const coordOf = (d) => `1440:${idPub(seller)}:${d}`;
const committedAt = async (coord) => { const st = await c.state(); return st.auctions[coord] ? Date.now() : null; };

console.log('\n[probe 1] a tx commits within the documented bound (handover §3)');
ok(await upCount() === 4, 'all four validators up');
const d1 = 'latency-' + Date.now();
const t0 = Date.now();
await c.createAuction(seller, lot('latency probe'), d1);
const t1 = await until(() => committedAt(coordOf(d1)), 15000, 'the create to commit');
console.log(`      /api/tx -> committed block in ${t1 - t0} ms`);
ok(t1 - t0 <= 2000, `a tx in a mempool commits within the 2 s bound (measured ${t1 - t0} ms)`);

console.log('\n[probe 2] events published while the chain is down are ingested on restart (handover §2)');
await stopAll();
await until(async () => (await upCount()) === 0, 30000, 'all validators down');
let chainDown = false;
try { await c.state(); } catch (e) { chainDown = e.status === 503; }
ok(chainDown, 'the chain is down: /api/state answers 503');
const d2 = 'restart-' + Date.now();
let refused = false;
try { await c.createAuction(seller, lot('restart probe'), d2); } catch (e) { refused = true; console.log(`      /api/tx answered ${e.status}: ${e.message}`); }
ok(refused, '/api/tx does NOT claim ok when no validator can accept the tx (the relay still has it)');
await startAll();
await until(async () => (await upCount()) === 4, 60000, 'all validators up again');
const t2 = await until(() => committedAt(coordOf(d2)), 60000, 'the missed event to be re-ingested');
ok(t2 > 0, 'an event published while the chain was down IS committed after the restart, with no client retry');

console.log(`\nprobe: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
