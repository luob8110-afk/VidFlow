<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'

import AppShell from '../components/AppShell.vue'
import CommentDrawer from '../components/CommentDrawer.vue'
import UserAvatar from '../components/UserAvatar.vue'
import type { FeedVideoItem } from '../api/types'
import { useAuthStore } from '../stores/auth'
import { useSocialStore } from '../stores/social'
import { useToastStore } from '../stores/toast'
import { useVideoFeed } from '../composables/useVideoFeed'
import type { TabKey } from '../composables/useVideoFeed'
import { useVideoPlayer } from '../composables/useVideoPlayer'
import { useLikeFollow } from '../composables/useLikeFollow'

const props = withDefaults(defineProps<{ feedTab?: TabKey }>(), { feedTab: 'recommend' })
const router = useRouter()
const auth = useAuthStore()
const social = useSocialStore()
const toast = useToastStore()

const { tab, following, currentState, loadRecommend, loadFollowing, ensureTabLoaded, loadMoreIfNeeded } = useVideoFeed()
const scroller = ref<HTMLDivElement | null>(null)
const { muted, activeIndex, videoMap, setVideoRef, scrollToIndex, onScroll, playActive, toggleMute, togglePlayPause } = useVideoPlayer(scroller)

async function needLogin() {
  toast.error('请先登录')
  await router.push('/account')
}

const { likeBusy, followBusy, toggleLike, toggleFollow, share } = useLikeFollow(needLogin)

const drawerVideo = ref<FeedVideoItem | null>(null)
const drawerOpen = ref(false)

function openComments(item: FeedVideoItem) { drawerVideo.value = item; drawerOpen.value = true }
function closeDrawer() { drawerOpen.value = false; drawerVideo.value = null }

const items = computed(() => currentState.value.items)
const activeItem = computed(() => items.value[activeIndex.value] ?? null)
const visibleRange = computed(() => {
  const idx = activeIndex.value
  const len = items.value.length
  return { start: Math.max(0, idx - 1), end: Math.min(len - 1, idx + 1) }
})
const myAccountId = computed(() => auth.claims?.account_id ?? 0)
let tabAction = 0
let tabRefreshPending = false

watch(activeItem, async () => {
  const action = tabAction
  await nextTick()
  if (action !== tabAction || tabRefreshPending) return
  await playActive(activeItem.value?.id)
  if (action !== tabAction || tabRefreshPending) return
  await loadMoreIfNeeded(activeIndex.value)
})

async function selectTab(key: TabKey) {
  const action = ++tabAction
  tabRefreshPending = true
  const switching = tab.value !== key
  tab.value = key
  closeDrawer()
  if (switching) {
    for (const video of videoMap.values()) video.pause()
    activeIndex.value = 0
    if (scroller.value) scroller.value.scrollTo({ top: 0, behavior: 'instant' })
  }
  let refreshed = false
  if (key === 'recommend') refreshed = await loadRecommend(true)
  else if (key === 'following') refreshed = await loadFollowing(true)
  else await ensureTabLoaded()
  if (action !== tabAction || tab.value !== key) return
  if (refreshed) {
    activeIndex.value = 0
    if (scroller.value) scroller.value.scrollTo({ top: 0, behavior: 'instant' })
  }
  await nextTick()
  if (action !== tabAction || tab.value !== key) return
  tabRefreshPending = false
  await playActive(activeItem.value?.id)
}

watch(() => items.value.length, (len) => {
  if (len === 0) activeIndex.value = 0
  else if (activeIndex.value > len - 1) activeIndex.value = len - 1
})

watch(() => auth.claims?.account_id, () => {
  if (tab.value === 'following') void selectTab('following')
})
watch(() => props.feedTab, (value) => void selectTab(value))

async function onKeydown(e: KeyboardEvent) {
  if (e.defaultPrevented || (e.target instanceof Element &&
      e.target.closest('a, button, input, select, textarea, summary, [contenteditable="true"]'))) return
  if (drawerOpen.value) return
  if (e.key === 'ArrowDown') { e.preventDefault(); scrollToIndex(activeIndex.value + 1, items.value.length) }
  else if (e.key === 'ArrowUp') { e.preventDefault(); scrollToIndex(activeIndex.value - 1, items.value.length) }
  else if (e.key === ' ') { e.preventDefault(); togglePlayPause(activeItem.value?.id) }
  else if (e.key.toLowerCase() === 'm') { e.preventDefault(); toggleMute() }
  else if (e.key.toLowerCase() === 'c') { if (activeItem.value) { e.preventDefault(); openComments(activeItem.value) } }
}

onMounted(async () => {
  window.addEventListener('keydown', onKeydown)
  await selectTab(props.feedTab)
})

onBeforeUnmount(() => {
  tabAction++
  window.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <AppShell full @refresh-feed="selectTab(props.feedTab)">
    <div class="page">
      <div v-if="currentState.loading && currentState.items.length > 0" class="feed-status" role="status">加载中…</div>
      <div v-if="currentState.error && currentState.items.length > 0" class="feed-status bad" role="alert">{{ currentState.error }}</div>
      <div v-if="tab === 'following' && following.items.length > 0 && !following.loading && !following.error && !following.hasMore" class="feed-status" role="status">已加载全部关注作品</div>
      <div ref="scroller" class="scroller" @scroll="onScroll">
        <div v-if="currentState.loading && currentState.items.length === 0" class="center-hint">加载中…</div>
        <div v-else-if="currentState.error && currentState.items.length === 0" class="center-hint">
          <div class="empty-feed">
            <span class="bad" role="alert">{{ currentState.error }}</span>
            <RouterLink v-if="tab === 'following' && !auth.isLoggedIn" class="chip" to="/account">去登录</RouterLink>
            <button v-else class="chip" type="button" @click="selectTab(tab)">重试</button>
          </div>
        </div>
        <div v-else-if="items.length === 0" class="center-hint">暂无视频</div>

        <section
          v-for="(item, idx) in items"
          :key="`${tab}-${item.id}`"
          class="slide"
          :class="{ active: idx === activeIndex }"
        >
          <div v-if="idx >= visibleRange.start && idx <= visibleRange.end" class="stage" @click="togglePlayPause(activeItem?.id)" @dblclick.prevent="toggleLike(item)">
            <video
              class="video"
              :ref="(el) => setVideoRef(item.id, el as HTMLVideoElement | null)"
              :src="item.play_url"
              :poster="item.cover_url"
              playsinline preload="metadata" loop
            />
            <div class="grad" />
            <div class="meta">
              <RouterLink class="author-link" :to="`/u/${item.author.id}`" @click.stop>
                <UserAvatar :username="item.author.username" :id="item.author.id" :size="34" />
                <span class="author-name">@{{ item.author.username }}</span>
              </RouterLink>
              <div class="title">{{ item.title }}</div>
              <div v-if="item.description" class="desc">{{ item.description }}</div>
            </div>
            <div class="actions">
              <button class="act" type="button" :disabled="!!likeBusy[String(item.id)]" @click.stop="toggleLike(item)">
                <span class="icon" :class="{ liked: item.is_liked }">♥</span>
                <span class="count">{{ item.likes_count }}</span>
              </button>
              <button class="act" type="button" @click.stop="openComments(item)">
                <span class="icon">💬</span>
                <span class="count">评论</span>
              </button>
              <button
                v-if="!myAccountId || myAccountId !== item.author.id"
                class="act" type="button"
                :disabled="!!followBusy[String(item.author.id)]"
                @click.stop="toggleFollow(item.author.id)">
                <span class="icon">＋</span>
                <span class="count">{{ social.isFollowing(item.author.id) ? '已关注' : '关注' }}</span>
              </button>
              <button class="act" type="button" @click.stop="share(item)">
                <span class="icon">↗</span>
                <span class="count">分享</span>
              </button>
              <div class="player-tools" @click.stop @dblclick.stop>
                <button
                  class="player-tool" type="button"
                  :aria-label="muted ? '开启声音' : '静音'" :title="muted ? '开启声音' : '静音'"
                  @click="toggleMute"
                >
                  <span class="volume-icon" :class="{ 'is-muted': muted }" aria-hidden="true" />
                </button>
                <RouterLink
                  class="player-tool" :to="{ name: 'video-detail', params: { id: item.id } }"
                  aria-label="查看视频详情" title="视频详情"
                >
                  <span class="details-icon" aria-hidden="true" />
                </RouterLink>
              </div>
            </div>
          </div>
        </section>
      </div>

      <CommentDrawer v-if="drawerOpen" :video="drawerVideo" @close="closeDrawer" />
    </div>
  </AppShell>
</template>

<style scoped>
.page { height: 100%; display: flex; flex-direction: column; }
.feed-status { padding: 6px 14px; color: rgba(255,255,255,0.8); background: rgba(0,0,0,0.4); }
.feed-status.bad { color: rgba(254,44,85,0.95); }
.scroller { flex: 1; min-height: 0; overflow-y: auto; scroll-snap-type: y mandatory; scroll-behavior: smooth; scrollbar-width: none; -ms-overflow-style: none; }
.scroller::-webkit-scrollbar { width: 0; height: 0; }
.center-hint { height: 100%; display: grid; place-items: center; padding: 12px; color: rgba(255,255,255,0.78); }
.center-hint .bad { color: #ff9bab; }
.empty-feed { display: grid; justify-items: center; gap: 12px; text-align: center; }
.slide { height: 100%; scroll-snap-align: start; padding: clamp(12px, 1.5vw, 24px); display: grid; grid-template-rows: minmax(0, 1fr); place-items: stretch center; }
.stage { width: min(1240px, 100%); position: relative; border-radius: 18px; overflow: hidden; border: 1px solid rgba(255,255,255,0.12); background: rgba(0,0,0,0.35); box-shadow: 0 20px 60px rgba(0,0,0,0.55); }
.video { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; background: rgba(0,0,0,0.4); }
.grad { position: absolute; inset: 0; background: linear-gradient(to top, rgba(0,0,0,0.68), rgba(0,0,0,0.12) 40%, rgba(0,0,0,0) 70%); pointer-events: none; }
.meta { position: absolute; left: 16px; bottom: 18px; max-width: min(620px, calc(100% - 124px)); }
.author-link { display: inline-flex; align-items: center; gap: 10px; font-weight: 800; letter-spacing: 0.2px; margin-bottom: 6px; text-decoration: none; }
.author-link:hover { text-decoration: none; }
.author-name { text-shadow: 0 14px 30px rgba(0,0,0,0.55); }
.title { font-size: 16px; font-weight: 700; margin-bottom: 6px; }
.desc { color: rgba(255,255,255,0.74); font-size: 13px; line-height: 1.35; }
.actions { position: absolute; right: 12px; bottom: 18px; display: grid; justify-items: end; gap: 12px; }
.player-tools { display: flex; gap: 6px; }
.player-tool {
  display: grid;
  place-items: center;
  width: 42px;
  height: 42px;
  padding: 0;
  border: 1px solid rgba(255,255,255,0.22);
  border-radius: 50%;
  background: rgba(0,0,0,0.48);
  color: #fff;
}
.player-tool:hover { background: rgba(32,44,55,0.86); }
.player-tool:focus-visible { outline: 2px solid #8de4d0; outline-offset: 2px; }
.volume-icon, .details-icon { position: relative; display: block; width: 20px; height: 20px; }
.volume-icon::before, .volume-icon::after, .details-icon::before { content: ''; position: absolute; }
.volume-icon::before {
  inset: 2px 9px 2px 0;
  background: currentColor;
  clip-path: polygon(0 32%, 40% 32%, 100% 0, 100% 100%, 40% 68%, 0 68%);
}
.volume-icon::after {
  top: 3px;
  right: 0;
  width: 9px;
  height: 14px;
  border-right: 2px solid currentColor;
  border-radius: 50%;
}
.volume-icon.is-muted::after {
  top: 6px;
  width: 8px;
  height: 8px;
  border: 0;
  border-radius: 0;
  background:
    linear-gradient(45deg, transparent 42%, currentColor 42% 58%, transparent 58%),
    linear-gradient(-45deg, transparent 42%, currentColor 42% 58%, transparent 58%);
}
.details-icon::before {
  inset: 0;
  width: 3px;
  height: 3px;
  margin: auto;
  border-radius: 50%;
  background: currentColor;
  box-shadow: -6px 0 currentColor, 6px 0 currentColor;
}
.act { width: 70px; border-radius: 16px; border: 1px solid rgba(255,255,255,0.14); background: rgba(0,0,0,0.32); color: rgba(255,255,255,0.92); padding: 10px 10px; cursor: pointer; display: grid; gap: 6px; justify-items: center; }
.act:hover { background: rgba(255,255,255,0.1); }
.act:disabled { opacity: 0.55; cursor: not-allowed; }
.icon { font-size: 20px; line-height: 1; opacity: 0.92; }
.icon.liked { color: rgba(254,44,85,1); text-shadow: 0 10px 20px rgba(254,44,85,0.25); }
.count { font-size: 12px; color: rgba(255,255,255,0.8); }
.chip { display: inline-flex; align-items: center; gap: 8px; padding: 7px 10px; border-radius: 999px; border: 1px solid rgba(255,255,255,0.14); background: rgba(0,0,0,0.28); color: rgba(255,255,255,0.86); font-size: 12px; text-decoration: none; }
.chip.primary { border-color: rgba(254,44,85,0.45); background: rgba(254,44,85,0.14); }
.chip.danger { border-color: rgba(254,44,85,0.55); background: rgba(254,44,85,0.12); }

@media (max-width: 620px), (max-height: 480px) {
  .actions { right: 7px; bottom: 10px; gap: 6px; }
  .act { width: 48px; padding: 6px 2px; border-radius: 11px; }
  .player-tool { width: 38px; height: 38px; }
}

@media (max-width: 620px) {
  .slide { padding: 8px; }
  .stage { border-radius: 12px; }
  .meta { left: 10px; bottom: 10px; max-width: calc(100% - 106px); }
  .author-link { max-width: 100%; gap: 5px; }
  .author-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .title, .desc { overflow-wrap: anywhere; }
}

@media (max-height: 480px) {
  .actions { grid-template-columns: repeat(2, max-content); }
  .player-tools { grid-column: 1 / -1; }
  .meta { max-width: min(620px, calc(100% - 132px)); }
}
</style>
