import { computed, reactive, readonly } from 'vue'
import {
  fetchExchange,
  fetchExchanges,
  fetchSessions,
  fetchThreads,
  subscribeEvents,
} from './api.js'
import { endedMs, startedMs } from './lib/format.js'

const state = reactive({
  sessions: [],
  exchanges: [],
  detail: null,
  selectedSession: '',
  selectedId: '',
  // One agent's thread inside the selected session (Codex only), or '' for all.
  selectedThread: '',
  // The selected session's per-thread rollup, from /api/threads: an aggregate of
  // the whole session, so it is right however the turn list is paged or
  // filtered, and survives a filter that hides every other agent's rows.
  threads: [],
  // Session-wide turn numbers by id; see numberTurns.
  turnNo: {},
  // Per session: how many turns the server's numbering covered, and the turns
  // numbered since, by start time; see applySeq.
  turnNoBase: {},
  provisional: {},
  showNoise: false,
  follow: true,
  liveStatus: 'offline',
  loadingList: false,
  loadingDetail: false,
  error: '',
  // Fallback context windows learned from loaded details, for captures taken
  // before the window was recorded per turn (`goxray migrate-usage` backfills
  // those; until it runs, opening one turn is still what lights up its siblings).
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

// Codex lane colours, assigned to threads in the order they started, so the root
// agent is always the first and a sub-agent keeps its colour as turns stream in.
const LANE_COLORS = [
  '#5fb3f9',
  '#f0a35e',
  '#9be37a',
  '#d58df5',
  '#f27a8a',
  '#5ed6c8',
  '#e8d36b',
  '#a3a8f8',
]

// Codex reports an agent as a path: /root, /root/<name>, /root/<name>/<child>.
// A reviewer or compaction thread reports its kind instead (review, compact).
const agentLabel = (agent) => {
  if (!agent || agent === '/root') return 'root'
  return agent.startsWith('/') ? agent.slice(agent.lastIndexOf('/') + 1) : agent
}

// Lanes exist only inside one session and only when it really has more than one
// agent: Claude Code turns carry no thread, and a single-agent Codex session
// would gain nothing but a stripe.
export const lanes = computed(() => {
  const all = state.threads
  if (!state.selectedSession || all.length < 2) return {}
  const byId = Object.fromEntries(all.map((t) => [t.thread, t]))
  const count = {}
  for (const t of all) count[agentLabel(t.agent)] = (count[agentLabel(t.agent)] || 0) + 1
  const depthOf = (t) => {
    let d = 0
    for (let p = t.parent_thread; p && d < 3; p = byId[p]?.parent_thread) d++
    return d
  }
  const out = {}
  // The server lists threads in the order they started, so the root agent is
  // always the first colour and a sub-agent keeps its colour as turns stream in.
  all.forEach((t, i) => {
    const base = agentLabel(t.agent)
    out[t.thread] = {
      thread: t.thread,
      color: LANE_COLORS[i % LANE_COLORS.length],
      depth: depthOf(t),
      // Two sub-agents may share a name; the thread id's tail tells them apart.
      label: count[base] > 1 ? `${base} ${t.thread.slice(-4)}` : base,
      agent: t.agent,
      parent: t.parent_thread,
      running: t.running,
      lastState: t.last_state,
      turns: t.turns,
      cost: t.cost,
      lastAt: t.last_ts,
    }
  })
  return out
})

// threadOfAgent finds the thread a multi-agent tool call addresses. Codex
// resolves a relative name (count_go) against the calling agent's path, so that
// is tried first: two agents may each have a child of the same name. A bare
// suffix match is the fallback for a caller whose path is unknown, and only
// when it is unambiguous — a guess that opens the wrong agent is worse than no
// link. Returns '' if no thread, or more than one, could be meant.
export function threadOfAgent(name, fromAgent) {
  if (!name) return ''
  const n = String(name)
  const exact = (path) => state.threads.find((t) => t.agent === path)?.thread || ''
  if (n.startsWith('/')) return exact(n)
  const from = fromAgent?.startsWith('/') ? fromAgent : '/root'
  const hit = exact(`${from}/${n}`)
  if (hit) return hit
  const tails = state.threads.filter((t) => t.agent.endsWith(`/${n}`))
  return tails.length === 1 ? tails[0].thread : ''
}

// focusThread narrows the list to one thread and returns the turn it began
// with, so a jump from a spawn_agent call lands on the sub-agent's first turn.
export async function focusThread(thread) {
  state.selectedThread = thread
  await loadExchanges()
  const own = state.exchanges.filter((e) => e.req?.thread === thread && !e.is_noise)
  own.sort((a, b) => startedMs(a) - startedMs(b))
  return own[0]?.id || ''
}

// Turn numbers count forward within a session, so #1 is where the session began.
// Inside a selected session the server numbers the rows it sends (seq) over the
// whole session: the list is a capped page of the newest turns, or one thread's,
// and numbering that page from 1 would call a 700-turn session's turn #201 "#1"
// and give a turn a different number under a thread filter. Only the global
// feed, which spans sessions and has no server numbering, counts the rows it
// holds.
//
// Turns that stream in after a load are numbered provisionally, after the
// server's count, in the order they started — not the order their frames
// arrived, which for concurrent agents can differ. The server's numbering
// replaces all of it on the next load, and is counted in the same noise domain
// as the list it came with.
function applySeq(seq, seqMax) {
  state.turnNo = { ...seq }
  state.turnNoBase = { [state.selectedSession]: seqMax || 0 }
  state.provisional = {}
}

function numberTurns() {
  const bySession = new Map()
  const out = {}
  for (const e of [...state.exchanges].sort((a, b) => startedMs(a) - startedMs(b))) {
    const sid = e.req?.session_id || ''
    const n = (bySession.get(sid) || 0) + 1
    bySession.set(sid, n)
    out[e.id] = n
  }
  state.turnNo = out
  state.turnNoBase = Object.fromEntries(bySession)
  state.provisional = {}
}

function numberNewTurn(ex) {
  // Hidden noise is not counted by the server's numbering either; giving a
  // prewarm a number would push every later turn one past its real one.
  if (ex.is_noise && !state.showNoise) return
  const sid = ex.req?.session_id || ''
  const prov = (state.provisional[sid] ||= {})
  if (state.turnNo[ex.id] != null && !(ex.id in prov)) return
  prov[ex.id] = startedMs(ex)
  const base = state.turnNoBase[sid] || 0
  Object.entries(prov)
    .sort(([ia, ta], [ib, tb]) => ta - tb || (ia < ib ? -1 : ia > ib ? 1 : 0))
    .forEach(([id], i) => {
      state.turnNo[id] = base + i + 1
    })
}

export const turnNumbers = computed(() => state.turnNo)

// Gap to the previous turn of the same conversation: how long the context sat
// idle, which is what determines whether the cache survived. A Codex sub-agent is
// its own conversation with its own cache, so the gap is per thread — measured
// across agents it would report the root as waiting while a sub-agent worked.
export const gaps = computed(() => {
  const out = {}
  const byThread = new Map()
  for (const e of [...state.exchanges].sort((a, b) => startedMs(a) - startedMs(b))) {
    const key = `${e.req?.session_id || ''}|${e.req?.thread || ''}`
    const prev = byThread.get(key)
    if (prev) {
      const prevEnd = endedMs(prev) || startedMs(prev)
      out[e.id] = Math.max(0, startedMs(e) - prevEnd)
    }
    byThread.set(key, e)
  }
  return out
})

// The session a loaded row belongs to. Opening a turn scopes the list to this,
// so the "Turns" pane means one session rather than a global feed a new session
// can slide into. A sessionless turn (side turn, subagent) resolves to '' and so
// deliberately leaves the list unfiltered: there is no session to narrow to.
export const sessionOfExchange = (id) =>
  state.exchanges.find((e) => e.id === id)?.req?.session_id || ''

const ctxKey = (sid, model) => `${sid}|${model || ''}`

// The window a turn was working against. Recorded on the turn itself, so a row
// that has just streamed in already has its denominator — the learned cache is
// only for records written before that was captured. 0 means unknown, which the
// bar renders as composition-only rather than assuming 200K.
export const ctxWindowOf = (ex) =>
  ex.req?.ctx_window || state.ctxWindowByModel[ctxKey(ex.req?.session_id || '', ex.req?.model)] || 0

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
    const {
      exchanges,
      seq,
      seq_max: seqMax,
    } = await fetchExchanges({
      session: state.selectedSession,
      thread: state.selectedThread,
      noise: state.showNoise ? 1 : '',
      limit: 500,
    })
    if (gen !== exchangesGen) return
    state.exchanges = exchanges || []
    if (seq) {
      applySeq(seq, seqMax)
      // An inflight row is not indexed yet and so has no number of its own.
      const unnumbered = state.exchanges.filter((e) => state.turnNo[e.id] == null)
      for (const e of unnumbered.sort((a, b) => startedMs(a) - startedMs(b))) numberNewTurn(e)
    } else if (!state.selectedThread) numberTurns()
    state.error = ''
  } catch (e) {
    if (gen === exchangesGen) state.error = e.message
  } finally {
    if (gen === exchangesGen) state.loadingList = false
  }
}

// Does not touch the turn selection: the route carries both, and applyRoute
// applies the id on its own. Clearing it here was redundant when a session was
// picked from the pane (that route has no id, so selectExchange('') clears it
// anyway) and actively wrong now that opening a turn also sets the session —
// re-opening the turn already on screen would have blanked it.
export async function selectSession(sid) {
  if (state.selectedSession === sid) return
  state.selectedSession = sid
  state.selectedThread = ''
  state.threads = []
  state.turnNo = {}
  state.turnNoBase = {}
  state.provisional = {}
  await Promise.all([loadExchanges(), loadThreads()])
}

let threadsGen = 0

export async function loadThreads() {
  const gen = ++threadsGen
  if (!state.selectedSession) {
    state.threads = []
    return
  }
  try {
    const { threads } = await fetchThreads({ session: state.selectedSession })
    if (gen !== threadsGen) return
    state.threads = threads || []
    // The hub drops frames for a client that falls behind rather than block,
    // without closing the stream, so a turn's final frame can be lost with no
    // reconnect to recover it. While anything shows running, poll: a summary
    // must not pulse "running" forever on the strength of one missed frame.
    if (state.threads.some((t) => t.running)) pollThreadsSoon()
  } catch (e) {
    if (gen !== threadsGen) return
    state.error = e.message
    // A failed poll must not end the polling: the summary it would have
    // corrected still shows the agent running.
    if (state.threads.some((t) => t.running)) pollThreadsSoon()
  }
}

// Picking the thread already selected clears the filter, so a lane or chip is a
// toggle.
export function selectThread(thread) {
  state.selectedThread = state.selectedThread === thread ? '' : thread
  return loadExchanges()
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
  // A foreign turn is invisible under an active session filter anyway, so
  // dropping it here just keeps a long-lived page from accumulating every
  // session's turns forever. Deselecting the session refetches, so nothing is
  // lost by not holding them.
  if (state.selectedSession && (ex.req?.session_id || '') !== state.selectedSession) return
  numberNewTurn(ex)
  if (state.selectedThread && (ex.req?.thread || '') !== state.selectedThread) return
  const i = state.exchanges.findIndex((e) => e.id === ex.id)
  if (i >= 0) state.exchanges[i] = ex
  else state.exchanges.unshift(ex)
}

// Every live frame moves the session rollups — a new session, another turn, more
// cost — and the pane is pure aggregate, so refetching it can never disturb the
// selection. Gating it on "nothing selected" therefore bought nothing and cost
// the feature: a reader who had clicked any turn saw no new session at all until
// they reloaded the page. Coalescing is the real concern, since each turn arrives
// twice and a busy proxy streams several a second.
const sessionsRefreshMS = 500
let sessionsTimer = null

function refreshSessionsSoon() {
  if (sessionsTimer) return
  sessionsTimer = setTimeout(() => {
    sessionsTimer = null
    loadSessions()
  }, sessionsRefreshMS)
}

function cancelSessionsRefresh() {
  clearTimeout(sessionsTimer)
  sessionsTimer = null
}

// The agent summary is refetched rather than patched from the stream, for the
// reason the session pane is: a patched copy drifts. A turn of a filtered-out
// thread whose final frame was missed would stay "running" forever, where the
// server's rollup simply no longer has it inflight.
// (A turn that has committed but is not indexed yet needs no timing here: the
// server counts it from the hub until the index has it.)
let threadsTimer = null
let threadsPollTimer = null
const threadsPollMS = 10000

function refreshThreadsSoon() {
  if (threadsTimer) return
  threadsTimer = setTimeout(() => {
    threadsTimer = null
    loadThreads()
  }, sessionsRefreshMS)
}

function pollThreadsSoon() {
  if (threadsPollTimer) return
  threadsPollTimer = setTimeout(() => {
    threadsPollTimer = null
    loadThreads()
  }, threadsPollMS)
}

function cancelThreadsRefresh() {
  for (const t of [threadsTimer, threadsPollTimer]) clearTimeout(t)
  threadsTimer = threadsPollTimer = null
}

export function startLive() {
  const stop = subscribeEvents({
    onStatus: (s) => {
      state.liveStatus = s
    },
    onExchange: (ex) => {
      upsert(ex)
      // A turn that finishes while selected should refresh, since a response
      // body now exists where a moment ago there was none.
      if (ex.id === state.selectedId && ex.state !== 'inflight') selectExchange(ex.id)
      if (state.follow) refreshSessionsSoon()
      if (state.selectedSession && ex.req?.session_id === state.selectedSession)
        refreshThreadsSoon()
    },
    // No replay buffer server-side, so a reconnect means refetching from the
    // durable index rather than trusting the stream to have been continuous.
    onReconnect: () => {
      loadSessions()
      loadExchanges()
      loadThreads()
    },
  })
  return () => {
    cancelSessionsRefresh()
    cancelThreadsRefresh()
    stop()
  }
}

export async function init() {
  await Promise.all([loadSessions(), loadExchanges()])
}
