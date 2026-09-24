import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Button } from '@astryxdesign/core/Button'
import { Clock3, KeyRound, ListOrdered, RefreshCw, Workflow } from 'lucide-react'
import { api, ApiError, type ClientKey, type QueueEntry, type QueueState } from './api'

const number = new Intl.NumberFormat('zh-CN')
const limitText = (value: number) => value === -1 ? '不限' : number.format(value)

function elapsed(when?: string) {
  if (!when) return '—'
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(when).getTime()) / 1000))
  if (seconds < 60) return `${seconds} 秒`
  return `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`
}

function routeName(route: string) {
  if (route.includes('/generate-image')) return '图像生成'
  if (route.includes('/upscale')) return '图像超分'
  if (route.includes('/encode-vibe')) return 'Vibe 编码'
  if (route.includes('/augment-image')) return '图像增强'
  if (route.includes('/subscription') || route.includes('/quota')) return '额度查询'
  return route.startsWith('GET /admin/') || route.startsWith('POST /admin/') || route.startsWith('PUT /admin/') ? '管理操作' : route
}

export function QueuePage({ adminKey, keys, onAuthFailure }: { adminKey: string; keys: ClientKey[]; onAuthFailure: () => void }) {
  const [state, setState] = useState<QueueState | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [refreshing, setRefreshing] = useState(false)
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)
  const inFlight = useRef(false)

  const refresh = useCallback(async () => {
    if (inFlight.current) return
    inFlight.current = true
    setRefreshing(true)
    try {
      setState(await api.queue(adminKey))
      setUpdatedAt(new Date())
      setError(null)
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) onAuthFailure()
      else setError(cause instanceof Error ? cause.message : '无法读取队列。')
    } finally {
      inFlight.current = false
      setRefreshing(false)
    }
  }, [adminKey, onAuthFailure])

  useEffect(() => {
    void refresh()
    const timer = window.setInterval(() => { if (!document.hidden) void refresh() }, 5000)
    const onVisible = () => { if (!document.hidden) void refresh() }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [refresh])

  const keyByID = useMemo(() => new Map(keys.map((key) => [key.id, key])), [keys])
  const occupancy = useMemo(() => {
    const counts = new Map<string, { name: string; count: number }>()
    for (const item of state?.waiting ?? []) {
      const id = item.key_id || ''
      const current = counts.get(id)
      counts.set(id, { name: item.key_name, count: (current?.count ?? 0) + 1 })
    }
    return [...counts].sort((a, b) => b[1].count - a[1].count)
  }, [state])
  const waiting = state?.waiting ?? []
  const capacity = state?.capacity ?? 64

  const keyMeta = (item: QueueEntry) => {
    const key = item.key_id ? keyByID.get(item.key_id) : undefined
    return <div className="queue-key"><strong>{item.key_name}</strong><small>{key ? key.id : item.key_id || '系统任务'}</small></div>
  }

  return <>
    <div className="page-intro"><div><span className="eyebrow">REQUEST QUEUE</span><h1>任务队列</h1><p>{updatedAt ? `更新于 ${updatedAt.toLocaleTimeString('zh-CN')}` : '读取中'}</p></div><Button label="刷新队列" variant="secondary" icon={<RefreshCw size={16} />} isLoading={refreshing} onClick={() => void refresh()} /></div>
    {error && <div className="inline-alert" role="alert">{error}</div>}
    <div className="metric-grid three">
      <div className="metric"><div className="metric-top"><span className="metric-icon green"><Workflow size={18} /></span><span className="metric-label">正在执行</span></div><strong>{state?.active ? '1' : '0'} <span className="queue-metric-suffix">/ 1</span></strong><span className="metric-detail">{state?.active ? state.active.key_name : '当前空闲'}</span></div>
      <div className="metric"><div className="metric-top"><span className="metric-icon blue"><ListOrdered size={18} /></span><span className="metric-label">等待中</span></div><strong>{number.format(waiting.length)} <span className="queue-metric-suffix">/ {number.format(capacity)}</span></strong><span className="metric-detail">全局等待位置</span></div>
      <div className="metric"><div className="metric-top"><span className="metric-icon orange"><KeyRound size={18} /></span><span className="metric-label">排队密钥</span></div><strong>{number.format(occupancy.filter(([id]) => id !== '').length)}</strong><span className="metric-detail">当前占用等待位置</span></div>
    </div>
    <section className="page-section"><div className="section-heading"><div><span className="eyebrow">ACTIVE REQUEST</span><h2>正在执行</h2></div></div>
      {state?.active ? <div className="queue-active"><span className="queue-live"><span className="status-dot" /> 执行中</span>{keyMeta(state.active)}<span className="queue-route" title={state.active.route}>{routeName(state.active.route)}<small>{state.active.route}</small></span><span className="queue-time"><Clock3 size={15} /> {elapsed(state.active.started_at)}</span></div> : <div className="queue-empty">暂无正在执行的请求</div>}
    </section>
    <section className="page-section"><div className="section-heading"><div><span className="eyebrow">WAITING POOL</span><h2>等待池</h2></div><span className="queue-capacity">{waiting.length} / {capacity}</span></div>
      <div className="queue-track" role="progressbar" aria-label="全局队列占用" aria-valuenow={waiting.length} aria-valuemin={0} aria-valuemax={capacity}><span style={{ width: `${Math.min(100, waiting.length / capacity * 100)}%` }} /></div>
      {waiting.length ? <div className="queue-list"><div className="queue-row queue-row-head"><span>位置</span><span>密钥</span><span>请求</span><span>等待时间</span><span>此密钥上限</span></div>{waiting.slice(0, 200).map((item) => <div className="queue-row" key={`${item.position}-${item.queued_at}`}><strong className="queue-position">{String(item.position).padStart(2, '0')}</strong>{keyMeta(item)}<span className="queue-route" title={item.route}>{routeName(item.route)}<small>{item.route}</small></span><span className="queue-time">{elapsed(item.queued_at)}</span><span className="queue-limit">{item.key_id ? limitText(keyByID.get(item.key_id)?.queue_limit ?? -1) : '—'}</span></div>)}</div> : <div className="queue-empty">等待池为空</div>}
      {waiting.length > 200 && <div className="table-footer">另有 {number.format(waiting.length - 200)} 项等待中</div>}
    </section>
    {occupancy.length > 0 && <section className="page-section"><div className="section-heading"><div><span className="eyebrow">BY KEY</span><h2>密钥占用</h2></div></div><div className="queue-occupancy">{occupancy.map(([id, item]) => {
      const limit = id ? keyByID.get(id)?.queue_limit ?? -1 : -1
      const maximum = limit >= 0 ? Math.max(1, limit) : capacity
      return <div className="queue-occupancy-row" key={id}><span title={item.name}>{item.name}</span><div className="queue-track"><span style={{ width: `${Math.min(100, item.count / maximum * 100)}%` }} /></div><strong>{item.count} / {limitText(limit)}</strong></div>
    })}</div></section>}
  </>
}
