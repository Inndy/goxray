<script setup>
import { onMounted, onUnmounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NAlert, NSplit, NSwitch, NTag } from 'naive-ui'
import SessionList from '@/components/SessionList.vue'
import TurnList from '@/components/TurnList.vue'
import TurnDetail from '@/components/TurnDetail.vue'
import {
  ctxWindowOf,
  gaps,
  init,
  selectExchange,
  selectSession,
  sessionOfExchange,
  setFollow,
  setShowNoise,
  startLive,
  store,
  turnNumbers,
  visibleExchanges,
} from '@/store.js'

const route = useRoute()
const router = useRouter()
let stopLive = null

// The route needs a session segment to carry a turn id, so "-" stands in for
// "no session filter". It must be decoded back to empty or it would be applied
// as a literal session id and match nothing.
const NO_SESSION = '-'

async function applyRoute() {
  const raw = route.params.session || ''
  const sid = raw === NO_SESSION ? '' : raw
  const id = route.params.id || ''
  // Both comparisons read the pre-change state, so they must happen before
  // either call mutates it. The two fetches then overlap rather than serializing
  // the detail behind the list — opening a turn from the unfiltered list now
  // reloads both, and awaiting them in sequence showed a spinner for the sum.
  const jobs = []
  if (id !== store.selectedId) jobs.push(selectExchange(id))
  if (sid !== store.selectedSession) jobs.push(selectSession(sid))
  await Promise.all(jobs)
}

function pushSelection(sid, id) {
  if (!sid && !id) {
    router.push({ name: 'dashboard' })
    return
  }
  router.push({ name: 'turn', params: { session: sid || NO_SESSION, id: id || undefined } })
}

onMounted(async () => {
  await init()
  await applyRoute()
  stopLive = startLive()
})

onUnmounted(() => stopLive?.())

watch(() => [route.params.session, route.params.id], applyRoute)

const onSession = (sid) => pushSelection(sid, '')

// Opening a turn adopts its session, rather than carrying whichever session was
// already selected. Passing the old one meant a turn reached from the unfiltered
// list left the list unfiltered, so every new session's turns kept sliding into
// what read as one session's history. A sessionless turn resolves to '' and
// keeps the list global, which is the only honest scope for it.
const onTurn = (id) => pushSelection(sessionOfExchange(id), id)
</script>

<template>
  <div class="app">
    <header class="topbar">
      <span class="brand">goxray</span>
      <span class="dim small">recording proxy dashboard</span>
      <span class="spacer" />
      <label class="row small">
        <NSwitch size="small" :value="store.follow" @update:value="setFollow" />
        <span class="dim">follow live</span>
      </label>
      <NTag
        size="tiny"
        :bordered="false"
        :type="store.liveStatus === 'live' ? 'success' : 'warning'"
      >
        {{ store.liveStatus }}
      </NTag>
    </header>

    <NAlert v-if="store.error" type="error" :bordered="false" class="err">{{ store.error }}</NAlert>

    <div class="body">
      <NSplit direction="horizontal" :default-size="0.17" :min="0.1" :max="0.4">
        <template #1>
          <SessionList
            :sessions="store.sessions"
            :selected="store.selectedSession"
            :show-noise="store.showNoise"
            @select="onSession"
            @update:show-noise="setShowNoise"
          />
        </template>
        <template #2>
          <NSplit direction="horizontal" :default-size="0.32" :min="0.15" :max="0.6">
            <template #1>
              <TurnList
                :exchanges="visibleExchanges"
                :selected="store.selectedId"
                :turn-numbers="turnNumbers"
                :gaps="gaps"
                :loading="store.loadingList"
                :ctx-window-of="ctxWindowOf"
                @select="onTurn"
              />
            </template>
            <template #2>
              <TurnDetail :detail="store.detail" :loading="store.loadingDetail" />
            </template>
          </NSplit>
        </template>
      </NSplit>
    </div>
  </div>
</template>

<style scoped>
.app {
  display: flex;
  flex-direction: column;
  height: 100vh;
  overflow: hidden;
}
.topbar {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 12px;
  border-bottom: 1px solid var(--gx-border);
  flex: none;
}
.brand {
  font-weight: 700;
  font-family: var(--gx-mono);
  color: var(--gx-accent);
}
.body {
  flex: 1;
  min-height: 0;
}
.body :deep(.n-split-pane) {
  min-width: 0;
  overflow: hidden;
}
.row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.small {
  font-size: 11px;
}
.dim {
  color: var(--gx-dim);
}
.spacer {
  flex: 1;
}
.err {
  border-radius: 0;
  font-size: 11px;
  flex: none;
}
</style>
