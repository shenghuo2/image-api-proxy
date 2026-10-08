import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { Button } from '@astryxdesign/core/Button'
import {
  ArrowRight, BarChart3, Check, Clock3, Copy, CreditCard,
  KeyRound, LayoutDashboard, LockKeyhole, LogOut, Pencil, Plus,
  RefreshCw, Search, Settings, ShieldCheck, SlidersHorizontal, Trash2, X, Zap, Eye, RotateCw, Users, ListOrdered, Images, FileText,
} from 'lucide-react'
import { api, apiAddress, ApiError, type Account, type AccountQuota, type AdminQuota, type AdminSettings, type ArchiveStats, type ClientKey, type KeyPolicy } from './api'
import { QueuePage } from './QueuePage'
import { ArchivePage } from './ArchivePage'
import { LogsPage } from './LogsPage'
import { UsageHeatmap } from './UsageHeatmap'
import { NumberInput } from './NumberInput'
import { useAutoRefresh } from './useAutoRefresh'

type View = 'overview' | 'queue' | 'accounts' | 'keys' | 'usage' | 'archive' | 'logs' | 'settings'
type DialogState =
  | { type: 'create' }
  | { type: 'edit'; key: ClientKey }
  | { type: 'reconcile'; key: ClientKey }
  | { type: 'revoke'; key: ClientKey }
  | { type: 'rotate'; key: ClientKey }
  | { type: 'reveal'; key: string; name: string }
  | { type: 'account-create' }
  | { type: 'account-edit'; account: Account }
  | { type: 'account-delete'; account: Account }
  | null

const sessionKey = 'novelai-proxy-admin-key'
const number = new Intl.NumberFormat('zh-CN')
const fmt = (value: number) => number.format(Math.max(0, value))
const fmtLimit = (value: number) => value === -1 ? '不限' : fmt(value)
const opusLimit = (key: ClientKey) => key.opus_limit_mode === 'percent' ? `${fmt(key.opus_effective_limit_images)} 次容量` : fmtLimit(key.opus_limit_images)
const opusShown = (key: ClientKey) => key.opus_limit_mode === 'percent' ? key.opus_remaining_images : key.opus_used_images
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
  return <div className="brand"><span className="brand-mark"><img src={`${import.meta.env.BASE_URL}favicon.svg`} alt="" /></span><span className="brand-text">NovelAI <strong>Proxy</strong><small>ADMIN CONSOLE</small></span></div>
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

function Sidebar({ view, setView, quota, showArchive, onLogout }: { view: View; setView: (view: View) => void; quota: AdminQuota | null; showArchive: boolean; onLogout: () => void }) {
  const items: { id: View; label: string; icon: ReactNode }[] = [
    { id: 'overview', label: '概览', icon: <LayoutDashboard size={18} /> },
    { id: 'queue', label: '任务队列', icon: <ListOrdered size={18} /> },
    { id: 'accounts', label: '账号管理', icon: <Users size={18} /> },
    { id: 'keys', label: '密钥管理', icon: <KeyRound size={18} /> },
    { id: 'usage', label: '用量统计', icon: <BarChart3 size={18} /> },
    { id: 'archive', label: '生成图库', icon: <Images size={18} /> },
    { id: 'logs', label: '请求日志', icon: <FileText size={18} /> },
    { id: 'settings', label: '配置', icon: <Settings size={18} /> },
  ]
  return <aside className="sidebar">
    <Brand />
    <div className="side-label">工作区</div>
    <nav aria-label="主导航" className="side-nav">
      {items.filter((item) => item.id !== 'archive' || showArchive).map((item) => <button type="button" key={item.id} className={view === item.id ? 'active' : ''} aria-current={view === item.id ? 'page' : undefined} title={item.label} aria-label={item.label} onClick={() => setView(item.id)}>{item.icon}<span>{item.label}</span></button>)}
    </nav>
    <div className="side-spacer" />
    <div className="side-account"><span className="status-dot" /> <span>{quota ? `${quota.account_quotas?.length ?? 1} 个服务账号` : '服务账户'}</span></div>
    <button type="button" className="logout" onClick={onLogout}><LogOut size={17} /> 退出管理</button>
  </aside>
}

function Metric({ icon, label, value, detail, tone }: { icon: ReactNode; label: string; value: string; detail: string; tone: string }) {
  return <div className="metric"><div className="metric-top"><span className={`metric-icon ${tone}`}>{icon}</span><span className="metric-label">{label}</span></div><strong>{value}</strong><span className="metric-detail">{detail}</span></div>
}

function QuotaBand({ quota, error, onRefresh, refreshing }: { quota: AdminQuota | null; error: string | null; onRefresh: () => void; refreshing: boolean }) {
  const rows = [
    { label: '订阅点数', projected: quota?.projected_fixed_anlas ?? 0, allocated: quota?.allocated_fixed_anlas ?? 0, available: quota?.unallocated_fixed_anlas ?? 0, unlimited: quota?.unlimited_fixed_keys ?? 0, tone: 'blue' },
    { label: '付费购入点数', projected: quota?.projected_purchased_anlas ?? 0, allocated: quota?.allocated_purchased_anlas ?? 0, available: quota?.unallocated_purchased_anlas ?? 0, unlimited: quota?.unlimited_purchased_keys ?? 0, tone: 'orange' },
  ]
  return <section className="page-section quota-section">
    <div className="section-heading"><div><span className="eyebrow">UPSTREAM QUOTA</span><h2>服务账户额度</h2></div><Button label="刷新官方额度" variant="secondary" size="sm" icon={<RefreshCw size={15} />} onClick={onRefresh} isLoading={refreshing} /></div>
    {error && <div className="inline-alert" role="alert">{error}</div>}
    {quota && (quota.account_errors?.length ?? 0) > 0 && <div className="inline-alert" role="alert">{quota.account_errors?.map((item) => item.name).join('、')} 的官方额度暂不可用</div>}
    <div className="quota-grid">
      {rows.map((row) => <div className="quota-column" key={row.label}><div className="quota-name"><span className={`legend-dot ${row.tone}`} />{row.label}</div><strong>{quota ? fmt(row.projected) : '—'}</strong><span className="quota-caption">预计可用</span><div className="quota-track"><span className={row.tone} style={{ width: `${clampPercent(row.allocated, row.projected)}%` }} /></div><div className="quota-meta"><span>有限分配 <b>{quota ? fmt(row.allocated) : '—'}</b></span><span>未预留 <b>{quota ? fmt(row.available) : '—'}</b></span></div>{row.unlimited > 0 && <div className="quota-caption">{fmt(row.unlimited)} 把不限额度密钥共享余额</div>}</div>)}
      <div className="quota-column opus-column"><div className="quota-name"><span className="legend-dot purple" />Opus 配额</div><strong>{quota ? (quota.account_quotas?.length ?? 1) > 1 ? `${quota.account_quotas.length} 个账号` : `${Math.max(0, quota.projected_opus_percent).toFixed(1)}%` : '—'}</strong><span className="quota-caption">{quota && (quota.account_quotas?.length ?? 1) > 1 ? '各账号配额独立' : '预计剩余'}</span><div className="opus-meter"><span style={{ width: `${quota && (quota.account_quotas?.length ?? 1) > 1 ? 100 : Math.min(100, Math.max(0, quota?.projected_opus_percent ?? 0))}%` }} /></div><div className="quota-meta"><span>{quota && (quota.account_quotas?.length ?? 1) > 1 ? '在账号管理中查看明细' : quota ? `Tier ${quota.tier}` : '未连接'}</span><span>{quota?.active ? '订阅有效' : quota?.isGracePeriod ? '宽限期' : '订阅未激活'}</span></div></div>
    </div>
  </section>
}

function KeyIdentity({ item }: { item: ClientKey }) {
  const accountLabel = item.account_name ?? (item.account_id === 'pool' ? '账号池' : item.account_id === 'default' ? '默认账号' : item.account_id)
  return <div className="key-identity"><span className={`key-avatar ${item.revoked ? 'muted' : ''}`}>{item.name.slice(0, 1).toUpperCase()}</span><span className="key-ident-text"><strong>{item.name}</strong><small>{item.id} · {accountLabel}{item.opus_share_warning ? ' · 比例超额，已按比例调整' : ''}</small></span></div>
}

function UsageBars({ keys, metric = 'anlas', limit = 6 }: { keys: ClientKey[]; metric?: 'generations' | 'anlas' | 'fixed' | 'purchased' | 'opus'; limit?: number }) {
  const getValue = (key: ClientKey) => metric === 'fixed'
    ? Math.max(0, key.fixed_anlas_spent - key.fixed_anlas_pending)
    : metric === 'purchased'
      ? Math.max(0, key.purchased_anlas_spent - key.purchased_anlas_pending)
      : metric === 'opus' ? committedOpus(key) : metric === 'generations' ? key.successful_generations : committedAnlas(key)
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
      <Metric icon={<Clock3 size={18} />} label="待核对" value={fmt(pending)} detail="需核对的预留量" tone="green" />
    </div>
    <QuotaBand quota={quota} error={quotaError} onRefresh={onRefreshQuota} refreshing={refreshingQuota} />
    <section className="page-section overview-usage"><div className="section-heading"><div><span className="eyebrow">KEY CONSUMPTION</span><h2>密钥用量排行</h2></div><button className="text-link" type="button" onClick={() => setView('usage')}>查看全部 <ArrowRight size={16} /></button></div><UsageBars keys={keys} /></section>
  </>
}

function AccountsPage({ accounts, keys, quota, busy, onCreate, onEdit, onDelete, onRefresh }: { accounts: Account[]; keys: ClientKey[]; quota: AdminQuota | null; busy: boolean; onCreate: () => void; onEdit: (account: Account) => void; onDelete: (account: Account) => void; onRefresh: (account: Account) => void }) {
  const quotas = new Map((quota?.account_quotas ?? []).map((item) => [item.account_id, item]))
  const errors = new Set((quota?.account_errors ?? []).map((item) => item.account_id))
  return <>
    <div className="page-intro"><div><span className="eyebrow">UPSTREAM ACCOUNTS</span><h1>账号管理</h1><p>{fmt(accounts.filter((item) => item.enabled).length)} 个已启用账号</p></div><Button label="添加账号" variant="primary" icon={<Plus size={17} />} onClick={onCreate} /></div>
    <section className="page-section"><div className="section-heading"><div><h2>NovelAI 账号</h2></div></div>
      <div className="account-list">{accounts.map((account) => {
        const item: AccountQuota | undefined = quotas.get(account.id)
        return <div className="account-row" key={account.id}>
          <div className="account-name"><strong>{account.name}</strong><small>{account.id} · {fmt(account.key_count)} 把固定密钥</small></div>
          <span className={`status-badge ${account.enabled ? 'active' : 'revoked'}`}>{account.enabled ? '已启用' : '已停用'}</span>
          <div className="account-balances"><span>订阅点数 <b>{item ? fmt(item.projected_fixed_anlas) : '—'}</b></span><span>付费购入点数 <b>{item ? fmt(item.projected_purchased_anlas) : '—'}</b></span><span>Opus 配额 <b>{item ? `${item.projected_opus_percent.toFixed(1)}%` : '—'}</b></span><span>已分配 / 未分配 <b>{item ? `${fmt(item.allocated_opus_images)} / ${fmt(item.unallocated_opus_images)}` : '—'}</b></span></div>
          <div className="account-state">{!account.token_configured ? 'Token 无法解密' : errors.has(account.id) ? '官方额度暂不可用' : item ? `Tier ${item.tier} · ${item.snapshot_age_seconds} 秒前更新${item.opus_predicted ? ' · 含预测回充' : ''}` : account.enabled ? '等待额度同步' : '已暂停使用'}</div>
          <div className="row-actions"><IconAction label={`刷新 ${account.name} 额度`} icon={<RefreshCw size={16} />} onClick={() => onRefresh(account)} disabled={!account.enabled || busy} /><IconAction label={`编辑 ${account.name}`} icon={<Pencil size={16} />} onClick={() => onEdit(account)} /><IconAction label={`删除 ${account.name}`} icon={<Trash2 size={16} />} onClick={() => onDelete(account)} disabled={account.key_count > 0 || (accounts.length === 1 && keys.some((key) => !key.revoked && key.account_id === 'pool'))} danger /></div>
        </div>
      })}</div>
      {!accounts.length && <div className="empty-state">尚未添加账号</div>}
    </section>
  </>
}

function KeyActions({ item, onEdit, onReconcile, onRevoke, onReveal, onRotate }: { item: ClientKey; onEdit: () => void; onReconcile: () => void; onRevoke: () => void; onReveal: () => void; onRotate: () => void }) {
  if (item.revoked) return <span className="muted-text">—</span>
  return <div className="row-actions">{item.key && <IconAction label={`查看 ${item.name} 的密钥`} icon={<Eye size={16} />} onClick={onReveal} />}<IconAction label={`轮换 ${item.name} 的密钥`} icon={<RotateCw size={16} />} onClick={onRotate} /><IconAction label={`编辑 ${item.name}`} icon={<Pencil size={16} />} onClick={onEdit} />{item.pending_anlas + item.opus_pending_images > 0 && <IconAction label={`核对 ${item.name}`} icon={<SlidersHorizontal size={16} />} onClick={onReconcile} />}<IconAction label={`撤销 ${item.name}`} icon={<Trash2 size={16} />} onClick={onRevoke} danger /></div>
}

function KeysPage({ keys, onCreate, onEdit, onReconcile, onRevoke, onReveal, onRotate }: { keys: ClientKey[]; onCreate: () => void; onEdit: (key: ClientKey) => void; onReconcile: (key: ClientKey) => void; onRevoke: (key: ClientKey) => void; onReveal: (key: ClientKey) => void; onRotate: (key: ClientKey) => void }) {
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState<'active' | 'all' | 'revoked'>('active')
  const visible = useMemo(() => keys.filter((item) => (filter === 'all' || (filter === 'active' ? !item.revoked : item.revoked)) && `${item.name} ${item.id}`.toLowerCase().includes(search.toLowerCase())).sort((a, b) => a.name.localeCompare(b.name, 'zh-CN')), [keys, search, filter])
  return <>
    <div className="page-intro"><div><span className="eyebrow">ACCESS CONTROL</span><h1>密钥管理</h1><p>{fmt(keys.filter((key) => !key.revoked).length)} 把有效密钥</p></div><Button label="签发密钥" variant="primary" icon={<Plus size={17} />} onClick={onCreate} /></div>
    <section className="page-section key-section"><div className="section-heading key-heading"><div><h2>分发密钥</h2></div><div className="table-tools"><label className="search-field"><Search size={16} /><input aria-label="搜索密钥" placeholder="搜索名称或 ID" value={search} onChange={(event) => setSearch(event.target.value)} /></label><select aria-label="筛选密钥状态" value={filter} onChange={(event) => setFilter(event.target.value as typeof filter)}><option value="active">有效</option><option value="all">全部</option><option value="revoked">已撤销</option></select></div></div>
      <div className="table-scroll"><table className="data-table key-table"><thead><tr><th>密钥</th><th>状态</th><th>订阅点数</th><th>付费购入点数</th><th>Opus 配额</th><th>成功生成</th><th>排队上限</th><th>待核对</th><th className="actions-head">操作</th></tr></thead><tbody>{visible.map((item) => <tr key={item.id}><td><KeyIdentity item={item} /></td><td><span className={`status-badge ${item.revoked ? 'revoked' : 'active'}`}>{item.revoked ? '已撤销' : '有效'}</span></td><td><div className="table-number">{fmt(item.fixed_anlas_spent)} <small>/ {fmtLimit(item.fixed_anlas_limit)}</small></div><div className="mini-track"><span className="blue" style={{ width: `${clampPercent(item.fixed_anlas_spent, item.fixed_anlas_limit)}%` }} /></div></td><td><div className="table-number">{fmt(item.purchased_anlas_spent)} <small>/ {fmtLimit(item.purchased_anlas_limit)}</small></div><div className="mini-track"><span className="orange" style={{ width: `${clampPercent(item.purchased_anlas_spent, item.purchased_anlas_limit)}%` }} /></div></td><td><div className="table-number" title={item.opus_limit_mode === 'percent' ? `累计已生成 ${fmt(item.opus_used_images)} 次` : undefined}>{fmt(opusShown(item))} <small>/ {opusLimit(item)}</small></div><div className="mini-track"><span className="purple" style={{ width: `${clampPercent(opusShown(item), item.opus_effective_limit_images)}%` }} /></div>{item.opus_limit_mode === 'percent' && <small>{item.opus_limit_percent}% 自动回充 · 累计 {fmt(committedOpus(item))}{item.opus_predicted ? ' · 含预测' : ''}</small>}</td><td><div className="table-number">{fmt(item.successful_generations)} <small>次</small></div><small>{fmt(item.successful_images)} 张图片</small></td><td>{fmtLimit(item.queue_limit)}</td><td>{fmt(item.pending_anlas + item.opus_pending_images)}</td><td><KeyActions item={item} onEdit={() => onEdit(item)} onReconcile={() => onReconcile(item)} onRevoke={() => onRevoke(item)} onReveal={() => onReveal(item)} onRotate={() => onRotate(item)} /></td></tr>)}</tbody></table></div>
      <div className="mobile-key-list">{visible.map((item) => <article className="mobile-key" key={item.id}><div className="mobile-key-head"><KeyIdentity item={item} /><span className={`status-badge ${item.revoked ? 'revoked' : 'active'}`}>{item.revoked ? '已撤销' : '有效'}</span></div><div className="mobile-key-stats"><span>订阅点数 <b>{fmt(item.fixed_anlas_spent)} / {fmtLimit(item.fixed_anlas_limit)}</b></span><span>付费购入点数 <b>{fmt(item.purchased_anlas_spent)} / {fmtLimit(item.purchased_anlas_limit)}</b></span><span>Opus {item.opus_limit_mode === 'percent' ? '当前可用' : '累计使用'} <b>{fmt(opusShown(item))} / {opusLimit(item)}</b></span><span>成功生成 <b>{fmt(item.successful_generations)} 次 · {fmt(item.successful_images)} 张</b></span><span>排队上限 <b>{fmtLimit(item.queue_limit)}</b></span><span>待核对 <b>{fmt(item.pending_anlas + item.opus_pending_images)}</b></span></div><KeyActions item={item} onEdit={() => onEdit(item)} onReconcile={() => onReconcile(item)} onRevoke={() => onRevoke(item)} onReveal={() => onReveal(item)} onRotate={() => onRotate(item)} /></article>)}</div>
      {!visible.length && <div className="empty-state">没有匹配的密钥</div>}
      <div className="table-footer">显示 {fmt(visible.length)} / {fmt(keys.length)} 把密钥</div>
    </section>
  </>
}

function UsagePage({ keys, adminKey }: { keys: ClientKey[]; adminKey: string }) {
  const [metric, setMetric] = useState<'generations' | 'anlas' | 'fixed' | 'purchased' | 'opus'>('generations')
  const [includeRevoked, setIncludeRevoked] = useState(true)
  const visible = keys.filter((key) => includeRevoked || !key.revoked).sort((a, b) => committedAnlas(b) - committedAnlas(a))
  const totalFixed = keys.reduce((sum, key) => sum + Math.max(0, key.fixed_anlas_spent - key.fixed_anlas_pending), 0)
  const totalPurchased = keys.reduce((sum, key) => sum + Math.max(0, key.purchased_anlas_spent - key.purchased_anlas_pending), 0)
  const totalOpus = keys.reduce((sum, key) => sum + committedOpus(key), 0)
  return <>
    <div className="page-intro"><div><span className="eyebrow">USAGE ANALYTICS</span><h1>用量统计</h1><p>逐密钥累计估算</p></div></div>
    <div className="metric-grid"><Metric icon={<Images size={18} />} label="成功生成" value={fmt(keys.reduce((sum, key) => sum + key.successful_generations, 0))} detail={`${fmt(keys.reduce((sum, key) => sum + key.successful_images, 0))} 张图片 · 包括付费生成`} tone="green" /><Metric icon={<CreditCard size={18} />} label="订阅点数" value={fmt(totalFixed)} detail="已计入用量" tone="blue" /><Metric icon={<CreditCard size={18} />} label="付费购入点数" value={fmt(totalPurchased)} detail="已计入用量" tone="orange" /><Metric icon={<Zap size={18} />} label="Opus 生成" value={fmt(totalOpus)} detail="已计入次数" tone="purple" /></div>
    <UsageHeatmap adminKey={adminKey} />
    <section className="page-section"><div className="section-heading"><div><span className="eyebrow">DISTRIBUTION</span><h2>密钥用量分布</h2></div><div className="segmented" role="group" aria-label="用量类型">{([['generations', '成功次数'], ['anlas', '全部 Anlas'], ['fixed', '订阅点数'], ['purchased', '付费购入点数'], ['opus', 'Opus']] as const).map(([id, label]) => <button type="button" key={id} aria-pressed={metric === id} className={metric === id ? 'active' : ''} onClick={() => setMetric(id)}>{label}</button>)}</div></div><UsageBars keys={visible} metric={metric} limit={10} /></section>
    <section className="page-section usage-list"><div className="section-heading"><div><span className="eyebrow">PER KEY</span><h2>逐密钥明细</h2></div><label className="check-line"><input type="checkbox" checked={includeRevoked} onChange={(event) => setIncludeRevoked(event.target.checked)} /> 包含已撤销</label></div><div className="table-scroll"><table className="data-table"><thead><tr><th>密钥</th><th>成功次数</th><th>图片张数</th><th>订阅点数</th><th>付费购入点数</th><th>合计</th><th>Opus 次数</th><th>待核对 Anlas</th></tr></thead><tbody>{visible.map((item) => <tr key={item.id}><td><KeyIdentity item={item} /></td><td>{fmt(item.successful_generations)} 次</td><td>{fmt(item.successful_images)} 张</td><td>{fmt(Math.max(0, item.fixed_anlas_spent - item.fixed_anlas_pending))}</td><td>{fmt(Math.max(0, item.purchased_anlas_spent - item.purchased_anlas_pending))}</td><td><strong>{fmt(committedAnlas(item))}</strong></td><td>{fmt(committedOpus(item))}</td><td>{fmt(item.pending_anlas)}</td></tr>)}</tbody></table></div>{!visible.length && <div className="empty-state">暂无密钥用量</div>}</section>
  </>
}

function SettingsPage({ settings, busy, onChange, onSaveAdminPath }: { settings: AdminSettings | null; busy: boolean; onChange: (changes: Partial<AdminSettings>) => void; onSaveAdminPath: (path: string) => Promise<void> }) {
  const [days, setDays] = useSettingsDraft(settings?.archive_retention_days, 30)
  const [capacity, setCapacity] = useSettingsDraft(settings ? settings.archive_max_bytes / 1073741824 : undefined, 20)
  const [adminPath, setAdminPath] = useSettingsDraft(settings?.admin_ui_path, '/console')
  const [logDays, setLogDays] = useSettingsDraft(settings?.log_retention_days, 7)
  const [logCapacity, setLogCapacity] = useSettingsDraft(settings ? settings.log_max_bytes / 1048576 : undefined, 100)
  const validAdminPath = adminPath.length <= 128 && /^\/[A-Za-z0-9_-]+(?:\/[A-Za-z0-9_-]+)*$/.test(adminPath) && !['admin', 'ai', 'image', 'user', 'quota', 'healthz', 'jobs'].includes(adminPath.split('/')[1])
  return <>
    <div className="page-intro"><div><span className="eyebrow">CONFIGURATION</span><h1>配置</h1></div></div>
    <section className="page-section settings-section"><div className="section-heading"><h2>管理页面入口</h2></div>
      <form className="admin-path-settings" onSubmit={(event) => { event.preventDefault(); if (validAdminPath) void onSaveAdminPath(adminPath) }}>
        <label htmlFor="admin-ui-path">访问路径</label>
        <div className="admin-path-controls"><input id="admin-ui-path" value={adminPath} maxLength={128} spellCheck={false} autoCapitalize="off" autoComplete="off" disabled={!settings || busy} onChange={(event) => setAdminPath(event.target.value)} placeholder="/console" required /><button type="submit" disabled={!settings || busy || !validAdminPath || adminPath === settings.admin_ui_path}>保存并前往新地址</button></div>
        <small>以 / 开头且不以 / 结尾，仅使用字母、数字、-、_ 和 /；不能占用 API 首段，管理 API 仍在 /admin/*。</small>
      </form>
    </section>
    <section className="page-section settings-section"><div className="section-heading"><h2>生成权限</h2></div>
      <div className="policy-row"><div className="policy-row-top"><div><strong>允许分配单次多图</strong><small>开启后可在单个密钥中单独授权；多图请求全部消耗点数，不使用 Opus 配额</small></div><label className="switch"><input type="checkbox" checked={settings?.allow_multi_image ?? false} disabled={!settings || busy} onChange={(event) => onChange({ allow_multi_image: event.target.checked })} aria-label="允许分配单次多图" /><span /></label></div></div>
    </section>
    <section className="page-section settings-section"><div className="section-heading"><h2>计费核对</h2></div>
      <div className="policy-row"><div className="policy-row-top"><div><strong>待核对预留全部计费</strong><small>开启时立即将已有待核对预留计入本地预算，之后未确认的请求也按预留计费；不会重复扣减或增加成功次数。关闭后仅影响之后的请求。</small></div><label className="switch"><input type="checkbox" checked={settings?.charge_pending_as_spent ?? false} disabled={!settings || busy} onChange={(event) => onChange({ charge_pending_as_spent: event.target.checked })} aria-label="待核对预留全部计费" /><span /></label></div></div>
    </section>
    <section className="page-section settings-section"><div className="section-heading"><h2>请求日志</h2></div>
      <div className="policy-row"><div className="policy-row-top"><div><strong>记录生成和图片工具请求</strong><small>记录参数、账号、错误原因和预算处理；不保存 key、提示词或图片内容</small></div><label className="switch"><input type="checkbox" checked={settings?.logs_enabled ?? true} disabled={!settings || busy} onChange={(event) => onChange({ logs_enabled: event.target.checked })} aria-label="开启请求日志" /><span /></label></div></div>
      <form className="archive-settings" onSubmit={(event) => { event.preventDefault(); onChange({ log_retention_days: logDays, log_max_bytes: Math.round(logCapacity * 1048576) }) }}><label>保留天数（-1 不限）<NumberInput min="-1" max="36500" step="1" value={logDays} disabled={!settings || busy} onChange={setLogDays} required /></label><label>容量上限（MiB）<NumberInput min="1" max="1024" step="1" value={logCapacity} disabled={!settings || busy} onChange={setLogCapacity} required /></label><button type="submit" disabled={!settings || busy || logDays === 0}>保存日志策略</button></form>
    </section>
    <section className="page-section settings-section"><div className="section-heading"><h2>图片归档</h2></div>
      <div className="policy-row"><div className="policy-row-top"><div><strong>归档成功生成的图片</strong><small>保存原始 PNG 与小于 100 KiB 的 JPEG 缩略图</small></div><label className="switch"><input type="checkbox" checked={settings?.archive_enabled ?? false} disabled={!settings || busy} onChange={(event) => onChange({ archive_enabled: event.target.checked })} aria-label="开启图片归档" /><span /></label></div></div>
      <form className="archive-settings" onSubmit={(event) => { event.preventDefault(); onChange({ archive_retention_days: days, archive_max_bytes: Math.round(capacity * 1073741824) }) }}><label>保留天数（-1 不限）<NumberInput min="-1" max="36500" step="1" value={days} disabled={!settings || busy} onChange={(value) => setDays(value)} /></label><label>容量上限（GiB）<NumberInput min="0.001" max="1024" step="0.1" value={capacity} disabled={!settings || busy} onChange={(value) => setCapacity(value)} /></label><button type="submit" disabled={!settings || busy}>保存保留策略</button></form>
    </section>
  </>
}

function useSettingsDraft<T extends string | number>(source: T | undefined, fallback: T) {
  const [draft, setDraft] = useState(source ?? fallback)
  const previous = useRef(source ?? fallback)
  useEffect(() => {
    if (source === undefined) return
    const old = previous.current
    setDraft((current) => Object.is(current, old) ? source : current)
    previous.current = source
  }, [source])
  return [draft, setDraft] as const
}

const emptyPolicy: KeyPolicy = { name: '', account_id: 'pool', allow_fixed_anlas: false, fixed_anlas_limit: 0, allow_purchased_anlas: false, purchased_anlas_limit: 0, allow_opus: false, opus_limit_mode: 'images', opus_limit_percent: 0, opus_limit_images: 0, allow_multi_image: false, archive_enabled: true, queue_limit: -1 }

function KeyForm({ existing, accounts, multiImageAvailable, busy, error, onSave, onClose }: { existing?: ClientKey; accounts: Account[]; multiImageAvailable: boolean; busy: boolean; error: string | null; onSave: (policy: KeyPolicy) => Promise<void>; onClose: () => void }) {
  const [policy, setPolicy] = useState<KeyPolicy>(existing ? {
    name: existing.name, account_id: existing.account_id, allow_fixed_anlas: existing.allow_fixed_anlas, fixed_anlas_limit: existing.fixed_anlas_limit,
    allow_purchased_anlas: existing.allow_purchased_anlas, purchased_anlas_limit: existing.purchased_anlas_limit,
    allow_opus: existing.allow_opus, opus_limit_mode: existing.opus_limit_mode, opus_limit_percent: existing.opus_limit_percent, opus_limit_images: existing.opus_limit_images, allow_multi_image: existing.allow_multi_image, archive_enabled: existing.archive_enabled, queue_limit: existing.queue_limit,
  } : emptyPolicy)
  const update = <K extends keyof KeyPolicy>(key: K, value: KeyPolicy[K]) => setPolicy((previous) => ({ ...previous, [key]: value }))
  const submit = (event: FormEvent) => {
    event.preventDefault()
    void onSave(policy)
  }
  return <form className="policy-form" onSubmit={submit}>
    <div className="field"><label htmlFor="key-name">名称</label><input id="key-name" value={policy.name} maxLength={80} onChange={(event) => update('name', event.target.value)} placeholder="例如：生产环境" required autoFocus /></div>
    <div className="field"><label htmlFor="key-account">上游账号</label><select id="key-account" value={policy.account_id} onChange={(event) => update('account_id', event.target.value)} disabled={Boolean(existing && existing.spent_anlas + existing.opus_used_images > 0)}><option value="pool">已启用账号轮询</option>{accounts.map((account) => <option key={account.id} value={account.id} disabled={!account.enabled && policy.account_id !== account.id}>{account.name}{account.enabled ? '' : '（已停用）'}</option>)}</select></div>
    <div className="field"><label htmlFor="queue-limit">最多等待请求（-1 不限，0 不排队）</label><NumberInput id="queue-limit" min="-1" max="10000" step="1" value={policy.queue_limit} onChange={(value) => update('queue_limit', value)} required /></div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>订阅点数</strong><small>订阅获得的 Anlas</small></div><label className="switch"><input type="checkbox" checked={policy.allow_fixed_anlas} onChange={(event) => update('allow_fixed_anlas', event.target.checked)} aria-label="允许使用订阅点数" /><span /></label></div><div className="field inline"><label htmlFor="fixed-limit">累计上限（-1 为不限）</label><NumberInput id="fixed-limit" min="-1" max="1000000000" step="1" value={policy.fixed_anlas_limit} onChange={(value) => update('fixed_anlas_limit', value)} /></div></div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>付费购入点数</strong><small>另行购买的 Anlas</small></div><label className="switch"><input type="checkbox" checked={policy.allow_purchased_anlas} onChange={(event) => update('allow_purchased_anlas', event.target.checked)} aria-label="允许使用付费购入点数" /><span /></label></div><div className="field inline"><label htmlFor="purchased-limit">累计上限（-1 为不限）</label><NumberInput id="purchased-limit" min="-1" max="1000000000" step="1" value={policy.purchased_anlas_limit} onChange={(value) => update('purchased_anlas_limit', value)} /></div></div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>Opus 配额</strong><small>此密钥可用的免费生成次数</small></div><label className="switch"><input type="checkbox" checked={policy.allow_opus} onChange={(event) => update('allow_opus', event.target.checked)} aria-label="允许使用 Opus 配额" /><span /></label></div><div className="segmented policy-segmented" role="group" aria-label="Opus 限制方式"><button type="button" aria-pressed={policy.opus_limit_mode === 'images'} className={policy.opus_limit_mode === 'images' ? 'active' : ''} onClick={() => update('opus_limit_mode', 'images')}>按次数</button><button type="button" aria-pressed={policy.opus_limit_mode === 'percent'} className={policy.opus_limit_mode === 'percent' ? 'active' : ''} onClick={() => update('opus_limit_mode', 'percent')}>按比例自动回充</button></div>{policy.opus_limit_mode === 'images' ? <div className="field inline"><label htmlFor="opus-limit">累计上限（-1 为不限）</label><NumberInput id="opus-limit" min="-1" max="10000000" step="1" value={policy.opus_limit_images} onChange={(value) => update('opus_limit_images', value)} /></div> : <div className="field inline"><label htmlFor="opus-percent">满额占比（额度条约 {fmt(Math.floor(policy.opus_limit_percent * 1730 / 100))} 次）</label><NumberInput id="opus-percent" min="0" max="100" step="0.1" value={policy.opus_limit_percent} onChange={(value) => update('opus_limit_percent', value)} /></div>}</div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>单次多图</strong><small>{multiImageAvailable ? '允许一次生成 2–4 张；全部消耗点数，不使用 Opus 配额' : '全局功能已关闭；请先在配置页开启'}</small></div><label className="switch"><input type="checkbox" checked={policy.allow_multi_image} disabled={!multiImageAvailable} onChange={(event) => update('allow_multi_image', event.target.checked)} aria-label="允许此密钥单次多图" /><span /></label></div></div>
    <div className="policy-row"><div className="policy-row-top"><div><strong>参与图片归档</strong><small>只影响之后成功生成的图片</small></div><label className="switch"><input type="checkbox" checked={policy.archive_enabled} onChange={(event) => update('archive_enabled', event.target.checked)} aria-label="允许此密钥参与归档" /><span /></label></div></div>
    {error && <div className="form-error" role="alert">{error}</div>}
    <div className="modal-actions"><Button label="取消" variant="secondary" onClick={onClose} /><Button label={existing ? '保存更改' : '签发密钥'} variant="primary" type="submit" isLoading={busy} /></div>
  </form>
}

function AccountForm({ existing, busy, error, onSave, onClose }: { existing?: Account; busy: boolean; error: string | null; onSave: (input: { name: string; token: string; enabled: boolean }) => Promise<void>; onClose: () => void }) {
  const [name, setName] = useState(existing?.name ?? '')
  const [token, setToken] = useState('')
  const [enabled, setEnabled] = useState(existing?.enabled ?? true)
  return <form className="policy-form" onSubmit={(event) => { event.preventDefault(); void onSave({ name: name.trim(), token: token.trim(), enabled }) }}>
    <div className="field"><label htmlFor="account-name">账号名称</label><input id="account-name" value={name} maxLength={80} onChange={(event) => setName(event.target.value)} required autoFocus /></div>
    <div className="field"><label htmlFor="account-token">{existing ? '替换 Token（可选）' : 'NovelAI Token'}</label><input id="account-token" type="password" value={token} minLength={16} onChange={(event) => setToken(event.target.value)} autoComplete="off" required={!existing} /></div>
    <div className="policy-row"><div className="policy-row-top"><strong>启用账号</strong><label className="switch"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} aria-label="启用账号" /><span /></label></div></div>
    {error && <div className="form-error" role="alert">{error}</div>}
    <div className="modal-actions"><Button label="取消" variant="secondary" onClick={onClose} /><Button label={existing ? '保存更改' : '添加账号'} variant="primary" type="submit" isLoading={busy} /></div>
  </form>
}

export function App() {
  const [adminKey, setAdminKey] = useState(() => sessionStorage.getItem(sessionKey) || '')
  const [view, setView] = useState<View>('overview')
  const [keys, setKeys] = useState<ClientKey[]>([])
  const [accounts, setAccounts] = useState<Account[]>([])
  const [quota, setQuota] = useState<AdminQuota | null>(null)
  const [settings, setSettings] = useState<AdminSettings | null>(null)
  const [archiveStats, setArchiveStats] = useState<ArchiveStats | null>(null)
  const [quotaError, setQuotaError] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [loading, setLoading] = useState(false)
  const [refreshingQuota, setRefreshingQuota] = useState(false)
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)
  const [dialog, setDialog] = useState<DialogState>(null)
  const [copied, setCopied] = useState(false)
  const loadNumber = useRef(0)
  const loadsInFlight = useRef(0)

  const logout = useCallback(() => {
    ++loadNumber.current
    setLoading(false)
    sessionStorage.removeItem(sessionKey)
    setAdminKey('')
    setKeys([])
    setAccounts([])
    setQuota(null)
    setSettings(null)
    setArchiveStats(null)
    setError(null)
    setDialog(null)
  }, [])

  const load = useCallback(async (key: string, background = false) => {
    if (background && loadsInFlight.current > 0) return
    const request = ++loadNumber.current
    ++loadsInFlight.current
    if (!background) setLoading(true)
    const [keyResult, quotaResult, settingsResult, accountResult, archiveResult] = await Promise.allSettled([api.keys(key), api.quota(key), api.settings(key), api.accounts(key), api.imageStats(key)])
    --loadsInFlight.current
    if (request !== loadNumber.current) return
    const authFailure = [keyResult, quotaResult, settingsResult, accountResult, archiveResult].some((result) => result.status === 'rejected' && result.reason instanceof ApiError && result.reason.status === 401)
    if (authFailure) { setLoading(false); logout(); return }
    if (keyResult.status === 'rejected') {
      if (!background) setError(describeError(keyResult.reason))
    } else {
      setKeys(keyResult.value)
      if (!background) setError(null)
      setUpdatedAt(new Date())
    }
    if (quotaResult.status === 'fulfilled') {
      setQuota(quotaResult.value)
      setQuotaError(null)
    } else {
      setQuotaError(describeError(quotaResult.reason))
    }
    if (settingsResult.status === 'fulfilled') setSettings(settingsResult.value)
    else if (!background) setError(describeError(settingsResult.reason))
    if (accountResult.status === 'fulfilled') setAccounts(accountResult.value)
    else if (!background) setError(describeError(accountResult.reason))
    if (archiveResult.status === 'fulfilled') setArchiveStats(archiveResult.value)
    setLoading(false)
  }, [logout])

  useEffect(() => { if (adminKey) void load(adminKey) }, [adminKey, load])
  const refreshData = useCallback(() => load(adminKey, true), [adminKey, load])
  useAutoRefresh(refreshData, Boolean(adminKey) && !busy && !refreshingQuota, 5000, false)
  const showArchive = settings?.archive_enabled !== false || archiveStats === null || archiveStats.ever_archived || archiveStats.count > 0 || archiveStats.pending > 0
  useEffect(() => { if (!showArchive && view === 'archive') setView('usage') }, [showArchive, view])

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
    ++loadNumber.current
    setLoading(false)
    setBusy(true)
    setError(null)
    try { await work() } catch (cause) { setError(describeError(cause)) } finally { setBusy(false) }
  }

  const refreshQuota = async () => {
    ++loadNumber.current
    setLoading(false)
    setRefreshingQuota(true)
    try { setQuota(await api.refreshQuota(adminKey)); setQuotaError(null) } catch (cause) { setQuotaError(describeError(cause)) } finally { setRefreshingQuota(false) }
  }

  const changeSettings = (changes: Partial<AdminSettings>) => mutate(async () => {
    const previous = settings
    setSettings((current) => current ? { ...current, ...changes } : current)
    try {
      setSettings(await api.updateSettings(adminKey, changes))
    } catch (cause) {
      setSettings(previous)
      throw cause
    }
    await load(adminKey)
  })

  const changeAdminPath = async (path: string) => {
    setBusy(true)
    setError(null)
    try {
      const next = await api.updateSettings(adminKey, { admin_ui_path: path })
      setSettings(next)
      window.location.assign(`${next.admin_ui_path}/`)
    } catch (cause) {
      setError(describeError(cause))
    } finally {
      setBusy(false)
    }
  }

  const startCreateKey = () => {
    if (accounts.some((account) => account.enabled)) setDialog({ type: 'create' })
    else setView('accounts')
  }

  const saveAccount = async (input: { name: string; token: string; enabled: boolean }) => mutate(async () => {
    if (dialog?.type === 'account-edit') await api.updateAccount(adminKey, dialog.account.id, input)
    else await api.createAccount(adminKey, input)
    setDialog(null)
    await load(adminKey)
  })

  const deleteAccount = (account: Account) => mutate(async () => {
    await api.deleteAccount(adminKey, account.id)
    setDialog(null)
    await load(adminKey)
  })

  const refreshAccount = (account: Account) => mutate(async () => {
    await api.refreshAccountQuota(adminKey, account.id)
    await load(adminKey)
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

  const rotate = (key: ClientKey) => mutate(async () => {
    const result = await api.rotateKey(adminKey, key.id)
    setDialog({ type: 'reveal', key: result.key, name: result.client.name })
    setCopied(false)
    await load(adminKey)
  })

  const reconcile = (key: ClientKey, charged: number, opus: number, byAccount?: Record<string, number>) => mutate(async () => {
    await api.reconcile(adminKey, key.id, charged, opus, byAccount)
    setDialog(null)
    await load(adminKey)
  })

  if (!adminKey) return <Login onConnect={connect} busy={busy} error={error} />

  return <div className="app-shell">
    <Sidebar view={view} setView={setView} quota={quota} showArchive={showArchive} onLogout={logout} />
    <div className="main-shell">
      <header className="topbar"><div className="breadcrumb">工作区 <span>/</span> {view === 'overview' ? '概览' : view === 'queue' ? '任务队列' : view === 'accounts' ? '账号管理' : view === 'keys' ? '密钥管理' : view === 'usage' ? '用量统计' : view === 'archive' ? '生成图库' : view === 'logs' ? '请求日志' : '配置'}</div><div className="topbar-right"><span className="topbar-time">{updatedAt ? `同步于 ${updatedAt.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}` : '正在连接'}</span><IconAction label="刷新面板数据" icon={<RefreshCw size={17} className={loading ? 'spin' : ''} />} onClick={() => void load(adminKey)} disabled={loading} /><span className="topbar-separator" /><span className="admin-chip"><ShieldCheck size={15} /> 管理员</span></div></header>
      <main className="content">
        {error && <div className="inline-alert page-alert" role="alert">{error}<button type="button" onClick={() => setError(null)} aria-label="关闭错误"><X size={16} /></button></div>}
        {view === 'overview' && <Overview keys={keys} quota={quota} quotaError={quotaError} onRefreshQuota={() => void refreshQuota()} refreshingQuota={refreshingQuota} setView={setView} onCreate={startCreateKey} />}
        {view === 'queue' && <QueuePage adminKey={adminKey} keys={keys} onAuthFailure={logout} />}
        {view === 'accounts' && <AccountsPage accounts={accounts} keys={keys} quota={quota} busy={busy} onCreate={() => setDialog({ type: 'account-create' })} onEdit={(account) => setDialog({ type: 'account-edit', account })} onDelete={(account) => setDialog({ type: 'account-delete', account })} onRefresh={(account) => void refreshAccount(account)} />}
        {view === 'keys' && <KeysPage keys={keys} onCreate={startCreateKey} onEdit={(key) => setDialog({ type: 'edit', key })} onReconcile={(key) => setDialog({ type: 'reconcile', key })} onRevoke={(key) => setDialog({ type: 'revoke', key })} onReveal={(key) => { if (key.key) { setCopied(false); setDialog({ type: 'reveal', key: key.key, name: key.name }) } }} onRotate={(key) => setDialog({ type: 'rotate', key })} />}
        {view === 'usage' && <UsagePage keys={keys} adminKey={adminKey} />}
        {view === 'archive' && showArchive && <ArchivePage adminKey={adminKey} keys={keys} onStatsChange={setArchiveStats} />}
        {view === 'logs' && <LogsPage adminKey={adminKey} keys={keys} accounts={accounts} onAuthFailure={logout} />}
        {view === 'settings' && <SettingsPage settings={settings} busy={busy} onChange={(changes) => void changeSettings(changes)} onSaveAdminPath={changeAdminPath} />}
      </main>
    </div>
    {(dialog?.type === 'create' || dialog?.type === 'edit') && <Modal title={dialog.type === 'create' ? '签发密钥' : `编辑 ${dialog.key.name}`} onClose={() => setDialog(null)}><KeyForm existing={dialog.type === 'edit' ? dialog.key : undefined} accounts={accounts} multiImageAvailable={settings?.allow_multi_image ?? false} busy={busy} error={error} onSave={savePolicy} onClose={() => setDialog(null)} /></Modal>}
    {(dialog?.type === 'account-create' || dialog?.type === 'account-edit') && <Modal title={dialog.type === 'account-create' ? '添加账号' : `编辑 ${dialog.account.name}`} onClose={() => setDialog(null)}><AccountForm existing={dialog.type === 'account-edit' ? dialog.account : undefined} busy={busy} error={error} onSave={saveAccount} onClose={() => setDialog(null)} /></Modal>}
    {dialog?.type === 'account-delete' && <Modal title="删除账号" onClose={() => setDialog(null)}><div className="modal-body"><p>确认删除 <strong>{dialog.account.name}</strong>？账号 Token 将从服务端移除。</p><div className="modal-actions"><Button label="取消" variant="secondary" onClick={() => setDialog(null)} /><Button label="删除账号" variant="destructive" isLoading={busy} onClick={() => void deleteAccount(dialog.account)} /></div></div></Modal>}
    {dialog?.type === 'revoke' && <Modal title="撤销密钥" onClose={() => setDialog(null)}><div className="modal-body"><p>确认撤销 <strong>{dialog.key.name}</strong>？该密钥将立即无法访问代理。</p><div className="modal-actions"><Button label="取消" variant="secondary" onClick={() => setDialog(null)} /><Button label="撤销密钥" variant="destructive" isLoading={busy} onClick={() => void revoke(dialog.key)} /></div></div></Modal>}
    {dialog?.type === 'rotate' && <Modal title="轮换密钥" onClose={() => setDialog(null)}><div className="modal-body"><p>确认轮换 <strong>{dialog.key.name}</strong>？旧密钥会立即失效；额度和累计用量保留。</p><div className="modal-actions"><Button label="取消" variant="secondary" onClick={() => setDialog(null)} /><Button label="轮换密钥" variant="primary" isLoading={busy} onClick={() => void rotate(dialog.key)} /></div></div></Modal>}
    {dialog?.type === 'reconcile' && <ReconcileModal item={dialog.key} accounts={accounts} busy={busy} error={error} onClose={() => setDialog(null)} onSave={(charged, opus, byAccount) => reconcile(dialog.key, charged, opus, byAccount)} />}
    {dialog?.type === 'reveal' && <Modal title="客户端密钥" onClose={() => setDialog(null)}><div className="modal-body"><p><strong>{dialog.name}</strong> 的客户端密钥</p><div className="secret-line"><code>{dialog.key}</code><IconAction label={copied ? '已复制' : '复制密钥'} icon={copied ? <Check size={17} /> : <Copy size={17} />} onClick={() => { void navigator.clipboard.writeText(dialog.key).then(() => setCopied(true)).catch(() => setError('复制失败，请手动选择密钥。')) }} /></div><div className="modal-actions"><Button label="完成" variant="primary" onClick={() => setDialog(null)} /></div></div></Modal>}
  </div>
}

function ReconcileModal({ item, accounts, busy, error, onSave, onClose }: { item: ClientKey; accounts: Account[]; busy: boolean; error: string | null; onSave: (charged: number, opus: number, byAccount?: Record<string, number>) => Promise<void>; onClose: () => void }) {
  const [charged, setCharged] = useState(item.pending_anlas)
  const pendingByAccount = Object.entries(item.opus_pending_by_account ?? {})
  const unknownPending = Math.max(0, item.opus_pending_images - pendingByAccount.reduce((sum, [, count]) => sum + count, 0))
  const [byAccount, setByAccount] = useState<Record<string, number>>(item.opus_pending_by_account ?? {})
  const [unknownCharged, setUnknownCharged] = useState(unknownPending)
  const [legacyOpus, setLegacyOpus] = useState(item.opus_pending_images)
  const opus = pendingByAccount.length ? Object.values(byAccount).reduce((sum, count) => sum + count, 0) + unknownCharged : legacyOpus
  return <Modal title={`核对 ${item.name}`} onClose={onClose}>
    <form className="modal-body" onSubmit={(event) => { event.preventDefault(); void onSave(charged, opus, pendingByAccount.length ? byAccount : undefined) }}>
      <div className="reconcile-summary"><span>待核对 Anlas <strong>{fmt(item.pending_anlas)}</strong></span><span>待核对 Opus <strong>{fmt(item.opus_pending_images)}</strong></span></div>
      <div className="field"><label htmlFor="charged-anlas">实际计入 Anlas</label><NumberInput id="charged-anlas" min="0" max="1000000000" step="1" value={charged} onChange={(value) => setCharged(value)} required /></div>
      {pendingByAccount.length ? <>
        {pendingByAccount.map(([accountID, count]) => <div className="field" key={accountID}><label htmlFor={`charged-opus-${accountID}`}>{accounts.find((account) => account.id === accountID)?.name ?? accountID} 实际计入 Opus（待核对 {fmt(count)}）</label><NumberInput id={`charged-opus-${accountID}`} min="0" max={count} step="1" value={byAccount[accountID] ?? 0} onChange={(value) => setByAccount((current) => ({ ...current, [accountID]: value }))} required /></div>)}
        {unknownPending > 0 && <div className="field"><label htmlFor="charged-opus-unknown">旧记录实际计入 Opus（待核对 {fmt(unknownPending)}）</label><NumberInput id="charged-opus-unknown" min="0" max={unknownPending} step="1" value={unknownCharged} onChange={(value) => setUnknownCharged(value)} required /></div>}
      </> : <div className="field"><label htmlFor="charged-opus">实际计入 Opus 次数</label><NumberInput id="charged-opus" min="0" max="10000000" step="1" value={legacyOpus} onChange={(value) => setLegacyOpus(value)} required /></div>}
      {error && <div className="form-error" role="alert">{error}</div>}
      <div className="modal-actions"><Button label="取消" variant="secondary" onClick={onClose} /><Button label="确认核对" variant="primary" type="submit" isLoading={busy} /></div>
    </form>
  </Modal>
}
