import { useAuthStore } from '../stores/auth'
import { reportError } from '../utils/error-reporter'

export class ApiError extends Error {
  status: number
  payload?: unknown

  constructor(message: string, status: number, payload?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.payload = payload
  }
}

type ApiErrorBody = { error?: string }

const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'

let isRefreshing = false
let refreshPromise: Promise<string | null> | null = null

async function tryRefresh(): Promise<string | null> {
  const auth = useAuthStore()
  if (!auth.refreshToken) return null
  if (isRefreshing) return refreshPromise
  isRefreshing = true
  refreshPromise = (async () => {
    try {
      const res = await fetch(`${API_BASE}/account/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: auth.refreshToken }),
      })
      if (!res.ok) { auth.clearTokens(); return null }
      const data = await res.json()
      auth.setToken(data.token)
      return data.token as string
    } catch {
      auth.clearTokens()
      return null
    } finally {
      isRefreshing = false
    }
  })()
  return refreshPromise
}

type RequestOptions = { authRequired?: boolean; signal?: AbortSignal }

export function postJson<T>(path: string, body: unknown, options?: RequestOptions): Promise<T> {
  return requestJson<T>('POST', path, body ?? {}, options)
}

export function getJson<T>(path: string, options?: RequestOptions): Promise<T> {
  return requestJson<T>('GET', path, undefined, options)
}

export function deleteJson<T>(path: string, options?: RequestOptions): Promise<T> {
  return requestJson<T>('DELETE', path, undefined, options)
}

async function requestJson<T>(method: string, path: string, body: unknown, options?: RequestOptions): Promise<T> {
  const auth = useAuthStore()
  const token = auth.token

  if (options?.authRequired && !token) {
    throw new ApiError('需要先登录（缺少 token）', 401)
  }

  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (token) headers.Authorization = `Bearer ${token}`

  const send = () => fetch(`${API_BASE}${path}`, {
    method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal: options?.signal,
  })
  const res = await send()

  if (res.status === 401 && path !== '/account/refresh') {
    options?.signal?.throwIfAborted()
    const newToken = await tryRefresh()
    options?.signal?.throwIfAborted()
    if (newToken) {
      headers.Authorization = `Bearer ${newToken}`
      const retryRes = await send()
      return handleResponse<T>(retryRes, path)
    }
  }

  return handleResponse<T>(res, path)
}

export async function postForm<T>(path: string, body: FormData, options?: { authRequired?: boolean }): Promise<T> {
  const auth = useAuthStore()
  const token = auth.token

  if (options?.authRequired && !token) {
    throw new ApiError('需要先登录（缺少 token）', 401)
  }

  const headers: Record<string, string> = {}
  if (token) headers.Authorization = `Bearer ${token}`

  const res = await fetch(`${API_BASE}${path}`, {
    method: 'POST',
    headers,
    body,
  })

  if (res.status === 401 && path !== '/account/refresh') {
    const newToken = await tryRefresh()
    if (newToken) {
      headers.Authorization = `Bearer ${newToken}`
      const retryRes = await fetch(`${API_BASE}${path}`, {
        method: 'POST', headers, body,
      })
      return handleResponse<T>(retryRes, path)
    }
  }

  return handleResponse<T>(res, path)
}

async function handleResponse<T>(res: Response, path: string): Promise<T> {
  const auth = useAuthStore()
  const text = await res.text()
  let data: unknown = null
  if (text) {
    try { data = JSON.parse(text) } catch { data = text }
  }

  if (!res.ok) {
    if (res.status === 401) auth.clearTokens()
    const msg = data && typeof data === 'object' && (data as ApiErrorBody).error
      ? String((data as ApiErrorBody).error)
      : `请求失败 (${res.status})`
    const apiErr = new ApiError(msg, res.status, data)
    reportError(apiErr, { path, status: res.status })
    throw apiErr
  }

  return data as T
}
