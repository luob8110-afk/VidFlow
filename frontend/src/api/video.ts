import { deleteJson, getJson, postForm, postJson } from './client'
import { normalizeVideoList } from './normalize'
import type { Video } from './types'

type Publication = { title: string; description: string } &
  ({ asset_id: string } | { play_url: string; cover_url: string })

export function publishVideo(input: Publication, signal?: AbortSignal) {
  return postJson<Video>('/video/publish', input, { authRequired: true, signal })
}

export type MediaAsset = {
  asset_id: string
  status: 'uploading' | 'queued' | 'processing' | 'ready' | 'failed' | 'published' | 'expired'
  part_size: number
  part_count: number
  uploaded_parts: number[]
  video_id?: number
  failure?: string
}

export function mediaConfig(signal: AbortSignal) {
  return getJson<{ enabled: boolean }>('/video/assets/config', { signal })
}

export function initAsset(input: { draft_id: string; size: number; sha256: string }, signal: AbortSignal) {
  return postJson<MediaAsset>('/video/assets', input, { authRequired: true, signal })
}

export function signPart(id: string, part: number, signal: AbortSignal) {
  return postJson<{ url: string }>(`/video/assets/${id}/parts/${part}`, {}, { authRequired: true, signal })
}

export function completeAsset(id: string, signal: AbortSignal) {
  return postJson<MediaAsset>(`/video/assets/${id}/complete`, {}, { authRequired: true, signal })
}

export function assetStatus(id: string, signal: AbortSignal) {
  return getJson<MediaAsset>(`/video/assets/${id}`, { authRequired: true, signal })
}

export function discardAsset(id: string, signal: AbortSignal) {
  return deleteJson<null>(`/video/assets/${id}`, { authRequired: true, signal })
}

export type UploadResponse = { url: string; play_url?: string; cover_url?: string }

export function uploadVideo(file: File) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<UploadResponse>('/video/uploadVideo', fd, { authRequired: true })
}

export function uploadCover(file: File) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<UploadResponse>('/video/uploadCover', fd, { authRequired: true })
}

export async function listByAuthorId(authorId: number) {
  const videos = await postJson<Video[] | null>('/video/listByAuthorID', { author_id: authorId })
  return normalizeVideoList(videos)
}

export function getDetail(id: number) {
  return postJson<Video>('/video/getDetail', { id })
}

// --- Chunk Upload API ---

export type InitChunkUploadResponse = {
  upload_id: string
  uploaded_chunks: number[]
}

export function initChunkUpload(input: {
  filename: string
  file_size: number
  chunk_size: number
  total_chunks: number
  file_hash: string
}) {
  return postJson<InitChunkUploadResponse>('/video/chunk/init', input, { authRequired: true })
}

export function uploadChunk(uploadId: string, chunkIndex: number, chunkHash: string, blob: Blob) {
  const fd = new FormData()
  fd.append('upload_id', uploadId)
  fd.append('chunk_index', String(chunkIndex))
  fd.append('chunk_hash', chunkHash)
  fd.append('file', blob)
  return postForm<{ chunk_index: number }>('/video/chunk/upload', fd, { authRequired: true })
}

export function chunkStatus(uploadId: string) {
  return postJson<{ upload_id: string; uploaded_chunks: number[]; total_chunks: number }>(
    '/video/chunk/status',
    { upload_id: uploadId },
    { authRequired: true },
  )
}

export function completeChunkUpload(uploadId: string) {
  return postJson<UploadResponse>('/video/chunk/complete', { upload_id: uploadId }, { authRequired: true })
}
