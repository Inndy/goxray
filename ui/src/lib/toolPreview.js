const truncate = (s, n) => {
  const str = String(s ?? '')
  return str.length > n ? `${str.slice(0, n)}…` : str
}

const firstLine = (s) => String(s ?? '').split('\n')[0]

/**
 * toolPreview produces the one-line summary shown beside a tool name in the
 * timeline. It is per-tool on purpose: the useful identifier differs by tool
 * (a command, a filename, a pattern, a URL), and a generic JSON dump of the
 * input is unreadable at list density.
 */
export function toolPreview(name, input) {
  const inp = input && typeof input === 'object' ? input : {}
  switch (name) {
    case 'Bash':
      return truncate(firstLine(inp.command || inp.cmd), 72)
    case 'Read':
    case 'Write':
    case 'Edit':
    case 'NotebookEdit':
      return basename(inp.file_path || inp.notebook_path)
    case 'Grep':
      return truncate(inp.pattern, 48)
    case 'Glob':
      return truncate(inp.pattern, 48)
    case 'Agent':
      return truncate(inp.description || inp.prompt, 56)
    case 'Skill':
      return String(inp.skill ?? '')
    case 'WebSearch':
      return truncate(inp.query, 56)
    case 'WebFetch':
      return truncate(String(inp.url ?? '').replace(/^https?:\/\//, ''), 56)
    case 'TaskCreate':
    case 'TaskUpdate':
      return truncate(inp.subject || inp.taskId, 48)
    case 'TaskGet':
      return String(inp.taskId ?? '')
    case 'SendMessage':
      return truncate(inp.summary || inp.to, 48)
    case 'Monitor':
      return truncate(inp.description, 48)
    case 'AskUserQuestion':
      return truncate(inp.questions?.[0]?.question, 56)
    // Codex's multi-agent tools. The task text itself is encrypted by the API,
    // so the name of the agent it goes to is the only readable identifier.
    case 'spawn_agent':
      return String(inp.task_name ?? '')
    case 'send_message':
    case 'followup_task':
    case 'close_agent':
      return String(inp.target ?? '')
    case 'wait_agent':
      return [].concat(inp.targets ?? inp.target ?? []).join(', ')
    default: {
      const first = Object.keys(inp)[0]
      if (!first) return ''
      const v = inp[first]
      // String() on an object yields "[object Object]", which tells the reader
      // nothing; a JSON fragment at least shows the shape.
      return truncate(v !== null && typeof v === 'object' ? JSON.stringify(v) : v, 48)
    }
  }
}

function basename(p) {
  if (!p) return ''
  const parts = String(p).split('/')
  return parts[parts.length - 1] || String(p)
}

// Anything past this is a distinct tool family worth its own visual grouping.
export const isMCP = (name) => String(name || '').startsWith('mcp__')
