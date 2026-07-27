<script setup>
import { computed, ref, watch } from 'vue'
import { NAlert, NEmpty, NSpin, NSplit, NTabPane, NTabs, NTag } from 'naive-ui'
import StepDetail from './StepDetail.vue'
import StepList from './StepList.vue'
import SystemBlocks from './SystemBlocks.vue'
import ToolChips from './ToolChips.vue'
import RawViewer from './RawViewer.vue'
import { mergeSteps } from '@/lib/mergeSteps.js'
import {
  ctxUsed,
  elapsedMs,
  fmtCost,
  fmtDuration,
  fmtTokens,
  stateLabel,
  stateTone,
} from '@/lib/format.js'

const props = defineProps({
  detail: { type: Object, default: null },
  loading: { type: Boolean, default: false },
})

const tab = ref('timeline')
const selected = ref(null)

const ex = computed(() => props.detail?.exchange || null)
const steps = computed(() => mergeSteps(props.detail?.messages, props.detail?.response?.message))
const usage = computed(() => ex.value?.res?.usage || null)

const selectedStep = computed(() =>
  selected.value ? (steps.value[selected.value.stepIdx] ?? null) : null,
)

// Reset the step selection when a different turn loads, or the pane would show
// a step from the previous turn's timeline.
watch(
  () => ex.value?.id,
  () => {
    selected.value = null
  },
)

const errorCount = computed(() =>
  steps.value.reduce((n, s) => n + (s.calls?.filter((c) => c.isError).length || 0), 0),
)
</script>

<template>
  <div class="pane">
    <NEmpty v-if="!detail && !loading" description="Select a turn" class="empty" />
    <div v-else-if="loading" class="loading"><NSpin size="small" /></div>

    <template v-else>
      <header class="hd">
        <NTag size="tiny" :type="stateTone(ex.state)" :bordered="false">{{
          stateLabel(ex.state)
        }}</NTag>
        <span class="mono">{{ ex.req?.model || '—' }}</span>
        <span v-if="ex.status" class="mono dim">HTTP {{ ex.status }}</span>
        <span v-if="ex.res?.stop_reason" class="mono dim">{{ ex.res.stop_reason }}</span>
        <span v-if="fmtDuration(elapsedMs(ex))" class="mono dim">{{
          fmtDuration(elapsedMs(ex))
        }}</span>
        <span class="spacer" />
        <span v-if="fmtCost(ex.res?.cost)" class="mono cost">{{ fmtCost(ex.res.cost) }}</span>
      </header>

      <div class="usage mono">
        <span>in {{ fmtTokens(usage?.input_tokens) }}</span>
        <span>out {{ fmtTokens(usage?.output_tokens) }}</span>
        <span class="read">cache r {{ fmtTokens(usage?.cache_read_input_tokens) }}</span>
        <span class="write">cache w {{ fmtTokens(usage?.cache_creation_input_tokens) }}</span>
        <span class="spacer" />
        <span class="dim">
          ctx {{ fmtTokens(ctxUsed(usage)) }} / {{ fmtTokens(detail.ctx_window) }}
        </span>
      </div>

      <NAlert
        v-for="(w, i) in detail.warnings || []"
        :key="i"
        type="warning"
        :bordered="false"
        class="warn"
      >
        {{ w }}
      </NAlert>

      <NTabs v-model:value="tab" type="line" size="small" class="tabs" pane-class="pane-body">
        <NTabPane
          name="timeline"
          :tab="`Timeline (${steps.length}${errorCount ? ` · ${errorCount}✗` : ''})`"
        >
          <NSplit direction="horizontal" :default-size="0.44" :min="0.2" :max="0.75" class="split">
            <template #1>
              <div class="scroll">
                <StepList :steps="steps" :selected="selected" @select="selected = $event" />
              </div>
            </template>
            <template #2>
              <div class="scroll">
                <StepDetail :step="selectedStep" :call-idx="selected?.callIdx ?? null" />
              </div>
            </template>
          </NSplit>
        </NTabPane>

        <NTabPane name="system" :tab="`System (${fmtTokens(detail.tokens.system)})`">
          <div class="scroll">
            <SystemBlocks :blocks="detail.system" :cc-version="detail.cc_version" />
          </div>
        </NTabPane>

        <NTabPane
          name="tools"
          :tab="`Tools (${(detail.tools.core?.length || 0) + (detail.tools.mcp?.length || 0)})`"
        >
          <div class="scroll">
            <ToolChips :tools="detail.tools" :total-tokens="detail.tokens.total" />
          </div>
        </NTabPane>

        <NTabPane name="raw" tab="Raw">
          <div class="scroll">
            <RawViewer :detail="detail" />
          </div>
        </NTabPane>
      </NTabs>
    </template>
  </div>
</template>

<style scoped>
.pane {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-width: 0;
}
.hd,
.usage {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 10px;
  font-size: 11px;
}
.hd {
  border-bottom: 1px solid var(--gx-border);
}
.usage {
  border-bottom: 1px solid var(--gx-border);
  font-size: 10px;
  color: var(--gx-dim);
}
.usage .read {
  color: var(--gx-cache-read);
}
.usage .write {
  color: var(--gx-cache-write);
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
.cost {
  color: var(--gx-cost);
}
.warn {
  font-size: 11px;
  border-radius: 0;
}
.tabs {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.tabs :deep(.n-tabs-nav) {
  padding: 0 8px;
}
.tabs :deep(.n-tab-pane) {
  padding: 0;
  height: 100%;
  min-height: 0;
}
.tabs :deep(.n-tabs-pane-wrapper) {
  flex: 1;
  min-height: 0;
}
.split {
  height: 100%;
}
.scroll {
  height: 100%;
  overflow-y: auto;
  min-width: 0;
}
.loading,
.empty {
  padding: 40px;
  display: flex;
  justify-content: center;
}
</style>
