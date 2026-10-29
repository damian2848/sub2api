import { baseCompile } from '@intlify/message-compiler'
import type { MessageFunction } from 'vue-i18n'

type CompiledMessages = { [key: string]: MessageFunction | CompiledMessages }

// Vitest uses vue-i18n's runtime-only build. Compile the real locale strings
// into message functions so rendering assertions also exercise interpolation.
export function compileMessages(messages: Record<string, unknown>): CompiledMessages {
  return Object.fromEntries(Object.entries(messages).map(([key, value]) => [
    key,
    typeof value === 'string'
      ? new Function(`return ${baseCompile(value).code}`)() as MessageFunction
      : compileMessages(value as Record<string, unknown>),
  ]))
}
