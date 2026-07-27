<script setup>
import { computed } from 'vue'
import { NBadge, NEmpty, NSwitch, NTag } from 'naive-ui'
import { basename, fmtCost, fmtTokens, relTime, shortId } from '@/lib/format.js'

const props = defineProps({
  sessions: { type: Array, required: true },
  selected: { type: String, default: '' },
  showNoise: { type: Boolean, default: false },
})
const emit = defineEmits(['select', 'update:showNoise'])

const total = computed(() => ({
  turns: props.sessions.reduce((n, s) => n + s.turns, 0),
  cost: props.sessions.reduce((n, s) => n + s.cost, 0),
}))

const label = (s) =>
  s.title || basename(s.cwd) || (s.session_id ? shortId(s.session_id) : 'no session id')
</script>

<template>
  <div class="pane">
    <header class="pane-head">
      <div class="row">
        <strong>Sessions</strong>
        <NBadge :value="sessions.length" type="default" />
      </div>
      <div class="row muted small">
        <span>{{ total.turns }} turns</span>
        <span v-if="fmtCost(total.cost)">{{ fmtCost(total.cost) }}</span>
      </div>
      <label class="row small noise">
        <NSwitch
          size="small"
          :value="showNoise"
          @update:value="(v) => emit('update:showNoise', v)"
        />
        <span class="muted">show noise</span>
      </label>
    </header>

    <div class="scroll">
      <NEmpty
        v-if="!sessions.length"
        size="small"
        description="No recorded sessions"
        class="empty"
      />

      <button
        v-for="s in sessions"
        :key="s.session_id"
        class="item"
        :class="{ active: s.session_id === selected }"
        @click="emit('select', s.session_id === selected ? '' : s.session_id)"
      >
        <div class="title">{{ label(s) }}</div>
        <div class="row small muted">
          <span>{{ s.turns }}t</span>
          <span v-if="s.subagents">↳{{ s.subagents }}</span>
          <span>{{ fmtTokens(s.cache_read + s.input_tokens + s.cache_create) }} in</span>
          <span v-if="fmtCost(s.cost)" class="cost">{{ fmtCost(s.cost) }}</span>
        </div>
        <div class="row small muted">
          <span class="mono">{{ shortId(s.session_id) || '—' }}</span>
          <span>{{ relTime(s.last_ts) }}</span>
        </div>
        <div v-if="s.sources?.length" class="row small">
          <NTag v-for="src in s.sources" :key="src" size="tiny" type="info" :bordered="false">
            {{ src }}
          </NTag>
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
  flex-direction: column;
  gap: 4px;
}
.scroll {
  overflow-y: auto;
  flex: 1;
}
.row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.small {
  font-size: 11px;
}
.muted {
  color: var(--gx-dim);
}
.mono {
  font-family: var(--gx-mono);
}
.noise {
  margin-top: 2px;
}
.item {
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
.item:hover {
  background: var(--gx-hover);
}
.item.active {
  background: var(--gx-active);
  border-left-color: var(--gx-accent);
}
.title {
  font-size: 12px;
  margin-bottom: 3px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.cost {
  color: var(--gx-cost);
}
.empty {
  padding: 24px 8px;
}
</style>
