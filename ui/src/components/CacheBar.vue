<script setup>
import { computed } from 'vue'
import { NTooltip } from 'naive-ui'
import { cacheHitPct, ctxUsed, fmtTokens } from '@/lib/format.js'

const props = defineProps({
  usage: { type: Object, default: null },
  // 0 when unknown. The window is only derivable from a request body, so rows
  // render composition-only until a detail load reveals it — never assuming a
  // 200K window, which would show a 1M session at over 100%.
  ctxWindow: { type: Number, default: 0 },
})

const used = computed(() => ctxUsed(props.usage))
const hit = computed(() => cacheHitPct(props.usage))

const segments = computed(() => {
  const u = props.usage || {}
  // Scale against the window when known, else against this turn's own total so
  // the bar still shows composition.
  const denom = props.ctxWindow || used.value || 1
  const seg = (n, cls, label) => ({
    cls,
    label,
    tokens: n || 0,
    pct: ((n || 0) / denom) * 100,
  })
  return [
    seg(u.cache_read_input_tokens, 'read', 'cache read'),
    seg(u.cache_creation_input_tokens, 'write', 'cache write'),
    seg(u.input_tokens, 'input', 'fresh input'),
  ].filter((s) => s.tokens > 0)
})

const ctxPct = computed(() => (props.ctxWindow ? (used.value / props.ctxWindow) * 100 : null))

const ctxClass = computed(() => {
  if (ctxPct.value == null) return ''
  if (ctxPct.value > 95) return 'critical'
  if (ctxPct.value > 85) return 'warning'
  return ''
})
</script>

<template>
  <div v-if="used" class="wrap">
    <NTooltip trigger="hover" placement="top">
      <template #trigger>
        <div class="bar">
          <div
            v-for="s in segments"
            :key="s.cls"
            class="seg"
            :class="s.cls"
            :style="{ width: `${s.pct}%` }"
          />
        </div>
      </template>
      <div class="tip">
        <div v-for="s in segments" :key="s.cls">{{ s.label }}: {{ fmtTokens(s.tokens) }}</div>
        <div class="tip-total">
          used {{ fmtTokens(used) }}<span v-if="ctxWindow"> / {{ fmtTokens(ctxWindow) }}</span>
        </div>
        <div v-if="!ctxWindow" class="tip-note">window unknown until this turn is opened</div>
      </div>
    </NTooltip>

    <div class="labels">
      <span v-if="hit !== null" :class="{ cold: hit < 10 }">cache {{ hit }}%</span>
      <span v-if="ctxPct !== null" :class="ctxClass">ctx {{ Math.round(ctxPct) }}%</span>
      <span v-else class="dim">ctx {{ fmtTokens(used) }}</span>
    </div>
  </div>
</template>

<style scoped>
.wrap {
  display: flex;
  align-items: center;
  gap: 8px;
}
.bar {
  flex: 1;
  min-width: 60px;
  height: 6px;
  border-radius: 3px;
  background: var(--gx-track);
  display: flex;
  overflow: hidden;
}
.seg.read {
  background: var(--gx-cache-read);
}
.seg.write {
  background: var(--gx-cache-write);
}
.seg.input {
  background: var(--gx-input);
}
.labels {
  display: flex;
  gap: 7px;
  font-size: 10px;
  color: var(--gx-dim);
  white-space: nowrap;
  font-family: var(--gx-mono);
}
.cold {
  color: var(--gx-error);
}
.warning {
  color: var(--gx-warn);
}
.critical {
  color: var(--gx-error);
  font-weight: 600;
}
.dim {
  opacity: 0.75;
}
.tip {
  font-size: 11px;
  font-family: var(--gx-mono);
}
.tip-total {
  margin-top: 4px;
  border-top: 1px solid rgba(255, 255, 255, 0.2);
  padding-top: 3px;
}
.tip-note {
  opacity: 0.7;
  font-style: italic;
}
</style>
