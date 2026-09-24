import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { Button } from '@astryxdesign/core/Button'
import {
  ArrowRight, BarChart3, Check, Clock3, Copy, CreditCard,
  KeyRound, LayoutDashboard, LockKeyhole, LogOut, Pencil, Plus,
  RefreshCw, Search, Settings, ShieldCheck, SlidersHorizontal, Trash2, X, Zap,
} from 'lucide-react'
import { api, apiAddress, ApiError, type AdminQuota, type AdminSettings, type ClientKey, type KeyPolicy } from './api'

type View = 'overview' | 'keys' | 'usage' | 'settings'
type DialogState =
  | { type: 'create' }
  | { type: 'edit'; key: ClientKey }
  | { type: 'reconcile'; key: ClientKey }
  | { type: 'revoke'; key: ClientKey }
  | { type: 'reveal'; key: string; name: string }
  | null

const sessionKey = 'novelai-proxy-admin-key'
const number = new Intl.NumberFormat('zh-CN')
const fmt = (value: number) => number.format(Math.max(0, value))
const committedAnlas = (key: ClientKey) => Math.max(0, key.spent_anlas - key.pending_anlas)
const committedOpus = (key: ClientKey) => Math.max(0, key.opus_used_images - key.opus_pending_images)
const clampPercent = (used: number, limit: number) => limit > 0 ? Math.min(100, Math.max(0, used / limit * 100)) : 0

function describeError(error: unknown) {
  return error instanceof Error ? error.message : '操作失败，请重试。'
}

function IconAction({ label, icon, onClick, danger = false, disabled = false }: { label: string; icon: ReactNode; onClick: () => void; danger?: boolean; disabled?: boolean }) {
  return <button type="button" className={`icon-action ${danger ? 'danger' : ''}`} title={label} aria-label={label} onClick={onClick} disabled={disabled}>{icon}</button>
}

function Brand() {
  return <div className="brand"><span className="brand-mark"><LayoutDashboard size={20} strokeWidth={2.5} /></span><span className="brand-text">NovelAI <strong>Proxy</strong><small>ADMIN CONSOLE</small></span></div>
}

function Modal({ title, children, onClose, width = 'normal' }: { title: string; children: ReactNode; onClose: () => void; width?: 'normal' | 'wide' }) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const node = ref.current
    node?.showModal()
    return () => node?.close()
  }, [])
  return <dialog ref={ref} className={`modal modal-${width}`} onCancel={(event) => { event.preventDefault(); onClose() }} onClick={(event) => { if (event.target === event.currentTarget) onClose() }}>
    <div className="modal-head"><h2>{title}</h2><IconAction label="关闭" icon={<X size={18} />} onClick={onClose} /></div>
    {children}
  </dialog>
}

function Login({ onConnect, busy, error }: { onConnect: (key: string) => Promise<void>; busy: boolean; error: string | null }) {
  const [value, setValue] = useState('')
  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (value.trim()) void onConnect(value.trim())
  }
  return <div className="login-screen">
    <div className="login-top"><Brand /><span className="login-security"><ShieldCheck size={15} /> 管理端</span></div>
    <main className="login-main">
      <div className="login-icon"><LockKeyhole size={25} /></div>
      <h1>连接管理面板</h1>
      <p className="login-subtitle">使用管理员密钥登录</p>
      <form onSubmit={submit} className="login-form">
        <label htmlFor="admin-key">管理员密钥</label>
        <input id="admin-key" type="password" value={value} onChange={(event) => setValue(event.target.value)} placeholder="输入 PROXY_ADMIN_KEY" autoComplete="off" autoFocus required />
        {error && <div className="form-error" role="alert">{error}</div>}
        <Button label="进入控制台" variant="primary" type="submit" width="100%" isLoading={busy} endContent={<ArrowRight size={16} />} />
      </form>
      <div className="login-endpoint">API <span>{apiAddress}</span></div>
    </main>
    <div className="login-bottom">NovelAI Proxy</div>
  </div>
}

function Sidebar({ view, setView, quota, onLogout }: { view: View; setView: (view: View) => void; quota: AdminQuota | null; onLogout: () => void }) {
  const items: { id: View; label: string; icon: ReactNode }[] = [
    { id: 'overview', label: '概览', icon: <LayoutDashboard size={18} /> },
    { id: 'keys', label: '密钥管理', icon: <KeyRound size={18} /> },
    { id: 'usage', label: '用量统计', icon: <BarChart3 size={18} /> },
    { id: 'settings', label: '配置', icon: <Settings size={18} /> },
  ]
  return <aside className="sidebar">
    <Brand />
    <div className="side-label">工作区</div>
    <nav aria-label="主导航" className="side-nav">
      {items.map((item) => <button type="button" key={item.id} className={view === item.id ? 'active' : ''} aria-current={view === item.id ? 'page' : undefined} title={item.label} aria-label={item.label} onClick={() => setView(item.id)}>{item.icon}<span>{item.label}</span></button>)}
    </nav>
    <div className="side-spacer" />
    <div className="side-account"><span className="status-dot" /> <span>{quota ? `NovelAI Tier ${quota.tier}` : '服务账户'}</span></div>
    <button type="button" className="logout" onClick={onLogout}><LogOut size={17} /> 退出管理</button>
  </aside>
}

function Metric({ icon, label, value, detail, tone }: { icon: ReactNode; label: string; value: string; detail: string; tone: string }) {
  return <div className="metric"><div className="metric-top"><span className={`metric-icon ${tone}`}>{icon}</span><span className="metric-label">{label}</span></div><strong>{value}</strong><span className="metric-detail">{detail}</span></div>
}

function QuotaBand({ quota, error, onRefresh, refreshing }: { quota: AdminQuota | null; error: string | null; onRefresh: () => void; refreshing: boolean }) {
  const rows = [
    { label: '订阅点数', projected: quota?.projected_fixed_anlas ?? 0, allocated: quota?.allocated_fixed_anlas ?? 0, available: quota?.unallocated_fixed_anlas ?? 0, tone: 'blue' },
    { label: '付费购入点数', projected: quota?.projected_purchased_anlas ?? 0, allocated: quota?.allocated_purchased_anlas ?? 0, available: quota?.unallocated_purchased_anlas ?? 0, tone: 'orange' },
  ]
  return <section className="page-section quota-section">
    <div className="section-heading"><div><span className="eyebrow">UPSTREAM QUOTA</span><h2>服务账户额度</h2></div><Button label="刷新官方额度" variant="secondary" size="sm" icon={<RefreshCw size={15} />} onClick={onRefresh} isLoading={refreshing} /></div>
    {error && <div className="inline-alert" role="alert">{error}</div>}
    <div className="quota-grid">
      {rows.map((row) => <div className="quota-column" key={row.label}><div className="quota-name"><span className={`legend-dot ${row.tone}`} />{row.label}</div><strong>{quota ? fmt(row.projected) : '—'}</strong><span className="quota-caption">预计可用</span><div className="quota-track"><span className={row.tone} style={{ width: `${clampPercent(row.allocated, row.projected)}%` }} /></div><div className="quota-meta"><span>已分配 <b>{quota ? fmt(row.allocated) : '—'}</b></span><span>待分配 <b>{quota ? fmt(row.available) : '—'}</b></span></div></div>)}
      <div className="quota-column opus-column"><div className="quota-name"><span className="legend-dot purple" />Opus 配额</div><strong>{quota ? `${Math.max(0, quota.projected_opus_percent).toFixed(1)}%` : '—'}</strong><span className="quota-caption">预计剩余</span><div className="opus-meter"><span style={{ width: `${Math.min(100, Math.max(0, quota?.projected_opus_percent ?? 0))}%` }} /></div><div className="quota-meta"><span>{quota ? `Tier ${quota.tier}` : '未连接'}</span><span>{quota?.active ? '订阅有效' : quota?.isGracePeriod ? '宽限期' : '订阅未激活'}</span></div></div>
    </div>
  </section>
}

function KeyIdentity({ item }: { item: ClientKey }) {
  return <div className="key-identity"><span className={`key-avatar ${item.revoked ? 'muted' : ''}`}>{item.name.slice(0, 1).toUpperCase()}</span><span className="key-ident-text"><strong>{item.name}</strong><small>{item.id}</small></span></div>
}

function UsageBars({ keys, metric = 'anlas', limit = 6 }: { keys: ClientKey[]; metric?: 'anlas' | 'fixed' | 'purchased' | 'opus'; limit?: number }) {
  const getValue = (key: ClientKey) => metric === 'fixed'
    ? Math.max(0, key.fixed_anlas_spent - key.fixed_anlas_pending)
    : metric === 'purchased'
      ? Math.max(0, key.purchased_anlas_spent - key.purchased_anlas_pending)
      : metric === 'opus' ? committedOpus(key) : committedAnlas(key)
  const ordered = [...keys].sort((a, b) => getValue(b) - getValue(a)).slice(0, limit)
  const maximum = Math.max(1, ...ordered.map(getValue))
  if (!ordered.length) return <div className="empty-state">暂无密钥用量</div>
  return <div className="usage-bars">{ordered.map((key) => <div className="usage-bar-row" key={key.id}><div className="usage-bar-name"><span>{key.name}</span><small>{key.revoked ? '已撤销' : key.id.slice(0, 8)}</small></div><div className="usage-bar-track"><span className={`usage-fill ${metric}`} style={{ width: `${getValue(key) / maximum * 100}%` }} /></div><strong>{fmt(getValue(key))}</strong></div>)}</div>
}

function Overview({ keys, quota, quotaError, onRefreshQuota, refreshingQuota, setView, onCreate }: { keys: ClientKey[]; quota: AdminQuota | null; quotaError: string | null; onRefreshQuota: () => void; refreshingQuota: boolean; setView: (view: View) => void; onCreate: () => void }) {
  const active = keys.filter((key) => !key.revoked).length
  const totalAnlas = keys.reduce((sum, key) => sum + committedAnlas(key), 0)
  const totalOpus = keys.reduce((sum, key) => sum + committedOpus(key), 0)
  const pending = keys.reduce((sum, key) => sum + key.pending_anlas + key.opus_pending_images, 0)
  return <>
    <div className="page-intro"><div><span className="eyebrow">DASHBOARD</span><h1>概览</h1><p>服务账户与分发密钥</p></div><Button label="签发密钥" variant="primary" icon={<Plus size={17} />} onClick={onCreate} /></div>
    <div className="metric-grid">
      <Metric icon={<KeyRound size={18} />} label="有效密钥" value={fmt(active)} detail={`共 ${fmt(keys.length)} 把`} tone="blue" />
      <Metric icon={<CreditCard size={18} />} label="估算 Anlas 用量" value={fmt(totalAnlas)} detail="所有密钥累计" tone="orange" />
      <Metric icon={<Zap size={18} />} label="Opus 生成" value={fmt(totalOpus)} detail="所有密钥累计" tone="purple" />
      <Metric icon={<Clock3 size={18} />} label="待核对" value={fmt(pending)} detail={`排队中 ${fmt(quota?.queue_length ?? 0)}`} tone="green" />
    </div>
    <QuotaBand quota={quota} error={quotaError} onRefresh={onRefreshQuota} refreshing={refreshingQuota} />
    <section className="page-section overview-usage"><div className="section-heading"><div><span className="eyebrow">KEY CONSUMPTION</span><h2>密钥用量排行</h2></div><button className="text-link" type="button" onClick={() => setView('usage')}>查看全部 <ArrowRight size={16} /></button></div><UsageBars keys={keys} /></section>
  </>
}

function KeyActions({ item, onEdit, onReconcile, onRevoke }: { item: ClientKey; onEdit: () => void; onReconcile: () => void; onRevoke: () => void }) {
  if (item.revoked) return <span className="muted-text">—</span>
  return <div className="row-actions"><IconAction label={`编辑 ${item.name}`} icon={<Pencil size={16} />} onClick={onEdit} />{item.pending_anlas + item.opus_pending_images > 0 && <IconAction label={`核对 ${item.name}`} icon={<SlidersHorizontal size={16} />} onClick={onReconcile} />}<IconAction label={`撤销 ${item.name}`} icon={<Trash2 size={16} />} onClick={onRevoke} danger /></div>
}

function KeysPage({ keys, onCreate, onEdit, onReconcile, onRevoke }: { keys: ClientKey[]; onCreate: () => void; onEdit: (key: ClientKey) => void; onReconcile: (key: ClientKey) => void; onRevoke: (key: ClientKey) => void }) {
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState<'active' | 'all' | 'revoked'>('active')
  const visible = useMemo(() => keys.filter((item) => (filter === 'all' || (filter === 'active' ? !item.revoked : item.revoked)) && `${item.name} ${item.id}`.toLowerCase().includes(search.toLowerCase())).sort((a, b) => a.name.localeCompare(b.name, 'zh-CN')), [keys, search, filter])
  return <>
    <div className="page-intro"><div><span className="eyebrow">ACCESS CONTROL</span><h1>密钥管理</h1><p>{fmt(keys.filter((key) => !key.revoked).length)} 把有效密钥</p></div><Button label="签发密钥" variant="primary" icon={<Plus size={17} />} onClick={onCreate} /></div>
    <section className="page-section key-section"><div className="section-heading key-heading"><div><h2>分发密钥</h2></div><div className="table-tools"><label className="search-field"><Search size={16} /><input aria-label="搜索密钥" placeholder="搜索名称或 ID" value={search} onChange={(event) => setSearch(event.target.value)} /></label><select aria-label="筛选密钥状态" value={filter} onChange={(event) => setFilter(event.target.value as typeof filter)}><option value="active">有效</option><option value="all">全部</option><option value="revoked">已撤销</option></select></div></div>
      <div className="table-scroll"><table className="data-table key-table"><thead><tr><th>密钥</th><th>状态</th><th>订阅点数</th><th>付费购入点数</th><th>Opus 配额</th><th>待核对</th><th className="actions-head">操作</th></tr></thead><tbody>{visible.map((item) => <tr key={item.id}><td><KeyIdentity item={item} /></td><td><span className={`status-badge ${item.revoked ? 'revoked' : 'active'}`}>{item.revoked ? '已撤销' : '有效'}</span></td><td><div className="table-number">{fmt(item.fixed_anlas_spent)} <small>/ {fmt(item.fixed_anlas_limit)}</small></div><div className="mini-track"><span className="blue" style={{ width: `${clampPercent(item.fixed_anlas_spent, item.fixed_anlas_limit)}%` }} /></div></td><td><div className="table-number">{fmt(item.purchased_anlas_spent)} <small>/ {fmt(item.purchased_anlas_limit)}</small></div><div className="mini-track"><span className="orange" style={{ width: `${clampPercent(item.purchased_anlas_spent, item.purchased_anlas_limit)}%` }} /></div></td><td><div className="table-number">{fmt(item.opus_used_images)} <small>/ {fmt(item.opus_limit_images)}</small></div><div className="mini-track"><span className="purple" style={{ width: `${clampPercent(item.opus_used_images, item.opus_limit_images)}%` }} /></div></td><td>{fmt(item.pending_anlas + item.opus_pending_images)}</td><td><KeyActions item={item} onEdit={() => onEdit(item)} onReconcile={() => onReconcile(item)} onRevoke={() => onRevoke(item)} /></td></tr>)}</tbody></table></div>
      <div className="mobile-key-list">{visible.map((item) => <article className="mobile-key" key={item.id}><div className="mobile-key-head"><KeyIdentity item={item} /><span className={`status-badge ${item.revoked ? 'revoked' : 'active'}`}>{item.revoked ? '已撤销' : '有效'}</span></div><div className="mobile-key-stats"><span>订阅点数 <b>{fmt(item.fixed_anlas_spent)} / {fmt(item.fixed_anlas_limit)}</b></span><span>付费购入点数 <b>{fmt(item.purchased_anlas_spent)} / {fmt(item.purchased_anlas_limit)}</b></span><span>Opus 配额 <b>{fmt(item.opus_used_images)} / {fmt(item.opus_limit_images)}</b></span><span>待核对 <b>{fmt(item.pending_anlas + item.opus_pending_images)}</b></span></div><KeyActions item={item} onEdit={() => onEdit(item)} onReconcile={() => onReconcile(item)} onRevoke={() => onRevoke(item)} /></article>)}</div>
      {!visible.length && <div className="empty-state">没有匹配的密钥</div>}
      <div className="table-footer">显示 {fmt(visible.length)} / {fmt(keys.length)} 把密钥</div>
    </section>
  </>
}

function UsagePage({ keys }: { keys: ClientKey[] }) {
  const [metric, setMetric] = useState<'anlas' | 'fixed' | 'purchased' | 'opus'>('anlas')
  const [includeRevoked, setIncludeRevoked] = useState(true)
  const visible = keys.filter((key) => includeRevoked || !key.revoked).sort((a, b) => committedAnlas(b) - committedAnlas(a))
  const totalFixed = keys.reduce((sum, key) => sum + Math.max(0, key.fixed_anlas_spent - key.fixed_anlas_pending), 0)
  const totalPurchased = keys.reduce((sum, key) => sum + Math.max(0, key.purchased_anlas_spent - key.purchased_anlas_pending), 0)
  const totalOpus = keys.reduce((sum, key) => sum + committedOpus(key), 0)
  return <>
    <div className="page-intro"><div><span className="eyebrow">USAGE ANALYTICS</span><h1>用量统计</h1><p>逐密钥累计估算</p></div></div>
    <div className="metric-grid three"><Metric icon={<CreditCard size={18} />} label="订阅点数" value={fmt(totalFixed)} detail="已计入用量" tone="blue" /><Metric icon={<CreditCard size={18} />} label="付费购入点数" value={fmt(totalPurchased)} detail="已计入用量" tone="orange" /><Metric icon={<Zap size={18} />} label="Opus 生成" value={fmt(totalOpus)} detail="已计入次数" tone="purple" /></div>
    <section className="page-section"><div className="section-heading"><div><span className="eyebrow">DISTRIBUTION</span><h2>密钥用量分布</h2></div><div className="segmented" role="group" aria-label="用量类型">{([['anlas', '全部 Anlas'], ['fixed', '订阅点数'], ['purchased', '付费购入点数'], ['opus', 'Opus']] as const).map(([id, label]) => <button type="button" key={id} aria-pressed={metric === id} className={metric === id ? 'active' : ''} onClick={() => setMetric(id)}>{label}</button>)}</div></div><UsageBars keys={visible} metric={metric} limit={10} /></section>
    <section className="page-section usage-list"><div className="section-heading"><div><span className="eyebrow">PER KEY</span><h2>逐密钥明细</h2></div><label className="check-line"><input type="checkbox" checked={includeRevoked} onChange={(event) => setIncludeRevoked(event.target.checked)} /> 包含已撤销</label></div><div className="table-scroll"><table className="data-table"><thead><tr><th>密钥</th><th>订阅点数</th><th>付费购入点数</th><th>合计</th><th>Opus 次数</th><th>待核对 Anlas</th></tr></thead><tbody>{visible.map((item) => <tr key={item.id}><td><KeyIdentity item={item} /></td><td>{fmt(Math.max(0, item.fixed_anlas_spent - item.fixed_anlas_pending))}</td><td>{fmt(Math.max(0, item.purchased_anlas_spent - item.purchased_anlas_pending))}</td><td><strong>{fmt(committedAnlas(item))}</strong></td><td>{fmt(committedOpus(item))}</td><td>{fmt(item.pending_anlas)}</td></tr>)}</tbody></table></div>{!visible.length && <div className="empty-state">暂无密钥用量</div>}</section>
  </>
}

function SettingsPage({ settings, busy, onChange }: { settings: AdminSettings | null; busy: boolean; onChange: (enabled: boolean) => void }) {
  return <>
    <div className="page-intro"><div><span className="eyebrow">CONFIGURATION</span><h1>配置</h1></div></div>
    <section className="page-section settings-section"><div className="section-heading"><h2>生成权限</h2></div>
      <div className="policy-row"><div className="policy-row-top"><div><strong>允许分配单次多图</strong><small>开启后可在单个密钥中单独授权；多图请求全部消耗点数，不使用 Opus 配额</small></div><label className="switch"><input type="checkbox" checked={settings?.allow_multi_image ?? false} disabled={!settings || busy} onChange={(event) => onChange(event.target.checked)} aria-label="允许分配单次多图" /><span /></label></div></div>
    </section>
  </>
}

const emptyPolicy: KeyPolicy = { name: '', allow_fixed_anlas: false, fixed_anlas_limit: 0, allow_purchased_anlas: false, purchased_anlas_limit: 0, allow_opus: false, opus_limit_images: 0, allow_multi_image: false }

function KeyForm({ existing, multiImageAvailable, busy, error, onSave, onClose }: { existing?: ClientKey; multiImageAvailable: boolean; busy: boolean; error: string | null; onSave: (policy: KeyPolicy) => Promise<void>; onClose: () => void }) {
  const [policy, setPolicy] = useState<KeyPolicy>(existing ? {
    name: existing.name, allow_fixed_anlas: existing.allow_fixed_anlas, fixed_anlas_limit: existing.fixed_anlas_limit,
    allow_purchased_anlas: existing.allow_purchased_anlas, purchased_anlas_limit: existing.purchased_anlas_limit,
    allow_opus: existing.allow_opus, opus_limit_images: existing.opus_limit_images, allow_multi_image: existing.allow_multi_image,
  } : emptyPolicy)
  const update = <K extends keyof KeyPolicy>(key: K, value: KeyPolicy[K]) => setPolicy((previous) => ({ ...previous, [key]: value }))
  const submit = (event: FormEvent) => {
    event.preventDefault()
    void onSave(policy)
  }
  return <form className="policy-form" onSubmit={submit}>
    <div className="field"><label htmlFor="key-name">名称</label><input id="key-name" value={policy.name} maxLength={80} onChange={(event) => update('name', event.target.value)} placeholder="例如：生产环境" required autoFocus /></div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>订阅点数</strong><small>订阅获得的 Anlas</small></div><label className="switch"><input type="checkbox" checked={policy.allow_fixed_anlas} onChange={(event) => update('allow_fixed_anlas', event.target.checked)} aria-label="允许使用订阅点数" /><span /></label></div><div className="field inline"><label htmlFor="fixed-limit">累计上限</label><input id="fixed-limit" type="number" min="0" max="1000000000" step="1" value={policy.fixed_anlas_limit} onChange={(event) => update('fixed_anlas_limit', Number(event.target.value))} /></div></div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>付费购入点数</strong><small>另行购买的 Anlas</small></div><label className="switch"><input type="checkbox" checked={policy.allow_purchased_anlas} onChange={(event) => update('allow_purchased_anlas', event.target.checked)} aria-label="允许使用付费购入点数" /><span /></label></div><div className="field inline"><label htmlFor="purchased-limit">累计上限</label><input id="purchased-limit" type="number" min="0" max="1000000000" step="1" value={policy.purchased_anlas_limit} onChange={(event) => update('purchased_anlas_limit', Number(event.target.value))} /></div></div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>Opus 配额</strong><small>此密钥可用的免费生成次数</small></div><label className="switch"><input type="checkbox" checked={policy.allow_opus} onChange={(event) => update('allow_opus', event.target.checked)} aria-label="允许使用 Opus 配额" /><span /></label></div><div className="field inline"><label htmlFor="opus-limit">累计上限</label><input id="opus-limit" type="number" min="0" max="10000000" step="1" value={policy.opus_limit_images} onChange={(event) => update('opus_limit_images', Number(event.target.value))} /></div></div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>单次多图</strong><small>{multiImageAvailable ? '允许一次生成 2–4 张；全部消耗点数，不使用 Opus 配额' : '全局功能已关闭；请先在配置页开启'}</small></div><label className="switch"><input type="checkbox" checked={policy.allow_multi_image} disabled={!multiImageAvailable} onChange={(event) => update('allow_multi_image', event.target.checked)} aria-label="允许此密钥单次多图" /><span /></label></div></div>
    {error && <div className="form-error" role="alert">{error}</div>}
    <div className="modal-actions"><Button label="取消" variant="secondary" onClick={onClose} /><Button label={existing ? '保存更改' : '签发密钥'} variant="primary" type="submit" isLoading={busy} /></div>
  </form>
}

export function App() {
  const [adminKey, setAdminKey] = useState(() => sessionStorage.getItem(sessionKey) || '')
  const [view, setView] = useState<View>('overview')
  const [keys, setKeys] = useState<ClientKey[]>([])
  const [quota, setQuota] = useState<AdminQuota | null>(null)
  const [settings, setSettings] = useState<AdminSettings | null>(null)
  const [quotaError, setQuotaError] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [loading, setLoading] = useState(false)
  const [refreshingQuota, setRefreshingQuota] = useState(false)
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)
  const [dialog, setDialog] = useState<DialogState>(null)
  const [copied, setCopied] = useState(false)

  const logout = useCallback(() => {
    sessionStorage.removeItem(sessionKey)
    setAdminKey('')
    setKeys([])
    setQuota(null)
    setSettings(null)
    setError(null)
    setDialog(null)
  }, [])

  const load = useCallback(async (key: string) => {
    setLoading(true)
    const [keyResult, quotaResult, settingsResult] = await Promise.allSettled([api.keys(key), api.quota(key), api.settings(key)])
    if (keyResult.status === 'rejected') {
      if (keyResult.reason instanceof ApiError && keyResult.reason.status === 401) logout()
      else setError(describeError(keyResult.reason))
    } else {
      setKeys(keyResult.value)
      setError(null)
      setUpdatedAt(new Date())
    }
    if (quotaResult.status === 'fulfilled') {
      setQuota(quotaResult.value)
      setQuotaError(null)
    } else {
      setQuotaError(describeError(quotaResult.reason))
    }
    if (settingsResult.status === 'fulfilled') setSettings(settingsResult.value)
    else setError(describeError(settingsResult.reason))
    setLoading(false)
  }, [logout])

  useEffect(() => { if (adminKey) void load(adminKey) }, [adminKey, load])

  const connect = async (key: string) => {
    setBusy(true)
    setError(null)
    try {
      const list = await api.keys(key)
      sessionStorage.setItem(sessionKey, key)
      setKeys(list)
      setAdminKey(key)
    } catch (cause) {
      setError(describeError(cause))
    } finally {
      setBusy(false)
    }
  }

  const mutate = async (work: () => Promise<void>) => {
    setBusy(true)
    setError(null)
    try { await work() } catch (cause) { setError(describeError(cause)) } finally { setBusy(false) }
  }

  const refreshQuota = async () => {
    setRefreshingQuota(true)
    try { setQuota(await api.refreshQuota(adminKey)); setQuotaError(null) } catch (cause) { setQuotaError(describeError(cause)) } finally { setRefreshingQuota(false) }
  }

  const changeMultiImage = (enabled: boolean) => mutate(async () => {
    setSettings(await api.updateSettings(adminKey, { allow_multi_image: enabled }))
  })

  const savePolicy = async (policy: KeyPolicy) => mutate(async () => {
    if (dialog?.type === 'edit') {
      await api.updateKey(adminKey, dialog.key.id, policy)
      setDialog(null)
      await load(adminKey)
    } else {
      const result = await api.createKey(adminKey, policy)
      setDialog({ type: 'reveal', key: result.key, name: result.client.name })
      setCopied(false)
      await load(adminKey)
    }
  })

  const revoke = (key: ClientKey) => mutate(async () => {
    await api.revokeKey(adminKey, key.id)
    setDialog(null)
    await load(adminKey)
  })

  const reconcile = (key: ClientKey, charged: number, opus: number) => mutate(async () => {
    await api.reconcile(adminKey, key.id, charged, opus)
    setDialog(null)
    await load(adminKey)
  })

  if (!adminKey) return <Login onConnect={connect} busy={busy} error={error} />

  return <div className="app-shell">
    <Sidebar view={view} setView={setView} quota={quota} onLogout={logout} />
    <div className="main-shell">
      <header className="topbar"><div className="breadcrumb">工作区 <span>/</span> {view === 'overview' ? '概览' : view === 'keys' ? '密钥管理' : view === 'usage' ? '用量统计' : '配置'}</div><div className="topbar-right"><span className="topbar-time">{updatedAt ? `同步于 ${updatedAt.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}` : '正在连接'}</span><IconAction label="刷新面板数据" icon={<RefreshCw size={17} className={loading ? 'spin' : ''} />} onClick={() => void load(adminKey)} disabled={loading} /><span className="topbar-separator" /><span className="admin-chip"><ShieldCheck size={15} /> 管理员</span></div></header>
      <main className="content">
        {error && <div className="inline-alert page-alert" role="alert">{error}<button type="button" onClick={() => setError(null)} aria-label="关闭错误"><X size={16} /></button></div>}
        {view === 'overview' && <Overview keys={keys} quota={quota} quotaError={quotaError} onRefreshQuota={() => void refreshQuota()} refreshingQuota={refreshingQuota} setView={setView} onCreate={() => setDialog({ type: 'create' })} />}
        {view === 'keys' && <KeysPage keys={keys} onCreate={() => setDialog({ type: 'create' })} onEdit={(key) => setDialog({ type: 'edit', key })} onReconcile={(key) => setDialog({ type: 'reconcile', key })} onRevoke={(key) => setDialog({ type: 'revoke', key })} />}
        {view === 'usage' && <UsagePage keys={keys} />}
        {view === 'settings' && <SettingsPage settings={settings} busy={busy} onChange={(enabled) => void changeMultiImage(enabled)} />}
      </main>
    </div>
    {(dialog?.type === 'create' || dialog?.type === 'edit') && <Modal title={dialog.type === 'create' ? '签发密钥' : `编辑 ${dialog.key.name}`} onClose={() => setDialog(null)}><KeyForm existing={dialog.type === 'edit' ? dialog.key : undefined} multiImageAvailable={settings?.allow_multi_image ?? false} busy={busy} error={error} onSave={savePolicy} onClose={() => setDialog(null)} /></Modal>}
    {dialog?.type === 'revoke' && <Modal title="撤销密钥" onClose={() => setDialog(null)}><div className="modal-body"><p>确认撤销 <strong>{dialog.key.name}</strong>？该密钥将立即无法访问代理。</p><div className="modal-actions"><Button label="取消" variant="secondary" onClick={() => setDialog(null)} /><Button label="撤销密钥" variant="destructive" isLoading={busy} onClick={() => void revoke(dialog.key)} /></div></div></Modal>}
    {dialog?.type === 'reconcile' && <ReconcileModal item={dialog.key} busy={busy} error={error} onClose={() => setDialog(null)} onSave={(charged, opus) => reconcile(dialog.key, charged, opus)} />}
    {dialog?.type === 'reveal' && <Modal title="密钥已签发" onClose={() => setDialog(null)}><div className="modal-body"><p><strong>{dialog.name}</strong> 的客户端密钥</p><div className="secret-line"><code>{dialog.key}</code><IconAction label={copied ? '已复制' : '复制密钥'} icon={copied ? <Check size={17} /> : <Copy size={17} />} onClick={() => { void navigator.clipboard.writeText(dialog.key).then(() => setCopied(true)).catch(() => setError('复制失败，请手动选择密钥。')) }} /></div><p className="modal-note">密钥只显示这一次。</p><div className="modal-actions"><Button label="完成" variant="primary" onClick={() => setDialog(null)} /></div></div></Modal>}
  </div>
}

function ReconcileModal({ item, busy, error, onSave, onClose }: { item: ClientKey; busy: boolean; error: string | null; onSave: (charged: number, opus: number) => Promise<void>; onClose: () => void }) {
  const [charged, setCharged] = useState(item.pending_anlas)
  const [opus, setOpus] = useState(item.opus_pending_images)
  return <Modal title={`核对 ${item.name}`} onClose={onClose}><form className="modal-body" onSubmit={(event) => { event.preventDefault(); void onSave(charged, opus) }}><div className="reconcile-summary"><span>待核对 Anlas <strong>{fmt(item.pending_anlas)}</strong></span><span>待核对 Opus <strong>{fmt(item.opus_pending_images)}</strong></span></div><div className="field"><label htmlFor="charged-anlas">实际计入 Anlas</label><input id="charged-anlas" type="number" min="0" max="1000000000" step="1" value={charged} onChange={(event) => setCharged(Number(event.target.value))} required /></div><div className="field"><label htmlFor="charged-opus">实际计入 Opus 次数</label><input id="charged-opus" type="number" min="0" max="10000000" step="1" value={opus} onChange={(event) => setOpus(Number(event.target.value))} required /></div>{error && <div className="form-error" role="alert">{error}</div>}<div className="modal-actions"><Button label="取消" variant="secondary" onClick={onClose} /><Button label="确认核对" variant="primary" type="submit" isLoading={busy} /></div></form></Modal>
}
