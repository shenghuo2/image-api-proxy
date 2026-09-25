import { useCallback, useEffect, useState } from 'react'
import { Button } from '@astryxdesign/core/Button'
import { RefreshCw } from 'lucide-react'
import { api, type UsageHours } from './api'

const number = new Intl.NumberFormat('zh-CN')

function localDate(date: Date) {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
}

export function UsageHeatmap({ adminKey }: { adminKey: string }) {
  const [data, setData] = useState<UsageHours | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    const today = new Date()
    const start = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 6)
    const end = new Date(today.getFullYear(), today.getMonth(), today.getDate() + 1)
    const params = new URLSearchParams({ from: start.toISOString(), to: end.toISOString(), offset_minutes: String(today.getTimezoneOffset()) })
    try {
      setData(await api.usageHours(adminKey, params))
      setError('')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '生成热力图加载失败')
    } finally {
      setLoading(false)
    }
  }, [adminKey])

  useEffect(() => { void load() }, [load])

  const today = new Date()
  const days = Array.from({ length: 7 }, (_, index) => new Date(today.getFullYear(), today.getMonth(), today.getDate() - 6 + index))
  const counts = new Map(data?.hours.map((item) => [`${item.date}-${item.hour}`, item.count]))
  const maximum = Math.max(0, ...Array.from(counts.values()))

  return <section className="page-section usage-activity">
    <div className="section-heading"><div><h2>生成时间分布</h2><span className="usage-activity-caption">最近 7 天 · 本地时间 · 从此版本开始记录</span></div><Button label="刷新热力图" variant="secondary" size="sm" icon={<RefreshCw size={15} />} isLoading={loading} onClick={() => void load()} /></div>
    {error && <div className="inline-alert" role="alert">{error}</div>}
    <div className="archive-panel-heading"><strong>每小时成功生成图片</strong><span>峰值 {number.format(maximum)} 张 / 小时</span></div>
    <div className="archive-heatmap-scroll"><div className="archive-heatmap" role="img" aria-label="最近七天每小时成功生成图片数量热力图">
      <div className="archive-heatmap-hours"><span />{Array.from({ length: 24 }, (_, hour) => <span key={hour}>{hour % 6 === 0 ? `${hour}:00` : ''}</span>)}</div>
      {days.map((day) => <div className="archive-heatmap-row" key={localDate(day)}><span className="archive-heatmap-day">{day.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' })}</span>{Array.from({ length: 24 }, (_, hour) => {
        const count = counts.get(`${localDate(day)}-${hour}`) ?? 0
        const level = count === 0 ? 0 : Math.max(1, Math.ceil(count / maximum * 4))
        return <span className={`archive-heatmap-cell level-${level}`} key={hour} title={`${localDate(day)} ${String(hour).padStart(2, '0')}:00 · ${number.format(count)} 张`} />
      })}</div>)}
    </div></div>
    <div className="archive-heatmap-legend"><span>少</span>{[0, 1, 2, 3, 4].map((level) => <i className={`level-${level}`} key={level} />)}<span>多</span></div>
  </section>
}
