import { afterEach, describe, expect, it, vi } from 'vitest'
import { testAccountModel, testAccountModelIntelligence } from '../accounts'

function sse(...events: object[]) {
  const body = events.map(event => `data: ${JSON.stringify(event)}\n\n`).join('')
  return new Response(body, { status: 200 })
}

describe('testAccountModel', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('asks the server not to recover account state and reports success', async () => {
    const fetchMock = vi.fn().mockResolvedValue(sse({ type: 'test_start' }, { type: 'test_complete', success: true }))
    vi.stubGlobal('fetch', fetchMock)

    await expect(testAccountModel(7, 'gpt-x')).resolves.toEqual({ success: true })
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toMatchObject({ model_id: 'gpt-x', skip_recovery: true })
  })

  it.each([
    [{ type: 'test_complete', success: false, error: 'bad' }, 'bad'],
    [{ type: 'error', error: 'boom' }, 'boom']
  ])('maps %o to a failure', async (event, error) => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(sse(event)))
    await expect(testAccountModel(7, 'm')).resolves.toEqual({ success: false, error })
  })

  it('treats a stream without a result and an HTTP error as failures', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(sse({ type: 'content', text: 'hi' })))
    await expect(testAccountModel(7, 'm')).resolves.toMatchObject({ success: false })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response('', { status: 502 })))
    await expect(testAccountModel(7, 'm')).resolves.toEqual({ success: false, error: 'HTTP 502' })
  })
})

describe('testAccountModelIntelligence', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('sends the candy question and passes a correct answer', async () => {
    const fetchMock = vi.fn().mockResolvedValue(sse(
      { type: 'content', text: '2' },
      { type: 'content', text: '1' },
      { type: 'test_complete', success: true }
    ))
    vi.stubGlobal('fetch', fetchMock)

    await expect(testAccountModelIntelligence(7, 'gpt-x')).resolves.toEqual({ success: true, answer: '21' })
    const body = JSON.parse(fetchMock.mock.calls[0][1].body)
    expect(body).toMatchObject({ model_id: 'gpt-x', reasoning_effort: 'medium' })
    expect(body.prompt).toContain('只输出最终整数，不要解释。')
  })

  it.each(['20', '210', '答案是21', '21或22', '', '二十一'])('fails the answer %j', async (answer) => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(sse(
      { type: 'content', text: answer },
      { type: 'test_complete', success: true }
    )))

    await expect(testAccountModelIntelligence(7, 'gpt-x')).resolves.toEqual({
      success: false,
      error: 'Expected answer 21',
      answer
    })
  })

  it('allows whitespace around 21', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(sse(
      { type: 'content', text: ' \n21\n ' },
      { type: 'test_complete', success: true }
    )))
    await expect(testAccountModelIntelligence(7, 'm')).resolves.toEqual({ success: true, answer: '21' })
  })

  it.each([
    [{ type: 'status' }, 'No test result received'],
    [{ type: 'test_complete', success: false, error: 'truncated' }, 'truncated'],
    [{ type: 'error', error: 'upstream failed' }, 'upstream failed']
  ])('rejects an incomplete or failed stream even when it contains 21', async (event, error) => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(sse({ type: 'content', text: '21' }, event)))
    await expect(testAccountModelIntelligence(7, 'm')).resolves.toEqual({ success: false, error, answer: '21' })
  })

  it('preserves a stream error even if a completion event follows', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(sse(
      { type: 'content', text: '21' },
      { type: 'error', error: 'bad stream' },
      { type: 'test_complete', success: true }
    )))
    await expect(testAccountModelIntelligence(7, 'm')).resolves.toMatchObject({ success: false, error: 'bad stream' })
  })
})
