import { describe, expect, it } from 'vitest'
import { BPS_BATCH_CONCURRENCY, parseBpsLoginBatch } from '../bpsBatchLogin'

const secret = 'JBSWY3DPEHPK3PXP'
describe('BPS multiline credential parsing', () => {
  it('parses CRLF, blanks, Base32 and preserves password delimiters/whitespace', () => {
    const result = parseBpsLoginBatch(`a@example.test---- pass----word ----${secret.toLowerCase()}

b@example.test----pw----${secret}`)
    expect(result.issues).toEqual([])
    expect(result.entries[0]).toEqual({ line: 1, email: 'a@example.test', password: ' pass----word ', totp_secret: secret })
    expect(result.entries[1].line).toBe(3)
    expect(BPS_BATCH_CONCURRENCY).toBe(5)
  })
  it('rejects malformed/OTP lines without echoing credentials and deduplicates email', () => {
    const result = parseBpsLoginBatch(`a@example.test----secret-password----${secret}
A@example.test----different-password----${secret}
b@example.test----private-password----123456
private-malformed-line`)
    expect(result.entries).toHaveLength(1)
    expect(result.issues).toEqual([{ line: 2, code: 'DUPLICATE_EMAIL' }, { line: 3, code: 'INVALID_LINE' }, { line: 4, code: 'INVALID_LINE' }])
    expect(JSON.stringify(result.issues)).not.toMatch(/password|malformed-line/)
  })
  it('fails closed for over 100 nonempty rows or oversized input', () => {
    expect(parseBpsLoginBatch(Array.from({ length: 101 }, (_, i) => `${i}@example.test----pw----${secret}`).join('\n'))).toEqual({ entries: [], issues: [{ line: 0, code: 'BATCH_LIMIT' }] })
    expect(parseBpsLoginBatch('x'.repeat(200001)).entries).toEqual([])
  })
})
