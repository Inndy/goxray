<script setup>
import { computed } from 'vue'
import { NAlert, NButton, NCollapse, NCollapseItem, NTag } from 'naive-ui'
import { rawURL } from '@/api.js'
import { fmtTokens, prettyJSON } from '@/lib/format.js'

const props = defineProps({
  detail: { type: Object, required: true },
})

const ex = computed(() => props.detail.exchange || {})
const res = computed(() => props.detail.response)

const headerRows = (h) => Object.entries(h || {}).sort(([a], [b]) => a.localeCompare(b))

// Events are shown as a list rather than one blob so a stream can be read frame
// by frame, which is the only way to see where a truncated one stopped.
const events = computed(() => res.value?.events || [])
</script>

<template>
  <div class="wrap">
    <section>
      <h4>Exchange</h4>
      <div class="kv">
        <div>
          <span class="k">id</span><span class="v mono">{{ ex.id }}</span>
        </div>
        <div>
          <span class="k">state</span><span class="v mono">{{ ex.state }}</span>
        </div>
        <div>
          <span class="k">capture ver</span><span class="v mono">{{ ex.capture_ver }}</span>
        </div>
        <div>
          <span class="k">method / url</span
          ><span class="v mono">{{ ex.method }} {{ ex.url }}</span>
        </div>
        <div>
          <span class="k">status</span><span class="v mono">{{ ex.status }}</span>
        </div>
        <div v-if="ex.source">
          <span class="k">source</span><span class="v mono">{{ ex.source }}</span>
        </div>
        <div>
          <span class="k">started</span><span class="v mono">{{ ex.started_at }}</span>
        </div>
        <div v-if="ex.ended_at">
          <span class="k">ended</span><span class="v mono">{{ ex.ended_at }}</span>
        </div>
        <div>
          <span class="k">parsed</span>
          <span class="v">
            <NTag size="tiny" :type="ex.parse_ok ? 'success' : 'warning'" :bordered="false">
              {{ ex.parse_ok ? 'yes' : 'no' }}
            </NTag>
          </span>
        </div>
      </div>
    </section>

    <section>
      <h4>Estimated context</h4>
      <div class="kv">
        <div>
          <span class="k">system</span
          ><span class="v mono">{{ fmtTokens(detail.tokens.system) }}</span>
        </div>
        <div>
          <span class="k">tools</span
          ><span class="v mono">{{ fmtTokens(detail.tokens.tools) }}</span>
        </div>
        <div>
          <span class="k">messages</span
          ><span class="v mono">{{ fmtTokens(detail.tokens.messages) }}</span>
        </div>
        <div>
          <span class="k">total</span
          ><span class="v mono">{{ fmtTokens(detail.tokens.total) }}</span>
        </div>
        <div>
          <span class="k">window</span
          ><span class="v mono">{{ fmtTokens(detail.ctx_window) }}</span>
        </div>
      </div>
      <p class="note">
        Estimated at ~4 characters per token, which runs below the API's real count. The response
        <code>usage</code> above the timeline is the authoritative figure.
      </p>
    </section>

    <section>
      <h4>Raw bytes</h4>
      <div class="btns">
        <NButton size="tiny" tag="a" :href="rawURL(ex.id, 'req')" target="_blank">request</NButton>
        <NButton
          size="tiny"
          tag="a"
          :href="rawURL(ex.id, 'res')"
          target="_blank"
          :disabled="!ex.res_ref"
        >
          response
        </NButton>
        <span class="mono dim" v-if="res">{{ res.bytes }} bytes · {{ res.content_type }}</span>
      </div>
      <p class="note">Verbatim recorded bytes, exactly as they crossed the wire.</p>
    </section>

    <NCollapse arrow-placement="right">
      <NCollapseItem v-if="detail.options" name="options" title="Request options">
        <pre class="code">{{ prettyJSON(detail.options) }}</pre>
      </NCollapseItem>

      <NCollapseItem name="req-headers" title="Request headers">
        <div class="kv">
          <div v-for="[k, v] in headerRows(ex.req_headers)" :key="k">
            <span class="k mono">{{ k }}</span
            ><span class="v mono">{{ v }}</span>
          </div>
        </div>
      </NCollapseItem>

      <NCollapseItem name="res-headers" title="Response headers">
        <div class="kv">
          <div v-for="[k, v] in headerRows(ex.res_headers)" :key="k">
            <span class="k mono">{{ k }}</span
            ><span class="v mono">{{ v }}</span>
          </div>
        </div>
      </NCollapseItem>

      <NCollapseItem
        v-if="events.length"
        name="events"
        :title="`Response events (${events.length})`"
      >
        <div class="events">
          <div v-for="(e, i) in events" :key="i" class="ev">
            <span class="ev-n mono">{{ i }}</span>
            <span class="ev-t mono">{{ e.type }}</span>
            <pre class="ev-d">{{ e.data }}</pre>
          </div>
        </div>
      </NCollapseItem>

      <NCollapseItem v-if="res?.json" name="res-json" title="Response JSON">
        <pre class="code">{{ prettyJSON(res.json) }}</pre>
      </NCollapseItem>

      <NCollapseItem v-if="res?.text" name="res-text" title="Response body">
        <pre class="code">{{ res.text }}</pre>
      </NCollapseItem>
    </NCollapse>

    <NAlert v-if="!res" type="warning" :bordered="false" class="note-alert">
      No response body available for this exchange.
    </NAlert>
  </div>
</template>

<style scoped>
.wrap {
  padding: 8px 10px;
  font-size: 12px;
}
section {
  margin-bottom: 12px;
}
h4 {
  margin: 0 0 5px;
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--gx-dim);
  font-weight: 600;
}
.kv > div {
  display: flex;
  gap: 8px;
  padding: 1px 0;
  align-items: baseline;
}
.k {
  min-width: 110px;
  color: var(--gx-dim);
  font-size: 11px;
  flex: none;
}
.v {
  word-break: break-all;
  font-size: 11px;
}
.mono {
  font-family: var(--gx-mono);
}
.dim {
  color: var(--gx-dim);
}
.btns {
  display: flex;
  align-items: center;
  gap: 8px;
}
.note {
  margin: 5px 0 0;
  font-size: 10px;
  color: var(--gx-dim);
  line-height: 1.5;
}
.note-alert {
  font-size: 11px;
  margin-top: 8px;
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
  max-height: 420px;
  overflow-y: auto;
  white-space: pre-wrap;
  word-break: break-word;
}
.events {
  max-height: 420px;
  overflow-y: auto;
}
.ev {
  display: flex;
  gap: 6px;
  align-items: baseline;
  border-bottom: 1px solid var(--gx-border);
  padding: 2px 0;
}
.ev-n {
  color: var(--gx-dim);
  font-size: 10px;
  min-width: 26px;
}
.ev-t {
  color: var(--gx-accent);
  font-size: 10px;
  min-width: 148px;
  flex: none;
}
.ev-d {
  margin: 0;
  font-family: var(--gx-mono);
  font-size: 10px;
  white-space: pre-wrap;
  word-break: break-all;
  flex: 1;
  color: var(--gx-dim);
}
</style>
