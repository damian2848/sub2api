import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, readdir, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { TurnJournal } from '../src/turn-journal.mjs';
import { PrismError } from '../src/errors.mjs';

async function directory(t) {
  const value = await mkdtemp(join(tmpdir(), 'prism-turn-journal-'));
  t.after(() => rm(value, { recursive: true, force: true }));
  return value;
}

test('persists submitted turns without writing prompt or tool arguments and blocks after restart', async t => {
  const dataDir = await directory(t);
  const journal = new TurnJournal({ dataDir });
  await journal.init();
  const turn = await journal.begin('32', { model: 'gpt-6.1-sol', effort: 'low',
    input: [{ role: 'user', content: [{ type: 'input_text', text: 'secret prompt' }] }],
    tools: [{ name: 'exec_command', parameters: { secret: 'tool argument' } }] });
  await journal.submitted(turn, { request_id: 'req-1', conversation_id: 'conv-1' });
  const file = (await readdir(join(dataDir, 'turns', '32'))).find(value => value.endsWith('.json'));
  const text = await readFile(join(dataDir, 'turns', '32', file), 'utf8');
  assert.doesNotMatch(text, /secret prompt|tool argument/);
  assert.equal((await stat(join(dataDir, 'turns', '32', file))).mode & 0o777, 0o600);

  const restarted = new TurnJournal({ dataDir });
  await restarted.init();
  assert.equal(restarted.list('32')[0].state, 'submitted');
  await assert.rejects(restarted.begin('32', { model: 'gpt-6.1-sol' }), error =>
    error instanceof PrismError && error.code === 'pending_turn_reconciliation_required' && error.status === 409);
  await restarted.resolve('32', turn.id);
  assert.deepEqual(restarted.list('32'), []);
});

test('known pre-submit failures are cleared and successful turns remove their journal', async t => {
  const dataDir = await directory(t);
  const journal = new TurnJournal({ dataDir });
  await journal.init();
  const refused = await journal.begin('32', { model: 'gpt-6.1-sol' });
  await journal.fail(refused, new PrismError('prism_start_rejected', 429));
  assert.deepEqual(journal.list('32'), []);
  const completed = await journal.begin('32', { model: 'gpt-6.1-sol' });
  await journal.complete(completed);
  assert.deepEqual(journal.list('32'), []);
});

test('an unknown result blocks later turns in the same process and active records cannot be cleared', async t => {
  const dataDir = await directory(t);
  const journal = new TurnJournal({ dataDir });
  await journal.init();
  const turn = await journal.begin('32', { model: 'gpt-6.1-sol' });
  await journal.submitted(turn, { request_id: 'req-2' });
  await journal.fail(turn, new PrismError('request_timeout', 504));
  await assert.rejects(journal.begin('32', { model: 'gpt-6.1-sol' }), error =>
    error.code === 'pending_turn_reconciliation_required' && error.status === 409);

  const active = await journal.begin('33', { model: 'gpt-6.1-sol' });
  await assert.rejects(journal.resolve('33', active.id), error =>
    error.code === 'pending_turn_active' && error.status === 409);
  await journal.clear(active);
});
