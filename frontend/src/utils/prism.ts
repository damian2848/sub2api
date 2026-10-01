import type { Account } from '@/types'

export function isManagedPrismAccount(account: Account | null | undefined): boolean {
  return account?.platform === 'openai' && account.type === 'apikey' &&
    account.extra?.provider_preset === 'prism_browser' &&
    typeof account.extra.prism_source_account_id === 'number' && account.extra.prism_source_account_id > 0
}
