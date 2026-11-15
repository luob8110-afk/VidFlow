import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import test from 'node:test'
import ts from 'typescript'
import * as vue from 'vue'

const require = createRequire(import.meta.url)
const source = readFileSync(new URL('../src/composables/useVideoFeed.ts', import.meta.url), 'utf8')
const compiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText

function setup(t) {
  const pending = []
  const auth = vue.reactive({ isLoggedIn: true, claims: { account_id: 1 } })
  const request = (input, signal) => new Promise((resolve, reject) => {
    // Intentionally ignore cancellation to simulate an already-delivered response.
    pending.push({ input, signal, resolve, reject })
  })
  const mocks = {
    vue,
    '../api/client': { ApiError: Error },
    '../api/feed': { listLatest: request, listByFollowing: request, listLikesCount: request },
    '../stores/auth': { useAuthStore: () => auth },
  }
  const module = { exports: {} }
  new Function('require', 'module', 'exports', compiled)(
    (name) => mocks[name] ?? require(name), module, module.exports,
  )
  const scope = vue.effectScope()
  const feed = scope.run(() => module.exports.useVideoFeed())
  t.after(() => scope.stop())
  return { feed, pending, auth, scope }
}

function page(id, hasMore = true) {
  return { video_list: [{ id }], next_time: id, next_cursor: hasMore ? `cursor-${id}` : '', has_more: hasMore }
}

test('refresh supersedes late pagination, including its cursor and loading state', async (t) => {
  const { feed, pending } = setup(t)
  const initial = feed.loadFollowing(true)
  pending[0].resolve(page(10))
  await initial
  const older = feed.loadFollowing(false)
  const refresh = feed.loadFollowing(true)
  assert.equal(pending[1].signal.aborted, true)
  assert.equal(pending[2].input.refresh, true)
  assert.equal(pending[2].input.cursor, '')
  pending[1].resolve(page(5))
  assert.equal(await older, false)
  assert.equal(feed.following.loading, true)
  pending[2].resolve(page(20))
  assert.equal(await refresh, true)
  assert.deepEqual(feed.following.items.map((item) => item.id), [20])
  assert.equal(feed.following.nextCursor, 'cursor-20')
  assert.equal(feed.following.loading, false)
})

test('the latest repeated refresh wins even when an older response arrives last', async (t) => {
  const { feed, pending } = setup(t)
  const older = feed.loadFollowing(true)
  const latest = feed.loadFollowing(true)
  pending[1].resolve(page(30))
  await latest
  pending[0].resolve(page(10))
  assert.equal(await older, false)
  assert.deepEqual(feed.following.items.map((item) => item.id), [30])
})

test('refresh failure retains content, pagination cursor and hasMore', async (t) => {
  const { feed, pending } = setup(t)
  const initial = feed.loadFollowing(true)
  pending[0].resolve(page(10, false))
  await initial
  const refresh = feed.loadFollowing(true)
  pending[1].reject(new Error('database unavailable'))
  assert.equal(await refresh, false)
  assert.deepEqual(feed.following.items.map((item) => item.id), [10])
  assert.equal(feed.following.nextCursor, '')
  assert.equal(feed.following.hasMore, false)
  assert.match(feed.following.error, /刷新失败/)
})

test('recommendation and following requests cannot overwrite each other', async (t) => {
  const { feed, pending } = setup(t)
  const recommend = feed.loadRecommend(true)
  const following = feed.loadFollowing(true)
  feed.tab.value = 'following'
  pending[1].resolve(page(20))
  await following
  pending[0].resolve(page(10))
  await recommend
  assert.deepEqual(feed.currentState.value.items.map((item) => item.id), [20])
  assert.deepEqual(feed.recommend.items.map((item) => item.id), [10])
})

test('refresh blocks load-more and late errors cannot clear the latest loading state', async (t) => {
  const { feed, pending } = setup(t)
  const older = feed.loadFollowing(true)
  const latest = feed.loadFollowing(true)
  assert.equal(await feed.loadFollowing(false), false)
  assert.equal(pending.length, 2)
  pending[0].reject(new Error('late error'))
  await older
  assert.equal(feed.following.loading, true)
  assert.equal(feed.following.error, '')
  pending[1].resolve(page(30))
  await latest
})

test('account changes and disposal invalidate in-flight requests', async (t) => {
  const { feed, pending, auth, scope } = setup(t)
  const following = feed.loadFollowing(true)
  auth.claims = { account_id: 2 }
  pending[0].resolve(page(10))
  assert.equal(await following, false)
  assert.deepEqual(feed.following.items, [])
  const recommend = feed.loadRecommend(true)
  scope.stop()
  assert.equal(pending[1].signal.aborted, true)
  pending[1].resolve(page(20))
  assert.equal(await recommend, false)
})

test('following passes the opaque cursor unchanged and stops at the last page', async (t) => {
  const { feed, pending } = setup(t)
  const cursor = 'eyJ2IjoxLCJ0aW1lIjoiMjAyNi0xMS0xM1QwMDowMDowMC4xMjM0NTZaIiwiaWQiOiI5MDA3MTk5MjU0NzQwOTkzIn0'
  const first = feed.loadFollowing(true)
  pending[0].resolve({ ...page(10), next_cursor: cursor })
  await first
  const next = feed.loadFollowing(false)
  assert.equal(pending[1].input.cursor, cursor)
  assert.equal(pending[1].input.latest_time, undefined)
  assert.equal(pending[1].input.refresh, false)
  pending[1].resolve(page(5, false))
  await next
  assert.deepEqual(feed.following.items.map((item) => item.id), [10, 5])
  assert.equal(feed.following.nextCursor, '')
  assert.equal(await feed.loadFollowing(false), false)
  assert.equal(pending.length, 2)
  const refresh = feed.loadFollowing(true)
  assert.equal(pending[2].input.cursor, '')
  pending[2].resolve(page(20, false))
  await refresh
  assert.deepEqual(feed.following.items.map((item) => item.id), [20])
})

test('publication, failed refresh, retry and late pagination still yield all 26 works', async (t) => {
  const { feed, pending } = setup(t)
  const items = (ids, cursor, hasMore) => ({ video_list: ids.map((id) => ({ id })), next_cursor: cursor, next_time: 0, has_more: hasMore })
  const initial = feed.loadFollowing(true)
  pending[0].resolve(items(Array.from({ length: 10 }, (_, i) => 25 - i), 'old-page-2', true))
  await initial
  const older = feed.loadFollowing(false)
  const failedRefresh = feed.loadFollowing(true)
  pending[2].reject(new Error('injected refresh failure'))
  await failedRefresh
  assert.equal(feed.following.items.length, 10)
  assert.equal(feed.following.nextCursor, 'old-page-2')
  assert.match(feed.following.error, /刷新失败/)
  const retry = feed.loadFollowing(true)
  pending[3].resolve(items(Array.from({ length: 10 }, (_, i) => 26 - i), 'new-page-2', true))
  await retry
  pending[1].resolve(items(Array.from({ length: 10 }, (_, i) => 15 - i), 'old-page-3', true))
  assert.equal(await older, false)
  assert.equal(feed.following.items.length, 10)
  assert.equal(feed.following.nextCursor, 'new-page-2')
  const second = feed.loadFollowing(false)
  assert.equal(pending[4].input.cursor, 'new-page-2')
  pending[4].resolve(items(Array.from({ length: 10 }, (_, i) => 16 - i), 'new-page-3', true))
  await second
  const last = feed.loadFollowing(false)
  assert.equal(pending[5].input.cursor, 'new-page-3')
  pending[5].resolve(items([6, 5, 4, 3, 2, 1], '', false))
  await last
  assert.deepEqual(feed.following.items.map((item) => item.id), Array.from({ length: 26 }, (_, i) => 26 - i))
  assert.equal(feed.following.error, '')
  assert.equal(await feed.loadFollowing(false), false)
  assert.equal(pending.length, 6)
})
