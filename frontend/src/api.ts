export interface ClientKey {
  id: string
  name: string
  account_id: string
  account_name?: string
  allow_fixed_anlas: boolean
  fixed_anlas_limit: number
  fixed_anlas_spent: number
  fixed_anlas_pending: number
  fixed_anlas_remaining: number
  allow_purchased_anlas: boolean
  purchased_anlas_limit: number
  purchased_anlas_spent: number
  purchased_anlas_pending: number
  purchased_anlas_remaining: number
  allow_opus: boolean
  allow_multi_image: boolean
  opus_limit_mode: 'images' | 'percent'
  opus_limit_percent: number
  opus_limit_images: number
  opus_effective_limit_images: number
  opus_used_images: number
  opus_pending_images: number
  opus_remaining_images: number
  allocated_anlas: number
  spent_anlas: number
  pending_anlas: number
  remaining_anlas: number
  queue_limit: number
  revoked: boolean
  key?: string
}

export interface AdminQuota {
  upstream_fixed_anlas: number
  upstream_purchased_anlas: number
  upstream_anlas: number
  projected_fixed_anlas: number
  projected_purchased_anlas: number
  allocated_fixed_anlas: number
  allocated_purchased_anlas: number
  allocated_remaining_anlas: number
  unallocated_fixed_anlas: number
  unallocated_purchased_anlas: number
  unallocated_anlas: number
  active: boolean
  isGracePeriod: boolean
  tier: number
  projected_opus_percent: number
  snapshot_age_seconds: number
  queue_length: number
  unlimited_fixed_keys: number
  unlimited_purchased_keys: number
  unlimited_opus_keys: number
  account_quotas: AccountQuota[]
  account_errors: { account_id: string; name: string }[]
}

export interface Account {
  id: string
  name: string
  enabled: boolean
  token_configured: boolean
  key_count: number
}

export interface AccountQuota {
  account_id: string
  name: string
  upstream_fixed_anlas: number
  upstream_purchased_anlas: number
  projected_fixed_anlas: number
  projected_purchased_anlas: number
  allocated_fixed_anlas: number
  allocated_purchased_anlas: number
  unallocated_fixed_anlas: number
  unallocated_purchased_anlas: number
  projected_opus_percent: number
  active: boolean
  isGracePeriod: boolean
  tier: number
  snapshot_age_seconds: number
}

export interface KeyPolicy {
  name: string
  account_id: string
  allow_fixed_anlas: boolean
  fixed_anlas_limit: number
  allow_purchased_anlas: boolean
  purchased_anlas_limit: number
  allow_opus: boolean
  allow_multi_image: boolean
  opus_limit_mode: 'images' | 'percent'
  opus_limit_percent: number
  opus_limit_images: number
  queue_limit: number
}

export interface AdminSettings {
  allow_multi_image: boolean
}

export interface QueueEntry {
  position?: number
  key_id?: string
  key_name: string
  route: string
  queued_at: string
  started_at?: string
}

export interface QueueState {
  capacity: number
  active: QueueEntry | null
  waiting: QueueEntry[]
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message)
  }
}

const baseUrl = (import.meta.env.VITE_API_BASE_URL || '').replace(/\/$/, '')

async function request<T>(key: string, path: string, method = 'GET', body?: unknown): Promise<T> {
  let response: Response
  try {
    response = await fetch(`${baseUrl}${path}`, {
      method,
      headers: {
        Authorization: `Bearer ${key}`,
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
    })
  } catch {
    throw new ApiError(0, '无法连接管理 API，请检查服务地址与跨域配置。')
  }
  if (!response.ok) {
    const detail = (await response.text()).trim()
    const message = response.status === 401
      ? '管理员密钥无效。'
      : response.status === 502
        ? '官方额度暂时不可用。'
        : detail || `请求失败 (${response.status})`
    throw new ApiError(response.status, message)
  }
  if (response.status === 204) return undefined as T
  return response.json() as Promise<T>
}

export const api = {
  accounts: (key: string) => request<Account[]>(key, '/admin/accounts'),
  createAccount: (key: string, input: { name: string; token: string; enabled: boolean }) => request<Account>(key, '/admin/accounts', 'POST', input),
  updateAccount: (key: string, id: string, input: { name?: string; token?: string; enabled?: boolean }) => request<Account>(key, `/admin/accounts/${id}`, 'PUT', input),
  deleteAccount: (key: string, id: string) => request<void>(key, `/admin/accounts/${id}`, 'DELETE'),
  refreshAccountQuota: (key: string, id: string) => request<AccountQuota>(key, `/admin/accounts/${id}/quota/refresh`, 'POST'),
  settings: (key: string) => request<AdminSettings>(key, '/admin/settings'),
  updateSettings: (key: string, settings: AdminSettings) => request<AdminSettings>(key, '/admin/settings', 'PUT', settings),
  keys: (key: string) => request<ClientKey[]>(key, '/admin/keys'),
  quota: (key: string) => request<AdminQuota>(key, '/admin/quota'),
  queue: (key: string) => request<QueueState>(key, '/admin/queue'),
  refreshQuota: (key: string) => request<AdminQuota>(key, '/admin/quota/refresh', 'POST'),
  createKey: (key: string, policy: KeyPolicy) => request<{ key: string; client: ClientKey }>(key, '/admin/keys', 'POST', policy),
  updateKey: (key: string, id: string, policy: KeyPolicy) => request<ClientKey>(key, `/admin/keys/${id}`, 'PUT', policy),
  revokeKey: (key: string, id: string) => request<void>(key, `/admin/keys/${id}`, 'DELETE'),
  rotateKey: (key: string, id: string) => request<{ key: string; client: ClientKey }>(key, `/admin/keys/${id}/rotate`, 'POST'),
  reconcile: (key: string, id: string, charged: number, opus: number) => request<ClientKey>(key, `/admin/keys/${id}/reconcile`, 'POST', { charged_anlas: charged, opus_charged_images: opus }),
}

export const apiAddress = baseUrl || window.location.origin
