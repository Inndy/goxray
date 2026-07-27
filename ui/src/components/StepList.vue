<script setup>
import { NTag, NTooltip } from 'naive-ui'
import { toolPreview } from '@/lib/toolPreview.js'

const props = defineProps({
  steps: { type: Array, required: true },
  selected: { type: Object, default: null },
})
const emit = defineEmits(['select'])

const isSel = (i, callIdx = null) =>
  props.selected?.stepIdx === i && (props.selected?.callIdx ?? null) === callIdx

const clip = (s, n) => {
  const t = String(s ?? '')
    .replace(/\s+/g, ' ')
    .trim()
  return t.length > n ? `${t.slice(0, n)}…` : t
}

// Box-drawing brackets make parallel calls in one assistant message read as a
// unit instead of as unrelated consecutive rows.
const bracket = (idx, len) => (len === 1 ? '─' : idx === 0 ? '┌' : idx === len - 1 ? '└' : '│')

const callIcon = (c) => (c.pending ? '⏳' : c.isError ? '✗' : '✓')
</script>

<template>
  <div class="steps">
    <template v-for="(step, i) in steps" :key="i">
      <!-- human turn: the boundary a reader scans for -->
      <div
        v-if="step.type === 'human'"
        class="step human"
        :class="{ sel: isSel(i) }"
        @click="emit('select', { stepIdx: i, callIdx: null })"
      >
        <div class="head">
          <span class="idx">#{{ i + 1 }}</span>
          <span class="icon">👤</span>
          <span class="text">{{ clip(step.humanText, 220) || '(no typed text)' }}</span>
        </div>
        <div v-if="step.hasInjected || step.orphans.length || step.images.length" class="sub">
          <NTag v-if="step.hasInjected" size="tiny" :bordered="false">injected context</NTag>
          <NTag v-if="step.images.length" size="tiny" :bordered="false">
            {{ step.images.length }} image{{ step.images.length > 1 ? 's' : '' }}
          </NTag>
          <NTooltip v-if="step.orphans.length" trigger="hover">
            <template #trigger>
              <NTag size="tiny" type="warning" :bordered="false">
                {{ step.orphans.length }} orphan result
              </NTag>
            </template>
            A tool_result with no matching tool_use — usually a compaction dropped the call
          </NTooltip>
        </div>
      </div>

      <!-- harness system message: a real context consumer that is easy to miss -->
      <div
        v-else-if="step.type === 'system'"
        class="step system"
        :class="{ sel: isSel(i) }"
        @click="emit('select', { stepIdx: i, callIdx: null })"
      >
        <div class="head">
          <span class="idx">#{{ i + 1 }}</span>
          <span class="icon">⚙</span>
          <span class="text">{{ clip(step.text, 180) }}</span>
        </div>
      </div>

      <div
        v-else-if="step.type === 'assistant-text'"
        class="step assistant"
        :class="{ sel: isSel(i) }"
        @click="emit('select', { stepIdx: i, callIdx: null })"
      >
        <div v-if="step.thinking" class="thinking">🧠 {{ clip(step.thinking, 90) }}</div>
        <div class="head">
          <span class="idx">#{{ i + 1 }}</span>
          <span class="icon">🤖</span>
          <span class="text">{{ clip(step.text, 200) }}</span>
        </div>
      </div>

      <div v-else-if="step.type === 'tool-group'" class="step group">
        <div
          v-if="step.thinking"
          class="thinking clickable"
          :class="{ sel: isSel(i) }"
          @click="emit('select', { stepIdx: i, callIdx: null })"
        >
          🧠 {{ clip(step.thinking, 90) }}
        </div>
        <div
          v-for="(c, ci) in step.calls"
          :key="ci"
          class="call"
          :class="{ sel: isSel(i, ci), err: c.isError }"
          @click="emit('select', { stepIdx: i, callIdx: ci })"
        >
          <span class="bracket">{{ bracket(ci, step.calls.length) }}</span>
          <span class="tool">{{ c.name }}</span>
          <span class="preview">{{ c.preview || toolPreview(c.name, c.input) }}</span>
          <span class="mark" :class="{ err: c.isError, pend: c.pending }">{{ callIcon(c) }}</span>
        </div>
      </div>

      <div
        v-else
        class="step other"
        :class="{ sel: isSel(i) }"
        @click="emit('select', { stepIdx: i, callIdx: null })"
      >
        <span class="idx">#{{ i + 1 }}</span>
        <span class="icon">?</span>
        <span class="text">{{ step.role }}: {{ clip(step.text, 160) }}</span>
      </div>
    </template>
  </div>
</template>

<style scoped>
.steps {
  font-size: 12px;
}
.step {
  padding: 4px 8px;
  cursor: pointer;
  border-left: 2px solid transparent;
}
.step:hover,
.call:hover,
.clickable:hover {
  background: var(--gx-hover);
}
.sel {
  background: var(--gx-active) !important;
}
.head {
  display: flex;
  align-items: baseline;
  gap: 6px;
}
.idx {
  font-family: var(--gx-mono);
  font-size: 10px;
  color: var(--gx-dim);
  min-width: 26px;
}
.icon {
  flex: none;
}
.text {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sub {
  display: flex;
  gap: 5px;
  margin: 2px 0 2px 32px;
}
/* A human turn starts a new exchange — rules above and below make that scannable. */
.human {
  background: var(--gx-human-bg);
  border-left-color: var(--gx-accent);
  border-top: 1px solid var(--gx-accent-dim);
  border-bottom: 1px solid var(--gx-accent-dim);
  margin: 5px 0;
}
.assistant {
  background: var(--gx-assistant-bg);
}
.system {
  color: var(--gx-dim);
  font-style: italic;
}
.thinking {
  font-size: 11px;
  color: var(--gx-think);
  padding: 2px 8px 2px 34px;
}
.call {
  display: flex;
  align-items: baseline;
  gap: 6px;
  padding: 2px 8px 2px 26px;
  cursor: pointer;
  border-left: 2px solid transparent;
}
.call.err {
  border-left-color: var(--gx-error);
}
.bracket {
  font-family: var(--gx-mono);
  color: var(--gx-dim);
}
.tool {
  font-family: var(--gx-mono);
  color: var(--gx-ok);
  font-weight: 600;
}
.preview {
  color: var(--gx-dim);
  font-family: var(--gx-mono);
  font-size: 11px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  flex: 1;
}
.mark {
  color: var(--gx-ok);
}
.mark.err {
  color: var(--gx-error);
}
.mark.pend {
  color: var(--gx-warn);
}
</style>
