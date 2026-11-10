import type { WorkBuddyImportItem } from '../types'

// wb2api 授权文件（auths/workbuddy-<uid>.json）解析：
//   { "account": { "uid", "nickname", "enterpriseId" },
//     "auth":    { "accessToken", "refreshToken", "expiresAt", "domain", "realm" } }
// 同一个文件也可能是上述对象的数组，或是 { accounts: [...] } / { items: [...] } 包装。
// 本模块只做前端预检与预览（数量、缺字段），最终仍把原文交给后端解析，避免两边规则漂移。

export interface WorkBuddyParseResult {
  items: WorkBuddyImportItem[]
  /** 结构不完整（缺 uid 或 access token）而跳过的条目数。 */
  skipped: number
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null
}

function pickString(record: Record<string, unknown> | null, keys: readonly string[]): string {
  if (!record) return ''
  for (const key of keys) {
    const value = record[key]
    if (typeof value === 'string' && value.trim()) return value.trim()
    if (typeof value === 'number' && Number.isFinite(value)) return String(value)
  }
  return ''
}

// expiresAt 可能是 ISO 字符串、毫秒/秒时间戳或数字字符串：统一成 ISO，解析不了就留空
// （留空表示"不设过期"，比塞一个假时间更安全）。
export function normalizeWorkBuddyExpiry(value: unknown): string {
  if (value === null || value === undefined) return ''
  if (typeof value === 'number') return epochToISO(value)
  const text = String(value).trim()
  if (!text) return ''
  if (/^\d+$/.test(text)) return epochToISO(Number(text))
  const parsed = Date.parse(text)
  return Number.isNaN(parsed) ? '' : new Date(parsed).toISOString()
}

function epochToISO(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return ''
  // 秒级时间戳（10 位）与毫秒级（13 位）都出现过来：1e11 秒 ≈ 5138 年，可安全分界。
  const ms = value < 1e11 ? value * 1000 : value
  const date = new Date(ms)
  return Number.isNaN(date.getTime()) ? '' : date.toISOString()
}

// 模型名一律是上游裸 id（如 deepseek-v4-flash）。用户从别处粘贴的 cn:/global: 前缀
// 在前端就剥掉，避免落库出现网关不认识的名字。
export function normalizeWorkBuddyModelId(value: string): string {
  return value
    .trim()
    .replace(/^(?:cn|global):/i, '')
    .trim()
}

export function parseWorkBuddyModelLines(text: string): string[] {
  const seen = new Set<string>()
  for (const raw of text.split(/[\s,，;；]+/)) {
    const model = normalizeWorkBuddyModelId(raw)
    if (model) seen.add(model)
  }
  return Array.from(seen)
}

function pickModels(record: Record<string, unknown> | null): string[] | undefined {
  if (!record) return undefined
  const raw = record.models
  if (!Array.isArray(raw)) return undefined
  const models = raw
    .filter((item): item is string => typeof item === 'string')
    .map(normalizeWorkBuddyModelId)
    .filter(Boolean)
  return models.length > 0 ? Array.from(new Set(models)) : undefined
}

function itemFromEntry(entry: unknown): WorkBuddyImportItem | null {
  const root = asRecord(entry)
  if (!root) return null
  const account = asRecord(root.account)
  const auth = asRecord(root.auth) ?? asRecord(root.token)
  const flat = root

  const uid =
    pickString(account, ['uid', 'userId', 'user_id']) || pickString(flat, ['uid', 'user_id', 'userId'])
  const accessToken =
    pickString(auth, ['accessToken', 'access_token', 'token']) ||
    pickString(flat, ['access_token', 'accessToken', 'token'])
  if (!uid || !accessToken) return null

  const refreshToken =
    pickString(auth, ['refreshToken', 'refresh_token']) || pickString(flat, ['refresh_token', 'refreshToken'])
  const expiresAt = normalizeWorkBuddyExpiry(
    auth?.expiresAt ?? auth?.expires_at ?? flat.expires_at ?? flat.expiresAt,
  )
  // wb2api 的 auth 里领域字段可能叫 realm 或 domain，与列表接口的 realm 对齐。
  const realm =
    pickString(auth, ['realm', 'domain']) || pickString(flat, ['realm', 'domain']) || 'global'
  const nickname = pickString(account, ['nickname', 'name', 'userName']) || pickString(flat, ['nickname', 'name'])
  const enterpriseId =
    pickString(account, ['enterpriseId', 'enterprise_id']) || pickString(flat, ['enterprise_id', 'enterpriseId'])
  const models = pickModels(flat) ?? pickModels(auth)

  const item: WorkBuddyImportItem = { uid, access_token: accessToken }
  if (refreshToken) item.refresh_token = refreshToken
  if (expiresAt) item.expires_at = expiresAt
  if (realm) item.realm = realm
  if (nickname) item.nickname = nickname
  if (enterpriseId) item.enterprise_id = enterpriseId
  if (models) item.models = models
  return item
}

function collectEntries(payload: unknown): unknown[] {
  if (Array.isArray(payload)) return payload
  const record = asRecord(payload)
  if (!record) return []
  for (const key of ['accounts', 'auths', 'items', 'data'] as const) {
    const nested = record[key]
    if (Array.isArray(nested)) return nested
  }
  return [payload]
}

/** 把授权文件原文解析成导入条目；结构不完整的条目计入 skipped。 */
export function parseWorkBuddyAuthText(text: string): WorkBuddyParseResult {
  const trimmed = text.trim()
  if (!trimmed) return { items: [], skipped: 0 }
  let payload: unknown
  try {
    payload = JSON.parse(trimmed)
  } catch {
    return { items: [], skipped: 0 }
  }
  const entries = collectEntries(payload)
  const byUid = new Map<string, WorkBuddyImportItem>()
  let skipped = 0
  for (const entry of entries) {
    const item = itemFromEntry(entry)
    if (!item) {
      skipped += 1
      continue
    }
    // 同一个 uid 重复出现时以最后一条为准（重新授权后 token 更新）。
    byUid.set(item.uid, item)
  }
  return { items: Array.from(byUid.values()), skipped }
}

/** 多文件/多段粘贴合并：按 uid 去重，保留后到的条目。 */
export function mergeWorkBuddyImportItems(...groups: WorkBuddyImportItem[][]): WorkBuddyImportItem[] {
  const byUid = new Map<string, WorkBuddyImportItem>()
  for (const group of groups) {
    for (const item of group) byUid.set(item.uid, item)
  }
  return Array.from(byUid.values())
}

export type WorkBuddyExpiryState = 'unset' | 'expired' | 'soon' | 'ok'

/** 过期态：无时间为 unset，已过期为 expired，7 天内为 soon。 */
export function workBuddyExpiryState(expiresAt?: string | null, now: number = Date.now()): WorkBuddyExpiryState {
  if (!expiresAt) return 'unset'
  const parsed = Date.parse(expiresAt)
  if (Number.isNaN(parsed)) return 'unset'
  if (parsed <= now) return 'expired'
  if (parsed - now <= 7 * 24 * 60 * 60 * 1000) return 'soon'
  return 'ok'
}

/** 列表展示用的账号名：name → nickname → uid。 */
export function workBuddyAccountLabel(account: { name?: string; nickname?: string; uid?: string }): string {
  return account.name?.trim() || account.nickname?.trim() || account.uid?.trim() || '—'
}
