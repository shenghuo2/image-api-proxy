import { useCallback, useEffect, useState } from 'react'
import { Download, Eye, Image as ImageIcon, RefreshCw, Trash2, X } from 'lucide-react'
import { api, imageBlob, type ArchiveImage, type ArchiveList, type ArchiveStats, type ClientKey } from './api'

function Thumbnail({ adminKey, id }: { adminKey: string; id: string }) {
  const [url, setUrl] = useState('')
  useEffect(() => {
    let active = true
    let current = ''
    void imageBlob(adminKey, id, 'thumbnail').then((blob) => {
      if (!active) return
      current = URL.createObjectURL(blob)
      setUrl(current)
    }).catch(() => { if (active) setUrl('') })
    return () => { active = false; if (current) URL.revokeObjectURL(current) }
  }, [adminKey, id])
  return url ? <img src={url} alt="生成图片缩略图" loading="lazy" /> : <span className="archive-placeholder"><ImageIcon size={26} /></span>
}

function OriginalPreview({ adminKey, image, onClose }: { adminKey: string; image: ArchiveImage; onClose: () => void }) {
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
  return <div className="archive-overlay" role="presentation" onClick={onClose}><div className="archive-preview" role="dialog" aria-modal="true" aria-label="原图预览" onClick={(event) => event.stopPropagation()}><header><strong>{image.key_name}</strong><button type="button" title="关闭" aria-label="关闭" onClick={onClose}><X size={20} /></button></header>{url ? <img src={url} alt={`由 ${image.key_name} 生成的原图`} /> : <p>{error || '加载中…'}</p>}</div></div>
}

export function ArchivePage({ adminKey, keys }: { adminKey: string; keys: ClientKey[] }) {
  const [keyID, setKeyID] = useState('')
  const [ip, setIP] = useState('')
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [page, setPage] = useState(1)
  const [list, setList] = useState<ArchiveList | null>(null)
  const [stats, setStats] = useState<ArchiveStats | null>(null)
  const [preview, setPreview] = useState<ArchiveImage | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const load = useCallback(async () => {
    setLoading(true)
    try {
      const params = new URLSearchParams({ page: String(page) })
      if (keyID) params.set('key_id', keyID)
      if (ip.trim()) params.set('ip', ip.trim())
      if (from) params.set('from', new Date(from).toISOString())
      if (to) params.set('to', new Date(new Date(to).getTime() + 59999).toISOString())
      const [images, summary] = await Promise.all([api.images(adminKey, params), api.imageStats(adminKey)])
      setList(images); setStats(summary); setError('')
    } catch (cause) { setError(cause instanceof Error ? cause.message : '图库加载失败') }
    finally { setLoading(false) }
  }, [adminKey, keyID, ip, from, to, page])
  useEffect(() => { void load() }, [load])
  const download = async (item: ArchiveImage) => {
    try {
      const blob = await imageBlob(adminKey, item.id, 'original')
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url; link.download = `${item.id}.png`; link.click()
      window.setTimeout(() => URL.revokeObjectURL(url), 60000)
    } catch (cause) { setError(cause instanceof Error ? cause.message : '下载失败') }
  }
  const remove = async (item: ArchiveImage) => {
    if (!window.confirm(`删除 ${item.key_name} 的这张归档图片？`)) return
    try { await api.deleteImage(adminKey, item.id); await load() }
    catch (cause) { setError(cause instanceof Error ? cause.message : '删除失败') }
  }
  return <>
    <div className="page-intro"><div><span className="eyebrow">IMAGE ARCHIVE</span><h1>生成图库</h1></div><button type="button" className="archive-tool" title="刷新图库" aria-label="刷新图库" onClick={() => void load()} disabled={loading}><RefreshCw size={18} /></button></div>
    {error && <div className="inline-alert" role="alert">{error}</div>}
    <div className="archive-stats"><span>图片 <strong>{stats?.count ?? '—'}</strong></span><span>已占用 <strong>{stats ? `${(stats.bytes / 1073741824).toFixed(2)} GiB` : '—'}</strong></span><span>待处理 <strong>{stats?.pending ?? '—'}</strong></span><span>归档失败 <strong>{stats?.failures ?? '—'}</strong></span></div>
    {stats?.last_error && <div className="inline-alert" role="status">最近失败：{stats.last_error}</div>}
    <section className="page-section archive-section"><div className="archive-filters"><label>密钥<select value={keyID} onChange={(event) => { setKeyID(event.target.value); setPage(1) }}><option value="">全部</option>{keys.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></label><label>IP<input value={ip} onChange={(event) => { setIP(event.target.value); setPage(1) }} placeholder="全部 IP" /></label><label>开始时间<input type="datetime-local" value={from} onChange={(event) => { setFrom(event.target.value); setPage(1) }} /></label><label>结束时间<input type="datetime-local" value={to} onChange={(event) => { setTo(event.target.value); setPage(1) }} /></label></div>
      <div className="archive-grid">{list?.items.map((item) => <article className="archive-item" key={item.id}><div className="archive-image"><Thumbnail adminKey={adminKey} id={item.id} /></div><div className="archive-item-body"><strong>{item.key_name}</strong><small>{new Date(item.created_at).toLocaleString('zh-CN')} · {item.ip}</small><small title={`生成组 ${item.group_id}`}>{(item.bytes / 1048576).toFixed(2)} MiB{item.group_size > 1 ? ` · 同组 ${item.group_size} 张` : ''}</small><div className="archive-actions"><button type="button" title="查看原图" aria-label="查看原图" onClick={() => setPreview(item)}><Eye size={17} /></button><button type="button" title="下载原图" aria-label="下载原图" onClick={() => void download(item)}><Download size={17} /></button><button type="button" title="删除图片" aria-label="删除图片" onClick={() => void remove(item)}><Trash2 size={17} /></button></div></div></article>)}</div>
      {!loading && list?.items.length === 0 && <div className="empty-state">暂无归档图片</div>}
      <div className="archive-pager"><span>{list ? `共 ${list.total} 张 · 第 ${page} 页` : '加载中…'}</span><div><button type="button" disabled={page <= 1} onClick={() => setPage(page - 1)}>上一页</button><button type="button" disabled={!list || page * list.page_size >= list.total} onClick={() => setPage(page + 1)}>下一页</button></div></div>
    </section>
    {preview && <OriginalPreview adminKey={adminKey} image={preview} onClose={() => setPreview(null)} />}
  </>
}
