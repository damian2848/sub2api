// Mirrors backend/internal/pkg/geminiweb/models.go. Gemini Web identifies
// models by opaque 16-hex IDs; the backend translates known IDs back to public
// names and stores unknown ones raw, so keep both tables in sync.
const models: Array<{ id: string; headerID?: string }> = [
  { id: 'gemini-4-argon', headerID: '95a4c0abcba24364' },
  { id: 'gemini-3.8-flash', headerID: '56fdd199312815e2' },
  { id: 'gemini-3.8-flash-thinking' },
  { id: 'gemini-3.1-pro', headerID: 'e6fa609c3fa255c0' },
  { id: 'gemini-auto' },
  { id: 'gemini-3.8-flash-thinking-lite' },
  { id: 'gemini-3.5-flash-lite', headerID: 'cf41b0e0dd7d53e5' },
  { id: 'gemini-3.6-flash', headerID: 'fbb127bbb056c959' },
]

const aliases: Record<string, string> = {
  'gemini-flash': 'gemini-3.8-flash', 'gemini-flash-plus': 'gemini-3.8-flash',
  'gemini-flash-thinking': 'gemini-3.8-flash-thinking', 'gemini-pro': 'gemini-3.1-pro',
  'gemini-3-flash': 'gemini-3.8-flash', 'gemini-3-pro': 'gemini-3.1-pro',
  'gemini-2.5-flash': 'gemini-3.8-flash', 'gemini-2.5-pro': 'gemini-3.1-pro',
  'gemini-1.5-flash': 'gemini-3.8-flash', 'gemini-1.5-pro': 'gemini-3.1-pro',
}

/** True when a response model is a raw Gemini Web ID the backend could not name. */
export const isUnrecognizedGeminiWebModelID = (model: string | null | undefined): boolean =>
  /^[0-9a-f]{16}$/i.test(model?.trim() || '')

/** Resolves a public or alias model name to the Gemini Web model it is sent as. */
export const lookupGeminiWebModel = (model: string | null | undefined): { id: string; headerID?: string } | null => {
  const key = model?.trim().toLowerCase() || ''
  const canonical = aliases[key] || key
  return models.find((m) => m.id === canonical) || null
}
