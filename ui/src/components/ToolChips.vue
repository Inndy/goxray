<script setup>
import { computed } from 'vue'
import { NCollapse, NCollapseItem, NEmpty, NTooltip } from 'naive-ui'
import { fmtTokens } from '@/lib/format.js'

const props = defineProps({
  tools: { type: Object, default: () => ({ core: [], mcp: [], total_tokens: 0 }) },
  // Context the tool definitions compete with, for the "how much of my window is
  // just tool schemas" question.
  totalTokens: { type: Number, default: 0 },
})

const core = computed(() => props.tools?.core || [])
const mcp = computed(() => props.tools?.mcp || [])
const any = computed(() => core.value.length + mcp.value.length > 0)

const calledCount = (list) => list.filter((t) => t.calls > 0).length
const sumTokens = (list) => list.reduce((n, t) => n + t.tokens, 0)

const share = computed(() => {
  if (!props.totalTokens || !props.tools?.total_tokens) return null
  return Math.round((props.tools.total_tokens / props.totalTokens) * 100)
})

const byPlugin = computed(() => {
  const m = new Map()
  for (const t of mcp.value) {
    const key = t.plugin || '(unknown)'
    if (!m.has(key)) m.set(key, { plugin: key, tools: [], tokens: 0 })
    const g = m.get(key)
    g.tools.push(t)
    g.tokens += t.tokens
  }
  return [...m.values()].sort((a, b) => b.tokens - a.tokens)
})
</script>

<template>
  <div class="wrap">
    <NEmpty v-if="!any" size="small" description="No tools in this request" class="empty" />

    <template v-else>
      <div class="summary">
        <span class="mono">
          {{ core.length + mcp.length }} tools · {{ fmtTokens(tools.total_tokens) }} tok
        </span>
        <NTooltip v-if="share !== null" trigger="hover">
          <template #trigger
            ><span class="mono share">{{ share }}% of context</span></template
          >
          Tool definitions are re-sent every turn; this is their share of the estimated total
        </NTooltip>
        <span class="spacer" />
        <span class="mono dim"> {{ calledCount(core) + calledCount(mcp) }} used this turn </span>
      </div>

      <section v-if="core.length">
        <h4>
          Core <span class="dim">{{ core.length }} · {{ fmtTokens(sumTokens(core)) }} tok</span>
        </h4>
        <div class="chips">
          <NTooltip v-for="t in core" :key="t.name" trigger="hover" :delay="200">
            <template #trigger>
              <span class="chip" :class="{ used: t.calls > 0 }">
                {{ t.name }}
                <span v-if="t.calls" class="n">{{ t.calls }}×</span>
              </span>
            </template>
            <div class="tip">
              <div class="mono">{{ t.name }} · {{ fmtTokens(t.tokens) }} tok</div>
              <div v-if="t.description">{{ t.description }}</div>
            </div>
          </NTooltip>
        </div>
      </section>

      <section v-if="mcp.length">
        <h4>
          MCP <span class="dim">{{ mcp.length }} · {{ fmtTokens(sumTokens(mcp)) }} tok</span>
        </h4>
        <NCollapse arrow-placement="right">
          <NCollapseItem v-for="g in byPlugin" :key="g.plugin" :name="g.plugin">
            <template #header>
              <span class="mono">
                {{ g.plugin }}
                <span class="dim">{{ g.tools.length }} · {{ fmtTokens(g.tokens) }} tok</span>
              </span>
            </template>
            <div class="chips">
              <span
                v-for="t in g.tools"
                :key="t.name"
                class="chip"
                :class="{ used: t.calls > 0 }"
                :title="t.description"
              >
                {{ t.name.replace(`mcp__${g.plugin}__`, '') }}
                <span v-if="t.calls" class="n">{{ t.calls }}×</span>
              </span>
            </div>
          </NCollapseItem>
        </NCollapse>
      </section>
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
  border-bottom: 1px solid var(--gx-border);
  font-size: 11px;
}
.spacer {
  flex: 1;
}
.share {
  color: var(--gx-warn);
  cursor: help;
}
h4 {
  margin: 12px 0 5px;
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--gx-fg);
  font-weight: 600;
}
h4 .dim,
.dim {
  color: var(--gx-dim);
  text-transform: none;
  letter-spacing: 0;
  font-weight: 400;
}
.mono {
  font-family: var(--gx-mono);
}
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.chip {
  font-family: var(--gx-mono);
  font-size: 10px;
  padding: 2px 6px;
  border-radius: 3px;
  background: var(--gx-code-bg);
  color: var(--gx-dim);
  border: 1px solid transparent;
  white-space: nowrap;
}
/* Tools actually invoked this turn earn emphasis; the rest only cost context. */
.chip.used {
  color: var(--gx-ok);
  border-color: var(--gx-ok-dim);
  background: var(--gx-ok-bg);
}
.n {
  font-weight: 700;
  margin-left: 3px;
}
.tip {
  font-size: 11px;
  max-width: 380px;
}
.empty {
  padding: 32px 8px;
}
</style>
