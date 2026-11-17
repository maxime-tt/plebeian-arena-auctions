// The locktime a bid carries. The chain derives its floor from the time
// of the BLOCK that admits the bid, not from the bidder's clock, and
// bid_to_consensus_latency is the allowance for the gap between the two. An
// anti-snipe bid placed inside the window used to lock at client_now + window +
// safety + latency -- always inside the floor the admitting block computes, so
// the bid was admitted by CheckTx and then rejected in the transition, never
// appearing in the state. Runs offline: node test/bid-locktime.mjs
import { minBidLocktime } from '../src/lib/arena.js';
let pass = 0, fail = 0;
const ok = (c, m) => { if (c) { pass++; console.log('ok  ', m); } else { fail++; console.log('FAIL', m); } };

const params = { settlementSafetySec: 900, bidToConsensusLatencySec: 15 };
const window = 60;
// the numbers measured live: the bidder's clock said 1794931235, the block that
// admitted the bid said 1794931238 (now + 3), and the chain's floor was 1794932213.
const now = 1794931235;
const block = 1794931238;
// the floor the chain computes for a block at time b when this bid is what sets
// the end: max(establishedEnd, b + window) + safety + latency, and for an
// anti-snipe bid inside the window establishedEnd is b + window.
const floor = (b) => b + window + params.settlementSafetySec + params.bidToConsensusLatencySec;
// a lot whose end is already inside the window: this bid resets the clock
const snipe = { mode: 'antisnipe', endAt: now - 5, windowSec: window };

{
  // 1. the assertion that fails on the old formula: whatever block in
  //    [now, now + latency] admits the bid, the lock clears that block's floor.
  const lock = minBidLocktime(snipe, params, now);
  let worst = Infinity;
  for (let b = now; b <= now + params.bidToConsensusLatencySec; b++) worst = Math.min(worst, lock - floor(b));
  ok(worst >= 0, `an in-window anti-snipe lock clears the chain floor for any admitting block up to now+latency (worst margin ${worst}s)`);
  ok(lock >= floor(block), `the lock clears the floor of the measured run's block (now + 3s): ${lock} >= ${floor(block)}`);
}
{
  // 2. a fixed lot carries no block-time term: unchanged, both directions.
  const future = { mode: 'fixed', endAt: now + 600 }, past = { mode: 'fixed', endAt: now - 600 };
  const expectFuture = Math.ceil(Math.max(now + 600, now) + params.settlementSafetySec + params.bidToConsensusLatencySec);
  const expectPast = Math.ceil(Math.max(now - 600, now) + params.settlementSafetySec + params.bidToConsensusLatencySec);
  ok(minBidLocktime(future, params, now) === expectFuture, `a fixed lot ending later locks at end + safety + latency (${expectFuture})`);
  ok(minBidLocktime(past, params, now) === expectPast, `a fixed lot whose end has passed locks at now + safety + latency (${expectPast})`);
}
{
  // 3. an anti-snipe lot whose end is beyond window + latency: no over-lock.
  const far = { mode: 'antisnipe', endAt: now + window + params.bidToConsensusLatencySec + 3600, windowSec: window };
  const expect = Math.ceil(far.endAt + params.settlementSafetySec + params.bidToConsensusLatencySec);
  ok(minBidLocktime(far, params, now) === expect, `an anti-snipe lot with a far end locks at end + safety + latency (${expect})`);
}
{
  // 4. an anti-snipe bid inside the window, with the block estimated as now + latency.
  const expect = Math.ceil(now + params.bidToConsensusLatencySec + window + params.settlementSafetySec + params.bidToConsensusLatencySec);
  ok(minBidLocktime(snipe, params, now) === expect, `an in-window anti-snipe bid locks at now + latency + window + safety + latency (${expect})`);
}
{
  // 5. params defaulting, mirroring the ?? in the function.
  const expect = Math.ceil(now + 15 + window + 900 + 15);
  ok(minBidLocktime(snipe, {}, now) === expect, `missing params default to safety 900 / latency 15 (${expect})`);
  const fixed = { mode: 'fixed', endAt: now + 600 };
  ok(minBidLocktime(fixed, { settlementSafetySec: 900 }, now) === Math.ceil(now + 600 + 900 + 15), 'a missing latency alone still defaults to 15');
}
console.log(`${pass}/${pass + fail}`); process.exit(fail ? 1 : 0);
