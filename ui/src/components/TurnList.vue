<script setup>
import { computed } from 'vue'
import { NEmpty, NSpin, NTag, NTooltip } from 'naive-ui'
import CacheBar from './CacheBar.vue'
import {
  clockTime,
  elapsedMs,
  fmtCost,
  fmtDuration,
  fmtGap,
  gapSeverity,
  gapTooltip,
  stateLabel,
  stateTone,
  startedMs,
} from '@/lib/format.js'

const props = defineProps({
  exchanges: { type: Array, required: true },
  selected: { type: String, default: '' },
  turnNumbers: { type: Object, required: true },
  gaps: { type: Object, required: true },
  loading: { type: Boolean, default: false },
  ctxWindowOf: { type: Function, required: true },
  // thread → { color, depth, label, parent, running, lastState, turns, cost,
  // lastAt }; empty unless the selected session has more than one agent thread.
  lanes: { type: Object, default: () => ({}) },
  selectedThread: { type: String, default: '' },
})
const emit = defineEmits(['select', 'thread'])

const laneOf = (ex) => props.lanes[ex.req?.thread || '']
const laneList = computed(() => Object.values(props.lanes))
// What an agent is doing, in the words a reader tracking it wants: working now,
// or waiting with its last turn complete, or stopped on something that went
// wrong.
const agentStatus = (lane) => {
  if (lane.running) return { text: 'running', tone: 'run' }
  if (!lane.lastState || lane.lastState === 'complete') return { text: 'idle', tone: 'idle' }
  const bad = ['upstream_error', 'persist_failed'].includes(lane.lastState)
  return { text: stateLabel(lane.lastState), tone: bad ? 'bad' : 'warn' }
}

const laneTip = (lane) => {
  const parent = props.lanes[lane.parent]
  const from = parent ? ` · spawned by ${parent.label}` : ''
  const act = lane.thread === props.selectedThread ? 'show every thread' : 'show only this thread'
  return `${lane.label} · thread …${lane.thread.slice(-8)}${from} — click to ${act}`
}

// The ordinary model call for each client: Claude Code's messages call, and
// Codex's Responses endpoint on either backend.
const STD_ROUTE = /^(\/v1\/messages|\/backend-api\/codex\/responses|\/v1\/responses)(\?|$)/

// Only Claude Code's title-generation turns carry a real title. Rather than
// padding every other row with the same "/v1/messages?beta=true", the route is
// shown only when it is not the ordinary messages call — where it genuinely
// distinguishes the turn (count_tokens, /api/hello, a quota probe).
//
// Codex names its own requests instead: which agent made the call (a spawned
// sub-agent, a reviewer) and what kind it was (compaction, prewarm). Those are
// shown in the route's place; a plain turn by the root agent needs neither.
const title = (ex) => {
  if (ex.res?.title) return ex.res.title
  const parts = []
  if (ex.req?.kind && ex.req.kind !== 'turn') parts.push(ex.req.kind)
  if (ex.req?.agent && ex.req.agent !== '/root') parts.push(ex.req.agent)
  if (parts.length) return parts.join(' · ')
  if (ex.url && !STD_ROUTE.test(ex.url)) return ex.url
  return ''
}

// parse_ok is decided when the response is parsed, so an inflight turn has not
// failed to parse — it has not been parsed yet.
const unparsed = (ex) => !ex.parse_ok && ex.state !== 'inflight'

// Both clients' way of saying the model finished and handed back to the user.
const endedTurn = (ex) => ['end_turn', 'completed'].includes(ex.res?.stop_reason)
</script>

<template>
  <div class="pane">
    <header class="pane-head">
      <strong>Turns</strong>
      <span class="muted small">{{ exchanges.length }}</span>
      <NSpin v-if="loading" size="small" />
    </header>

    <div v-if="laneList.length" class="agents">
      <button
        v-for="lane in laneList"
        :key="lane.thread"
        class="agent"
        :class="{ on: lane.thread === selectedThread }"
        :style="{ '--lane': lane.color, '--depth': lane.depth }"
        :title="laneTip(lane)"
        @click="emit('thread', lane.thread)"
      >
        <span class="dot" :class="{ pulse: lane.running }" />
        <span class="name">{{ lane.label }}</span>
        <span class="status" :class="agentStatus(lane).tone">{{ agentStatus(lane).text }}</span>
        <span class="mono muted">{{ lane.turns }}t</span>
        <span class="mono cost">{{ fmtCost(lane.cost) || '—' }}</span>
        <span class="mono muted">{{ clockTime(lane.lastAt) }}</span>
      </button>
    </div>

    <div class="scroll">
      <NEmpty
        v-if="!exchanges.length && !loading"
        size="small"
        description="No turns"
        class="empty"
      />

      <button
        v-for="ex in exchanges"
        :key="ex.id"
        class="turn"
        :class="[{ active: ex.id === selected, laned: laneOf(ex) }, `st-${ex.state}`]"
        :style="
          laneOf(ex) ? { '--lane': laneOf(ex).color, '--depth': laneOf(ex).depth } : undefined
        "
        @click="emit('select', ex.id)"
      >
        <span
          v-if="laneOf(ex)"
          class="lane"
          :title="laneTip(laneOf(ex))"
          @click.stop="emit('thread', ex.req.thread)"
        />
        <div class="line1">
          <NTag size="tiny" :type="stateTone(ex.state)" :bordered="false" class="state">
            {{ stateLabel(ex.state) }}
          </NTag>
          <span class="num">
            <span v-if="ex.req?.is_subagent" class="sub" title="subagent turn">↳</span>#{{
              turnNumbers[ex.id] ?? '?'
            }}
          </span>
          <span class="model">{{ ex.req?.model || '—' }}</span>
          <NTooltip v-if="endedTurn(ex)" trigger="hover">
            <template #trigger><span class="endturn">↵</span></template>
            Ended the turn — was waiting on the user
          </NTooltip>
          <span class="spacer" />
          <span v-if="ex.status && ex.status !== 200 && ex.status !== 101" class="status">{{
            ex.status
          }}</span>
          <span v-if="fmtCost(ex.res?.cost)" class="cost">{{ fmtCost(ex.res.cost) }}</span>
        </div>

        <div v-if="title(ex)" class="line2">{{ title(ex) }}</div>

        <CacheBar :usage="ex.res?.usage" :ctx-window="ctxWindowOf(ex)" />

        <div class="line4">
          <span v-if="fmtDuration(elapsedMs(ex))" class="mono">{{
            fmtDuration(elapsedMs(ex))
          }}</span>
          <NTooltip v-if="gaps[ex.id] != null" trigger="hover">
            <template #trigger>
              <span class="mono gap" :class="gapSeverity(gaps[ex.id])">
                wait {{ fmtGap(gaps[ex.id]) }}
              </span>
            </template>
            {{ gapTooltip(gaps[ex.id]) }}
          </NTooltip>
          <span class="spacer" />
          <span class="mono muted"
            >{{ ex.req?.msg_count ?? 0 }}m · {{ ex.req?.tool_count ?? 0 }}t</span
          >
          <span class="mono muted">{{ clockTime(startedMs(ex)) }}</span>
        </div>

        <div v-if="ex.is_noise || unparsed(ex) || ex.source" class="line5">
          <NTag v-if="ex.source" size="tiny" type="info" :bordered="false">{{ ex.source }}</NTag>
          <NTag v-if="ex.is_noise" size="tiny" :bordered="false">noise</NTag>
          <NTooltip v-if="unparsed(ex)" trigger="hover">
            <template #trigger>
              <NTag size="tiny" type="warning" :bordered="false">unparsed</NTag>
            </template>
            Metadata could not be parsed — the raw bytes are still recorded verbatim
          </NTooltip>
        </div>
      </button>
    </div>
  </div>
</template>

<style scoped>
.pane {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-width: 0;
}
.pane-head {
  padding: 8px 10px;
  border-bottom: 1px solid var(--gx-border);
  display: flex;
  align-items: center;
  gap: 8px;
}
.scroll {
  overflow-y: auto;
  flex: 1;
}
/* One row per agent thread: the overview a lane alone cannot give — who is
   working right now and who has gone quiet. Capped so a guardian pool of a
   dozen threads cannot push the turns off screen. */
.agents {
  max-height: 30%;
  overflow-y: auto;
  padding: 4px 0;
  border-bottom: 1px solid var(--gx-border);
  flex: none;
}
.agent {
  display: grid;
  grid-template-columns: 8px minmax(0, 1fr) auto 3em 4.5em auto;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 2px 10px 2px calc(10px + var(--depth) * 12px);
  border: none;
  border-left: 2px solid transparent;
  background: none;
  color: var(--gx-fg);
  font: inherit;
  font-size: 11px;
  text-align: left;
  cursor: pointer;
}
.agent:hover {
  background: var(--gx-hover);
}
.agent.on {
  background: var(--gx-active);
  border-left-color: var(--lane);
}
.agent .name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.agent .status {
  font-size: 10px;
}
.status.run {
  color: var(--gx-accent);
}
.status.idle {
  color: var(--gx-dim);
}
.status.warn {
  color: var(--gx-warn);
}
.status.bad {
  color: var(--gx-error);
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--lane);
}
.dot.pulse {
  animation: blink 1.2s ease-in-out infinite;
}
@keyframes blink {
  50% {
    opacity: 0.25;
  }
}
/* A Codex session's turns stay in time order; the lane says which agent made
   each one, and the indent how deep in the spawn tree that agent sits. */
.turn.laned {
  position: relative;
  padding-left: calc(16px + var(--depth) * 10px);
}
.lane {
  position: absolute;
  top: 0;
  bottom: 0;
  left: calc(4px + var(--depth) * 10px);
  width: 5px;
  border-radius: 2px;
  background: var(--lane);
  opacity: 0.75;
  cursor: pointer;
}
.lane:hover {
  opacity: 1;
}
.turn {
  display: block;
  width: 100%;
  text-align: left;
  background: none;
  border: none;
  border-bottom: 1px solid var(--gx-border);
  border-left: 2px solid transparent;
  padding: 7px 10px;
  cursor: pointer;
  color: inherit;
  font: inherit;
}
.turn:hover {
  background: var(--gx-hover);
}
.turn.active {
  background: var(--gx-active);
  border-left-color: var(--gx-accent);
}
/* A truncated recording is the one thing that must never look routine. */
.turn.st-upstream_error,
.turn.st-persist_failed {
  border-left-color: var(--gx-error);
}
.turn.st-client_aborted,
.turn.st-timeout {
  border-left-color: var(--gx-warn);
}
.turn.st-inflight {
  border-left-color: var(--gx-accent);
  animation: pulse 1.6s ease-in-out infinite;
}
@keyframes pulse {
  50% {
    background: var(--gx-hover);
  }
}
.line1,
.line4,
.line5 {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
}
.line4,
.line5 {
  margin-top: 3px;
}
.line2 {
  font-size: 12px;
  margin: 3px 0 4px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--gx-fg);
}
.spacer {
  flex: 1;
}
.num {
  font-family: var(--gx-mono);
  font-weight: 600;
}
.sub {
  color: var(--gx-dim);
}
.model {
  color: var(--gx-dim);
  font-size: 10px;
}
.endturn {
  color: var(--gx-accent);
}
.status {
  font-family: var(--gx-mono);
  color: var(--gx-error);
}
.cost {
  font-family: var(--gx-mono);
  color: var(--gx-cost);
}
.mono {
  font-family: var(--gx-mono);
  font-size: 10px;
}
.muted {
  color: var(--gx-dim);
}
.small {
  font-size: 11px;
}
.gap.ok {
  color: var(--gx-ok);
}
.gap.warn {
  color: var(--gx-warn);
}
.gap.bad {
  color: var(--gx-error);
}
.state {
  text-transform: lowercase;
}
.empty {
  padding: 24px 8px;
}
</style>
