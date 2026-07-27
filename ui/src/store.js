import { computed, reactive, readonly } from 'vue'
import { fetchExchange, fetchExchanges, fetchSessions, subscribeEvents } from './api.js'
import { startedMs } from './lib/format.js'

const state = reactive({
  sessions: [],
  exchanges: [],
  detail: null,
  selectedSession: '',
  selectedId: '',
  showNoise: false,
  follow: true,
  liveStatus: 'offline',
  loadingList: false,
  loadingDetail: false,
  error: '',
  // Context window is only derivable from a request body (Claude Code often
  // omits the [1m] marker from the model field and names it in the system
  // prompt), so it is learned from loaded details. Rows show a percentage only
  // once it is known, rather than assuming 200K.
  //
  // Keyed by session *and* model, because one session routinely mixes windows:
  // a 1M Opus turn beside a 200K Haiku side-turn. Caching a single window per
  // session let whichever turn was opened last set the denominator for every
  // other model's rows, which rendered a 1M turn as several hundred percent of
  // a 200K window.
  ctxWindowByModel: {},
})

const byStartedDesc = (a, b) => startedMs(b) - startedMs(a)

export const store = readonly(state)

export const visibleExchanges = computed(() => {
  let rows = state.exchanges
  if (state.selectedSession)
    rows = rows.filter((e) => (e.req?.session_id || '') === state.selectedSession)
  if (!state.showNoise) rows = rows.filter((e) => !e.is_noise)
  return [...rows].sort(byStartedDesc)
})

// Turn numbers count forward within a session, so #1 is where the session began.
export const turnNumbers = computed(() => {
  const bySession = new Map()
  for (const e of [...state.exchanges].sort((a, b) => startedMs(a) - startedMs(b))) {
    const sid = e.req?.session_id || ''
    const n = (bySession.get(sid) || 0) + 1
    bySession.set(sid, n)
    e.__turn = n
  }
  const out = {}
  for (const e of state.exchanges) out[e.id] = e.__turn
  return out
})

// Gap to the previous turn in the same session: how long the context sat idle,
// which is what determines whether the cache survived.
export const gaps = computed(() => {
  const out = {}
  const bySession = new Map()
  for (const e of [...state.exchanges].sort((a, b) => startedMs(a) - startedMs(b))) {
    const sid = e.req?.session_id || ''
    const prev = bySession.get(sid)
    if (prev) {
      const prevEnd = prev.ended_at ? Date.parse(prev.ended_at) : startedMs(prev)
      out[e.id] = Math.max(0, startedMs(e) - prevEnd)
    }
    bySession.set(sid, e)
  }
  return out
})

const ctxKey = (sid, model) => `${sid}|${model || ''}`

export const ctxWindowFor = (sid, model) => state.ctxWindowByModel[ctxKey(sid, model)] || 0

// Generation counters guard both list loads the same way selectExchange guards
// the detail fetch: rapid session/noise switching can leave an older request in
// flight, and its late response must not overwrite the newer state.
let sessionsGen = 0
let exchangesGen = 0

export async function loadSessions() {
  const gen = ++sessionsGen
  try {
    const { sessions } = await fetchSessions({ noise: state.showNoise ? 1 : '' })
    if (gen !== sessionsGen) return
    state.sessions = sessions || []
  } catch (e) {
    if (gen === sessionsGen) state.error = e.message
  }
}

export async function loadExchanges() {
  const gen = ++exchangesGen
  state.loadingList = true
  try {
    const { exchanges } = await fetchExchanges({
      session: state.selectedSession,
      noise: state.showNoise ? 1 : '',
      limit: 500,
    })
    if (gen !== exchangesGen) return
    state.exchanges = exchanges || []
    state.error = ''
  } catch (e) {
    if (gen === exchangesGen) state.error = e.message
  } finally {
    if (gen === exchangesGen) state.loadingList = false
  }
}

export async function selectSession(sid) {
  if (state.selectedSession === sid) return
  state.selectedSession = sid
  state.selectedId = ''
  state.detail = null
  await loadExchanges()
}

export async function selectExchange(id) {
  state.selectedId = id
  if (!id) {
    state.detail = null
    return
  }
  state.loadingDetail = true
  state.detail = null
  try {
    const d = await fetchExchange(id)
    // Guard against a slower earlier request landing after a newer selection.
    if (state.selectedId !== id) return
    state.detail = d
    const req = d.exchange?.req || {}
    if (d.ctx_window) state.ctxWindowByModel[ctxKey(req.session_id || '', req.model)] = d.ctx_window
    state.error = ''
  } catch (e) {
    state.error = e.message
  } finally {
    if (state.selectedId === id) state.loadingDetail = false
  }
}

export function setShowNoise(v) {
  state.showNoise = v
  return Promise.all([loadSessions(), loadExchanges()])
}

export function setFollow(v) {
  state.follow = v
}

function upsert(ex) {
  const i = state.exchanges.findIndex((e) => e.id === ex.id)
  if (i >= 0) state.exchanges[i] = ex
  else state.exchanges.unshift(ex)
}

export function startLive() {
  return subscribeEvents({
    onStatus: (s) => {
      state.liveStatus = s
    },
    onExchange: (ex) => {
      upsert(ex)
      // A turn that finishes while selected should refresh, since a response
      // body now exists where a moment ago there was none.
      if (ex.id === state.selectedId && ex.state !== 'inflight') selectExchange(ex.id)
      if (state.follow && !state.selectedId) loadSessions()
    },
    // No replay buffer server-side, so a reconnect means refetching from the
    // durable index rather than trusting the stream to have been continuous.
    onReconnect: () => {
      loadSessions()
      loadExchanges()
    },
  })
}

export async function init() {
  await Promise.all([loadSessions(), loadExchanges()])
}
