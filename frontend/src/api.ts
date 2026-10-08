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
  allow_fallback_high_steps: boolean
  archive_enabled: boolean
  opus_limit_mode: 'images' | 'percent'
  opus_limit_percent: number
  opus_limit_images: number
  opus_effective_limit_images: number
  opus_used_images: number
  opus_pending_images: number
  opus_remaining_images: number
  opus_predicted: boolean
  opus_confirmed_at?: string
  opus_pending_by_account?: Record<string, number>
  opus_share_warning: boolean
  successful_generations: number
  successful_images: number
  formula_anlas: number
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
  known_account_count: number
  unknown_balance_account_count: number
}

export interface Account {
  id: string
  name: string
  enabled: boolean
  token_configured: boolean
  key_count: number
  provider: 'novelai' | 'new_api'
  origin?: string
  enabled_models?: string[]
  fallback_account_id?: string
  fallback_high_steps: boolean
  fallback_reference_count: number
}

export interface AccountInput {
  name: string
  token: string
  enabled: boolean
  provider: Account['provider']
  origin?: string
  enabled_models?: string[]
  fallback_account_id?: string
  fallback_high_steps?: boolean
}

export interface AccountQuota {
  account_id: string
  name: string
  provider: Account['provider']
  upstream_balance_known: boolean
  upstream_fixed_anlas: number | null
  upstream_purchased_anlas: number | null
  projected_fixed_anlas: number | null
  projected_purchased_anlas: number | null
  allocated_fixed_anlas: number
  allocated_purchased_anlas: number
  unallocated_fixed_anlas: number | null
  unallocated_purchased_anlas: number | null
  projected_opus_percent: number | null
  allocated_opus_images: number
  unallocated_opus_images: number | null
  opus_predicted: boolean
  opus_confirmed_at: string
  opus_next_percent_at: string
  active: boolean
  isGracePeriod: boolean
  tier: number | null
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
  allow_fallback_high_steps: boolean
  archive_enabled: boolean
  opus_limit_mode: 'images' | 'percent'
  opus_limit_percent: number
  opus_limit_images: number
  queue_limit: number
}

export interface AdminSettings {
  charge_pending_as_spent: boolean
  allow_multi_image: boolean
  archive_enabled: boolean
  archive_retention_days: number
  archive_max_bytes: number
  admin_ui_path: string
  logs_enabled: boolean
  log_retention_days: number
  log_max_bytes: number
}

export interface RequestLog {
  id: string
  request_id: string
  client_ip?: string
  job_id?: string
  created_at: string
  completed_at: string
  duration_ms: number
  queue_ms: number
  route: string
  key_id: string
  key_name: string
  source_account_id: string
  account_id?: string
  account_name?: string
  provider?: string
  upstream_host?: string
  model?: string
  action?: string
  width?: number
  height?: number
  steps?: number
  n_samples?: number
  strength?: number
  upscaled_enhance: boolean
  stream: boolean
  multipart: boolean
  use_new_shared_trial?: boolean
  status: number
  upstream_status?: number
  stream_error_code?: number
  outcome: string
  error_source?: string
  error?: string
  interrupted: boolean
  response_bytes: number
  reserved_anlas: number
  reserved_opus: number
  formula_anlas: number
  billing_state: string
  key_fixed_remaining: number
  key_purchased_remaining: number
  key_opus_remaining: number
}

export interface RequestLogList { items: RequestLog[]; next_cursor?: string }
export interface RequestLogStats { enabled: boolean; bytes: number; max_bytes: number; retention_days: number; files: number; failures: number; last_error: string }

export interface ArchiveImage {
  id: string
  group_id: string
  group_size: number
  key_id: string
  key_name: string
  ip: string
  created_at: string
  bytes: number
}

export interface ArchiveList { items: ArchiveImage[]; total: number; page: number; page_size: number }
export interface ArchiveStats { count: number; bytes: number; pending: number; failures: number; last_error: string; ever_archived: boolean }
export interface ArchiveIPs { items: { ip: string; count: number }[]; truncated: boolean }
export interface ArchiveOverview {
  count: number
  bytes: number
  ip_count: number
  key_count: number
  group_count: number
  hours: { date: string; hour: number; count: number }[]
  keys: { key_id: string; key_name: string; count: number; bytes: number; ip_count: number; latest_at: string }[]
}

export interface UsageHours { hours: { date: string; hour: number; count: number; generations: number }[] }

export interface QueueEntry {
  id?: string
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
  logs: (key: string, params: URLSearchParams) => request<RequestLogList>(key, `/admin/logs?${params}`),
  logStats: (key: string) => request<RequestLogStats>(key, '/admin/logs/stats'),
  clearLogs: (key: string) => request<void>(key, '/admin/logs', 'DELETE'),
  accounts: (key: string) => request<Account[]>(key, '/admin/accounts'),
  createAccount: (key: string, input: AccountInput) => request<Account>(key, '/admin/accounts', 'POST', input),
  updateAccount: (key: string, id: string, input: AccountInput) => request<Account>(key, `/admin/accounts/${id}`, 'PUT', input),
  deleteAccount: (key: string, id: string) => request<void>(key, `/admin/accounts/${id}`, 'DELETE'),
  refreshAccountQuota: (key: string, id: string) => request<AccountQuota>(key, `/admin/accounts/${id}/quota/refresh`, 'POST'),
  settings: (key: string) => request<AdminSettings>(key, '/admin/settings'),
  updateSettings: (key: string, settings: Partial<AdminSettings>) => request<AdminSettings>(key, '/admin/settings', 'PUT', settings),
  images: (key: string, params: URLSearchParams) => request<ArchiveList>(key, `/admin/images?${params}`),
  imageStats: (key: string) => request<ArchiveStats>(key, '/admin/images/stats'),
  imageIPs: (key: string, search: string) => request<ArchiveIPs>(key, `/admin/images/ips?q=${encodeURIComponent(search)}`),
  imageOverview: (key: string, params: URLSearchParams) => request<ArchiveOverview>(key, `/admin/images/overview?${params}`),
  usageHours: (key: string, params: URLSearchParams) => request<UsageHours>(key, `/admin/usage/hours?${params}`),
  deleteImage: (key: string, id: string) => request<void>(key, `/admin/images/${id}`, 'DELETE'),
  keys: (key: string) => request<ClientKey[]>(key, '/admin/keys'),
  quota: (key: string) => request<AdminQuota>(key, '/admin/quota'),
  queue: (key: string) => request<QueueState>(key, '/admin/queue'),
  refreshQuota: (key: string) => request<AdminQuota>(key, '/admin/quota/refresh', 'POST'),
  createKey: (key: string, policy: KeyPolicy) => request<{ key: string; client: ClientKey }>(key, '/admin/keys', 'POST', policy),
  updateKey: (key: string, id: string, policy: KeyPolicy) => request<ClientKey>(key, `/admin/keys/${id}`, 'PUT', policy),
  revokeKey: (key: string, id: string) => request<void>(key, `/admin/keys/${id}`, 'DELETE'),
  rotateKey: (key: string, id: string) => request<{ key: string; client: ClientKey }>(key, `/admin/keys/${id}/rotate`, 'POST'),
  reconcile: (key: string, id: string, charged: number, opus: number, byAccount?: Record<string, number>) => request<ClientKey>(key, `/admin/keys/${id}/reconcile`, 'POST', { charged_anlas: charged, opus_charged_images: opus, ...(byAccount ? { opus_charged_by_account: byAccount } : {}) }),
}

export async function imageBlob(key: string, id: string, kind: 'thumbnail' | 'original'): Promise<Blob> {
  const response = await fetch(`${baseUrl}/admin/images/${id}/${kind}`, { headers: { Authorization: `Bearer ${key}` }, cache: 'no-store' })
  if (!response.ok) throw new ApiError(response.status, `图片加载失败 (${response.status})`)
  return response.blob()
}

export const apiAddress = baseUrl || window.location.origin
