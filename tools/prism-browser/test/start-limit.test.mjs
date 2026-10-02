import test from 'node:test';
import assert from 'node:assert/strict';
import { getEventListeners } from 'node:events';
import { NativeStartLimiter, nativeStartSettings } from '../src/start-limit.mjs';
import { PrismError } from '../src/errors.mjs';

function clock() {
  let current = 0;
  let sequence = 0;
  const timers = new Map();
  const result = { maxTimers: 0,
    now: () => current,
    setTimer(work, delay) {
      const id = ++sequence;
      timers.set(id, { work, at: current + delay });
      result.maxTimers = Math.max(result.maxTimers, timers.size);
      return id;
    },
    clearTimer(id) { timers.delete(id); },
    get pending() { return timers.size; },
    advance(ms) {
      const target = current + ms;
      while (true) {
        const next = [...timers].sort((a, b) => a[1].at - b[1].at || a[0] - b[0])[0];
        if (!next || next[1].at > target) break;
        current = next[1].at;
        timers.delete(next[0]);
        next[1].work();
      }
      current = target;
    },
    fireEarly() {
      const next = [...timers][0];
      assert.ok(next);
      timers.delete(next[0]);
      next[1].work();
    },
  };
  return result;
}

function fixture(t, options = {}) {
  const scheduler = clock();
  const limiter = new NativeStartLimiter({ limit: 2, windowMs: 20, refillMs: 20, ...options,
    now: scheduler.now, setTimer: scheduler.setTimer, clearTimer: scheduler.clearTimer });
  t.after(() => limiter.close());
  return { limiter, scheduler };
}

test('native start settings validate bounded integers and injectable clock functions', () => {
  for (const limit of [-1, 1.5, 121, NaN, '4']) {
    assert.throws(() => new NativeStartLimiter({ limit }), /invalid_native_start_limit/);
  }
  for (const windowMs of [0, -1, 1.5, 3600001, Infinity, '65000']) {
    assert.throws(() => new NativeStartLimiter({ windowMs }), /invalid_native_start_window/);
  }
  for (const property of ['now', 'setTimer', 'clearTimer']) {
    assert.throws(() => new NativeStartLimiter({ [property]: null }), /invalid_native_start_clock/);
  }
  new NativeStartLimiter({ limit: 120, windowMs: 3600000 }).close();
  new NativeStartLimiter({ limit: 0, windowMs: 1 }).close();
});

test('disabled limiter permits concurrent starts immediately and respects cancellation and close', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 0 });
  assert.deepEqual(await Promise.all(Array.from({ length: 200 }, () => limiter.acquire())), Array(200).fill(0));
  assert.equal(scheduler.pending, 0);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(limiter.acquire(controller.signal), error => error.code === 'request_cancelled');
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  limiter.close();
  await assert.rejects(limiter.acquire(), error => error.code === 'service_stopping' && error.status === 503);
});

test('a token bucket bursts to capacity then replenishes one permit per interval in FIFO order', async t => {
  const { limiter, scheduler } = fixture(t);
  const granted = [];
  const permits = Array.from({ length: 6 }, (_, index) => limiter.acquire().then(wait => {
    granted.push({ index, wait, at: scheduler.now() });
    return wait;
  }));
  await Promise.resolve();
  assert.deepEqual(granted.map(item => item.index), [0, 1]);
  assert.equal(scheduler.pending, 1);
  scheduler.advance(19);
  await Promise.resolve();
  assert.equal(granted.length, 2);
  scheduler.advance(1);
  await Promise.resolve();
  assert.deepEqual(granted.map(item => item.index), [0, 1, 2]);
  assert.equal(scheduler.pending, 1);
  scheduler.advance(60);
  assert.deepEqual(await Promise.all(permits), [0, 0, 20, 40, 60, 80]);
  assert.deepEqual(granted.map(item => item.index), [0, 1, 2, 3, 4, 5]);
  assert.equal(scheduler.pending, 0);
  assert.equal(scheduler.maxTimers, 1);
});

test('aborted waiters consume no permit and remove their listeners without delaying later work', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 1 });
  assert.equal(await limiter.acquire(), 0);
  scheduler.advance(5);
  const cancelled = new AbortController();
  const pending = limiter.acquire(cancelled.signal);
  const rejected = assert.rejects(pending, error => error.code === 'request_timeout' && error.status === 504);
  assert.equal(getEventListeners(cancelled.signal, 'abort').length, 1);
  scheduler.advance(2);
  const retained = new AbortController();
  const next = limiter.acquire(retained.signal);
  cancelled.abort(new PrismError('request_timeout', 504));
  await rejected;
  assert.equal(getEventListeners(cancelled.signal, 'abort').length, 0);
  assert.equal(scheduler.pending, 1);
  scheduler.advance(13);
  assert.equal(await next, 13);
  assert.equal(getEventListeners(retained.signal, 'abort').length, 0);
  retained.abort();
  const later = limiter.acquire();
  scheduler.advance(20);
  assert.equal(await later, 20, 'cancelling a granted request must not refund its start');
  assert.equal(scheduler.maxTimers, 1);
});

test('an already aborted signal is rejected as a Promise without consuming a start', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 1 });
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(limiter.acquire(controller.signal), error => error.code === 'request_cancelled');
  assert.equal(await limiter.acquire(), 0);
  assert.equal(scheduler.pending, 0);
});

test('revoking pending requests retains granted timestamps for the next session', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 1 });
  await limiter.acquire();
  scheduler.advance(5);
  const controller = new AbortController();
  const waiting = limiter.acquire(controller.signal);
  const rejected = assert.rejects(waiting, error => error.code === 'session_revoked' && error.status === 409);
  limiter.cancelPending();
  await rejected;
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  assert.equal(scheduler.pending, 0);
  scheduler.advance(5);
  const replacement = limiter.acquire();
  scheduler.advance(9);
  let granted = false;
  replacement.then(() => { granted = true; });
  await Promise.resolve();
  assert.equal(granted, false);
  scheduler.advance(1);
  assert.equal(await replacement, 10);
});

test('cancelPending propagates a supplied error and cancels its sole timer', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 1 });
  await limiter.acquire();
  const error = new PrismError('account_not_ready', 503);
  const rejected = assert.rejects(limiter.acquire(), actual => actual === error);
  limiter.cancelPending(error);
  await rejected;
  assert.equal(scheduler.pending, 0);
});

test('close rejects all pending and future starts and removes timers and abort listeners', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 1 });
  await limiter.acquire();
  const controllers = [new AbortController(), new AbortController()];
  const rejected = controllers.map(controller => assert.rejects(limiter.acquire(controller.signal),
    error => error.code === 'service_stopping' && error.status === 503));
  assert.equal(scheduler.pending, 1);
  limiter.close();
  limiter.close();
  await Promise.all(rejected);
  assert.equal(scheduler.pending, 0);
  for (const controller of controllers) assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  await assert.rejects(limiter.acquire(), error => error.code === 'service_stopping' && error.status === 503);
  scheduler.advance(100);
  assert.equal(scheduler.pending, 0);
});

test('partial refills are preserved and an idle bucket refills only to capacity', async t => {
  const { limiter, scheduler } = fixture(t);
  await limiter.acquire();
  scheduler.advance(5);
  await limiter.acquire();
  scheduler.advance(1);
  const third = limiter.acquire();
  scheduler.advance(14);
  assert.equal(await third, 14);
  scheduler.advance(1);
  const fourth = limiter.acquire();
  scheduler.advance(19);
  assert.equal(await fourth, 19);
  scheduler.advance(100);
  assert.equal(await limiter.acquire(), 0);
  assert.equal(await limiter.acquire(), 0);
  assert.equal(scheduler.pending, 0);
  assert.equal(scheduler.maxTimers, 1);
});

test('an early timer wake cannot grant a start before its window expires', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 1 });
  await limiter.acquire();
  let granted = false;
  const waiting = limiter.acquire().then(wait => { granted = true; return wait; });
  scheduler.advance(5);
  scheduler.fireEarly();
  await Promise.resolve();
  assert.equal(granted, false);
  assert.equal(scheduler.pending, 1);
  scheduler.advance(15);
  assert.equal(await waiting, 20);
  assert.equal(scheduler.maxTimers, 1);
});

test('a cooldown refuses waiting and new starts at once with the time left, then lets starts through again', async () => {
  const time = clock();
  const limiter = new NativeStartLimiter({ limit: 1, windowMs: 65000, maxWaitMs: 120000, ...time });
  assert.equal(await limiter.acquire(), 0);
  const waiting = limiter.acquire();
  limiter.cooldown(60000);
  await assert.rejects(waiting, error => error.code === 'prism_start_limited' && error.status === 429 &&
    error.retryAfterSeconds === 65);
  time.advance(20500);
  limiter.maxWaitMs = 15000;
  await assert.rejects(limiter.acquire(), error => error.code === 'prism_start_limited' && error.retryAfterSeconds === 45);
  assert.equal(limiter.cooling(), 39500);
  limiter.cooldown(1000); // a shorter cooldown never shortens a running one
  assert.equal(limiter.cooling(), 39500);
  time.advance(65000);
  assert.equal(limiter.cooling(), 0);
  assert.equal(await limiter.acquire(), 0);
  // Cooldowns also apply when no start limit is configured.
  const unlimited = new NativeStartLimiter({ limit: 0, maxWaitMs: 0, ...clock() });
  unlimited.cooldown(5000);
  await assert.rejects(unlimited.acquire(), error => error.code === 'prism_start_limited');
  unlimited.cooldown(0);
  unlimited.cooldown(-1);
});

test('legacy settings derive a four-token burst and a 16.25 second refill', () => {
  const options = nativeStartSettings({ PRISM_ACCOUNT_START_LIMIT: '4', PRISM_START_WINDOW_SECONDS: '65' });
  assert.equal(options.burst, 4);
  assert.equal(options.refillMs, 16250);
  assert.equal(options.refillMinMs, 16250);
  assert.equal(options.refillMaxMs, 90000);
  assert.equal(options.maxWaitMs, 16250);
  assert.equal(nativeStartSettings({ PRISM_ACCOUNT_START_LIMIT: '0' }).burst, 0);
  const explicit = nativeStartSettings({ PRISM_ACCOUNT_START_LIMIT: '4', PRISM_START_BURST: '3',
    PRISM_START_REFILL_SECONDS: '65', PRISM_START_REFILL_MIN_SECONDS: '30' });
  assert.equal(explicit.burst, 3);
  assert.equal(explicit.refillMs, 65000);
  assert.throws(() => nativeStartSettings({ PRISM_START_REFILL_SECONDS: 'bad' }), /invalid PRISM_START_REFILL_SECONDS/);
  assert.throws(() => new NativeStartLimiter({ burst: 1, refillMs: 10, refillMinMs: 20 }), /invalid_native_start_refill/);
});

test('rejections clear tokens and increase the refill interval while four accepted starts reduce it', async t => {
  const events = [];
  const { limiter, scheduler } = fixture(t, { refillMs: 40, refillMinMs: 20, refillMaxMs: 80,
    onAudit: (event, fields) => events.push({ event, ...fields }) });
  limiter.accepted();
  limiter.rejected(60);
  assert.deepEqual(limiter.status(), { tokens: 0, refill_seconds: 0.06, cooling_ms: 60 });
  limiter.rejected(60);
  assert.equal(limiter.refillMs, 80);
  limiter.rejected(60);
  assert.equal(limiter.refillMs, 80);
  for (let count = 0; count < 3; count += 1) limiter.accepted();
  assert.equal(limiter.refillMs, 80);
  limiter.accepted();
  assert.equal(limiter.refillMs, 72);
  for (let count = 0; count < 100; count += 1) limiter.accepted();
  assert.equal(limiter.refillMs, 20);
  scheduler.advance(200);
  assert.equal(limiter.status().tokens, 2);
  assert.equal(events.filter(item => item.event === 'start_bucket_rejected').length, 3);
  assert.equal(events[0].event, 'start_bucket_adjusted');
});

test('unset or blank max wait covers one initial refill while explicit overrides remain authoritative', () => {
  for (const refillSeconds of [1, 15, 16.25, 60, 90]) {
    const env = { PRISM_START_BURST: '1', PRISM_START_REFILL_SECONDS: String(refillSeconds) };
    for (const value of [undefined, '']) {
      assert.equal(nativeStartSettings({ ...env, PRISM_START_MAX_WAIT_SECONDS: value }).maxWaitMs,
        Math.max(15000, refillSeconds * 1000));
    }
    assert.equal(nativeStartSettings({ ...env, PRISM_START_MAX_WAIT_SECONDS: '7' }).maxWaitMs, 7000);
    assert.equal(nativeStartSettings({ ...env, PRISM_START_MAX_WAIT_SECONDS: '0' }).maxWaitMs, 0);
  }
});

test('the default wait admits the first available request after the bucket empties', async t => {
  for (const settings of [nativeStartSettings({ PRISM_ACCOUNT_START_LIMIT: '4', PRISM_START_WINDOW_SECONDS: '65' }),
    { burst: 1, refillMs: 60000 }]) {
    const { limiter, scheduler } = fixture(t, settings);
    assert.equal(limiter.maxWaitMs, Math.max(15000, limiter.refillMs));
    for (let count = 0; count < limiter.burst; count += 1) await limiter.acquire();
    const waiting = limiter.acquire(undefined, { failover: 'available' });
    assert.equal(limiter.jobs.length, 1);
    scheduler.advance(limiter.refillMs);
    assert.equal(await waiting, limiter.refillMs);
  }
});

test('unset or blank refill maximum admits legacy intervals above ninety seconds', () => {
  for (const windowSeconds of [65, 120, 3600]) {
    for (const value of [undefined, '']) {
      const settings = nativeStartSettings({ PRISM_ACCOUNT_START_LIMIT: '1',
        PRISM_START_WINDOW_SECONDS: String(windowSeconds), PRISM_START_REFILL_MAX_SECONDS: value });
      assert.equal(settings.refillMs, windowSeconds * 1000);
      assert.equal(settings.refillMaxMs, Math.max(90000, settings.refillMs));
      const limiter = new NativeStartLimiter(settings);
      limiter.rejected(0);
      assert.equal(limiter.refillMs, Math.min(settings.refillMaxMs, settings.refillMs * 1.5));
      limiter.close();
    }
    const direct = new NativeStartLimiter({ limit: 1, windowMs: windowSeconds * 1000 });
    assert.equal(direct.refillMaxMs, Math.max(90000, direct.refillMs));
    direct.close();
  }
});

test('explicit refill maximum overrides the derived default and still validates interval bounds', () => {
  const env = { PRISM_START_BURST: '1', PRISM_START_REFILL_SECONDS: '120' };
  assert.equal(nativeStartSettings({ ...env, PRISM_START_REFILL_MAX_SECONDS: '180' }).refillMaxMs, 180000);
  assert.equal(nativeStartSettings({ ...env, PRISM_START_REFILL_MAX_SECONDS: '120' }).refillMaxMs, 120000);
  assert.throws(() => nativeStartSettings({ ...env, PRISM_START_REFILL_MAX_SECONDS: '90' }), /invalid_native_start_refill/);
  assert.throws(() => nativeStartSettings({ ...env, PRISM_START_REFILL_MAX_SECONDS: '0' }), /invalid PRISM_START_REFILL_MAX_SECONDS/);
});

test('available fails fast with the exact wait while none queues until a token exists', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 1, refillMs: 16250, maxWaitMs: 15000 });
  await limiter.acquire();
  await assert.rejects(limiter.acquire(), error => error.status === 429 && error.retryAfterSeconds === 17);
  const pending = limiter.acquire(undefined, { failover: 'none' });
  scheduler.advance(16249);
  assert.equal(limiter.jobs.length, 1);
  scheduler.advance(1);
  assert.equal(await pending, 16250);
});

test('cooldown cancels available waiters but none waits for both cooldown and a token', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 1, refillMs: 20 });
  await limiter.acquire();
  const controller = new AbortController();
  const rejected = assert.rejects(limiter.acquire(controller.signal), error => error.retryAfterSeconds === 1);
  const retained = limiter.acquire(undefined, { failover: 'none' });
  limiter.rejected(40);
  await rejected;
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  scheduler.advance(39);
  assert.equal(limiter.jobs.length, 1);
  scheduler.advance(1);
  assert.equal(await retained, 40);
});

test('unlimited buckets still queue none through cooldown and cancellation', async t => {
  const { limiter, scheduler } = fixture(t, { limit: 0, maxWaitMs: 0 });
  limiter.rejected(50);
  const pending = limiter.acquire(undefined, { failover: 'none' });
  await assert.rejects(limiter.acquire(), error => error.retryAfterSeconds === 1);
  scheduler.advance(50);
  assert.equal(await pending, 50);
});
