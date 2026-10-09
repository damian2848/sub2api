import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { openJournal } from './openai_totp_journal.mjs'

function setup(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'totp-journal-test-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  return root
}
test('encrypted candidate survives restart and never appears in plaintext files', t => {
  const root = setup(t)
  const record = { task_id: 7, candidate: 'SYNTHETIC-SECRET', previous: 'OLD-SECRET', password: 'fake-password' }
  openJournal(root).write(record)
  assert.deepEqual(openJournal(root).read(7), record)
  assert.equal(fs.statSync(path.join(root, '7.sealed')).mode & 0o777, 0o600)
  for (const name of fs.readdirSync(root)) {
    const text = fs.readFileSync(path.join(root, name)).toString()
    assert.ok(!text.includes(record.candidate) && !text.includes(record.password))
  }
})
test('an existing candidate cannot be overwritten by a retry', t => {
  const journal = openJournal(setup(t))
  journal.write({ task_id: 7, candidate: 'NEW' })
  assert.throws(() => journal.write({ task_id: 7, candidate: 'OTHER' }), /overwrite/)
  assert.equal(journal.read(7).candidate, 'NEW')
})
test('missing key is never regenerated over existing credentials', t => {
  const root = setup(t)
  openJournal(root).write({ task_id: 7, candidate: 'NEW' })
  fs.unlinkSync(path.join(root, 'key.bin'))
  assert.throws(() => openJournal(root), /key_missing/)
  assert.equal(fs.existsSync(path.join(root, 'key.bin')), false)
})
test('disk corruption fails authentication without returning guessed credentials', t => {
  const root = setup(t)
  openJournal(root).write({ task_id: 7, candidate: 'NEW' })
  const file = path.join(root, '7.sealed'), raw = fs.readFileSync(file)
  raw[raw.length-1] ^= 1; fs.writeFileSync(file, raw)
  assert.throws(() => openJournal(root).read(7))
})
test('unsafe directory permissions fail before writing credentials', t => {
  const root = setup(t); fs.chmodSync(root, 0o755)
  assert.throws(() => openJournal(root), /unsafe_journal_directory/)
})
