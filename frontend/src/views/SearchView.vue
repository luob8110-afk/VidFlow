<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { ApiError } from '../api/client'
import { searchVideos, type SearchVideo } from '../api/search'
import AppShell from '../components/AppShell.vue'

const route = useRoute()
const router = useRouter()
function routeValue(key: string) {
  const value = route.query[key]
  return typeof value === 'string' ? value : ''
}
const keyword = computed(() => routeValue('q').trim())
const filters = reactive({ tag: '', author: '', from: '', to: '' })
const criteria = computed(() => ({
  tag: routeValue('tag'),
  author: routeValue('author'),
  from: routeValue('from'),
  to: routeValue('to'),
}))
const hasQuery = computed(() => keyword.value !== '' || Object.values(criteria.value).some(Boolean))

const items = ref<SearchVideo[]>([])
const cursor = ref('')
const loading = ref(false)
const error = ref('')
const expired = ref(false)
let request: AbortController | undefined

function dayBoundary(value: string, nextDay = false) {
  if (!value) return undefined
  const date = new Date(value + 'T00:00:00')
  if (nextDay) date.setDate(date.getDate() + 1)
  return date.getTime()
}

async function load(reset: boolean) {
  if (reset) {
    request?.abort()
    items.value = []
    cursor.value = ''
    loading.value = false
  }
  error.value = ''
  expired.value = false
  if (!hasQuery.value) return
  const { tag, author, from, to } = criteria.value
  const authorID = author ? Number(author) : undefined
  const start = dayBoundary(from)
  const end = dayBoundary(to, true)
  if (authorID !== undefined && (!Number.isSafeInteger(authorID) || authorID < 1)) {
    error.value = '请输入有效的作者 ID'
    return
  }
  if ((start !== undefined && (!Number.isFinite(start) || start <= 0)) ||
      (end !== undefined && (!Number.isFinite(end) || end <= 0)) ||
      (start !== undefined && end !== undefined && start >= end)) {
    error.value = '请选择有效的发布日期范围'
    return
  }
  const controller = new AbortController()
  request = controller
  loading.value = true
  try {
    const data = await searchVideos({
      q: keyword.value, tag, author_id: authorID, published_from: start, published_before: end,
      cursor: reset ? undefined : cursor.value,
    }, controller.signal)
    if (!controller.signal.aborted) {
      items.value = reset ? data.video_list : items.value.concat(data.video_list)
      cursor.value = data.next_cursor ?? ''
    }
  } catch (e) {
    if (!controller.signal.aborted) {
      expired.value = e instanceof ApiError && e.status === 410
      error.value = e instanceof ApiError ? e.message : '搜索请求失败，请稍后重试'
    }
  } finally {
    if (request === controller) loading.value = false
  }
}

async function applyFilters() {
  const query: Record<string, string> = {}
  if (keyword.value) query.q = keyword.value
  for (const [key, value] of Object.entries(filters)) {
    if (value.trim()) query[key] = value.trim()
  }
  const target = { name: 'search', query }
  if (router.resolve(target).fullPath === route.fullPath) await load(true)
  else await router.push(target)
}

watch(() => route.fullPath, () => {
  Object.assign(filters, criteria.value)
  void load(true)
}, { immediate: true })
onBeforeUnmount(() => request?.abort())
</script>

<template>
  <AppShell>
    <section class="search-page">
      <h1>{{ keyword ? '“' + keyword + '” 的搜索结果' : '搜索视频' }}</h1>
      <form class="filters" aria-label="筛选搜索结果" @submit.prevent="applyFilters">
        <label>话题 <input v-model="filters.tag" placeholder="例如 #户外" maxlength="100" /></label>
        <label>作者 ID <input v-model="filters.author" inputmode="numeric" pattern="[0-9]+" /></label>
        <label>开始日期 <input v-model="filters.from" type="date" /></label>
        <label>结束日期 <input v-model="filters.to" type="date" /></label>
        <button class="primary" type="submit">应用筛选</button>
      </form>

      <div v-if="error" class="search-status" role="alert">
        <span>{{ error }}</span>
        <button type="button" @click="load(expired || !cursor)">{{ expired ? '重新搜索' : '重试' }}</button>
      </div>
      <p v-else-if="loading && !items.length" class="search-status" role="status">搜索中…</p>
      <p v-else-if="!hasQuery" class="search-status">输入标题、简介或作者关键词，也可以按话题和发布日期筛选。</p>
      <p v-else-if="!items.length && !cursor" class="search-status" role="status">没有找到匹配的视频。</p>

      <ul class="results">
        <li v-for="item in items" :key="item.id">
          <RouterLink :to="{ name: 'video-detail', params: { id: item.id } }" class="preview">
            <img v-if="item.cover_url" :src="item.cover_url" alt="" loading="lazy" />
            <h2>{{ item.title }}</h2>
          </RouterLink>
          <div class="metadata">
            <RouterLink :to="{ name: 'user-profile', params: { id: item.author_id } }">{{ item.username }}</RouterLink>
            <time :datetime="item.create_time">{{ new Date(item.create_time).toLocaleDateString('zh-CN') }}</time>
          </div>
          <p v-if="item.description">{{ item.description }}</p>
        </li>
      </ul>
      <button v-if="cursor && !error" type="button" :disabled="loading" @click="load(false)">
        {{ loading ? '加载中…' : '加载更多' }}
      </button>
    </section>
  </AppShell>
</template>

<style scoped>
.search-page { display: grid; gap: 20px; align-items: start; }
h1 { margin: 0; overflow-wrap: anywhere; }
.filters { display: flex; flex-wrap: wrap; gap: 12px; align-items: end; }
.filters label { display: grid; gap: 5px; min-width: 0; color: var(--muted); font-size: 13px; }
.filters input { width: 142px; max-width: 100%; }
.search-status { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; margin: 0; }
.results { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 220px), 1fr)); gap: 16px; margin: 0; padding: 0; list-style: none; }
.results li { padding: 12px; border: 1px solid var(--border); border-radius: 12px; overflow-wrap: anywhere; }
.preview { display: block; }
.preview img { display: block; width: 100%; aspect-ratio: 16 / 9; object-fit: cover; border-radius: 8px; background: rgba(255, 255, 255, 0.05); }
.preview h2 { margin: 10px 0 6px; font-size: 16px; }
.metadata { display: flex; flex-wrap: wrap; gap: 10px; color: var(--muted); font-size: 13px; }
.results p { display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; margin: 8px 0 0; font-size: 14px; }
.search-page > button { justify-self: center; }
</style>
