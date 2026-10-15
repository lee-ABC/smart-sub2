import { describe, expect, it } from 'vitest'
import { safeBpsLoginCode } from '../bpsLogin'
describe('BPS public diagnostics', () => {
  it('allows only finite server diagnostics, not arbitrary uppercase values or upstream text', () => {
    expect(safeBpsLoginCode('PURE_PASSWORD_INVALID')).toBe('PURE_PASSWORD_INVALID')
    expect(safeBpsLoginCode('CLOUDFLARE_BLOCK')).toBe('CLOUDFLARE_BLOCK')
    expect(safeBpsLoginCode('PRIVATE_UPSTREAM_SECRET')).toBe('BPS_LOGIN_REQUEST_FAILED')
    expect(safeBpsLoginCode('private-token detail')).toBe('BPS_LOGIN_REQUEST_FAILED')
    expect(safeBpsLoginCode(400, 'BPS_IMPORT_FAILED')).toBe('BPS_IMPORT_FAILED')
  })
})
