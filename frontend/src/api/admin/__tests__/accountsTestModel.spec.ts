import { afterEach, describe, expect, it, vi } from 'vitest'
import { testAccountModel } from '../accounts'

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
