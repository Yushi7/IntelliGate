import { useCallback, useEffect, useMemo, useState } from 'react'

const REFRESH_MS = 15000
const serviceNames = { products: 'Product', orders: 'Order', users: 'User' }

const queries = {
  totalRequests: 'sum(intelligate_http_requests_total)',
  health: 'intelligate_backend_health',
  latency: 'intelligate_backend_ewma_latency_seconds',
  inFlight: 'intelligate_backend_in_flight_requests',
  samples: 'intelligate_backend_successful_samples_total',
  distribution: 'sum by (service, backend_id) (intelligate_backend_selections_total)',
  strategy: 'intelligate_routing_strategy_info',
}

async function fetchGatewayHealth() {
  const response = await fetch('/api/gateway/health', { cache: 'no-store' })
  if (!response.ok) throw new Error(`Gateway returned HTTP ${response.status}`)
  return response.json()
}

async function queryPrometheus(query) {
  const params = new URLSearchParams({ query })
  const response = await fetch(`/api/prometheus/api/v1/query?${params}`, { cache: 'no-store' })
  if (!response.ok) throw new Error(`Prometheus returned HTTP ${response.status}`)
  const payload = await response.json()
  if (payload.status !== 'success') throw new Error(payload.error || 'Prometheus query failed')
  return payload.data.result.map((item) => ({
    labels: item.metric,
    value: Number(item.value?.[1]),
  })).filter((item) => Number.isFinite(item.value))
}

async function loadDashboard() {
  const [gateway, ...metricResults] = await Promise.allSettled([
    fetchGatewayHealth(),
    ...Object.values(queries).map(queryPrometheus),
  ])
  const metricKeys = Object.keys(queries)
  const metrics = Object.fromEntries(metricKeys.map((key, index) => {
    const result = metricResults[index]
    return [key, result.status === 'fulfilled'
      ? { data: result.value, error: null }
      : { data: null, error: result.reason?.message || 'Metric unavailable' }]
  }))
  return {
    gateway: gateway.status === 'fulfilled'
      ? { data: gateway.value, error: null }
      : { data: null, error: gateway.reason?.message || 'Gateway unavailable' },
    prometheusError: metricResults.every((result) => result.status === 'rejected')
      ? 'Prometheus is unavailable. Check that the monitoring stack is running.'
      : null,
    metrics,
  }
}

function formatNumber(value, digits = 0) {
  if (!Number.isFinite(value)) return '—'
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: digits }).format(value)
}

function formatLatency(seconds) {
  if (!Number.isFinite(seconds)) return '—'
  if (seconds < 0.001) return `${formatNumber(seconds * 1e6, 0)} µs`
  if (seconds < 1) return `${formatNumber(seconds * 1000, 2)} ms`
  return `${formatNumber(seconds, 3)} s`
}

function useMetricRows(metric, expectedServices = null) {
  const data = metric?.data
  if (!data) return { rows: [], unavailable: metric?.error || 'Waiting for Prometheus data' }
  const rows = expectedServices
    ? expectedServices.map((service) => data.filter((row) => row.labels.service === service))
    : data
  return { rows: Array.isArray(rows) ? rows : [rows], unavailable: null }
}

function MetricCard({ label, value, detail, status = 'normal' }) {
  return (
    <article className={`metric-card ${status}`}>
      <span className="metric-label">{label}</span>
      <strong className="metric-value">{value}</strong>
      {detail && <span className="metric-detail">{detail}</span>}
    </article>
  )
}

function SectionHeading({ eyebrow, title, detail }) {
  return (
    <div className="section-heading">
      <div>
        <span className="eyebrow">{eyebrow}</span>
        <h2>{title}</h2>
      </div>
      {detail && <p>{detail}</p>}
    </div>
  )
}

function HealthCard({ service, rows, metricError }) {
  const healthy = rows.find((row) => row.labels.backend_id)
  const instances = rows.length
  const isHealthy = healthy?.value === 1 && rows.every((row) => row.value === 1)
  const state = instances === 0 ? 'No health series' : isHealthy ? 'Healthy' : 'Degraded'
  return (
    <article className="health-card">
      <div className="health-card-top">
        <div className={`service-icon ${service}`}>{service[0].toUpperCase()}</div>
        <div className="health-name"><h3>{serviceNames[service]}</h3><span>{service}</span></div>
        <span className={`status-pill ${instances === 0 ? 'unknown' : isHealthy ? 'healthy' : 'degraded'}`}>
          <i />{state}
        </span>
      </div>
      {metricError ? <p className="unavailable">{metricError}</p> : instances === 0 ? (
        <p className="unavailable">No backend health data has been reported.</p>
      ) : (
        <div className="instance-list">
          {rows.map((row) => (
            <div className="instance-row" key={row.labels.backend_id}>
              <span>{row.labels.backend_id || 'Backend'}</span>
              <span className={`instance-state ${row.value === 1 ? 'up' : 'down'}`}>
                {row.value === 1 ? 'Available' : 'Unhealthy'}
              </span>
            </div>
          ))}
        </div>
      )}
    </article>
  )
}

function BarChart({ rows, formatValue, emptyMessage }) {
  if (!rows.length) return <div className="chart-empty">{emptyMessage}</div>
  const max = Math.max(...rows.map((row) => row.value), 0)
  return (
    <div className="bar-chart">
      {rows.map((row) => {
        const width = max === 0 ? 0 : Math.max((row.value / max) * 100, row.value > 0 ? 2 : 0)
        return (
          <div className="bar-row" key={row.key}>
            <span className="bar-name" title={row.label}>{row.label}</span>
            <div className="bar-track"><div className={`bar-fill ${row.tone || ''}`} style={{ width: `${width}%` }} /></div>
            <span className="bar-value">{formatValue(row.value)}</span>
          </div>
        )
      })}
    </div>
  )
}

function App() {
  const [snapshot, setSnapshot] = useState(null)
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [lastUpdated, setLastUpdated] = useState(null)
  const [refreshError, setRefreshError] = useState(null)

  const refresh = useCallback(async (manual = false) => {
    if (manual) setRefreshing(true)
    try {
      const next = await loadDashboard()
      setSnapshot(next)
      setLastUpdated(new Date())
      setRefreshError(null)
    } catch (error) {
      setRefreshError(error.message || 'Could not refresh dashboard data')
    } finally {
      setLoading(false)
      setRefreshing(false)
    }
  }, [])

  useEffect(() => {
    refresh()
    const timer = window.setInterval(() => refresh(), REFRESH_MS)
    return () => window.clearInterval(timer)
  }, [refresh])

  const metrics = snapshot?.metrics || {}
  const health = useMetricRows(metrics.health)
  const serviceHealth = useMemo(() => Object.fromEntries(
    Object.keys(serviceNames).map((service) => [service, (metrics.health?.data || []).filter((row) => row.labels.service === service)]),
  ), [metrics.health])
  const sampleCounts = useMemo(() => new Map((metrics.samples?.data || []).map((row) => [row.labels.backend_id, row.value])), [metrics.samples])
  const latencyRows = useMemo(() => (metrics.latency?.data || []).map((row) => ({
    key: row.labels.backend_id,
    label: row.labels.backend_id || row.labels.service,
    value: row.value,
  })).filter((row) => row.key && (sampleCounts.get(row.key) || 0) > 0), [metrics.latency, sampleCounts])
  const distributionRows = useMemo(() => (metrics.distribution?.data || []).map((row) => ({
    key: row.labels.backend_id,
    label: row.labels.backend_id || row.labels.service,
    value: Math.max(row.value, 0),
    tone: 'violet',
  })).filter((row) => row.key), [metrics.distribution])
  const inFlightRows = useMemo(() => (metrics.inFlight?.data || []).map((row) => ({
    key: row.labels.backend_id,
    label: row.labels.backend_id || row.labels.service,
    value: row.value,
    tone: 'mint',
  })).filter((row) => row.key), [metrics.inFlight])
  const requestTotal = metrics.totalRequests?.data?.[0]?.value
  const activeStrategy = metrics.strategy?.data?.find((row) => row.value === 1)?.labels.strategy
  const strategyLabel = activeStrategy === 'round_robin' ? 'Round Robin' : activeStrategy === 'adaptive' ? 'Adaptive' : null
  const gatewayAvailable = Boolean(snapshot?.gateway.data)
  const gatewayStatus = refreshError ? 'Connection issue' : gatewayAvailable ? 'Operational' : loading ? 'Checking' : 'Unavailable'

  return (
    <main className="app-shell">
      <header className="topbar">
        <a className="brand" href="#top" aria-label="IntelliGate dashboard home">
          <span className="brand-mark"><span /><span /><span /></span>
          <span>Intelli<span className="brand-accent">Gate</span></span>
          <span className="brand-divider" />
          <span className="brand-subtitle">OPERATIONS</span>
        </a>
        <div className="topbar-actions">
          <span className="live-label"><i /> AUTO REFRESH · 15S</span>
          <button className="refresh-button" onClick={() => refresh(true)} disabled={refreshing}>
            <span className={refreshing ? 'refresh-icon spinning' : 'refresh-icon'}>↻</span>
            {refreshing ? 'Refreshing' : 'Refresh'}
          </button>
        </div>
      </header>

      <div className="content" id="top">
        <section className="page-intro">
          <div><span className="eyebrow">GATEWAY MONITORING</span><h1>System overview</h1><p>Live health and routing signals from your IntelliGate deployment.</p></div>
          <div className="updated-at">{lastUpdated ? <>LAST UPDATED <strong>{lastUpdated.toLocaleTimeString()}</strong></> : 'WAITING FOR DATA'}</div>
        </section>

        {refreshError && <div className="alert error-alert"><strong>Dashboard refresh failed.</strong> {refreshError} Check gateway and Prometheus connectivity.</div>}
        {snapshot?.prometheusError && <div className="alert warning-alert"><strong>Prometheus unavailable.</strong> {snapshot.prometheusError}</div>}

        <section className="overview-grid" aria-label="Gateway overview">
          <MetricCard label="GATEWAY STATUS" value={gatewayStatus} detail={snapshot?.gateway.data?.service || snapshot?.gateway.error || 'Checking /health'} status={gatewayAvailable ? 'positive' : 'muted'} />
          <MetricCard label="TOTAL REQUESTS" value={metrics.totalRequests?.error ? 'Unavailable' : requestTotal === undefined ? 'No data' : formatNumber(requestTotal)} detail="Counter total since gateway start" />
          <MetricCard label="ROUTING STRATEGY" value={metrics.strategy?.error ? 'Unavailable' : strategyLabel || (metrics.strategy?.data ? 'Unknown' : 'Loading')} detail={metrics.strategy?.error || 'Active gateway configuration'} status={strategyLabel ? 'positive' : 'muted'} />
          <MetricCard label="DATA REFRESH" value={refreshError ? 'Issue' : refreshing ? 'Refreshing' : 'Automatic'} detail={`Every ${REFRESH_MS / 1000}s · manual refresh available`} status={refreshError ? 'warning' : 'positive'} />
        </section>

        <section className="dashboard-section">
          <SectionHeading eyebrow="SERVICE STATUS" title="Backend health" detail="Reported by the gateway’s active health probes." />
          {health.unavailable && <div className="inline-unavailable">Health metric unavailable: {health.unavailable}</div>}
          <div className="health-grid">
            {Object.keys(serviceNames).map((service) => <HealthCard key={service} service={service} rows={serviceHealth[service]} metricError={metrics.health?.error} />)}
          </div>
        </section>

        <section className="dashboard-section">
          <SectionHeading eyebrow="ROUTING SIGNALS" title="Adaptive routing" detail="Per-backend telemetry from the gateway’s Prometheus registry." />
          <div className="adaptive-grid">
            <div className="panel chart-panel">
              <div className="panel-heading"><div><h3>EWMA latency</h3><p>Upstream time to first response headers</p></div><span className="unit-tag">TTFB</span></div>
              {metrics.latency?.error ? <div className="chart-empty">{metrics.latency.error}</div> : latencyRows.length === 0 ? <div className="chart-empty">No latency series reported yet.</div> : <BarChart rows={latencyRows.map((row) => ({ ...row, tone: 'blue' }))} formatValue={formatLatency} emptyMessage="No latency data" />}
            </div>
            <div className="panel chart-panel">
              <div className="panel-heading"><div><h3>Backend request distribution</h3><p>Cumulative selections since the gateway started</p></div><span className="unit-tag">TOTAL</span></div>
              {metrics.distribution?.error ? <div className="chart-empty">{metrics.distribution.error}</div> : <BarChart rows={distributionRows} formatValue={(value) => formatNumber(value, 1)} emptyMessage="No backend selections have been recorded yet." />}
            </div>
            <div className="panel chart-panel">
              <div className="panel-heading"><div><h3>In-flight requests</h3><p>Attempts currently executing, including streamed bodies</p></div><span className="unit-tag">LIVE</span></div>
              {metrics.inFlight?.error ? <div className="chart-empty">{metrics.inFlight.error}</div> : <BarChart rows={inFlightRows.map((row) => ({ ...row, value: Math.max(row.value, 0) }))} formatValue={(value) => formatNumber(value)} emptyMessage="No in-flight series reported." />}
            </div>
          </div>
          <div className="panel sample-panel">
            <div className="panel-heading"><div><h3>Successful latency samples</h3><p>Only completed response bodies with a valid TTFB contribute a sample.</p></div></div>
            {metrics.samples?.error ? <div className="chart-empty">{metrics.samples.error}</div> : latencyRows.length === 0 ? <div className="chart-empty">No backend sample series available.</div> : (
              <div className="sample-list">{latencyRows.map((row) => {
                const count = sampleCounts.get(row.key)
                return <div className="sample-item" key={row.key}><span className="sample-dot" /> <span>{row.label}</span><strong>{count === undefined ? 'No sample data' : formatNumber(count)}</strong></div>
              })}</div>
            )}
          </div>
        </section>

        <footer className="footer"><span>INTELLIGATE · ADAPTIVE API GATEWAY</span><span>Metrics are provided by the configured Prometheus scrape target.</span></footer>
      </div>
    </main>
  )
}

export default App
