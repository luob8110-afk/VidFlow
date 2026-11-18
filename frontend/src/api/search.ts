import { postJson } from './client'
import type { Video } from './types'

export type SearchVideo = Pick<Video, 'id' | 'author_id' | 'username' | 'title' | 'description' | 'cover_url' | 'create_time'>

type SearchQuery = {
  q: string
  tag: string
  author_id?: number
  published_from?: number
  published_before?: number
  cursor?: string
}

export function searchVideos(query: SearchQuery, signal: AbortSignal) {
  return postJson<{ video_list: SearchVideo[]; next_cursor?: string }>('/search/videos', query, { signal })
}
