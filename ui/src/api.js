const qs = (params) => {
  const u = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') u.set(k, String(v))
  }
  const s = u.toString()
  return s ? `?${s}` : ''
}

async function getJSON(path) {
  const res = await fetch(path, { headers: { accept: 'application/json' } })
  if (!res.ok) {
    let msg = `HTTP ${res.status}`
    try {
      const body = await res.json()
      if (body?.error) msg = body.error
    } catch {
      // Non-JSON error body; the status alone is the best we have.
    }
    throw new Error(msg)
  }
  return res.json()
}

export const fetchSessions = (opts = {}) => getJSON(`/api/sessions${qs(opts)}`)
export const fetchExchanges = (opts = {}) => getJSON(`/api/exchanges${qs(opts)}`)
export const fetchThreads = (opts = {}) => getJSON(`/api/threads${qs(opts)}`)
export const fetchExchange = (id) => getJSON(`/api/exchange/${encodeURIComponent(id)}`)
export const rawURL = (id, side) => `/api/exchange/${encodeURIComponent(id)}/raw?side=${side}`

// subscribeEvents streams committed exchanges. Each turn arrives twice in the
// normal case — once inflight, once final — so consumers upsert by id.
// EventSource reconnects on its own; onReconnect lets the caller refetch, since
// the server keeps no replay buffer and events during the gap are simply gone.
export function subscribeEvents({ onExchange, onReconnect, onStatus }) {
  const es = new EventSource('/api/events')
  let wasOpen = false

  es.onopen = () => {
    onStatus?.('live')
    if (wasOpen) onReconnect?.()
    wasOpen = true
  }
  es.onmessage = (e) => {
    try {
      onExchange(JSON.parse(e.data))
    } catch {
      // A malformed frame is not worth tearing the stream down for.
    }
  }
  es.onerror = () => onStatus?.('reconnecting')

  return () => es.close()
}
