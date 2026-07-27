// Recorded content is attacker-influenced: it includes web fetch results, file
// contents, and command output. Vue escapes interpolated text automatically, so
// nothing here needs to build HTML — but two things still need explicit care.

const SAFE_IMAGE_MIMES = /^image\/(png|jpeg|gif|webp)$/
const BASE64_ONLY = /^[A-Za-z0-9+/\n\r]+=*$/

/**
 * imageDataURL validates a recorded image block before it becomes a src.
 * Returns null when anything is off, so the caller renders a placeholder rather
 * than handing an unvalidated string to the browser as a URL.
 */
export function imageDataURL(block) {
  const src = block?.source
  if (!src || src.type !== 'base64') return null
  if (typeof src.media_type !== 'string' || !SAFE_IMAGE_MIMES.test(src.media_type)) return null
  if (typeof src.data !== 'string' || !src.data || !BASE64_ONLY.test(src.data)) return null
  return `data:${src.media_type};base64,${src.data}`
}

const CRED_PATTERNS = [
  /sk-ant-[a-zA-Z0-9_-]{20,}/g,
  /sk-[a-zA-Z0-9]{20,}/g,
  /ghp_[a-zA-Z0-9]{36}/g,
  /gho_[a-zA-Z0-9]{36}/g,
  /AKIA[0-9A-Z]{16}/g,
  /-----BEGIN (?:RSA|EC|OPENSSH|PRIVATE) [A-Z ]*KEY-----/g,
]

/**
 * credSegments splits text into alternating plain and credential-looking spans.
 *
 * Returned as data for v-for to render, never as an HTML string — highlighting
 * by string concatenation is exactly how an escaping bug gets introduced into
 * the one place that handles untrusted bytes.
 */
export function credSegments(text) {
  const s = String(text ?? '')
  if (!s) return []

  const ranges = []
  for (const re of CRED_PATTERNS) {
    re.lastIndex = 0
    let m
    while ((m = re.exec(s)) !== null) {
      ranges.push([m.index, m.index + m[0].length])
      if (m[0].length === 0) re.lastIndex++ // guard against a zero-width match
    }
  }
  if (!ranges.length) return [{ text: s, cred: false }]

  ranges.sort((a, b) => a[0] - b[0])
  const merged = [ranges[0]]
  for (const [start, end] of ranges.slice(1)) {
    const last = merged[merged.length - 1]
    if (start <= last[1]) last[1] = Math.max(last[1], end)
    else merged.push([start, end])
  }

  const out = []
  let pos = 0
  for (const [start, end] of merged) {
    if (start > pos) out.push({ text: s.slice(pos, start), cred: false })
    out.push({ text: s.slice(start, end), cred: true })
    pos = end
  }
  if (pos < s.length) out.push({ text: s.slice(pos), cred: false })
  return out
}

export const hasCredential = (text) => credSegments(text).some((seg) => seg.cred)
