export function fmtTokens(n) {
  if (!n) return '0'
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
  return `${(n / 1_000_000).toFixed(2)}M`
}

export function fmtChars(n) {
  if (!n) return '0'
  return n < 1000 ? String(n) : `${(n / 1000).toFixed(1)}k`
}

// Turns that round to zero are the majority once the cache is warm, and a column
// of "$0.00" is pure noise — callers render nothing when this returns ''.
export function fmtCost(c) {
  if (!c || c < 0.0001) return ''
  return c < 1 ? `$${c.toFixed(4)}` : `$${c.toFixed(2)}`
}

export function fmtDuration(ms) {
  if (!ms || ms <= 0) return ''
  if (ms < 1000) return `${ms}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return `${m}m${s ? ` ${s}s` : ''}`
}

export function fmtGap(ms) {
  if (ms == null || ms < 0) return ''
  if (ms < 60_000) return `${Math.round(ms / 1000)}s`
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)}m`
  return `${(ms / 3_600_000).toFixed(1)}h`
}

// Cache lifetime bands. Anthropic's default cache TTL is 5 minutes, so a gap
// past that means the next turn re-pays to write the cache; past an hour even
// extended-TTL entries are gone. This is the cheapest cache-miss diagnosis
// available — it needs nothing but two timestamps.
export function gapSeverity(ms) {
  if (ms == null) return ''
  if (ms < 300_000) return 'ok'
  if (ms < 3_600_000) return 'warn'
  return 'bad'
}

export function gapTooltip(ms) {
  switch (gapSeverity(ms)) {
    case 'ok':
      return 'Gap under 5m — cache likely still warm'
    case 'warn':
      return 'Gap of 5m–1h — the default 5m cache has expired'
    case 'bad':
      return 'Gap over 1h — all cache has expired'
    default:
      return ''
  }
}

export function ctxUsed(usage) {
  if (!usage) return 0
  return (
    (usage.input_tokens || 0) +
    (usage.cache_read_input_tokens || 0) +
    (usage.cache_creation_input_tokens || 0)
  )
}

export function cacheHitPct(usage) {
  const used = ctxUsed(usage)
  if (!used) return null
  return Math.round(((usage.cache_read_input_tokens || 0) / used) * 100)
}

export function shortId(id, n = 8) {
  return id ? id.slice(0, n) : ''
}

export function basename(p) {
  if (!p) return ''
  const parts = p.replace(/\/+$/, '').split('/')
  return parts[parts.length - 1] || p
}

export function relTime(ms) {
  if (!ms) return ''
  const diff = Date.now() - ms
  if (diff < 60_000) return 'just now'
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`
  return `${Math.floor(diff / 86_400_000)}d ago`
}

export function clockTime(ms) {
  if (!ms) return ''
  return new Date(ms).toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

export function startedMs(ex) {
  return ex?.started_at ? Date.parse(ex.started_at) : 0
}

// An inflight turn's ended_at is Go's zero time, 0001-01-01, not absent; read
// as a date it puts the turn's end two thousand years in the past. Any end not
// after the start is no end.
export function endedMs(ex) {
  const end = ex?.ended_at ? Date.parse(ex.ended_at) : 0
  return end > startedMs(ex) ? end : 0
}

export function elapsedMs(ex) {
  if (!ex?.started_at || !ex?.ended_at) return 0
  const d = Date.parse(ex.ended_at) - Date.parse(ex.started_at)
  return d > 0 ? d : 0
}

// State drives the most important signal on a turn row: whether the recording
// is a complete, trustworthy audit record or a truncated one.
const STATE_TONE = {
  complete: 'neutral',
  inflight: 'info',
  client_aborted: 'warning',
  timeout: 'warning',
  upstream_error: 'error',
  persist_failed: 'error',
}

export const stateTone = (s) => STATE_TONE[s] || 'default'

export function stateLabel(s) {
  return (s || 'unknown').replace(/_/g, ' ')
}

export function prettyJSON(v) {
  if (v == null) return ''
  if (typeof v === 'string') {
    try {
      return JSON.stringify(JSON.parse(v), null, 2)
    } catch {
      return v
    }
  }
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}
