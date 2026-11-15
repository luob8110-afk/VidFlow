import { computed, onScopeDispose, reactive, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import * as feedApi from '../api/feed'
import type { FeedVideoItem } from '../api/types'
import { useAuthStore } from '../stores/auth'

export type TabKey = 'recommend' | 'hot' | 'following'

export function useVideoFeed() {
  const auth = useAuthStore()
  const tab = ref<TabKey>('recommend')

  const recommend = reactive({
    items: [] as FeedVideoItem[],
    loading: false, error: '',
    hasMore: false, nextTime: 0,
  })

  const hot = reactive({
    items: [] as FeedVideoItem[],
    loading: false, error: '',
    hasMore: false,
    nextLikesCountBefore: undefined as number | undefined,
    nextIdBefore: undefined as number | undefined,
  })

  const following = reactive({
    items: [] as FeedVideoItem[],
    loading: false, error: '',
    hasMore: false, nextCursor: '',
  })

  const currentState = computed(() => {
    if (tab.value === 'hot') return hot
    if (tab.value === 'following') return following
    return recommend
  })

  const states = { recommend, hot, following }
  const requests: Partial<Record<TabKey, { number: number; controller: AbortController }>> = {}
  let requestNumber = 0

  function cancelRequest(key: TabKey) {
    requests[key]?.controller.abort()
    delete requests[key]
    states[key].loading = false
  }

  async function runRequest<T>(
    key: TabKey,
    reset: boolean,
    fetchPage: (signal: AbortSignal) => Promise<T>,
    applyPage: (result: T) => void,
  ): Promise<boolean> {
    const state = states[key]
    if (state.loading && !reset) return false
    cancelRequest(key)
    const request = { number: ++requestNumber, controller: new AbortController() }
    requests[key] = request
    state.loading = true
    state.error = ''
    try {
      const result = await fetchPage(request.controller.signal)
      if (requests[key]?.number !== request.number) return false
      applyPage(result)
      return true
    } catch (e) {
      if (requests[key]?.number === request.number && !request.controller.signal.aborted) {
        const message = e instanceof ApiError ? e.message : String(e)
        state.error = reset ? `刷新失败：${message}` : message
      }
      return false
    } finally {
      if (requests[key]?.number === request.number) {
        delete requests[key]
        state.loading = false
      }
    }
  }

  function loadRecommend(reset: boolean) {
    return runRequest('recommend', reset,
      (signal) => feedApi.listLatest({ limit: 10, latest_time: reset ? 0 : recommend.nextTime }, signal),
      (res) => {
        recommend.hasMore = res.has_more
        recommend.nextTime = res.next_time
        recommend.items = reset ? res.video_list : recommend.items.concat(res.video_list)
      },
    )
  }

  function loadHot(reset: boolean) {
    return runRequest('hot', reset,
      (signal) => feedApi.listLikesCount({
        limit: 10,
        likes_count_before: reset ? undefined : hot.nextLikesCountBefore,
        id_before: reset ? undefined : hot.nextIdBefore,
      }, signal),
      (res) => {
        hot.hasMore = res.has_more
        hot.nextLikesCountBefore = res.next_likes_count_before
        hot.nextIdBefore = res.next_id_before
        hot.items = reset ? res.video_list : hot.items.concat(res.video_list)
      },
    )
  }

  async function loadFollowing(reset: boolean): Promise<boolean> {
    if (!auth.isLoggedIn) {
      cancelRequest('following')
      following.error = '登录后才能查看关注流'
      return false
    }
    if (!reset && !following.hasMore) return false
    return runRequest('following', reset,
      (signal) => feedApi.listByFollowing({ limit: 10, cursor: reset ? '' : following.nextCursor, refresh: reset }, signal),
      (res) => {
        following.hasMore = res.has_more
        following.nextCursor = res.next_cursor
        following.items = reset ? res.video_list : following.items.concat(res.video_list)
      },
    )
  }

  watch(() => auth.claims?.account_id, () => {
    cancelRequest('following')
    following.items = []
    following.nextCursor = ''
    following.hasMore = false
    following.error = ''
  }, { flush: 'sync' })

  onScopeDispose(() => {
    for (const key of ['recommend', 'hot', 'following'] as const) cancelRequest(key)
  })

  async function ensureTabLoaded() {
    if (tab.value === 'recommend' && recommend.items.length === 0) await loadRecommend(true)
    if (tab.value === 'hot' && hot.items.length === 0) await loadHot(true)
    if (tab.value === 'following' && following.items.length === 0) await loadFollowing(true)
  }

  async function loadMoreIfNeeded(activeIndex: number) {
    const items = currentState.value.items
    if (items.length === 0) return
    if (activeIndex < items.length - 3) return
    if (tab.value === 'recommend' && recommend.hasMore) await loadRecommend(false)
    if (tab.value === 'hot' && hot.hasMore) await loadHot(false)
    if (tab.value === 'following' && following.hasMore) await loadFollowing(false)
  }

  return { tab, recommend, hot, following, currentState, loadRecommend, loadHot, loadFollowing, ensureTabLoaded, loadMoreIfNeeded }
}
