<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import AppShell from '../components/AppShell.vue'
import { ApiError } from '../api/client'
import * as videoApi from '../api/video'
import type { Video } from '../api/types'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'
import SparkMD5 from 'spark-md5'

const router = useRouter()
const auth = useAuthStore()
const toast = useToastStore()

const busy = ref(false)
const stage = ref('')
const published = ref<Video | null>(null)
const mediaEnabled = ref<boolean | null>(null)
const asset = ref<videoApi.MediaAsset | null>(null)
const uploadError = ref('')
const page = new AbortController()
const canPublish = computed(() => asset.value?.status === 'ready')
let draftKey = ''

const videoInput = ref<HTMLInputElement | null>(null)
const coverInput = ref<HTMLInputElement | null>(null)

const publishForm = reactive({
  title: '',
  description: '',
  video: null as File | null,
  cover: null as File | null,
})

const preview = reactive({
  videoUrl: '',
  coverUrl: '',
})

// chunk upload progress
const uploadProgress = reactive({
  uploadedBytes: 0,
  totalBytes: 0,
  percent: 0,
})

function setPreviewVideo(file: File | null) {
  if (preview.videoUrl) URL.revokeObjectURL(preview.videoUrl)
  preview.videoUrl = file ? URL.createObjectURL(file) : ''
}

function setPreviewCover(file: File | null) {
  if (preview.coverUrl) URL.revokeObjectURL(preview.coverUrl)
  preview.coverUrl = file ? URL.createObjectURL(file) : ''
}

watch(
  () => publishForm.video,
  (f) => setPreviewVideo(f),
)

watch(
  () => publishForm.cover,
  (f) => setPreviewCover(f),
)

onUnmounted(() => {
  page.abort()
  setPreviewVideo(null)
  setPreviewCover(null)
})

onMounted(async () => {
  try {
    mediaEnabled.value = (await videoApi.mediaConfig(page.signal)).enabled
  } catch (err) {
    if (!page.signal.aborted) uploadError.value = err instanceof Error ? err.message : String(err)
  }
})

function pickVideo(e: Event) {
  const input = e.target as HTMLInputElement
  publishForm.video = input.files?.[0] ?? null
  asset.value = null
  uploadError.value = ''
}

function pickCover(e: Event) {
  const input = e.target as HTMLInputElement
  publishForm.cover = input.files?.[0] ?? null
}

function openVideoPicker() {
  videoInput.value?.click()
}

function openCoverPicker() {
  coverInput.value?.click()
}

function clearVideo() {
  publishForm.video = null
  asset.value = null
  draftKey = ''
  if (videoInput.value) videoInput.value.value = ''
}

function clearCover() {
  publishForm.cover = null
  if (coverInput.value) coverInput.value.value = ''
}

function resetProgress() {
  uploadProgress.uploadedBytes = 0
  uploadProgress.totalBytes = 0
  uploadProgress.percent = 0
}

// Compute file md5 by reading in 2MB chunks
async function computeFileMD5(file: File): Promise<string> {
  const chunkSize = 2 << 20
  const spark = new SparkMD5.ArrayBuffer()
  for (let offset = 0; offset < file.size; offset += chunkSize) {
    const end = Math.min(offset + chunkSize, file.size)
    const buf = await file.slice(offset, end).arrayBuffer()
    spark.append(buf)
  }
  return spark.end()
}

// Compute md5 for a single chunk blob
async function computeChunkMD5(blob: Blob): Promise<string> {
  const buf = await blob.arrayBuffer()
  const spark = new SparkMD5.ArrayBuffer()
  spark.append(buf)
  return spark.end()
}

const CHUNK_SIZE = 5 << 20 // 5 MB
const MAX_CONCURRENT = 3
const MAX_RETRIES = 3

function makeDraftID() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, '0')).join('')
}

async function prepareAsset(file: File) {
  if (file.size <= 0 || file.size > 200 * 1024 * 1024) throw new Error('请选择不超过 200 MiB 的视频')
  if (!crypto.subtle) throw new Error('请通过 HTTPS 或 localhost 打开页面再上传')
  const signal = page.signal
  stage.value = '计算文件校验值'
  const digest = await crypto.subtle.digest('SHA-256', await file.arrayBuffer())
  const sha256 = Array.from(new Uint8Array(digest), b => b.toString(16).padStart(2, '0')).join('')
  signal.throwIfAborted()
  draftKey = `vidflow:media:${auth.claims?.account_id}:${sha256}`
  let draftID = localStorage.getItem(draftKey) || makeDraftID()
  localStorage.setItem(draftKey, draftID)
  const init = () => videoApi.initAsset({ draft_id: draftID, size: file.size, sha256 }, signal)
  let view = await init()
  if (view.status === 'expired') {
    draftID = makeDraftID()
    localStorage.setItem(draftKey, draftID)
    view = await init()
  }
  asset.value = view
  if (view.status === 'uploading') {
    stage.value = '上传视频'
    await sendAssetParts(file, view, signal)
    stage.value = '确认上传'
    view = await videoApi.completeAsset(view.asset_id, signal)
    asset.value = view
  }
  stage.value = '处理视频'
  const deadline = Date.now() + 8 * 60 * 1000
  while (view.status === 'queued' || view.status === 'processing') {
    if (Date.now() >= deadline) throw new Error('视频仍在处理中，可以稍后继续')
    await new Promise<void>(resolve => window.setTimeout(resolve, 1500))
    signal.throwIfAborted()
    view = await videoApi.assetStatus(view.asset_id, signal)
    asset.value = view
  }
  if (view.status === 'published') {
    if (view.video_id) published.value = await videoApi.getDetail(view.video_id)
    localStorage.removeItem(draftKey)
    clearVideo()
    return
  }
  if (view.status !== 'ready') throw new Error(view.failure || '视频处理未完成')
}

async function sendAssetParts(file: File, view: videoApi.MediaAsset, signal: AbortSignal) {
  const uploaded = new Set(view.uploaded_parts)
  const pending = Array.from({ length: view.part_count }, (_, i) => i + 1)
  const part = (number: number) => file.slice((number - 1) * view.part_size, number * view.part_size)
  uploadProgress.totalBytes = file.size
  uploadProgress.uploadedBytes = pending.filter(n => uploaded.has(n)).reduce((sum, n) => sum + part(n).size, 0)
  uploadProgress.percent = Math.round(uploadProgress.uploadedBytes / file.size * 100)
  const batch = new AbortController()
  const uploadSignal = AbortSignal.any([signal, batch.signal])
  let next = 0
  async function transfer() {
    while (next < pending.length) {
      const n = pending[next++]!
      if (uploaded.has(n)) continue
      const body = part(n)
      for (let attempt = 1; ; attempt++) {
        uploadSignal.throwIfAborted()
        try {
          const { url } = await videoApi.signPart(view.asset_id, n, uploadSignal)
          const response = await fetch(url, { method: 'PUT', body, signal: uploadSignal })
          if (!response.ok) throw new Error(`分片上传失败（${response.status}）`)
          break
        } catch (err) {
          if (uploadSignal.aborted || attempt === MAX_RETRIES) throw err
        }
      }
      uploadProgress.uploadedBytes += body.size
      uploadProgress.percent = Math.round(uploadProgress.uploadedBytes / file.size * 100)
    }
  }
  const transfers = Array.from({ length: MAX_CONCURRENT }, () => transfer())
  try {
    await Promise.all(transfers)
  } catch (err) {
    batch.abort()
    await Promise.allSettled(transfers)
    throw err
  }
}

async function resetAsset() {
  if (!asset.value || busy.value) return
  busy.value = true
  try {
    await videoApi.discardAsset(asset.value.asset_id, page.signal)
    if (draftKey) localStorage.removeItem(draftKey)
    asset.value = null
    uploadError.value = ''
  } catch (err) {
    if (!page.signal.aborted) uploadError.value = err instanceof Error ? err.message : String(err)
  } finally {
    busy.value = false
  }
}

async function uploadVideoChunked(file: File): Promise<videoApi.UploadResponse> {
  const totalChunks = Math.ceil(file.size / CHUNK_SIZE)
  const fileHash = await computeFileMD5(file)

  stage.value = '初始化上传'
  const initRes = await videoApi.initChunkUpload({
    filename: file.name,
    file_size: file.size,
    chunk_size: CHUNK_SIZE,
    total_chunks: totalChunks,
    file_hash: fileHash,
  })

  const uploadId = initRes.upload_id
  const uploadedSet = new Set(initRes.uploaded_chunks)

  uploadProgress.totalBytes = file.size
  uploadProgress.uploadedBytes = uploadedSet.size * CHUNK_SIZE
  // Last chunk might be smaller
  if (uploadedSet.has(totalChunks - 1)) {
    uploadProgress.uploadedBytes -= CHUNK_SIZE
    uploadProgress.uploadedBytes += file.size - (totalChunks - 1) * CHUNK_SIZE
  }
  uploadProgress.percent = uploadProgress.totalBytes > 0
    ? Math.round((uploadProgress.uploadedBytes / uploadProgress.totalBytes) * 100)
    : 0

  // Build list of chunks that still need uploading
  const pending: number[] = []
  for (let i = 0; i < totalChunks; i++) {
    if (!uploadedSet.has(i)) {
      pending.push(i)
    }
  }

  if (pending.length === 0) {
    stage.value = '合并文件'
    return videoApi.completeChunkUpload(uploadId)
  }

  stage.value = '上传视频'

  // Upload chunks with concurrency limit
  let idx = 0
  const advanceProgress = (chunkIndex: number) => {
    const chunkBytes = chunkIndex === totalChunks - 1
      ? file.size - chunkIndex * CHUNK_SIZE
      : CHUNK_SIZE
    uploadProgress.uploadedBytes += chunkBytes
    uploadProgress.percent = Math.round((uploadProgress.uploadedBytes / uploadProgress.totalBytes) * 100)
  }

  const uploadOne = async (chunkIndex: number): Promise<void> => {
    const start = chunkIndex * CHUNK_SIZE
    const end = Math.min(start + CHUNK_SIZE, file.size)
    const blob = file.slice(start, end)
    const chunkHash = await computeChunkMD5(blob)

    let lastErr: unknown
    for (let attempt = 0; attempt < MAX_RETRIES; attempt++) {
      try {
        await videoApi.uploadChunk(uploadId, chunkIndex, chunkHash, blob)
        advanceProgress(chunkIndex)
        return
      } catch (e) {
        lastErr = e
      }
    }
    throw lastErr
  }

  await new Promise<void>((resolve, reject) => {
    let active = 0
    let done = false

    const next = () => {
      if (done) return
      if (idx >= pending.length && active === 0) {
        resolve()
        return
      }
      while (active < MAX_CONCURRENT && idx < pending.length) {
        const ci = pending[idx++] as number
        active++
        uploadOne(ci)
          .then(() => { active--; next() })
          .catch((e) => { done = true; reject(e) })
      }
    }
    next()
  })

  stage.value = '合并文件'
  return videoApi.completeChunkUpload(uploadId)
}

async function onPublish() {
  if (busy.value || mediaEnabled.value === null) return
  if (!auth.isLoggedIn) {
    toast.error('请先登录')
    await router.push('/account')
    return
  }

  const title = publishForm.title.trim()
  const description = publishForm.description.trim()
  if ((!mediaEnabled.value || canPublish.value) && !title) {
    toast.error('请输入视频标题')
    return
  }
  if (!publishForm.video) {
    toast.error('请选择视频文件（.mp4）')
    return
  }
  if (!mediaEnabled.value && !publishForm.cover) {
    toast.error('请选择封面图片（jpg/png/webp）')
    return
  }

  busy.value = true
  stage.value = ''
  published.value = null
  uploadError.value = ''
  resetProgress()
  try {
    if (mediaEnabled.value) {
      if (!canPublish.value || !asset.value) {
        await prepareAsset(publishForm.video!)
        return
      }
      stage.value = '发布视频'
      published.value = await videoApi.publishVideo({ title, description, asset_id: asset.value.asset_id }, page.signal)
      if (draftKey) localStorage.removeItem(draftKey)
      toast.success('已发布')
      publishForm.title = ''
      publishForm.description = ''
      clearVideo()
      return
    }
    const videoRes = await uploadVideoChunked(publishForm.video!)

    stage.value = '上传封面'
    const coverRes = await videoApi.uploadCover(publishForm.cover!)

    const coverUrl = coverRes.url || coverRes.cover_url || ''
    const playUrl = videoRes.url || videoRes.play_url || ''
    if (!coverUrl || !playUrl) {
      toast.error('上传成功但缺少 url')
      return
    }

    stage.value = '发布视频'
    const res = await videoApi.publishVideo({ title, description, play_url: playUrl, cover_url: coverUrl })

    published.value = res
    toast.success('已发布')

    publishForm.title = ''
    publishForm.description = ''
    clearVideo()
    clearCover()
  } catch (e) {
    if (!page.signal.aborted) {
      uploadError.value = e instanceof ApiError ? e.message : String(e)
      toast.error(uploadError.value)
    }
  } finally {
    busy.value = false
    stage.value = ''
    resetProgress()
  }
}
</script>

<template>
  <AppShell>
    <div class="publish-wrap">
      <div class="card publish-card">
        <div class="row" style="justify-content: space-between; align-items: baseline">
          <p class="title" style="margin: 0">发布视频</p>
          <div v-if="busy" class="pill">进行中：{{ stage || '…' }}</div>
        </div>
        <p class="subtle" style="margin-top: 10px" v-if="mediaEnabled">选择视频后上传。封面和播放版本生成后即可发布。</p>
        <p class="subtle" style="margin-top: 10px" v-else-if="mediaEnabled === false">选择视频及封面后发布。</p>

        <div class="grid form-grid" style="margin-top: 16px">
          <div>
            <label for="publish-title">标题</label>
            <input id="publish-title" v-model.trim="publishForm.title" class="big-input" :disabled="busy" maxlength="255" />
          </div>
          <div>
            <label for="publish-description">简介</label>
            <textarea id="publish-description" v-model.trim="publishForm.description" class="big-input" :disabled="busy" maxlength="255" />
          </div>
          <div class="grid two">
            <div>
              <label>视频</label>
              <input ref="videoInput" class="file-native" type="file" :accept="mediaEnabled ? '.mp4,.mov,.webm,.mkv' : 'video/mp4'" :disabled="busy" @change="pickVideo" />
              <div class="file-box">
                <button type="button" :disabled="busy" @click="openVideoPicker">选择视频</button>
                <div class="file-name" :class="publishForm.video ? '' : 'muted'">
                  {{ publishForm.video ? publishForm.video.name : '未选择文件' }}
                </div>
                <button v-if="publishForm.video" type="button" :disabled="busy" @click="clearVideo">清除</button>
              </div>
              <div v-if="publishForm.video" class="subtle" style="margin-top: 6px">
                已选择：{{ publishForm.video.name }}（{{ Math.ceil(publishForm.video.size / 1024 / 1024) }} MB）
              </div>
            </div>
            <div v-if="mediaEnabled === false">
              <label>cover (jpg/png/webp)</label>
              <input
                ref="coverInput"
                class="file-native"
                type="file"
                accept="image/jpeg,image/png,image/webp"
                :disabled="busy"
                @change="pickCover"
              />
              <div class="file-box">
                <button type="button" :disabled="busy" @click="openCoverPicker">选择封面</button>
                <div class="file-name" :class="publishForm.cover ? '' : 'muted'">
                  {{ publishForm.cover ? publishForm.cover.name : '未选择文件' }}
                </div>
                <button v-if="publishForm.cover" type="button" :disabled="busy" @click="clearCover">清除</button>
              </div>
              <div v-if="publishForm.cover" class="subtle" style="margin-top: 6px">已选择：{{ publishForm.cover.name }}</div>
            </div>
          </div>

          <!-- Upload progress bar -->
          <div v-if="busy && uploadProgress.totalBytes > 0" class="progress-wrap">
            <div class="progress-bar">
              <div class="progress-fill" :style="{ width: uploadProgress.percent + '%' }"></div>
            </div>
            <div class="progress-text">
              {{ (uploadProgress.uploadedBytes / 1024 / 1024).toFixed(1) }} MB /
              {{ (uploadProgress.totalBytes / 1024 / 1024).toFixed(1) }} MB
              ({{ uploadProgress.percent }}%)
            </div>
          </div>

          <div v-if="preview.coverUrl || preview.videoUrl" class="grid two">
            <div v-if="preview.videoUrl" class="preview-card">
              <div class="subtle">视频预览</div>
              <video class="video" :src="preview.videoUrl" controls playsinline preload="metadata" />
            </div>
            <div v-if="preview.coverUrl" class="preview-card">
              <div class="subtle">封面预览</div>
              <img class="cover" :src="preview.coverUrl" alt="cover preview" />
            </div>
          </div>

          <p v-if="canPublish" class="subtle" role="status">视频处理完成，可以发布。</p>
          <p v-if="uploadError" class="subtle" role="alert">{{ uploadError }}</p>
          <div class="row" style="justify-content: flex-end; margin-top: 8px">
            <button v-if="asset?.status === 'failed'" type="button" :disabled="busy" @click="resetAsset">放弃并重新上传</button>
            <button v-else class="primary big-btn" type="button" :disabled="busy || mediaEnabled === null || !publishForm.video" @click="onPublish">
              {{ mediaEnabled && !canPublish ? (asset ? '继续处理' : '上传视频') : '发布' }}
            </button>
          </div>
        </div>

        <div v-if="published" class="card" style="margin-top: 14px">
          <p class="title">已发布</p>
          <div class="row" style="justify-content: space-between">
            <div>
              <div class="title" style="margin: 0">{{ published.title }}</div>
              <div class="subtle mono">#{{ published.id }}</div>
            </div>
            <div class="row">
              <RouterLink class="pill" :to="`/video/${published.id}`">去播放</RouterLink>
              <a class="pill mono" :href="published.play_url" target="_blank" rel="noreferrer">play_url</a>
              <a class="pill mono" :href="published.cover_url" target="_blank" rel="noreferrer">cover_url</a>
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppShell>
</template>

<style scoped>
.publish-wrap {
  display: grid;
  justify-items: center;
}

.publish-card {
  width: min(980px, 100%);
  padding: 22px;
}

.form-grid {
  gap: 16px;
}

.form-grid .grid.two {
  gap: 20px;
}

.form-grid .grid.two > * {
  min-width: 0;
}

.form-grid input[type='file'] {
  max-width: 100%;
}

.file-native {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}

.file-box {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  border: 1px solid rgba(255, 255, 255, 0.12);
  background: rgba(255, 255, 255, 0.06);
  border-radius: 14px;
  min-height: 46px;
}

.file-box button {
  padding: 8px 10px;
  border-radius: 12px;
}

.file-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13px;
  color: rgba(255, 255, 255, 0.88);
}

.muted {
  color: rgba(255, 255, 255, 0.55);
}

.big-input {
  box-sizing: border-box;
  width: 100%;
  max-width: 100%;
  padding: 12px 14px;
  font-size: 14px;
  border-radius: 14px;
}

.big-btn {
  padding: 12px 18px;
  font-size: 14px;
  border-radius: 14px;
}

.preview-card {
  border: 1px solid rgba(255, 255, 255, 0.12);
  background: rgba(255, 255, 255, 0.05);
  border-radius: 16px;
  padding: 12px;
  display: grid;
  gap: 10px;
}

.cover {
  width: 100%;
  aspect-ratio: 9/12;
  object-fit: cover;
  border-radius: 14px;
  border: 1px solid rgba(255, 255, 255, 0.1);
  background: rgba(0, 0, 0, 0.35);
}

.video {
  width: 100%;
  border-radius: 14px;
  border: 1px solid rgba(255, 255, 255, 0.1);
  background: rgba(0, 0, 0, 0.35);
}

.progress-wrap {
  display: grid;
  gap: 6px;
}

.progress-bar {
  height: 8px;
  background: rgba(255, 255, 255, 0.1);
  border-radius: 4px;
  overflow: hidden;
}

.progress-fill {
  height: 100%;
  background: #4a9eff;
  border-radius: 4px;
  transition: width 0.2s ease;
}

.progress-text {
  font-size: 13px;
  color: rgba(255, 255, 255, 0.7);
}
</style>
