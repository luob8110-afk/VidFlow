<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { findById } from '../api/account'
import { useAuthStore } from '../stores/auth'
import { useSocialStore } from '../stores/social'
import Toaster from './Toaster.vue'
import UserAvatar from './UserAvatar.vue'

defineProps<{ full?: boolean }>()
const emit = defineEmits<{ refreshFeed: [] }>()

const auth = useAuthStore()
const social = useSocialStore()
const router = useRouter()
const route = useRoute()
const accountMenu = ref<HTMLDetailsElement | null>(null)
const accountId = computed(() => auth.claims?.account_id)
const username = computed(() => auth.claims?.username || '我的账号')
const avatarURL = ref('')

const channels = [
  { route: 'home', label: '推荐', icon: 'play' },
  { route: 'following-feed', label: '关注', icon: 'person' },
  { route: 'hot', label: '热榜', icon: 'chart' },
]

const search = ref('')
watch(() => route.query.q, (value) => {
  search.value = typeof value === 'string' ? value : ''
}, { immediate: true })

watch(() => auth.isLoggedIn, (loggedIn) => {
  if (loggedIn) void social.refreshMine()
  else social.clear()
}, { immediate: true })

watch(accountId, async (id, _, onCleanup) => {
  avatarURL.value = ''
  if (!id) return
  const request = new AbortController()
  onCleanup(() => request.abort())
  try {
    const profile = await findById(id, request.signal)
    if (!request.signal.aborted) avatarURL.value = profile.avatar_url ?? ''
  } catch {
    // UserAvatar still displays the account initial.
  }
}, { immediate: true })

async function onSearch() {
  const q = search.value.trim()
  await router.push({ name: 'search', query: q ? { q } : {} })
}

function refreshActiveFeed(event: MouseEvent, name: string) {
  if (route.name === name && event.button === 0 && !event.ctrlKey && !event.metaKey &&
      !event.altKey && !event.shiftKey) emit('refreshFeed')
}

function closeAccountMenu() {
  if (accountMenu.value) accountMenu.value.open = false
}

function onOutsidePress(event: PointerEvent) {
  if (accountMenu.value?.open && event.target instanceof Node && !accountMenu.value.contains(event.target)) {
    closeAccountMenu()
  }
}

function onAccountEscape(event: KeyboardEvent) {
  if (!accountMenu.value?.open) return
  event.preventDefault()
  closeAccountMenu()
  accountMenu.value.querySelector('summary')?.focus()
}

watch(() => route.fullPath, closeAccountMenu)
onMounted(() => document.addEventListener('pointerdown', onOutsidePress))
onBeforeUnmount(() => document.removeEventListener('pointerdown', onOutsidePress))
</script>

<template>
  <div class="app-shell">
    <header class="app-topbar">
      <RouterLink class="brand" to="/" aria-label="VidFlow 首页">
        <span class="brand-mark" aria-hidden="true" />
        <span class="brand-name">VidFlow</span>
      </RouterLink>

      <form class="top-search" role="search" @submit.prevent="onSearch">
        <span class="app-icon icon-search" aria-hidden="true" />
        <input v-model="search" type="search" aria-label="关键词" placeholder="搜索全站视频" maxlength="100" />
        <button type="submit">搜索</button>
      </form>

      <div class="top-actions">
        <RouterLink class="publish-link" to="/video">
          <span aria-hidden="true">+</span> 发布视频
        </RouterLink>

        <details v-if="auth.isLoggedIn" ref="accountMenu" class="account-menu" @keydown.esc="onAccountEscape">
          <summary class="account-trigger" :aria-label="`打开 ${username} 的账号菜单`" :title="username">
            <UserAvatar :username="username" :id="accountId" :src="avatarURL" :size="34" />
          </summary>
          <div class="account-panel">
            <strong class="account-name">{{ username }}</strong>
            <RouterLink v-if="accountId" :to="{ name: 'user-profile', params: { id: accountId } }" @click="closeAccountMenu">我的主页</RouterLink>
            <RouterLink to="/account" @click="closeAccountMenu">账号中心</RouterLink>
            <RouterLink to="/messages" @click="closeAccountMenu">私信</RouterLink>
            <RouterLink class="menu-publish" to="/video" @click="closeAccountMenu">发布视频</RouterLink>
            <RouterLink to="/settings" @click="closeAccountMenu">设置</RouterLink>
          </div>
        </details>
        <RouterLink v-else class="account-trigger login-link" to="/account" aria-label="登录">
          <span class="app-icon icon-person" aria-hidden="true" />
          <span class="login-label">登录</span>
        </RouterLink>
      </div>
    </header>

    <aside class="side-rail">
      <nav class="side-nav" aria-label="内容导航">
        <RouterLink
          v-for="channel in channels" :key="channel.route" :to="{ name: channel.route }"
          class="side-link" @click="refreshActiveFeed($event, channel.route)"
        >
          <span class="app-icon" :class="`icon-${channel.icon}`" aria-hidden="true" />
          {{ channel.label }}
        </RouterLink>
        <RouterLink class="side-link side-link-secondary" :to="{ name: 'likes-feed' }">
          <span class="app-icon icon-heart" aria-hidden="true">♡</span>
          点赞榜
        </RouterLink>
      </nav>
    </aside>

    <main class="app-content" :class="{ 'app-content-full': full }">
      <slot v-if="full" />
      <div v-else class="container"><slot /></div>
    </main>

    <Toaster />
  </div>
</template>

<style scoped>
.app-shell {
  --chrome: #111720;
  --chrome-line: rgba(182, 202, 228, 0.13);
  --accent: #8de4d0;
  --rail-width: 128px;
  display: grid;
  grid-template-columns: var(--rail-width) minmax(0, 1fr);
  grid-template-rows: 66px minmax(0, 1fr);
  grid-template-areas: "top top" "rail content";
  height: 100vh;
  height: 100dvh;
}

.app-topbar {
  grid-area: top;
  position: relative;
  z-index: 10;
  display: grid;
  grid-template-columns: minmax(0, 1fr) min(45vw, 555px) minmax(0, 1fr);
  gap: 16px;
  align-items: center;
  padding-inline: clamp(14px, 2.4vw, 36px);
  background: var(--chrome);
  border-bottom: 1px solid var(--chrome-line);
}

.brand {
  display: inline-flex;
  justify-self: start;
  align-items: center;
  gap: 10px;
  border-radius: 9px;
}

.brand-mark {
  position: relative;
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  border-radius: 10px 10px 10px 4px;
  background: linear-gradient(145deg, #b5efdc 5%, #6bd4c5 58%, #65b9f0);
  box-shadow: 0 4px 16px rgba(95, 217, 198, 0.18);
}

.brand-mark::before {
  content: '';
  margin-left: 3px;
  border-block: 6px solid transparent;
  border-left: 9px solid #13282b;
}

.brand-name { font-size: 20px; font-weight: 850; letter-spacing: -0.045em; }

.top-search {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
  min-height: 42px;
  padding: 3px 4px 3px 14px;
  border: 1px solid var(--chrome-line);
  border-radius: 999px;
  background: #1a2330;
  color: #aabccc;
}

.top-search:focus-within {
  border-color: rgba(141, 228, 208, 0.6);
  box-shadow: 0 0 0 3px rgba(141, 228, 208, 0.1);
}

.top-search input {
  flex: 1;
  min-width: 0;
  padding: 0;
  border: 0;
  border-radius: 0;
  background: transparent;
  box-shadow: none;
  font: inherit;
  font-size: 14px;
}

.top-search input::placeholder { color: #a3afbf; }

.top-search button {
  align-self: stretch;
  flex: none;
  padding: 0 16px;
  border: 0;
  border-radius: 999px;
  background: rgba(141, 228, 208, 0.12);
  color: #c1f1e6;
  font-size: 13px;
  font-weight: 650;
}

.top-search button:hover { background: rgba(141, 228, 208, 0.22); }

.top-actions { display: flex; justify-content: flex-end; align-items: center; gap: 14px; }

.publish-link,
.account-trigger {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-height: 42px;
  border: 1px solid var(--chrome-line);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.025);
  color: var(--text);
  white-space: nowrap;
}

.publish-link { padding: 0 14px; gap: 6px; font-size: 13px; }
.publish-link span { color: var(--accent); font-size: 21px; line-height: 1; }
.account-trigger { min-width: 42px; padding: 3px; cursor: pointer; list-style: none; }
.account-trigger::-webkit-details-marker { display: none; }
.login-link { gap: 8px; padding-inline: 12px; font-size: 13px; }
.publish-link:hover,
.account-trigger:hover,
.account-menu[open] .account-trigger { border-color: rgba(141, 228, 208, 0.6); background: #1b2931; }

.account-menu { position: relative; }
.account-panel {
  position: absolute;
  z-index: 1;
  top: calc(100% + 10px);
  right: 0;
  display: grid;
  width: 176px;
  gap: 2px;
  padding: 8px;
  border: 1px solid var(--chrome-line);
  border-radius: 14px;
  background: #17212c;
  box-shadow: 0 16px 34px rgba(0, 0, 0, 0.42);
}

.account-name {
  padding: 7px 9px 10px;
  margin-bottom: 3px;
  overflow-wrap: anywhere;
  border-bottom: 1px solid var(--chrome-line);
  font-size: 14px;
}

.account-panel a { padding: 8px 9px; border-radius: 7px; font-size: 13px; }
.account-panel a:hover { background: rgba(141, 228, 208, 0.1); color: var(--accent); }
.account-panel .menu-publish { display: none; }

.side-rail {
  grid-area: rail;
  min-height: 0;
  overflow: auto;
  padding: 23px 10px 12px 9px;
  border-right: 1px solid var(--chrome-line);
  background: linear-gradient(180deg, #121a24, #0e141d);
}

.side-nav { display: grid; gap: 8px; }
.side-link {
  display: flex;
  min-height: 46px;
  align-items: center;
  gap: 11px;
  padding: 0 12px;
  border: 1px solid rgba(182, 202, 228, 0.06);
  border-radius: 9px 999px 999px 9px;
  background: rgba(182, 202, 228, 0.035);
  color: #b9c7d6;
  font-size: 14px;
  font-weight: 620;
  white-space: nowrap;
}

.side-link:hover { background: rgba(171, 213, 233, 0.08); color: var(--text); }
.side-link.router-link-exact-active {
  border-color: rgba(141, 228, 208, 0.23);
  background: linear-gradient(96deg, rgba(141, 228, 208, 0.19), rgba(141, 228, 208, 0.06));
  box-shadow: inset 2px 0 0 #8de4d0;
  color: #cbf5e9;
}

.side-link-secondary { margin-top: 10px; border-color: transparent; background: transparent; color: #aab8c8; font-size: 13px; }
.app-shell :is(a, button, summary):focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
.app-content { grid-area: content; min-width: 0; min-height: 0; overflow: auto; }
.app-content-full { overflow: hidden; }

.app-icon { position: relative; display: inline-block; flex: none; width: 18px; height: 18px; }
.app-icon::before,
.app-icon::after { content: ''; position: absolute; }

.icon-search::before {
  width: 11px; height: 11px; top: 2px; left: 2px;
  border: 1.5px solid currentColor; border-radius: 50%;
}
.icon-search::after {
  width: 5px; height: 1.5px; top: 12px; left: 11px;
  border-radius: 2px; background: currentColor; transform: rotate(45deg);
}
.icon-play::before { inset: 1px; border: 1.5px solid currentColor; border-radius: 6px; }
.icon-play::after {
  top: 5px; left: 7px;
  border-block: 4px solid transparent; border-left: 6px solid currentColor;
}
.icon-person::before {
  top: 1px; left: 5px; width: 8px; height: 8px;
  border: 1.5px solid currentColor; border-radius: 50%;
}
.icon-person::after {
  left: 2px; bottom: 1px; width: 14px; height: 7px;
  border: 1.5px solid currentColor; border-radius: 8px 8px 3px 3px;
}
.icon-chart::before {
  left: 2px; bottom: 2px; width: 3px; height: 6px;
  border-radius: 1.5px; background: currentColor; box-shadow: 5px -4px 0 currentColor, 10px -7px 0 currentColor;
}
.icon-chart::after { bottom: 1px; left: 2px; width: 13px; height: 2px; background: currentColor; border-radius: 1px; }
.icon-heart { font-size: 22px; line-height: 17px; text-align: center; }

@media (max-width: 820px) {
  .app-shell { --rail-width: 104px; grid-template-rows: 60px minmax(0, 1fr); }
  .top-actions { gap: 7px; }
  .publish-link { padding-inline: 10px; }
  .side-rail { padding-inline: 6px 7px; }
  .side-link { gap: 7px; padding-inline: 9px; }
}

@media (max-width: 740px) {
  .publish-link { display: none; }
  .account-panel .menu-publish { display: block; }
}

@media (max-width: 620px) {
  .app-shell { --rail-width: 59px; grid-template-rows: 58px minmax(0, 1fr); }
  .app-topbar { grid-template-columns: 42px minmax(0, 1fr) 42px; gap: 8px; padding-inline: 10px; }
  .brand-name,
  .login-label { display: none; }
  .brand { justify-self: center; }
  .login-link { padding-inline: 3px; }
  .top-search { padding-left: 10px; gap: 6px; }
  .top-search input { font-size: 13px; }
  .top-search button { padding-inline: 11px; }
  .side-rail { padding: 13px 5px 8px 4px; }
  .side-link {
    flex-direction: column;
    justify-content: center;
    gap: 3px;
    min-height: 51px;
    padding: 3px 0;
    border-radius: 8px 16px 16px 8px;
    font-size: 11px;
  }
  .side-link-secondary { margin-top: 5px; }
  .app-content .container { padding-inline: 12px; }
}
</style>
