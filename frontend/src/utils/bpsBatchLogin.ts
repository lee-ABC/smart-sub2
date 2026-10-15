import type { CodexSessionImportRequest } from '@/types'

export const BPS_BATCH_LIMIT = 100
export const BPS_BATCH_CONCURRENCY = 5
export type BpsBatchImportOptions = Omit<CodexSessionImportRequest, 'content' | 'contents'>
export interface BpsBatchCredential { line: number; email: string; password: string; totp_secret: string }
export interface BpsBatchIssue { line: number; code: 'INVALID_LINE' | 'DUPLICATE_EMAIL' | 'BATCH_LIMIT' }

// Never return the raw line in validation errors; passwords may contain ----.
export function parseBpsLoginBatch(text: string): { entries: BpsBatchCredential[]; issues: BpsBatchIssue[] } {
  const lines = text.split(/\r?\n/).map((raw, i) => ({ raw, line: i + 1 })).filter(({ raw }) => raw.trim())
  if (lines.length > BPS_BATCH_LIMIT || text.length > 200_000) return { entries: [], issues: [{ line: 0, code: 'BATCH_LIMIT' }] }
  const entries: BpsBatchCredential[] = []
  const issues: BpsBatchIssue[] = []
  const seen = new Set<string>()
  for (const { raw, line } of lines) {
    const first = raw.indexOf('----')
    const last = raw.lastIndexOf('----')
    const email = raw.slice(0, first).trim()
    const password = raw.slice(first + 4, last)
    const secret = raw.slice(last + 4).replace(/\s/g, '').toUpperCase()
    if (first < 1 || last <= first || email.length > 254 || !/^[^\s@]+@[^\s@]+$/.test(email) || !password || password.length > 1000 || [...password].some(char => char.charCodeAt(0) < 32 || char.charCodeAt(0) === 127) || !/^[A-Z2-7]{16,256}={0,6}$/.test(secret)) {
      issues.push({ line, code: 'INVALID_LINE' }); continue
    }
    const key = email.toLowerCase()
    if (seen.has(key)) { issues.push({ line, code: 'DUPLICATE_EMAIL' }); continue }
    seen.add(key)
    entries.push({ line, email, password, totp_secret: secret })
  }
  return { entries, issues }
}
