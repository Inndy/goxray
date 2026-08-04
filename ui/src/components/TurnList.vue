<script setup>
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
})
const emit = defineEmits(['select'])

const STD_ROUTE = /^\/v1\/messages(\?|$)/

// Only Claude Code's title-generation turns carry a real title. Rather than
// padding every other row with the same "/v1/messages?beta=true", the route is
// shown only when it is not the ordinary messages call — where it genuinely
// distinguishes the turn (count_tokens, /api/hello, a quota probe).
const title = (ex) => {
  if (ex.res?.title) return ex.res.title
  if (ex.url && !STD_ROUTE.test(ex.url)) return ex.url
  return ''
}
</script>

<template>
  <div class="pane">
    <header class="pane-head">
      <strong>Turns</strong>
      <span class="muted small">{{ exchanges.length }}</span>
      <NSpin v-if="loading" size="small" />
    </header>

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
        :class="[{ active: ex.id === selected }, `st-${ex.state}`]"
        @click="emit('select', ex.id)"
      >
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
          <NTooltip v-if="ex.res?.stop_reason === 'end_turn'" trigger="hover">
            <template #trigger><span class="endturn">↵</span></template>
            Ended the turn — was waiting on the user
          </NTooltip>
          <span class="spacer" />
          <span v-if="ex.status && ex.status !== 200" class="status">{{ ex.status }}</span>
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

        <div v-if="ex.is_noise || !ex.parse_ok || ex.source" class="line5">
          <NTag v-if="ex.source" size="tiny" type="info" :bordered="false">{{ ex.source }}</NTag>
          <NTag v-if="ex.is_noise" size="tiny" :bordered="false">noise</NTag>
          <NTooltip v-if="!ex.parse_ok" trigger="hover">
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
