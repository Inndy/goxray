import { toolPreview } from './toolPreview.js'

// Text the harness injected into a user turn rather than something a human
// typed. Two shapes exist and both matter: tag-wrapped context blocks, and bare
// notification text with no wrapper at all.
const INJECTED_TAG_RE =
  /^<(system-reminder|user-prompt-submit-hook|context|local-command-caveat|antml:function_calls)[^>]*>/

// Background-task results arrive as ordinary user messages carrying no tag. They
// run to tens of thousands of characters, so treating them as human input would
// bury the actual prompt under machine output.
const INJECTED_PREFIX_RE = /^\[SYSTEM NOTIFICATION - NOT USER INPUT\]/

export function isInjected(text) {
  const t = (text || '').trimStart()
  return INJECTED_TAG_RE.test(t) || INJECTED_PREFIX_RE.test(t)
}

// Content may be a bare string or an array of blocks; normalize to blocks.
function blocksOf(content) {
  if (typeof content === 'string') return [{ type: 'text', text: content }]
  return Array.isArray(content) ? content : []
}

const textOf = (b) => b.text || b.thinking || ''

function joinText(blocks) {
  return blocks
    .filter((b) => b.type === 'text')
    .map(textOf)
    .join('')
}

/**
 * mergeSteps flattens a raw Anthropic messages array into the logical steps a
 * reader actually cares about:
 *
 *   - every tool_use joined to its tool_result, wherever that result landed
 *   - parallel tool calls from one assistant message grouped together
 *   - thinking attached to the step it belongs to
 *   - injected context separated from text a human actually typed
 *   - role:"system" messages surfaced rather than dropped; current Claude Code
 *     puts substantial context there and it is easy to miss otherwise
 *
 * `response` is this turn's own reply, reassembled server-side. It is appended
 * because a request only ever contains the conversation up to the point the
 * model was asked to speak — the reply itself would otherwise appear nowhere
 * until the next turn quotes it back, and the newest turn never gets one.
 *
 * Returns a flat array so the list stays trivially virtualizable and each step
 * can carry its own message indices back to the raw view.
 */
export function mergeSteps(messages, response) {
  const list = Array.isArray(messages) ? messages : []

  // A tool_result can appear in any later user message, so index them all first.
  const resultMap = new Map()
  const knownToolUseIds = new Set()
  for (const m of list) {
    for (const b of blocksOf(m.content)) {
      if (b.type === 'tool_result' && b.tool_use_id) resultMap.set(b.tool_use_id, b)
      if (b.type === 'tool_use' && b.id) knownToolUseIds.add(b.id)
    }
  }

  const steps = []
  list.forEach((m, i) => {
    const blocks = blocksOf(m.content)
    if (m.role === 'system') {
      pushSystem(steps, blocks, i)
    } else if (m.role === 'user') {
      pushUser(steps, blocks, i, knownToolUseIds)
    } else if (m.role === 'assistant') {
      pushAssistant(steps, blocks, i, resultMap)
    } else {
      steps.push({ type: 'other', role: m.role, text: joinText(blocks), msgIndices: [i] })
    }
  })
  pushResponse(steps, response)
  return steps
}

// RESPONSE_MSG_INDEX marks a step that came from the response rather than from
// messages[], so nothing tries to index back into the request array with it.
const RESPONSE_MSG_INDEX = -1

function pushResponse(steps, response) {
  const blocks = blocksOf(response?.content)
  if (!blocks.length) return

  const at = steps.length
  // No result map: a call made in this turn is answered in the *next* request,
  // so its result cannot exist here by definition.
  pushAssistant(steps, blocks, RESPONSE_MSG_INDEX, new Map())

  for (const s of steps.slice(at)) {
    s.fromResponse = true
    s.stopReason = response.stop_reason || ''
    s.truncated = Boolean(response.truncated)
    for (const c of s.calls || []) c.fromResponse = true
  }
}

function pushSystem(steps, blocks, i) {
  const text = joinText(blocks)
  if (!text.trim()) return
  steps.push({ type: 'system', text, injected: true, msgIndices: [i] })
}

function pushUser(steps, blocks, i, knownToolUseIds) {
  const results = blocks.filter((b) => b.type === 'tool_result')
  // A result with no matching tool_use lost its call — normally to a context
  // compaction that dropped the assistant message. Surface it instead of
  // silently discarding it.
  const orphans = results.filter((b) => b.tool_use_id && !knownToolUseIds.has(b.tool_use_id))

  const texts = blocks.filter((b) => b.type === 'text' || typeof b.text === 'string')
  const human = texts.filter((b) => !isInjected(textOf(b)))
  const injected = texts.filter((b) => isInjected(textOf(b)))
  const humanText = human.map(textOf).join('')
  const images = blocks.filter((b) => b.type === 'image')

  const worthShowing =
    humanText.trim() ||
    orphans.length ||
    images.length ||
    // An injected-only message with no tool_result is standalone context, not a
    // wrapper around a result, so it deserves its own row.
    (injected.length && !results.length)

  if (!worthShowing) return

  steps.push({
    type: 'human',
    humanText,
    injectedText: injected.map(textOf).join('\n\n'),
    hasInjected: injected.length > 0,
    hasToolResult: results.length > 0,
    images,
    orphans,
    msgIndices: [i],
  })
}

function pushAssistant(steps, blocks, i, resultMap) {
  const thinkingBlocks = blocks.filter(
    (b) => b.type === 'thinking' || b.type === 'redacted_thinking',
  )
  const thinking = thinkingBlocks.map((b) => b.thinking || b.data || '').join('\n\n')
  const text = joinText(blocks)
  const uses = blocks.filter((b) => b.type === 'tool_use')

  // Thinking rides along with the tool group when there is one, so a single row
  // reads as "thought, then called these"; otherwise it attaches to the text.
  if (text.trim() || (thinking && !uses.length)) {
    steps.push({
      type: 'assistant-text',
      text,
      thinking: uses.length ? '' : thinking,
      msgIndices: [i],
    })
  }

  if (!uses.length) return
  steps.push({
    type: 'tool-group',
    thinking,
    calls: uses.map((u) => {
      const result = resultMap.get(u.id)
      return {
        name: u.name || '(unnamed)',
        preview: toolPreview(u.name, u.input),
        input: u.input,
        // Set when the argument JSON was cut mid-stream and never parsed.
        partialInput: u.partial_input || '',
        toolUseId: u.id,
        result: result ? result.content : null,
        isError: Boolean(result?.is_error),
        // No result in the transcript means the call was still outstanding when
        // this request was assembled.
        pending: !result,
      }
    }),
    msgIndices: [i],
  })
}

// resultText renders a tool_result's content, which may be a plain string or an
// array of blocks.
export function resultText(content) {
  if (content == null) return ''
  if (typeof content === 'string') return content
  if (!Array.isArray(content)) return JSON.stringify(content, null, 2)
  return content
    .map((b) => {
      if (b.type === 'text') return b.text || ''
      if (b.type === 'image') return '[image]'
      return JSON.stringify(b)
    })
    .join('\n')
}

// resultImages pulls out image blocks so they can be rendered through the
// data-URL allowlist rather than concatenated into text.
export function resultImages(content) {
  if (!Array.isArray(content)) return []
  return content.filter((b) => b.type === 'image')
}
