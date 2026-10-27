<script setup>
import { computed } from 'vue'
import { useRouter } from 'vue-router'
import { NAlert, NButton, NEmpty, NTag } from 'naive-ui'
import { prettyJSON } from '@/lib/format.js'
import { resultImages, resultText } from '@/lib/mergeSteps.js'
import { imageDataURL } from '@/lib/safe.js'
import { focusThread, store, threadOfAgent } from '@/store.js'
import SafeText from './SafeText.vue'

const props = defineProps({
  step: { type: Object, default: null },
  callIdx: { type: Number, default: null },
})

const call = computed(() => {
  if (!props.step || props.callIdx == null) return null
  return props.step.calls?.[props.callIdx] ?? null
})

const outText = computed(() => (call.value ? resultText(call.value.result) : ''))
const outImages = computed(() => (call.value ? resultImages(call.value.result) : []))
const lineCount = computed(() => (outText.value ? outText.value.split('\n').length : 0))

// Codex's multi-agent calls name another agent. Following one should land on
// that agent's own turns, which is the step a reader tracking a sub-agent takes
// next: from "spawned count_go" to what count_go then did.
const AGENT_CALLS = ['spawn_agent', 'send_message', 'followup_task', 'close_agent']
const router = useRouter()
const agentTarget = computed(() => {
  const c = call.value
  if (!c || !AGENT_CALLS.includes(c.name)) return null
  const name = c.input?.task_name || c.input?.target || ''
  // Relative names resolve against the agent that made the call, as in Codex.
  const from = store.detail?.exchange?.req?.agent
  return name ? { name, thread: threadOfAgent(name, from) } : null
})
async function openAgent() {
  const id = await focusThread(agentTarget.value.thread)
  if (id) router.push({ name: 'turn', params: { session: store.selectedSession || '-', id } })
}

const paragraphs = (t) =>
  String(t ?? '')
    .split(/\n{2,}/)
    .filter(Boolean)
</script>

<template>
  <div class="detail">
    <NEmpty v-if="!step" size="small" description="Select a step" class="empty" />

    <!-- one tool call -->
    <template v-else-if="call">
      <div class="hd">
        <span class="tool">{{ call.name }}</span>
        <NTag v-if="call.fromResponse" size="tiny" type="info" :bordered="false"
          >this turn's call</NTag
        >
        <NTag v-else-if="call.pending" size="tiny" type="warning" :bordered="false"
          >no result recorded</NTag
        >
        <NTag v-else-if="call.isError" size="tiny" type="error" :bordered="false">error</NTag>
        <NTag v-else size="tiny" type="success" :bordered="false">ok</NTag>
        <span class="id mono">{{ call.toolUseId }}</span>
      </div>
      <div v-if="call.preview" class="preview mono">{{ call.preview }}</div>
      <div v-if="agentTarget" class="jump">
        <NButton size="tiny" :disabled="!agentTarget.thread" @click="openAgent">
          open agent {{ agentTarget.name }} →
        </NButton>
        <span v-if="!agentTarget.thread" class="muted">
          not found in this session, or more than one agent has this name
        </span>
      </div>

      <h4>
        Input
        <span v-if="call.partialInput" class="muted">cut off mid-stream</span>
      </h4>
      <NAlert v-if="call.partialInput" type="warning" :bordered="false" class="note">
        The argument JSON never finished arriving, so this is the raw fragment rather than a parsed
        input.
      </NAlert>
      <pre v-if="call.partialInput" class="code wrap">{{ call.partialInput }}</pre>
      <pre v-else class="code">{{ prettyJSON(call.input) }}</pre>

      <h4>
        Output
        <span v-if="lineCount" class="muted">{{ lineCount }} lines</span>
      </h4>
      <NAlert v-if="call.fromResponse" type="info" :bordered="false" class="note">
        This call was made by the reply being viewed. Its result is sent back in the
        <em>next</em> request, so open the following turn to see what it returned.
      </NAlert>
      <NAlert v-else-if="call.pending" type="warning" :bordered="false" class="note">
        This call had no matching tool_result in the request — it was still outstanding when the
        request was assembled.
      </NAlert>
      <pre v-else class="code" :class="{ err: call.isError }"><SafeText :text="outText" /></pre>

      <div v-if="outImages.length" class="images">
        <template v-for="(img, i) in outImages" :key="i">
          <img v-if="imageDataURL(img)" :src="imageDataURL(img)" alt="tool output image" />
          <span v-else class="badimg">[invalid image data]</span>
        </template>
      </div>
    </template>

    <!-- thinking selected on a tool group -->
    <template v-else-if="step.type === 'tool-group'">
      <div class="hd"><span class="tool">🧠 Thinking</span></div>
      <p v-for="(p, i) in paragraphs(step.thinking)" :key="i" class="para">{{ p }}</p>
      <NEmpty v-if="!step.thinking" size="small" description="No thinking recorded" />
    </template>

    <template v-else-if="step.type === 'human'">
      <div class="hd"><span class="tool">👤 User turn</span></div>
      <h4 v-if="step.humanText.trim()">Typed</h4>
      <pre v-if="step.humanText.trim()" class="code wrap"><SafeText :text="step.humanText" /></pre>

      <div v-if="step.images.length" class="images">
        <template v-for="(img, i) in step.images" :key="i">
          <img v-if="imageDataURL(img)" :src="imageDataURL(img)" alt="pasted image" />
          <span v-else class="badimg">[invalid image data]</span>
        </template>
      </div>

      <template v-if="step.hasInjected">
        <h4>Injected context <span class="muted">not typed by the user</span></h4>
        <pre class="code wrap dim"><SafeText :text="step.injectedText" /></pre>
      </template>

      <template v-if="step.orphans.length">
        <h4>Orphaned tool results</h4>
        <NAlert type="warning" :bordered="false" class="note">
          These results have no matching tool_use in this request, which normally means a context
          compaction dropped the assistant message that made the call.
        </NAlert>
        <pre v-for="(o, i) in step.orphans" :key="i" class="code">{{ resultText(o.content) }}</pre>
      </template>
    </template>

    <template v-else-if="step.type === 'assistant-text'">
      <div class="hd">
        <span class="tool">🤖 Assistant</span>
        <NTag v-if="step.fromResponse" size="tiny" type="info" :bordered="false"
          >this turn's response</NTag
        >
        <NTag v-if="step.truncated" size="tiny" type="warning" :bordered="false">truncated</NTag>
      </div>
      <template v-if="step.thinking">
        <h4>Thinking</h4>
        <p v-for="(p, i) in paragraphs(step.thinking)" :key="i" class="para think">{{ p }}</p>
      </template>
      <h4>Text</h4>
      <pre class="code wrap"><SafeText :text="step.text" /></pre>
    </template>

    <template v-else-if="step.type === 'system'">
      <div class="hd">
        <span class="tool">⚙ System message</span>
        <NTag size="tiny" :bordered="false">injected by the harness</NTag>
      </div>
      <pre class="code wrap dim"><SafeText :text="step.text" /></pre>
    </template>

    <template v-else>
      <div class="hd">
        <span class="tool">{{ step.role }}</span>
      </div>
      <pre class="code wrap">{{ step.text }}</pre>
    </template>
  </div>
</template>

<style scoped>
.detail {
  padding: 8px 10px;
  font-size: 12px;
  min-width: 0;
}
.hd {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 4px;
}
.tool {
  font-family: var(--gx-mono);
  font-weight: 600;
  color: var(--gx-ok);
}
.id {
  color: var(--gx-dim);
  font-size: 10px;
  margin-left: auto;
}
.preview {
  color: var(--gx-dim);
  font-size: 11px;
  margin-bottom: 6px;
  word-break: break-all;
}
h4 {
  margin: 10px 0 3px;
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--gx-dim);
  font-weight: 600;
}
h4 .muted {
  text-transform: none;
  letter-spacing: 0;
  font-weight: 400;
  opacity: 0.8;
}
.code {
  margin: 0;
  padding: 6px 8px;
  background: var(--gx-code-bg);
  border-radius: 3px;
  font-family: var(--gx-mono);
  font-size: 11px;
  line-height: 1.5;
  overflow-x: auto;
  max-height: 460px;
  overflow-y: auto;
}
.code.wrap {
  white-space: pre-wrap;
  word-break: break-word;
}
.code.err {
  border-left: 2px solid var(--gx-error);
}
.code.dim {
  color: var(--gx-dim);
}
.para {
  margin: 0 0 8px;
  line-height: 1.55;
  white-space: pre-wrap;
}
.para.think {
  color: var(--gx-think);
}
.note {
  margin: 4px 0;
  font-size: 11px;
}
.images {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 8px;
}
.images img {
  max-width: 100%;
  max-height: 320px;
  border: 1px solid var(--gx-border);
  border-radius: 3px;
}
.badimg {
  color: var(--gx-error);
  font-family: var(--gx-mono);
  font-size: 11px;
}
.empty {
  padding: 32px 8px;
}
.muted {
  color: var(--gx-dim);
}
.jump {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 6px 0;
  font-size: 11px;
}
</style>
