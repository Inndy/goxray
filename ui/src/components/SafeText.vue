<script setup>
import { computed } from 'vue'
import { credSegments } from '@/lib/safe.js'

// Renders text with credential-shaped substrings highlighted.
//
// Every segment goes through Vue's text interpolation, so nothing here builds
// markup. That matters: this component exists specifically to render bytes from
// web fetches, file reads and command output, and string-concatenating
// highlights is the classic way an escaping bug gets introduced.
const props = defineProps({ text: { type: String, default: '' } })

const segments = computed(() => credSegments(props.text))
</script>

<template>
  <span
    ><template v-for="(seg, i) in segments" :key="i"
      ><span v-if="seg.cred" class="cred" title="looks like a credential">{{ seg.text }}</span
      ><template v-else>{{ seg.text }}</template></template
    ></span
  >
</template>

<style scoped>
.cred {
  background: var(--gx-cred-bg);
  color: var(--gx-cred-fg);
  border-radius: 2px;
  padding: 0 2px;
}
</style>
