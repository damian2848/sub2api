import test from 'node:test';
import assert from 'node:assert/strict';
import { getEventListeners } from 'node:events';
import { NativeStartLimiter } from '../src/start-limit.mjs';
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
  const limiter = new NativeStartLimiter({ limit: 2, windowMs: 20, ...options,
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

test('concurrent starts obey the window and waiting requests receive permits in FIFO order', async t => {
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
  assert.deepEqual(granted.map(item => item.index), [0, 1, 2, 3]);
  assert.equal(scheduler.pending, 1);
  scheduler.advance(20);
  assert.deepEqual(await Promise.all(permits), [0, 0, 20, 20, 40, 40]);
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

test('each timestamp expires independently and an idle expired window reopens immediately', async t => {
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
  scheduler.advance(4);
  assert.equal(await fourth, 4);
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
