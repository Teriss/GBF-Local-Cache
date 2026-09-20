import { useEffect, useMemo, useState } from 'react'
import {
  Activity,
  ArrowDownToLine,
  BarChart3,
  CircleAlert,
  Database,
  Eraser,
  ExternalLink,
  FolderOpen,
  Gauge,
  HardDrive,
  LockKeyhole,
  Moon,
  Network,
  Pause,
  Play,
  RefreshCw,
  Settings,
  ShieldCheck,
  Square,
  Sun,
  Trash2,
  Wrench,
  X,
} from 'lucide-react'

type Page = 'overview' | 'cache' | 'network' | 'security' | 'logs' | 'settings'
type ServiceState = 'stopped' | 'starting' | 'running' | 'stopping' | 'error' | 'migrating'
type Theme = 'light' | 'dark'
type NoticeKind = 'info' | 'success' | 'error'

type Snapshot = {
  state: ServiceState
  listen_address: string
  network_mode: 'direct' | 'clash'
  cache_root: string
  ram_used_bytes: number
  ram_max_bytes: number
  disk_bytes: number
  stats: {
    total_requests: number
    ram_hits: number
    disk_hits: number
    misses: number
    origin_bytes: number
    cache_bytes_served: number
    bytes_saved: number
    errors: number
    range_requests: number
    cacheable_requests: number
  }
  certificate: {
    ready: boolean
    installed: boolean
    fingerprint_sha256?: string
    created_at?: string
    directory: string
    error?: string
  }
  migration: {
    state: 'idle' | 'running' | 'paused' | 'completed' | 'canceled' | 'error'
    old_root?: string
    new_root?: string
    total_bytes: number
    copied_bytes: number
    total_files: number
    copied_files: number
    speed_bytes_per_second: number
    current_file?: string
    error?: string
  }
  logs: Array<{
    time: string
    category: string
    method?: string
    target?: string
    message?: string
    duration?: string
  }>
  error?: string
}

type ConnectionTestResult = {
  ok: boolean
  host: string
  status: number
  duration_ms: number
  dns_ms: number
  tcp_ms: number
  tls_ms: number
  ttfb_ms: number
  download_bytes: number
  throughput_bps: number
  http_version: string
  error?: string
}

type HealthReport = {
  checked_at: string
  files: number
  bytes: number
  valid_files: number
  invalid_files: number
  invalid_bytes: number
  roots: string[]
}

type BackendApp = {
  Snapshot?: () => Promise<Snapshot>
  Start?: () => Promise<void>
  Stop?: () => Promise<void>
  TestConnection?: () => Promise<ConnectionTestResult>
  OpenCacheFolder?: () => Promise<void>
  OpenConfigFolder?: () => Promise<void>
  ClearLogs?: () => Promise<void>
  ClearCache?: () => Promise<void>
  InspectCache?: () => Promise<HealthReport>
  ChooseCacheRoot?: () => Promise<string>
  ChangeCacheRoot?: (root: string) => Promise<void>
  PauseMigration?: () => Promise<void>
  ResumeMigration?: () => Promise<void>
  CancelMigration?: () => Promise<void>
  SetNetworkMode?: (mode: string, protocol: string, host: string, port: number, username: string, password: string) => Promise<void>
  InstallCertificate?: () => Promise<void>
  UninstallCertificate?: () => Promise<void>
  RegenerateCertificate?: () => Promise<void>
  GetStartupEnabled?: () => Promise<boolean>
  SetStartupEnabled?: (enabled: boolean) => Promise<void>
}

type Notify = (message: string, kind?: NoticeKind) => void

const emptySnapshot: Snapshot = {
  state: 'stopped',
  listen_address: '127.0.0.1:8124',
  network_mode: 'direct',
  cache_root: '',
  ram_used_bytes: 0,
  ram_max_bytes: 0,
  disk_bytes: 0,
  stats: {
    total_requests: 0,
    ram_hits: 0,
    disk_hits: 0,
    misses: 0,
    origin_bytes: 0,
    cache_bytes_served: 0,
    bytes_saved: 0,
    errors: 0,
    range_requests: 0,
    cacheable_requests: 0,
  },
  certificate: { ready: false, installed: false, directory: '' },
  migration: { state: 'idle', total_bytes: 0, copied_bytes: 0, total_files: 0, copied_files: 0, speed_bytes_per_second: 0 },
  logs: [],
}

const navItems: Array<{ id: Page; label: string; icon: typeof Activity }> = [
  { id: 'overview', label: '概览', icon: BarChart3 },
  { id: 'cache', label: '缓存', icon: ArrowDownToLine },
  { id: 'network', label: '网络', icon: Network },
  { id: 'security', label: '安全', icon: ShieldCheck },
  { id: 'logs', label: '日志', icon: Activity },
  { id: 'settings', label: '设置', icon: Settings },
]

declare global {
  interface Window {
    go?: { main?: { App?: BackendApp } }
  }
}

function getBackend(): BackendApp | undefined {
  return window.go?.main?.App
}

function formatBytes(bytes: number): string {
  if (!bytes) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  return `${(bytes / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`
}

function formatSpeed(bytesPerSecond: number): string {
  return `${formatBytes(bytesPerSecond)}/s`
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

function stateLabel(state: ServiceState): string {
  switch (state) {
    case 'running': return '运行中'
    case 'starting': return '启动中'
    case 'stopping': return '停止中'
    case 'migrating': return '迁移中'
    case 'error': return '异常'
    default: return '已停止'
  }
}

function App() {
  const [page, setPage] = useState<Page>('overview')
  const [snapshot, setSnapshot] = useState<Snapshot>(emptySnapshot)
  const [busy, setBusy] = useState(false)
  const [theme, setTheme] = useState<Theme>('light')
  const [notice, setNotice] = useState<{ message: string; kind: NoticeKind } | null>(null)

  const backend = getBackend()

  const notify: Notify = (message, kind = 'info') => {
    setNotice({ message, kind })
    window.setTimeout(() => setNotice((current) => current?.message === message ? null : current), 3600)
  }

  const refresh = async () => {
    const currentBackend = getBackend()
    if (!currentBackend?.Snapshot) return
    try {
      const next = await currentBackend.Snapshot()
      if (next) setSnapshot(next)
    } catch (error) {
      console.warn('Unable to refresh service snapshot:', error)
    }
  }

  useEffect(() => {
    document.documentElement.dataset.theme = theme
  }, [theme])

  useEffect(() => {
    void refresh()
    const timer = window.setInterval(() => void refresh(), 1500)
    return () => window.clearInterval(timer)
  }, [])

  const toggleService = async () => {
    const currentBackend = getBackend()
    if (!currentBackend) {
      notify('请通过 Wails 应用运行，浏览器预览没有后端服务。', 'error')
      return
    }
    setBusy(true)
    try {
      const wasRunning = snapshot.state === 'running'
      if (wasRunning) await currentBackend.Stop?.()
      else await currentBackend.Start?.()
      await refresh()
      notify(wasRunning ? '服务已停止' : '服务已启动', 'success')
    } catch (error) {
      notify(`服务操作失败：${errorText(error)}`, 'error')
    } finally {
      setBusy(false)
    }
  }

  const openCacheFolder = async () => {
    if (!backend?.OpenCacheFolder) {
      notify('当前预览环境没有连接到桌面后端。', 'error')
      return
    }
    try {
      await backend.OpenCacheFolder()
      notify('已打开缓存目录', 'success')
    } catch (error) {
      notify(`打开缓存目录失败：${errorText(error)}`, 'error')
    }
  }

  const openConfigFolder = async () => {
    if (!backend?.OpenConfigFolder) {
      notify('当前预览环境没有连接到桌面后端。', 'error')
      return
    }
    try {
      await backend.OpenConfigFolder()
      notify('已打开配置目录', 'success')
    } catch (error) {
      notify(`打开配置目录失败：${errorText(error)}`, 'error')
    }
  }

  const clearLogs = async () => {
    if (!backend?.ClearLogs) {
      notify('当前预览环境没有连接到桌面后端。', 'error')
      return
    }
    try {
      await backend.ClearLogs()
      await refresh()
      notify('日志已清空', 'success')
    } catch (error) {
      notify(`清空日志失败：${errorText(error)}`, 'error')
    }
  }

  const hitRate = useMemo(() => {
    const cacheable = snapshot.stats.cacheable_requests
    if (!cacheable) return 0
    return Math.round(((snapshot.stats.ram_hits + snapshot.stats.disk_hits) / cacheable) * 100)
  }, [snapshot])

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="brand">
          <div className="brand-mark"><img src="/appicon.png" alt="" /></div>
          <div><strong>GBF Local Cache</strong><span>静态资源加速</span></div>
        </div>
        <nav className="nav-list" aria-label="主导航">
          {navItems.map(({ id, label, icon: Icon }) => (
            <button className={`nav-item ${page === id ? 'active' : ''}`} key={id} onClick={() => setPage(id)}>
              <Icon size={18} strokeWidth={page === id ? 2.2 : 1.8} />
              <span>{label}</span>
            </button>
          ))}
        </nav>
      </aside>

      <main className="content">
        <header className="topbar">
          <div>
            <p className="eyebrow">LOCAL SERVICE</p>
            <h1>{navItems.find((item) => item.id === page)?.label}</h1>
          </div>
          <div className="topbar-actions">
            <span className={`status-pill ${snapshot.state === 'running' ? 'online' : snapshot.state === 'error' ? 'danger' : 'offline'}`}>
              <span className="status-dot" />
              {stateLabel(snapshot.state)}
            </span>
            <button className="service-button" onClick={() => void toggleService()} disabled={busy}>
              {snapshot.state === 'running' ? <Square size={15} /> : <Play size={15} />}
              {snapshot.state === 'running' ? '停止服务' : '启动服务'}
            </button>
          </div>
        </header>

        {snapshot.error && <div className="error-banner"><CircleAlert size={17} />{snapshot.error}</div>}

        {page === 'overview' && <Overview snapshot={snapshot} hitRate={hitRate} />}
        {page === 'cache' && <CachePage snapshot={snapshot} backend={backend} onNotify={notify} onRefresh={() => void refresh()} onOpenCacheFolder={() => void openCacheFolder()} />}
        {page === 'network' && <NetworkPage snapshot={snapshot} backend={backend} onNotify={notify} />}
        {page === 'security' && <SecurityPage snapshot={snapshot} backend={backend} onNotify={notify} onRefresh={() => void refresh()} />}
        {page === 'logs' && <LogsPage snapshot={snapshot} onRefresh={() => void refresh()} onClearLogs={() => void clearLogs()} />}
        {page === 'settings' && <SettingsPage snapshot={snapshot} backend={backend} theme={theme} onThemeChange={() => setTheme((current) => current === 'light' ? 'dark' : 'light')} onOpenConfigFolder={() => void openConfigFolder()} onNotify={notify} />}
      </main>

      {notice && <div className={`toast toast-${notice.kind}`} role="status"><CircleAlert size={16} /><span>{notice.message}</span><button className="toast-close" onClick={() => setNotice(null)} aria-label="关闭提示"><X size={15} /></button></div>}
    </div>
  )
}

function DonutChart({ value }: { value: number }) {
  const radius = 46
  const circumference = 2 * Math.PI * radius
  const safeValue = Math.max(0, Math.min(100, value))
  return <div className="donut-chart">
    <svg viewBox="0 0 120 120" aria-label={`命中率 ${safeValue}%`} role="img">
      <circle className="donut-track" cx="60" cy="60" r={radius} />
      <circle className="donut-value" cx="60" cy="60" r={radius} strokeDasharray={`${circumference * safeValue / 100} ${circumference}`} />
    </svg>
    <div className="donut-center"><strong>{safeValue}%</strong><span>命中率</span></div>
  </div>
}

function Overview({ snapshot, hitRate }: { snapshot: Snapshot; hitRate: number }) {
  const cards = [
    { label: '命中率', value: `${hitRate}%`, hint: 'RAM + Disk', icon: Gauge },
    { label: '缓存大小', value: formatBytes(snapshot.disk_bytes), hint: 'Disk object store', icon: HardDrive },
    { label: '节省流量', value: formatBytes(snapshot.stats.bytes_saved), hint: '估算值', icon: Database },
    { label: '请求数', value: snapshot.stats.total_requests.toLocaleString('zh-CN'), hint: '本次运行', icon: Activity },
  ]
  const sources = [
    { label: 'RAM 命中', value: snapshot.stats.ram_hits, color: 'blue' },
    { label: '磁盘命中', value: snapshot.stats.disk_hits, color: 'mint' },
    { label: 'MISS 回源', value: snapshot.stats.misses, color: 'amber' },
  ]
  const maxSource = Math.max(...sources.map((source) => source.value), 1)
  const recent = snapshot.logs.slice(-10)
  return (
    <div className="page-grid overview-grid">
      <section className="metric-grid">
        {cards.map(({ icon: Icon, ...card }) => <div className="metric-card" key={card.label}><div className="metric-label"><span>{card.label}</span><Icon size={15} /></div><strong>{card.value}</strong><small>{card.hint}</small></div>)}
      </section>
      <section className="chart-grid">
        <div className="panel chart-panel">
          <div className="panel-heading"><div><span className="section-label">CACHE PERFORMANCE</span><h3>缓存命中概览</h3></div></div>
          <div className="chart-layout">
            <DonutChart value={hitRate} />
            <div className="chart-summary"><div><strong>{snapshot.stats.ram_hits + snapshot.stats.disk_hits}</strong><span>已命中请求</span></div><div><strong>{snapshot.stats.misses}</strong><span>回源请求</span></div><div><strong>{formatBytes(snapshot.stats.cache_bytes_served)}</strong><span>缓存响应</span></div></div>
          </div>
        </div>
        <div className="panel chart-panel">
          <div className="panel-heading"><div><span className="section-label">REQUEST FLOW</span><h3>请求来源分布</h3></div></div>
          <div className="source-bars">
            {sources.map((source) => <div className="source-row" key={source.label}><div className="source-meta"><span>{source.label}</span><strong>{source.value.toLocaleString('zh-CN')}</strong></div><div className="bar-track"><span className={`bar-value ${source.color}`} style={{ width: `${source.value / maxSource * 100}%` }} /></div></div>)}
          </div>
          <p className="chart-footnote">总请求 {snapshot.stats.total_requests.toLocaleString('zh-CN')} · 可缓存 {snapshot.stats.cacheable_requests.toLocaleString('zh-CN')}</p>
        </div>
      </section>
      <section className="split-grid">
        <div className="panel"><div className="panel-heading"><div><span className="section-label">当前网络模式</span><h3>{snapshot.network_mode === 'direct' ? 'UU / 系统直连' : 'Clash'}</h3></div></div><p className="muted">代理监听 <code>{snapshot.listen_address}</code>，只绑定本机回环地址。</p></div>
        <div className="panel"><div className="panel-heading"><div><span className="section-label">最近活动</span><h3>{recent.length ? `${recent.length} 条记录` : '暂无活动'}</h3></div></div><div className="activity-strip">{recent.length ? recent.map((entry, index) => <span className={`activity-dot activity-${entry.category.toLowerCase()}`} title={`${entry.category} ${entry.message || ''}`} key={`${entry.time}-${index}`} />) : <span className="muted">启动服务后，静态 CDN 请求会显示在这里。</span>}</div></div>
      </section>
    </div>
  )
}

function CachePage({ snapshot, backend, onNotify, onRefresh, onOpenCacheFolder }: { snapshot: Snapshot; backend?: BackendApp; onNotify: Notify; onRefresh: () => void; onOpenCacheFolder: () => void }) {
  const [working, setWorking] = useState(false)
  const [health, setHealth] = useState<HealthReport | null>(null)
  const migration = snapshot.migration

  const inspect = async () => {
    if (!backend?.InspectCache) return onNotify('当前预览环境没有连接到桌面后端。', 'error')
    setWorking(true)
    try {
      const report = await backend.InspectCache()
      setHealth(report)
      onNotify(report.invalid_files ? `体检完成：发现 ${report.invalid_files} 个异常文件` : `体检完成：${report.valid_files} 个缓存文件正常`, report.invalid_files ? 'error' : 'success')
    } catch (error) {
      onNotify(`缓存体检失败：${errorText(error)}`, 'error')
    } finally { setWorking(false) }
  }

  const clear = async () => {
    if (!backend?.ClearCache) return onNotify('当前预览环境没有连接到桌面后端。', 'error')
    if (!window.confirm('确定清理全部本地缓存吗？此操作不可撤销。')) return
    setWorking(true)
    try { await backend.ClearCache(); setHealth(null); onRefresh(); onNotify('缓存已清理', 'success') }
    catch (error) { onNotify(`清理缓存失败：${errorText(error)}`, 'error') }
    finally { setWorking(false) }
  }

  const chooseRoot = async () => {
    if (!backend?.ChooseCacheRoot || !backend.ChangeCacheRoot) return onNotify('当前预览环境没有连接到桌面后端。', 'error')
    setWorking(true)
    try {
      const root = await backend.ChooseCacheRoot()
      if (root) { await backend.ChangeCacheRoot(root); onRefresh(); onNotify('缓存迁移已开始，可在迁移期间启动服务', 'success') }
    } catch (error) { onNotify(`修改缓存目录失败：${errorText(error)}`, 'error') }
    finally { setWorking(false) }
  }

  const migrationAction = async (action: 'pause' | 'resume' | 'cancel') => {
    const method = action === 'pause' ? backend?.PauseMigration : action === 'resume' ? backend?.ResumeMigration : backend?.CancelMigration
    if (!method) return onNotify('当前预览环境没有连接到桌面后端。', 'error')
    try { await method(); onRefresh(); onNotify(action === 'cancel' ? '迁移已取消，旧目录已恢复为主目录' : action === 'pause' ? '迁移已暂停' : '迁移已继续', 'success') }
    catch (error) { onNotify(`迁移操作失败：${errorText(error)}`, 'error') }
  }

  const migrationProgress = migration.total_bytes ? Math.min(100, Math.round(migration.copied_bytes / migration.total_bytes * 100)) : 0
  return <div className="page-grid">
    <section className="panel large-panel">
      <div className="panel-heading"><div><span className="section-label">DISK CACHE · RAM LRU</span><h2>缓存空间</h2></div></div>
      <div className="detail-list">
        <div><span>缓存目录</span><strong title={snapshot.cache_root}>{snapshot.cache_root || '未设置'}</strong></div>
        <div><span>磁盘占用</span><strong>{formatBytes(snapshot.disk_bytes)}</strong></div>
        <div><span>RAM 使用</span><strong>{formatBytes(snapshot.ram_used_bytes)} / {formatBytes(snapshot.ram_max_bytes)}</strong></div>
        <div><span>磁盘 / RAM 命中</span><strong>{snapshot.stats.disk_hits.toLocaleString('zh-CN')} / {snapshot.stats.ram_hits.toLocaleString('zh-CN')}</strong></div>
        <div><span>MISS / Range</span><strong>{snapshot.stats.misses.toLocaleString('zh-CN')} / {snapshot.stats.range_requests.toLocaleString('zh-CN')}</strong></div>
      </div>
      <div className="action-row">
        <button className="secondary-button" onClick={onOpenCacheFolder}><FolderOpen size={16} />打开缓存目录</button>
        <button className="secondary-button" onClick={() => void chooseRoot()} disabled={working || migration.state === 'running'}><ExternalLink size={16} />更改缓存位置</button>
        <button className="secondary-button" onClick={() => void inspect()} disabled={working}><Wrench size={16} />体检缓存</button>
        <button className="secondary-button danger-button" onClick={() => void clear()} disabled={working}><Trash2 size={16} />清理缓存</button>
      </div>
      {health && <div className={`test-result ${health.invalid_files ? 'test-failed' : 'test-ok'}`}><strong>{health.invalid_files ? '发现异常缓存' : '缓存完整性正常'}</strong><span>文件 {health.files} · 有效 {health.valid_files} · 异常 {health.invalid_files} · 数据 {formatBytes(health.bytes)}</span></div>}
      {migration.state === 'running' || migration.state === 'paused' ? <div className="migration-box"><div className="migration-heading"><strong>正在迁移缓存 · {migrationProgress}%</strong><span>{migration.copied_files}/{migration.total_files} 文件 · {formatSpeed(migration.speed_bytes_per_second)}</span></div><div className="progress-track"><span style={{ width: `${migrationProgress}%` }} /></div><small>{migration.current_file || '准备中…'}</small><div className="action-row"><button className="secondary-button" onClick={() => void migrationAction(migration.state === 'paused' ? 'resume' : 'pause')}>{migration.state === 'paused' ? <Play size={15} /> : <Pause size={15} />}{migration.state === 'paused' ? '继续' : '暂停'}</button><button className="secondary-button danger-button" onClick={() => void migrationAction('cancel')}><Trash2 size={15} />取消迁移</button></div></div> : migration.state === 'completed' ? <div className="notice"><ShieldCheck size={17} /><span>迁移已完成。旧缓存目录保留未删除，可确认无误后手动清理。</span></div> : null}
      {migration.error && <div className="test-result test-failed"><strong>迁移失败</strong><small>{migration.error}</small></div>}
      <div className="notice"><CircleAlert size={17} /><span>{backend ? '只缓存白名单 CDN 的 GET / HEAD 静态资源，缓存写入采用校验后原子替换。' : '浏览器预览没有桌面后端，请使用 Wails 应用运行。'}</span></div>
    </section>
  </div>
}

function NetworkPage({ snapshot, backend, onNotify }: { snapshot: Snapshot; backend?: BackendApp; onNotify: Notify }) {
  const [testing, setTesting] = useState(false)
  const [result, setResult] = useState<ConnectionTestResult | null>(null)
  const [mode, setMode] = useState<'direct' | 'clash'>(snapshot.network_mode)
  const [protocol, setProtocol] = useState('http')
  const [host, setHost] = useState('127.0.0.1')
  const [port, setPort] = useState(7897)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')

  useEffect(() => setMode(snapshot.network_mode), [snapshot.network_mode])

  const applyMode = async (nextMode: 'direct' | 'clash') => {
    setMode(nextMode)
    if (nextMode === 'direct') {
      onNotify('已选择 UU / 系统直连，正在验证网络…')
    } else {
      onNotify('已选择 Clash，请填写配置并点击应用。')
    }
  }

  const saveMode = async () => {
    if (!backend?.SetNetworkMode) return onNotify('当前预览环境没有连接到桌面后端。', 'error')
    setTesting(true)
    try {
      await backend.SetNetworkMode(mode, protocol, host, port, username, password)
      onNotify('网络模式已切换，缓存数据未改变', 'success')
    } catch (error) { onNotify(`切换网络失败：${errorText(error)}`, 'error') }
    finally { setTesting(false) }
  }

  const testConnection = async () => {
    if (!backend?.TestConnection) return onNotify('当前预览环境没有连接到桌面后端。', 'error')
    setTesting(true)
    try {
      const next = await backend.TestConnection()
      setResult(next)
      onNotify(next.ok ? `连接成功：${next.duration_ms} ms` : `连接失败：${next.error || '未知错误'}`, next.ok ? 'success' : 'error')
    } catch (error) { onNotify(`连接测试失败：${errorText(error)}`, 'error') }
    finally { setTesting(false) }
  }

  return <div className="page-grid"><section className="panel large-panel"><div className="panel-heading"><div><span className="section-label">ORIGIN NETWORK</span><h2>网络出口</h2></div></div><div className="radio-row"><button className={`radio ${mode === 'direct' ? 'active' : ''}`} onClick={() => void applyMode('direct')}><span /> <div><strong>UU / 系统直连</strong><small>不读取 HTTP_PROXY / HTTPS_PROXY，不继承系统代理。</small></div></button><button className={`radio ${mode === 'clash' ? 'active' : ''}`} onClick={() => void applyMode('clash')}><span /> <div><strong>Clash</strong><small>支持 HTTP Proxy 与 SOCKS5，切换前会先执行连接测试。</small></div></button></div>{mode === 'clash' && <div className="form-grid"><label>协议<select value={protocol} onChange={(event) => setProtocol(event.target.value)}><option value="http">HTTP</option><option value="socks5">SOCKS5</option></select></label><label>地址<input value={host} onChange={(event) => setHost(event.target.value)} /></label><label>端口<input type="number" min={1} max={65535} value={port} onChange={(event) => setPort(Number(event.target.value))} /></label><label>用户名<input value={username} onChange={(event) => setUsername(event.target.value)} /></label><label>密码<input type="password" value={password} onChange={(event) => setPassword(event.target.value)} /></label></div>}<div className="action-row"><button className="primary-button" onClick={() => void saveMode()} disabled={testing}><ShieldCheck size={16} />应用网络模式</button><button className="secondary-button" onClick={() => void testConnection()} disabled={testing}><RefreshCw size={16} className={testing ? 'spin' : ''} />{testing ? '测试中…' : '测试连接'}</button></div>{result && <div className={`test-result ${result.ok ? 'test-ok' : 'test-failed'}`}><strong>{result.ok ? '连接已建立' : '连接失败'}</strong><span>{result.host}{result.status ? ` · HTTP ${result.status}` : ''} · DNS {result.dns_ms} ms · TCP {result.tcp_ms} ms · TLS {result.tls_ms} ms · TTFB {result.ttfb_ms} ms</span><span>下载 {formatBytes(result.download_bytes)} · {formatSpeed(result.throughput_bps)} · {result.http_version || '—'}</span>{result.error && <small>{result.error}</small>}</div>}<div className="notice"><ShieldCheck size={17} /><span>当前模式：{snapshot.network_mode === 'direct' ? 'UU / 系统直连' : 'Clash'}。切换网络不会影响 RAM、Disk 或迁移状态。</span></div></section></div>
}

function SecurityPage({ snapshot, backend, onNotify, onRefresh }: { snapshot: Snapshot; backend?: BackendApp; onNotify: Notify; onRefresh: () => void }) {
  const [working, setWorking] = useState(false)
  const certificate = snapshot.certificate
  const operate = async (action: 'install' | 'uninstall' | 'regenerate') => {
    const method = action === 'install' ? backend?.InstallCertificate : action === 'uninstall' ? backend?.UninstallCertificate : backend?.RegenerateCertificate
    if (!method) return onNotify('当前预览环境没有连接到桌面后端。', 'error')
    if (action === 'regenerate' && !window.confirm('重新生成 Root CA 会使已安装的旧证书失效，确定继续吗？')) return
    setWorking(true)
    try { await method(); onRefresh(); onNotify(action === 'install' ? 'Root CA 已安装到当前用户证书存储' : action === 'uninstall' ? 'Root CA 已从当前用户证书存储移除' : 'Root CA 已重新生成，请重新安装', 'success') }
    catch (error) { onNotify(`证书操作失败：${errorText(error)}`, 'error') }
    finally { setWorking(false) }
  }
  return <div className="page-grid"><section className="panel large-panel"><div className="panel-heading"><div><span className="section-label">SECURITY BOUNDARY</span><h2>安全边界</h2></div></div><div className="security-status"><div className="security-icon"><ShieldCheck size={25} /></div><div><strong>Fail closed 已启用</strong><p>代理只接受精确配置的 GBF 静态 CDN 主机，未知域名 CONNECT 和请求都会返回 403。</p></div></div><div className="detail-list"><div><span>监听地址</span><strong>{snapshot.listen_address}</strong></div><div><span>Root CA 状态</span><strong>{certificate.ready ? certificate.installed ? '已安装' : '已生成，未安装' : certificate.error || '未准备'}</strong></div><div><span>SHA-256</span><strong className="breakable">{certificate.fingerprint_sha256 || '—'}</strong></div><div><span>创建时间</span><strong>{certificate.created_at ? new Date(certificate.created_at).toLocaleString('zh-CN') : '—'}</strong></div><div><span>证书目录</span><strong title={certificate.directory}>{certificate.directory || '—'}</strong></div><div><span>动态 API / WebSocket</span><strong>不在代理范围内</strong></div></div><div className="action-row"><button className="secondary-button" onClick={() => void operate('install')} disabled={working || certificate.installed}><ShieldCheck size={16} />安装证书</button><button className="secondary-button" onClick={() => void operate('uninstall')} disabled={working || !certificate.installed}><Trash2 size={16} />卸载证书</button><button className="secondary-button" onClick={() => void operate('regenerate')} disabled={working}><RefreshCw size={16} />重新生成</button></div><div className="notice"><LockKeyhole size={17} /><span>本程序只代理白名单 GBF 静态 CDN；登录、动态 API、战斗、抽卡、结算和 WebSocket 不会经过本程序。</span></div></section></div>
}

function LogsPage({ snapshot, onRefresh, onClearLogs }: { snapshot: Snapshot; onRefresh: () => void; onClearLogs: () => void }) {
  return <div className="page-grid"><section className="panel log-panel"><div className="panel-heading"><div><span className="section-label">RING BUFFER · 5000</span><h2>最近日志</h2></div><div className="panel-actions"><button className="icon-button" title="刷新日志" onClick={onRefresh}><RefreshCw size={17} /></button><button className="icon-button" title="清空日志" onClick={onClearLogs}><Eraser size={17} /></button></div></div>{snapshot.logs.length === 0 ? <div className="empty-state">暂无日志</div> : <div className="log-list">{snapshot.logs.slice().reverse().map((entry, index) => <div className="log-row" key={`${entry.time}-${index}`}><time>{new Date(entry.time).toLocaleTimeString('zh-CN')}</time><span className={`log-tag tag-${entry.category.toLowerCase()}`}>{entry.category}</span><code>{entry.target || '—'}</code><span className="log-message">{entry.message}</span><small>{entry.duration}</small></div>)}</div>}</section></div>
}

function SettingsPage({ snapshot, backend, theme, onThemeChange, onOpenConfigFolder, onNotify }: { snapshot: Snapshot; backend?: BackendApp; theme: Theme; onThemeChange: () => void; onOpenConfigFolder: () => void; onNotify: Notify }) {
  const [startupEnabled, setStartupEnabled] = useState(false)
  const [startupWorking, setStartupWorking] = useState(false)

  useEffect(() => {
    if (!backend?.GetStartupEnabled) return
    void backend.GetStartupEnabled().then(setStartupEnabled).catch((error) => onNotify(`读取开机自启状态失败：${errorText(error)}`, 'error'))
  }, [backend])

  const toggleStartup = async () => {
    if (!backend?.SetStartupEnabled) return onNotify('当前预览环境没有连接到桌面后端。', 'error')
    setStartupWorking(true)
    try {
      const next = !startupEnabled
      await backend.SetStartupEnabled(next)
      setStartupEnabled(next)
      onNotify(next ? '已开启开机自启，启动后会隐藏到托盘' : '已关闭开机自启', 'success')
    } catch (error) {
      onNotify(`设置开机自启失败：${errorText(error)}`, 'error')
    } finally { setStartupWorking(false) }
  }

  return <div className="page-grid"><section className="panel large-panel settings-panel"><div className="panel-heading"><div><span className="section-label">APPLICATION</span><h2>设置</h2></div></div><div className="detail-list"><div><span>版本</span><strong>1.0.0</strong></div><div><span>本地代理端口</span><strong>{snapshot.listen_address}</strong></div><div><span>缓存路径</span><strong title={snapshot.cache_root}>{snapshot.cache_root || '—'}</strong></div><div><span>配置文件</span><strong>%LOCALAPPDATA%\GBFLocalCache\config.json</strong></div></div><div className="setting-list"><div className="setting-row"><div><strong>开机自启</strong><small>登录 Windows 后自动启动服务，并隐藏到右下角托盘。</small></div><button className={`switch-control ${startupEnabled ? 'active' : ''}`} role="switch" aria-checked={startupEnabled} onClick={() => void toggleStartup()} disabled={startupWorking}><span /></button></div><div className="setting-row"><div><strong>关闭窗口</strong><small>点击右上角 X 只隐藏面板，服务继续在托盘后台运行。</small></div><span className="setting-value">托盘后台</span></div></div><div className="action-row"><button className="secondary-button" onClick={onOpenConfigFolder}><ExternalLink size={16} />打开配置目录</button><button className="secondary-button" onClick={onThemeChange}>{theme === 'light' ? <Moon size={16} /> : <Sun size={16} />}{theme === 'light' ? '切换深色主题' : '切换浅色主题'}</button><button className="secondary-button" onClick={() => onNotify('配置写入采用临时文件、flush、fsync 后原子替换。', 'success')}><Settings size={16} />配置说明</button></div></section></div>
}

export default App
