import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { Button } from '@astryxdesign/core/Button'
import { Dialog } from '@astryxdesign/core/Dialog'
import { Copy, Download, Eye, RefreshCw, Trash2, X } from 'lucide-react'
import { api, ApiError, type Account, type ClientKey, type RequestLog, type RequestLogList, type RequestLogStats } from './api'
import { useAutoRefresh } from './useAutoRefresh'

type Filters = { key: string; account: string; route: string; status: string; query: string; from: string; to: string; errors: boolean }
const emptyFilters: Filters = { key: '', account: '', route: '', status: '', query: '', from: '', to: '', errors: true }
const outcomeNames: Record<string, string> = { success: '成功', rejected: '拒绝', uncertain: '结果未确认' }
const billingNames: Record<string, string> = { not_reserved: '未预留', pending: '待核对', charged: '计入预算', charged_uncertain: '未确认 · 已计费', refunded: '已退还预留', accounting_failed: '记账失败' }
const sourceNames: Record<string, string> = { proxy: '代理', upstream: '上游', accounting: '记账' }
const remaining = (value: number) => value === -1 ? '不限' : String(value)

async function copyDetails(entry: RequestLog) {
  const text = JSON.stringify(entry, null, 2)
  if (navigator.clipboard) { await navigator.clipboard.writeText(text); return }
  const field = document.createElement('textarea')
  field.value = text
  field.style.position = 'fixed'
  field.style.opacity = '0'
  document.body.appendChild(field)
  try { field.select(); if (!document.execCommand('copy')) throw new Error('Copy unavailable') }
  finally { field.remove() }
}

function exportLogs(items: RequestLog[]) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(items, null, 2)], { type: 'application/json' }))
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `request-logs-${new Date().toISOString().replaceAll(':', '-')}.json`
  anchor.click()
  URL.revokeObjectURL(url)
}

function LogDetails({ entry, onClose }: { entry: RequestLog; onClose: () => void }) {
  const [copyError, setCopyError] = useState('')
  const [copied, setCopied] = useState(false)
  const details = [
    ['请求 ID', entry.request_id], ['时间', new Date(entry.created_at).toLocaleString('zh-CN')],
    ['路径', entry.route], ['密钥', `${entry.key_name} (${entry.key_id})`], ['客户端 IP', entry.client_ip || '—'], ['绑定账号', entry.source_account_id],
    ['实际账号', entry.account_name ? `${entry.account_name} (${entry.account_id})` : '未选择'], ['上游', entry.upstream_host || '未转发'],
    ['模型 / 操作', `${entry.model || '—'} / ${entry.upscaled_enhance ? 'Enhance Max' : entry.action || '—'}`],
    ['尺寸 / steps / 张数', `${entry.width || '—'} × ${entry.height || '—'} / ${entry.steps || '—'} / ${entry.n_samples || '—'}`],
    ['传输', `${entry.stream ? '流式' : '普通'} / ${entry.multipart ? 'multipart' : 'JSON'}`],
    ['HTTP / 流内错误', `${entry.status} / ${entry.stream_error_code || '无'}`], ['错误来源', entry.error_source ? sourceNames[entry.error_source] || entry.error_source : '无'],
    ['耗时 / 排队', `${(entry.duration_ms / 1000).toFixed(2)} 秒 / ${(entry.queue_ms / 1000).toFixed(2)} 秒`],
    ['预算处理', billingNames[entry.billing_state] || entry.billing_state], ['预留 / 公式参考', `${entry.reserved_anlas} 点 + ${entry.reserved_opus} 次 Opus / ${entry.formula_anlas} 点`],
    ['请求时 key 剩余额度', `订阅 ${remaining(entry.key_fixed_remaining)} / 购入 ${remaining(entry.key_purchased_remaining)} / Opus ${remaining(entry.key_opus_remaining)}`],
  ]
  return <Dialog isOpen onOpenChange={(open) => { if (!open) onClose() }} width="min(96vw, 900px)" maxHeight="92dvh" aria-label="请求日志详情">
    <div className="log-details"><div className="section-heading"><h2>请求日志详情</h2><button type="button" className="text-link" aria-label="关闭详情" onClick={onClose}><X size={20} /></button></div>
      <dl>{details.map(([name, value]) => <div key={name}><dt>{name}</dt><dd>{value}</dd></div>)}</dl>
      {entry.error && <div className="log-error"><strong>错误信息</strong><pre>{entry.error}</pre></div>}
      <details><summary>完整脱敏记录</summary><pre className="log-json">{JSON.stringify(entry, null, 2)}</pre></details>
      {copyError && <p role="alert" className="form-error">{copyError}</p>}
      <div className="modal-actions"><Button label={copied ? '已复制' : '复制详情'} variant="secondary" icon={<Copy size={16} />} onClick={() => { void copyDetails(entry).then(() => { setCopied(true); setCopyError('') }).catch(() => setCopyError('复制失败，可以使用“导出当前页”。')) }} /><Button label="关闭" variant="primary" onClick={onClose} /></div>
    </div>
  </Dialog>
}

export function LogsPage({ adminKey, keys, accounts, onAuthFailure }: { adminKey: string; keys: ClientKey[]; accounts: Account[]; onAuthFailure: () => void }) {
  const [draft, setDraft] = useState<Filters>(emptyFilters)
  const [filters, setFilters] = useState<Filters>(emptyFilters)
  const [cursors, setCursors] = useState<string[]>([''])
  const [page, setPage] = useState(0)
  const [list, setList] = useState<RequestLogList | null>(null)
  const [stats, setStats] = useState<RequestLogStats | null>(null)
  const [selected, setSelected] = useState<RequestLog | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [clearing, setClearing] = useState(false)
  const requestNumber = useRef(0)
  const inFlight = useRef(0)
  const cursor = cursors[page] || ''

  const load = useCallback(async (background = false) => {
    if (background && inFlight.current > 0) return
    const request = ++requestNumber.current
    ++inFlight.current
    if (!background) setLoading(true)
    try {
      const params = new URLSearchParams({ limit: '50', errors_only: String(filters.errors) })
      if (cursor) params.set('before', cursor)
      for (const [name, value] of [['key_id', filters.key], ['account_id', filters.account], ['route', filters.route], ['status', filters.status], ['q', filters.query]]) if (value) params.set(name, value)
      if (filters.from) params.set('from', new Date(filters.from).toISOString())
      if (filters.to) params.set('to', new Date(filters.to).toISOString())
      const [logs, info] = await Promise.allSettled([api.logs(adminKey, params), api.logStats(adminKey)])
      if (request !== requestNumber.current) return
      if ([logs, info].some((item) => item.status === 'rejected' && item.reason instanceof ApiError && item.reason.status === 401)) { onAuthFailure(); return }
      if (info.status === 'fulfilled') setStats(info.value)
      if (logs.status === 'rejected') throw logs.reason
      setList(logs.value)
      setError('')
    } catch (cause) {
      if (request === requestNumber.current) setError(cause instanceof Error ? cause.message : '日志加载失败')
    } finally {
      --inFlight.current
      if (request === requestNumber.current) setLoading(false)
    }
  }, [adminKey, cursor, filters, onAuthFailure])

  useEffect(() => { void load(); return () => { ++requestNumber.current } }, [load])
  const refresh = useCallback(() => load(true), [load])
  useAutoRefresh(refresh, page === 0 && !selected && !clearing, 5000, false)

  const apply = (event: FormEvent) => {
    event.preventDefault()
    if (draft.from && draft.to && new Date(draft.from) > new Date(draft.to)) { setError('结束时间不能早于开始时间'); return }
    setPage(0)
    setCursors([''])
    setFilters({ ...draft })
  }
  const clear = async () => {
    setClearing(true)
    try { await api.clearLogs(adminKey); setPage(0); setCursors(['']); setList({ items: [] }); await load() }
    catch (cause) { if (cause instanceof ApiError && cause.status === 401) onAuthFailure(); else setError(cause instanceof Error ? cause.message : '清空日志失败') }
    finally { setClearing(false) }
  }
  return <>
    <div className="page-intro"><div><span className="eyebrow">REQUEST LOGS</span><h1>请求日志</h1><p>查看参数、账号、错误来源和预算处理；详情可直接复制用于排查。</p></div><Button label="刷新日志" variant="secondary" icon={<RefreshCw size={17} />} isLoading={loading} onClick={() => { setPage(0); setCursors(['']); if (page === 0) void load() }} /></div>
    <section className="page-section"><div className="log-storage"><span>{stats ? `${(stats.bytes / 1048576).toFixed(2)} / ${(stats.max_bytes / 1048576).toFixed(0)} MiB · ${stats.retention_days === -1 ? '不限日期' : `保留 ${stats.retention_days} 天`} · ${stats.enabled ? '记录已开启' : '记录已关闭'}` : '正在加载保留策略…'}</span><div><Button label="导出当前页" variant="secondary" size="sm" icon={<Download size={15} />} isDisabled={!list?.items.length} onClick={() => exportLogs(list?.items ?? [])} /><Button label="清空日志" variant="secondary" size="sm" icon={<Trash2 size={15} />} isLoading={clearing} onClick={() => { if (window.confirm('清空全部请求日志？此操作无法恢复，用量统计不会受影响。')) void clear() }} /></div></div>
      {stats?.last_error && <p className="form-error" role="alert">日志写入或清理失败（{stats.failures} 次）：{stats.last_error}</p>}
      <form className="log-filters" onSubmit={apply}>
        <label>密钥<select value={draft.key} onChange={(event) => setDraft({ ...draft, key: event.target.value })}><option value="">全部密钥</option>{keys.map((key) => <option key={key.id} value={key.id}>{key.name}</option>)}</select></label>
        <label>实际账号<select value={draft.account} onChange={(event) => setDraft({ ...draft, account: event.target.value })}><option value="">全部账号</option>{accounts.map((account) => <option key={account.id} value={account.id}>{account.name}</option>)}</select></label>
        <label>HTTP 状态<select value={draft.status} onChange={(event) => setDraft({ ...draft, status: event.target.value })}><option value="">全部状态</option>{[200, 400, 402, 413, 429, 499, 500, 502, 503, 504].map((status) => <option key={status} value={status}>{status}</option>)}</select></label>
        <label>请求路径<input value={draft.route} maxLength={200} placeholder="例如 /ai/generate-image" onChange={(event) => setDraft({ ...draft, route: event.target.value })} /></label>
        <label>错误、模型或请求 ID<input value={draft.query} maxLength={200} placeholder="例如 Required: 18" onChange={(event) => setDraft({ ...draft, query: event.target.value })} /></label>
        <label>开始时间<input type="datetime-local" value={draft.from} onChange={(event) => setDraft({ ...draft, from: event.target.value })} /></label>
        <label>结束时间<input type="datetime-local" value={draft.to} onChange={(event) => setDraft({ ...draft, to: event.target.value })} /></label>
        <div className="log-filter-actions"><label className="check-line"><input type="checkbox" checked={draft.errors} onChange={(event) => setDraft({ ...draft, errors: event.target.checked })} />只看失败和未确认</label><Button label="应用筛选" type="submit" variant="primary" size="sm" /><button type="button" className="text-link" onClick={() => { setDraft(emptyFilters); setFilters({ ...emptyFilters }); setPage(0); setCursors(['']) }}>重置</button></div>
      </form>
    </section>
    {error && <div role="alert" className="inline-alert page-alert">{error}</div>}
    <section className="page-section"><div className="table-scroll"><table className="data-table log-table"><thead><tr><th>时间 / 密钥</th><th>请求 / 参数</th><th>实际账号</th><th>结果</th><th>预算 / 耗时</th><th>详情</th></tr></thead><tbody>{list?.items.map((entry) => <tr key={entry.id}>
      <td><strong>{entry.key_name}</strong><small>{new Date(entry.created_at).toLocaleString('zh-CN')}</small></td>
      <td><code>{entry.route}</code><small>{entry.model || '图片工具'} · {entry.upscaled_enhance ? 'Enhance Max' : entry.action || '—'}</small><small>{entry.width && entry.height ? `${entry.width} × ${entry.height}` : '—'}{entry.steps ? ` · ${entry.steps} steps` : ''}{entry.n_samples ? ` · ${entry.n_samples} 张` : ''} · {entry.stream ? '流式' : '普通'}</small></td>
      <td>{entry.account_name || '未转发'}<small>{entry.upstream_host || entry.source_account_id}</small></td>
      <td><span className={`log-outcome ${entry.outcome}`}>{entry.status}{entry.stream_error_code ? ` / 流内 ${entry.stream_error_code}` : ''} · {outcomeNames[entry.outcome] || entry.outcome}</span><small className="log-error-summary" title={entry.error}>{entry.error_source ? `${sourceNames[entry.error_source] || entry.error_source}：` : ''}{entry.error || '—'}</small></td>
      <td>{billingNames[entry.billing_state] || entry.billing_state}<small>预留 {entry.reserved_anlas} 点 · {(entry.duration_ms / 1000).toFixed(2)} 秒</small></td>
      <td><button type="button" className="text-link" aria-label={`查看 ${entry.key_name} 的请求详情`} onClick={() => setSelected(entry)}><Eye size={16} />查看</button></td>
    </tr>)}</tbody></table></div>{list?.items.length === 0 && <div className="empty-state">暂无匹配日志；日志开启后会记录新的生成和图片工具请求。</div>}
      <div className="log-pagination"><button type="button" disabled={page === 0 || loading} onClick={() => setPage(page - 1)}>较新</button><span>第 {page + 1} 页{page === 0 && !selected ? ' · 自动刷新' : ''}</span><button type="button" disabled={!list?.next_cursor || loading} onClick={() => { if (list?.next_cursor) { setCursors([...cursors.slice(0, page + 1), list.next_cursor]); setPage(page + 1) } }}>较早</button></div>
    </section>
    {selected && <LogDetails key={selected.id} entry={selected} onClose={() => setSelected(null)} />}
  </>
}
