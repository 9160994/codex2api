import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import {
  mergeWorkBuddyImportItems,
  normalizeWorkBuddyExpiry,
  normalizeWorkBuddyModelId,
  parseWorkBuddyAuthText,
  parseWorkBuddyModelLines,
  workBuddyAccountLabel,
  workBuddyExpiryState,
} from './workbuddyImport.ts'

const wb2apiAuthFile = {
  account: { uid: 'u-1001', nickname: '小助手', enterpriseId: 'ent-9' },
  auth: {
    accessToken: 'at-abc',
    refreshToken: 'rt-def',
    expiresAt: '2027-01-02T03:04:05Z',
    domain: 'workbuddy.tencent.com',
    realm: 'cn',
  },
}

test('parseWorkBuddyAuthText reads a single wb2api auth file', () => {
  const { items, skipped } = parseWorkBuddyAuthText(JSON.stringify(wb2apiAuthFile))
  assert.equal(skipped, 0)
  assert.equal(items.length, 1)
  assert.deepEqual(items[0], {
    uid: 'u-1001',
    access_token: 'at-abc',
    refresh_token: 'rt-def',
    expires_at: '2027-01-02T03:04:05.000Z',
    realm: 'cn',
    nickname: '小助手',
    enterprise_id: 'ent-9',
  })
})

test('parseWorkBuddyAuthText accepts arrays and wrapper objects', () => {
  const second = {
    account: { uid: 'u-1002' },
    auth: { accessToken: 'at-2', refreshToken: 'rt-2', expiresAt: 1893456000, domain: 'global' },
  }
  const asArray = parseWorkBuddyAuthText(JSON.stringify([wb2apiAuthFile, second]))
  assert.deepEqual(
    asArray.items.map((item) => item.uid),
    ['u-1001', 'u-1002'],
  )
  const wrapped = parseWorkBuddyAuthText(JSON.stringify({ accounts: [wb2apiAuthFile, second] }))
  assert.equal(wrapped.items.length, 2)
  // 秒级时间戳要转成 ISO；realm 缺失时用 domain 兜底。
  assert.equal(wrapped.items[1].expires_at, '2030-01-01T00:00:00.000Z')
  assert.equal(wrapped.items[1].realm, 'global')
})

test('parseWorkBuddyAuthText skips entries without uid or access token', () => {
  const { items, skipped } = parseWorkBuddyAuthText(
    JSON.stringify([wb2apiAuthFile, { account: { uid: 'no-token' }, auth: {} }, { auth: { accessToken: 'x' } }]),
  )
  assert.equal(items.length, 1)
  assert.equal(skipped, 2)
})

test('parseWorkBuddyAuthText tolerates invalid input', () => {
  assert.deepEqual(parseWorkBuddyAuthText(''), { items: [], skipped: 0 })
  assert.deepEqual(parseWorkBuddyAuthText('not json'), { items: [], skipped: 0 })
})

test('mergeWorkBuddyImportItems dedupes by uid, last one wins', () => {
  const merged = mergeWorkBuddyImportItems(
    [{ uid: 'a', access_token: 'old' }, { uid: 'b', access_token: 'b1' }],
    [{ uid: 'a', access_token: 'new' }],
  )
  assert.deepEqual(
    merged.map((item) => [item.uid, item.access_token]),
    [
      ['a', 'new'],
      ['b', 'b1'],
    ],
  )
})

test('model ids are bare upstream ids (cn:/global: prefixes stripped)', () => {
  assert.equal(normalizeWorkBuddyModelId('cn:deepseek-v4-flash'), 'deepseek-v4-flash')
  assert.equal(normalizeWorkBuddyModelId('global:glm-4.6'), 'glm-4.6')
  assert.deepEqual(parseWorkBuddyModelLines('deepseek-v4-flash, cn:glm-4.6\n deepseek-v4-flash '), [
    'deepseek-v4-flash',
    'glm-4.6',
  ])
})

test('expiry helpers classify unset / expired / soon', () => {
  const now = Date.parse('2026-01-01T00:00:00Z')
  assert.equal(normalizeWorkBuddyExpiry(undefined), '')
  assert.equal(normalizeWorkBuddyExpiry('2026-01-02T00:00:00Z'), '2026-01-02T00:00:00.000Z')
  assert.equal(workBuddyExpiryState('', now), 'unset')
  assert.equal(workBuddyExpiryState('2025-12-31T00:00:00Z', now), 'expired')
  assert.equal(workBuddyExpiryState('2026-01-03T00:00:00Z', now), 'soon')
  assert.equal(workBuddyExpiryState('2026-03-01T00:00:00Z', now), 'ok')
  assert.equal(workBuddyAccountLabel({ name: ' A ', nickname: 'b', uid: 'c' }), 'A')
  assert.equal(workBuddyAccountLabel({ nickname: 'b', uid: 'c' }), 'b')
  assert.equal(workBuddyAccountLabel({ uid: 'c' }), 'c')
})

test('WorkBuddy page uses shared UI components, the api client and i18n keys', () => {
  const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')
  const page = read('../pages/WorkBuddyAccounts.tsx')
  const addModal = read('../pages/workbuddy/WorkBuddyAddModal.tsx')
  const importModal = read('../pages/workbuddy/WorkBuddyImportModal.tsx')
  const editModal = read('../pages/workbuddy/WorkBuddyEditModal.tsx')
  const sources = [page, addModal, importModal, editModal]

  // DESIGN.md §1：表单控件只用共享组件，不写原生 select；开关一律用 Switch
  // （列表里用于多选的 checkbox 是既有账号页的统一做法，不算开关）。
  for (const source of sources) {
    assert.doesNotMatch(source, /<select[\s>]/)
  }
  for (const modal of [addModal, importModal, editModal]) {
    assert.doesNotMatch(modal, /<input[^>]*type="checkbox"/)
    assert.doesNotMatch(modal, /<switch/i)
  }
  assert.match(page, /from "@\/components\/ui\/switch"/)
  assert.match(page, /<Switch\b/)
  assert.match(page, /from "@\/components\/ui\/table"/)
  assert.match(page, /from "@\/components\/ui\/button"/)
  assert.match(page, /from "@\/components\/ui\/switch"/)
  assert.match(addModal, /from "@\/components\/ui\/select"/)
  assert.match(addModal, /from "@\/components\/ui\/input"/)
  assert.match(addModal, /ProxyField/)

  // 页面不直接 fetch：所有请求走 api 客户端。
  for (const source of sources) {
    assert.doesNotMatch(source, /\bfetch\(/)
  }
  assert.match(page, /api\.listWorkBuddyAccounts\(/)
  assert.match(page, /api\.deleteWorkBuddyAccounts\(/)
  assert.match(page, /api\.toggleAccountEnabled\(/)
  // 模型白名单必须走 workbuddy PATCH（通用 /accounts/{id}/models 对中转账号 400）。
  assert.match(editModal, /payload\.models = models/)
  assert.doesNotMatch(editModal, /api\.updateAccountModels\(/)
  assert.match(page, /api\.testWorkBuddyAccount\(/)
  assert.match(page, /api\.refreshWorkBuddyAccount\(/)
  assert.match(addModal, /api\.addWorkBuddyAccount\(/)
  assert.match(importModal, /api\.importWorkBuddyAccounts\(/)
  assert.match(editModal, /api\.updateWorkBuddyAccount\(/)

  // 渠道只支持 chat/completions 与 models，页面上必须有明确提示。
  assert.match(page, /accounts\.workbuddyEndpointNotice|workbuddy\.endpointNotice/)

  // 三份语言的 key 必须同时存在（DESIGN.md §3）。
  const zh = JSON.parse(read('../locales/zh.json'))
  const en = JSON.parse(read('../locales/en.json'))
  const zhTW = JSON.parse(read('../locales/zh-TW.json'))
  assert.ok(zh.workbuddy?.pageTitle)
  assert.ok(en.workbuddy?.pageTitle)
  assert.ok(zhTW.workbuddy?.pageTitle)
  for (const key of ['providerViewWorkBuddy']) {
    assert.ok(zh.accounts[key])
    assert.ok(en.accounts[key])
    assert.ok(zhTW.accounts[key])
  }
  for (const key of ['addTitle', 'importTitle', 'editTitle', 'endpointNotice', 'testOk', 'testFailed']) {
    assert.ok(zh.workbuddy[key], `zh.workbuddy.${key}`)
    assert.ok(en.workbuddy[key], `en.workbuddy.${key}`)
    assert.ok(zhTW.workbuddy[key], `zh-TW.workbuddy.${key}`)
  }
})
