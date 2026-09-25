import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { AlertDialog } from '@astryxdesign/core/AlertDialog'
import { Button } from '@astryxdesign/core/Button'
import type { ISODateString } from '@astryxdesign/core/Calendar'
import { DateInput } from '@astryxdesign/core/DateInput'
import { Dialog } from '@astryxdesign/core/Dialog'
import { MultiSelector } from '@astryxdesign/core/MultiSelector'
import { Pagination } from '@astryxdesign/core/Pagination'
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl'
import { Selector } from '@astryxdesign/core/Selector'
import { TextInput } from '@astryxdesign/core/TextInput'
import { Download, Eye, Filter, Image as ImageIcon, RefreshCw, Search, Trash2, X } from 'lucide-react'
import { api, imageBlob, type ArchiveImage, type ArchiveIPs, type ArchiveList, type ArchiveOverview, type ArchiveStats, type ClientKey } from './api'

type ArchiveFilters = { keyID: string; ips: string[]; ipMode: 'include' | 'exclude'; from?: ISODateString; to?: ISODateString }

const emptyFilters = (): ArchiveFilters => ({ keyID: 'all', ips: [], ipMode: 'include' })
const number = new Intl.NumberFormat('zh-CN')

function ArchiveSummary({ overview, keys }: { overview: ArchiveOverview | null; keys: ClientKey[] }) {
  const names = new Map(keys.map((key) => [key.id, key.name]))

  return <section className="page-section archive-analytics">
    <div className="section-heading"><h2>归档来源</h2><span className="archive-analytics-caption">{overview ? `${number.format(overview.key_count)} 把密钥` : '正在加载…'}</span></div>
    <div className="archive-summary-scroll"><table className="archive-summary-table"><thead><tr><th>密钥</th><th>图片</th><th>IP</th><th>空间</th></tr></thead><tbody>
          {overview?.keys.map((item) => <tr key={item.key_id}><td title={names.get(item.key_id) ?? item.key_name}>{names.get(item.key_id) ?? item.key_name}</td><td>{number.format(item.count)}</td><td>{number.format(item.ip_count)}</td><td>{(item.bytes / 1048576).toFixed(1)} MiB</td></tr>)}
          {overview?.keys.length === 0 && <tr><td colSpan={4} className="archive-summary-empty">暂无归档数据</td></tr>}
          {!overview && <tr><td colSpan={4} className="archive-summary-empty">正在加载统计…</td></tr>}
        </tbody></table></div>
  </section>
}

function Thumbnail({ adminKey, id }: { adminKey: string; id: string }) {
  const container = useRef<HTMLSpanElement>(null)
  const [visible, setVisible] = useState(false)
  const [url, setUrl] = useState('')

  useEffect(() => {
    if (!container.current || !('IntersectionObserver' in window)) {
      setVisible(true)
      return
    }
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) {
        setVisible(true)
        observer.disconnect()
      }
    }, { rootMargin: '240px' })
    observer.observe(container.current)
    return () => observer.disconnect()
  }, [])

  useEffect(() => {
    if (!visible) return
    let active = true
    let current = ''
    void imageBlob(adminKey, id, 'thumbnail').then((blob) => {
      if (!active) return
      current = URL.createObjectURL(blob)
      setUrl(current)
    }).catch(() => { if (active) setUrl('') })
    return () => { active = false; if (current) URL.revokeObjectURL(current) }
  }, [adminKey, id, visible])

  return <span ref={container} className="archive-thumbnail">{url ? <img src={url} alt="生成图片缩略图" /> : <ImageIcon size={26} aria-hidden="true" />}</span>
}

function OriginalPreview({ adminKey, image, onClose, onDownload }: { adminKey: string; image: ArchiveImage; onClose: () => void; onDownload: (image: ArchiveImage) => void }) {
  const [url, setUrl] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    let active = true
    let current = ''
    void imageBlob(adminKey, image.id, 'original').then((blob) => {
      if (!active) return
      current = URL.createObjectURL(blob)
      setUrl(current)
    }).catch((cause) => { if (active) setError(cause instanceof Error ? cause.message : '原图加载失败') })
    return () => { active = false; if (current) URL.revokeObjectURL(current) }
  }, [adminKey, image.id])

  return <Dialog isOpen onOpenChange={(open) => { if (!open) onClose() }} width="min(96vw, 1100px)" maxHeight="96dvh" padding={0} aria-label="原图预览">
    <div className="archive-preview">
      <header><div><h2>{image.key_name}</h2><span>{new Date(image.created_at).toLocaleString('zh-CN')} · {image.ip}</span></div><button type="button" title="关闭预览" aria-label="关闭预览" onClick={onClose}><X size={19} /></button></header>
      <div className="archive-preview-stage">{url ? <img src={url} alt={`由 ${image.key_name} 生成的原图`} /> : <p>{error || '原图加载中…'}</p>}</div>
      <footer><span>{(image.bytes / 1048576).toFixed(2)} MiB{image.group_size > 1 ? ` · 同组 ${image.group_size} 张` : ''}</span><Button label="下载原图" variant="secondary" size="sm" icon={<Download size={16} />} onClick={() => onDownload(image)} /></footer>
    </div>
  </Dialog>
}

export function ArchivePage({ adminKey, keys, onStatsChange }: { adminKey: string; keys: ClientKey[]; onStatsChange: (stats: ArchiveStats) => void }) {
  const [draft, setDraft] = useState<ArchiveFilters>(emptyFilters)
  const [filters, setFilters] = useState<ArchiveFilters>(emptyFilters)
  const [page, setPage] = useState(1)
  const [list, setList] = useState<ArchiveList | null>(null)
  const [stats, setStats] = useState<ArchiveStats | null>(null)
  const [overview, setOverview] = useState<ArchiveOverview | null>(null)
  const [ipChoices, setIPChoices] = useState<ArchiveIPs | null>(null)
  const [ipSearch, setIPSearch] = useState('')
  const [ipQuery, setIPQuery] = useState('')
  const [ipLoading, setIPLoading] = useState(false)
  const [preview, setPreview] = useState<ArchiveImage | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<ArchiveImage | null>(null)
  const [error, setError] = useState('')
  const [overviewError, setOverviewError] = useState('')
  const [ipError, setIPError] = useState('')
  const [filterError, setFilterError] = useState('')
  const [loading, setLoading] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const requestNumber = useRef(0)
  const ipRequestNumber = useRef(0)
  const overviewRequestNumber = useRef(0)

  const load = useCallback(async () => {
    const request = ++requestNumber.current
    setLoading(true)
    try {
      const params = new URLSearchParams({ page: String(page) })
      if (filters.keyID !== 'all') params.set('key_id', filters.keyID)
      for (const ip of filters.ips) params.append(filters.ipMode === 'exclude' ? 'exclude_ip' : 'ip', ip)
      if (filters.from) params.set('from', new Date(`${filters.from}T00:00:00`).toISOString())
      if (filters.to) params.set('to', new Date(`${filters.to}T23:59:59`).toISOString())
      const images = await api.images(adminKey, params)
      if (request !== requestNumber.current) return
      const lastPage = Math.max(1, Math.ceil(images.total / images.page_size))
      if (page > lastPage) {
        setPage(lastPage)
        return
      }
      setList(images)
      setError('')
    } catch (cause) {
      if (request === requestNumber.current) setError(cause instanceof Error ? cause.message : '图库加载失败')
    } finally {
      if (request === requestNumber.current) setLoading(false)
    }
  }, [adminKey, filters, page])

  useEffect(() => { void load() }, [load])

  const loadOverview = useCallback(async () => {
    const request = ++overviewRequestNumber.current
    const today = new Date()
    const start = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 6)
    const end = new Date(today.getFullYear(), today.getMonth(), today.getDate() + 1)
    const params = new URLSearchParams({ from: start.toISOString(), to: end.toISOString(), offset_minutes: String(today.getTimezoneOffset()) })
    const [summary, analytics] = await Promise.allSettled([api.imageStats(adminKey), api.imageOverview(adminKey, params)])
    if (request !== overviewRequestNumber.current) return
    if (summary.status === 'fulfilled') { setStats(summary.value); onStatsChange(summary.value) }
    if (analytics.status === 'fulfilled') setOverview(analytics.value)
    const failure = summary.status === 'rejected' ? summary.reason : analytics.status === 'rejected' ? analytics.reason : null
    if (failure) {
      setOverviewError(failure instanceof Error ? failure.message : '归档统计加载失败')
    } else {
      setOverviewError('')
    }
  }, [adminKey, onStatsChange])

  const loadIPs = useCallback(async () => {
    const request = ++ipRequestNumber.current
    setIPLoading(true)
    try {
      const result = await api.imageIPs(adminKey, ipQuery)
      if (request === ipRequestNumber.current) { setIPChoices(result); setIPError('') }
    } catch (cause) {
      if (request === ipRequestNumber.current) setIPError(cause instanceof Error ? cause.message : 'IP 列表加载失败')
    } finally {
      if (request === ipRequestNumber.current) setIPLoading(false)
    }
  }, [adminKey, ipQuery])

  useEffect(() => { void loadOverview() }, [loadOverview])
  useEffect(() => { void loadIPs() }, [loadIPs])

  const refresh = () => { void load(); void loadOverview(); void loadIPs() }

  const applyFilters = (event?: FormEvent) => {
    event?.preventDefault()
    const next = { ...draft }
    if (next.ips.length > 100) {
      setFilterError('一次最多筛选 100 个 IP')
      return
    }
    if (next.from && next.to && next.from > next.to) {
      setFilterError('结束日期不能早于开始日期')
      return
    }
    setFilterError('')
    setDraft(next)
    setFilters(next)
    setPage(1)
  }

  const resetFilters = () => {
    setDraft(emptyFilters())
    setFilters(emptyFilters())
    setFilterError('')
    setPage(1)
  }

  const filterByIP = (ip: string) => {
    const next = { ...emptyFilters(), ips: [ip] }
    setDraft(next)
    setFilters(next)
    setFilterError('')
    setPage(1)
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }

  const download = async (item: ArchiveImage) => {
    try {
      const blob = await imageBlob(adminKey, item.id, 'original')
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = `${item.id}.png`
      link.click()
      window.setTimeout(() => URL.revokeObjectURL(url), 60000)
    } catch (cause) { setError(cause instanceof Error ? cause.message : '下载失败') }
  }

  const remove = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await api.deleteImage(adminKey, deleteTarget.id)
      setDeleteTarget(null)
      setError('')
      await Promise.all([load(), loadOverview(), loadIPs()])
    } catch (cause) { setError(cause instanceof Error ? cause.message : '删除失败') }
    finally { setDeleting(false) }
  }

  const hasFilters = filters.keyID !== 'all' || filters.ips.length > 0 || Boolean(filters.from || filters.to)
  const keyOptions = [{ value: 'all', label: '全部密钥' }, ...keys.map((key) => ({ value: key.id, label: key.name }))]
  const availableIPs = new Map((ipChoices?.items ?? []).map((item) => [item.ip, item.count]))
  const ipOptions = [...availableIPs].map(([ip, count]) => ({ value: ip, label: `${ip} · ${number.format(count)} 张` }))
  for (const ip of draft.ips) if (!availableIPs.has(ip)) ipOptions.push({ value: ip, label: ip })

  return <>
    <div className="page-intro"><div><h1>生成图库</h1></div><Button label="刷新图库" variant="secondary" size="sm" icon={<RefreshCw size={16} />} isLoading={loading} onClick={refresh} /></div>
    {error && <div className="inline-alert" role="alert">{error}</div>}
    {overviewError && <div className="inline-alert" role="alert">统计：{overviewError}</div>}
    {ipError && <div className="inline-alert" role="alert">IP 列表：{ipError}</div>}
    <div className="archive-stats"><span>图片 <strong>{stats ? number.format(stats.count) : '—'}</strong></span><span>已占用 <strong>{stats ? `${(stats.bytes / 1073741824).toFixed(2)} GiB` : '—'}</strong></span><span>生成批次 <strong>{overview ? number.format(overview.group_count) : '—'}</strong></span><span>来源 IP <strong>{overview ? number.format(overview.ip_count) : '—'}</strong></span><span>待处理 <strong>{stats?.pending ?? '—'}</strong></span><span>归档失败 <strong>{stats?.failures ?? '—'}</strong></span></div>
    {stats?.last_error && <div className="inline-alert" role="status">最近失败：{stats.last_error}</div>}
    <ArchiveSummary overview={overview} keys={keys} />
    <section className="page-section archive-section">
      <div className="section-heading archive-gallery-heading"><div><h2>图片记录</h2></div><span>{list ? `${number.format(list.total)} 张符合条件` : '正在加载…'}</span></div>
      <form className="archive-filters" onSubmit={applyFilters}>
        <Selector label="密钥" options={keyOptions} value={draft.keyID} onChange={(keyID) => setDraft((value) => ({ ...value, keyID }))} hasSearch={keys.length > 8} width="100%" size="sm" />
        <MultiSelector label="IP 地址" options={ipOptions} value={draft.ips} onChange={(ips) => setDraft((value) => ({ ...value, ips }))} placeholder="全部 IP" hasSearch searchPlaceholder="搜索 IP" hasClear triggerDisplay="count" formatValue={(items) => `${items.length} 个 IP`} emptyText="暂无归档 IP" width="100%" size="sm" isLoading={ipLoading} />
        <div className="archive-ip-mode"><span>IP 条件</span><SegmentedControl label="IP 条件" value={draft.ipMode} onChange={(ipMode) => setDraft((value) => ({ ...value, ipMode: ipMode as ArchiveFilters['ipMode'] }))} size="sm"><SegmentedControlItem value="include" label="包含" /><SegmentedControlItem value="exclude" label="排除" /></SegmentedControl></div>
        <DateInput label="开始日期" value={draft.from} onChange={(from) => setDraft((value) => ({ ...value, from }))} max={draft.to} width="100%" size="sm" />
        <DateInput label="结束日期" value={draft.to} onChange={(to) => setDraft((value) => ({ ...value, to }))} min={draft.from} width="100%" size="sm" />
        <div className="archive-filter-actions"><Button label="筛选" variant="primary" size="sm" icon={<Search size={16} />} type="submit" /><Button label="清除" variant="secondary" size="sm" onClick={resetFilters} isDisabled={!hasFilters && draft.keyID === 'all' && draft.ips.length === 0 && !draft.from && !draft.to} /></div>
      </form>
      {(ipChoices?.truncated || ipQuery) && <form className="archive-ip-search" onSubmit={(event) => { event.preventDefault(); setIPQuery(ipSearch.trim()) }}><TextInput label="查找更多 IP" value={ipSearch} onChange={setIPSearch} placeholder="输入部分或完整 IP" hasClear width="100%" size="sm" /><Button label="查找" variant="secondary" size="sm" icon={<Search size={15} />} type="submit" />{ipChoices?.truncated && <span>结果较多，仅显示前 500 个 IP</span>}</form>}
      {filterError && <div className="inline-alert archive-filter-error" role="alert">{filterError}</div>}
      {hasFilters && <div className="archive-filter-summary"><Filter size={14} aria-hidden="true" /><span>已筛选{filters.ips.length ? ` · ${filters.ipMode === 'exclude' ? '排除' : '包含'} IP ${filters.ips.slice(0, 3).join('、')}${filters.ips.length > 3 ? ` 等 ${filters.ips.length} 个` : ''}` : ''}{filters.from || filters.to ? ' · 日期范围' : ''}{filters.keyID !== 'all' ? ' · 密钥' : ''}</span><button type="button" onClick={resetFilters}>清除筛选</button></div>}
      <div className="archive-results" aria-busy={loading}>
        <div className="archive-grid">{list?.items.map((item) => <article className="archive-item" key={item.id}>
          <button type="button" className="archive-image" title="查看原图" aria-label={`查看 ${item.key_name} 的原图`} onClick={() => setPreview(item)}><Thumbnail adminKey={adminKey} id={item.id} /></button>
          <div className="archive-item-body">
            <strong title={item.key_name}>{item.key_name}</strong>
            <time dateTime={item.created_at}>{new Date(item.created_at).toLocaleString('zh-CN')}</time>
            <button type="button" className="archive-ip" title={`按 ${item.ip} 筛选`} onClick={() => filterByIP(item.ip)}><Filter size={12} aria-hidden="true" /><span>{item.ip}</span></button>
            <div className="archive-item-bottom"><span>{(item.bytes / 1048576).toFixed(2)} MiB{item.group_size > 1 ? ` · 同组 ${item.group_size} 张` : ''}</span><div className="archive-actions"><button type="button" title="查看原图" aria-label="查看原图" onClick={() => setPreview(item)}><Eye size={17} /></button><button type="button" title="下载原图" aria-label="下载原图" onClick={() => void download(item)}><Download size={17} /></button><button type="button" title="删除图片" aria-label="删除图片" onClick={() => setDeleteTarget(item)}><Trash2 size={17} /></button></div></div>
          </div>
        </article>)}</div>
        {!loading && list?.items.length === 0 && <div className="empty-state">没有符合条件的图片</div>}
      </div>
      {list && list.total > list.page_size && <div className="archive-pager"><span>共 {list.total} 张 · 第 {page} / {Math.ceil(list.total / list.page_size)} 页</span><Pagination page={page} onChange={setPage} totalItems={list.total} pageSize={list.page_size} variant="pages" size="sm" siblingCount={0} isDisabled={loading} label="图库分页" /></div>}
    </section>
    {preview && <OriginalPreview adminKey={adminKey} image={preview} onClose={() => setPreview(null)} onDownload={(item) => void download(item)} />}
    <AlertDialog isOpen={Boolean(deleteTarget)} onOpenChange={(open) => { if (!open && !deleting) setDeleteTarget(null) }} title="删除归档图片" description={`确定删除 ${deleteTarget?.key_name ?? ''} 的这张图片？原图和缩略图将一起删除。`} cancelLabel="取消" actionLabel="删除" isActionLoading={deleting} onAction={() => void remove()} />
  </>
}
