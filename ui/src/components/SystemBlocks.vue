<script setup>
import { computed } from 'vue'
import { NCollapse, NCollapseItem, NEmpty, NTag, NTooltip } from 'naive-ui'
import { fmtChars, fmtTokens } from '@/lib/format.js'
import SafeText from './SafeText.vue'

const props = defineProps({
  blocks: { type: Array, default: () => [] },
  ccVersion: { type: String, default: '' },
})

const totals = computed(() => {
  const t = { chars: 0, tokens: 0, user: 0, anthropic: 0 }
  for (const b of props.blocks) {
    t.chars += b.chars
    t.tokens += b.tokens
    t[b.owner === 'user' ? 'user' : 'anthropic'] += b.tokens
  }
  return t
})

// Core instructions is the largest block and rarely what someone opened this
// tab to read, so it starts closed while everything else starts open.
const defaultOpen = computed(() =>
  props.blocks.filter((b) => b.key !== 'core-instructions').map((b) => b.key),
)
</script>

<template>
  <div class="wrap">
    <NEmpty
      v-if="!blocks.length"
      size="small"
      description="No system prompt recorded"
      class="empty"
    />

    <template v-else>
      <div class="summary">
        <span class="mono"
          >{{ fmtChars(totals.chars) }} chars · {{ fmtTokens(totals.tokens) }} tok</span
        >
        <span v-if="ccVersion" class="mono dim">cc {{ ccVersion }}</span>
        <span class="spacer" />
        <NTooltip trigger="hover">
          <template #trigger>
            <span class="split">
              <span class="ow-anthropic">{{ fmtTokens(totals.anthropic) }}</span>
              <span class="dim">/</span>
              <span class="ow-user">{{ fmtTokens(totals.user) }}</span>
            </span>
          </template>
          Shipped by Claude Code vs contributed by your machine and config
        </NTooltip>
      </div>

      <NCollapse :default-expanded-names="defaultOpen" arrow-placement="right">
        <NCollapseItem v-for="b in blocks" :key="b.key" :name="b.key">
          <template #header>
            <div class="hdr">
              <span class="label">{{ b.label }}</span>
              <NTag size="tiny" :bordered="false" :type="b.owner === 'user' ? 'success' : 'info'">
                {{ b.owner }}
              </NTag>
              <span class="mono dim">{{ fmtChars(b.chars) }}c · {{ fmtTokens(b.tokens) }}tok</span>
            </div>
          </template>
          <pre class="body"><SafeText :text="b.text" /></pre>
        </NCollapseItem>
      </NCollapse>
    </template>
  </div>
</template>

<style scoped>
.wrap {
  padding: 8px 10px;
  font-size: 12px;
}
.summary {
  display: flex;
  align-items: center;
  gap: 10px;
  padding-bottom: 8px;
  margin-bottom: 4px;
  border-bottom: 1px solid var(--gx-border);
  font-size: 11px;
}
.spacer {
  flex: 1;
}
.mono {
  font-family: var(--gx-mono);
}
.dim {
  color: var(--gx-dim);
}
.split {
  font-family: var(--gx-mono);
  cursor: help;
}
.ow-anthropic {
  color: var(--gx-accent);
}
.ow-user {
  color: var(--gx-ok);
}
.hdr {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.label {
  font-weight: 600;
}
.body {
  margin: 0;
  padding: 6px 8px;
  background: var(--gx-code-bg);
  border-radius: 3px;
  font-family: var(--gx-mono);
  font-size: 11px;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 420px;
  overflow-y: auto;
}
.empty {
  padding: 32px 8px;
}
</style>
