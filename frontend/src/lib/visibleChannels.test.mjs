import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { normalizeVisibleChannels, toggleVisibleChannel } from './visibleChannels.ts'

test('normalizeVisibleChannels mirrors the backend rules', () => {
  assert.deepEqual(normalizeVisibleChannels(null), ['codex', 'claude', 'antigravity', 'grok', 'workbuddy'])
  assert.deepEqual(normalizeVisibleChannels(undefined), ['codex', 'claude', 'antigravity', 'grok', 'workbuddy'])
  assert.deepEqual(normalizeVisibleChannels([]), ['codex'])
  assert.deepEqual(normalizeVisibleChannels(['grok']), ['codex', 'grok'])
  assert.deepEqual(normalizeVisibleChannels([' Grok ', '', 'claude', 'grok', 'openai', 42]), ['codex', 'claude', 'grok'])
  assert.deepEqual(normalizeVisibleChannels('codex'), ['codex', 'claude', 'antigravity', 'grok', 'workbuddy'])
  // WorkBuddy 与后端 database.AllUpstreamChannels 对齐：可开关、位置在末位。
  assert.deepEqual(normalizeVisibleChannels(['workbuddy']), ['codex', 'workbuddy'])
})

test('toggleVisibleChannel never drops the fallback channel', () => {
  assert.deepEqual(toggleVisibleChannel(['codex', 'claude'], 'claude'), ['codex'])
  assert.deepEqual(toggleVisibleChannel(['codex'], 'grok'), ['codex', 'grok'])
  assert.deepEqual(toggleVisibleChannel(['codex', 'grok'], 'codex'), ['codex', 'grok'])
  assert.deepEqual(toggleVisibleChannel([], 'workbuddy'), ['codex', 'workbuddy'])
})

test('dashboard, accounts and the usage channel filter consume the visibility setting', () => {
  const read = (p) => readFileSync(new URL(p, import.meta.url), 'utf8')
  assert.match(read('../App.tsx'), /<VisibleChannelsProvider>/)
  assert.match(read('../pages/Dashboard.tsx'), /useVisibleChannels\(\)/)
  assert.match(read('../pages/Accounts.tsx'), /useVisibleChannels\(\)/)
  assert.match(read('../pages/QualityTest.tsx'), /useVisibleChannels\(\)/)
  assert.match(read('../components/ChannelFilter.tsx'), /useVisibleChannels\(\)/)
  // 直接打开被隐藏渠道的账号路由要回落到 Codex，而不是渲染一个切换器里不存在的视图。
  assert.match(read('../pages/Accounts.tsx'), /isChannelVisible\(providerView\)/)
  // WorkBuddy tab 同样由可见性开关驱动（providerSwitcherOptions 过滤），路由也要落到 Accounts。
  assert.match(read('../pages/Accounts.tsx'), /\["workbuddy", t\("accounts\.providerViewWorkBuddy"\)\]/)
})
