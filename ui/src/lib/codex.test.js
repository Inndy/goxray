import test from 'node:test'
import assert from 'node:assert/strict'
import { isInjected, mergeSteps } from './mergeSteps.js'
import { toolPreview } from './toolPreview.js'

test('Codex harness parts stay separate from human input', () => {
  const steps = mergeSteps([
    {
      role: 'user',
      content: [
        { type: 'text', text: '<environment_context>cwd</environment_context>' },
        { type: 'text', text: 'Review the parser' },
        {
          type: 'text',
          text: '<codex_internal_context source="goal">goal</codex_internal_context>',
        },
      ],
    },
  ])
  assert.equal(steps.length, 1)
  assert.equal(steps[0].humanText, 'Review the parser')
  assert.equal(steps[0].hasInjected, true)
  assert.match(steps[0].injectedText, /environment_context/)
  assert.match(steps[0].injectedText, /codex_internal_context/)
  assert.equal(isInjected('<contextual>human text</contextual>'), false)
  assert.equal(isInjected('Explain <environment_context>'), false)
  assert.equal(isInjected('<system-reminder>notice</system-reminder>'), true)
})

test('Codex tool previews show code, commands and session identity', () => {
  assert.equal(
    toolPreview('exec', { input: 'text(await tools.exec_command({cmd: "pwd"}));\n// more' }),
    'text(await tools.exec_command({cmd: "pwd"}));',
  )
  assert.equal(toolPreview('exec_command', { cmd: 'git status\ngit diff' }), 'git status')
  assert.equal(toolPreview('write_stdin', { session_id: 42 }), 'session 42 (poll)')
  assert.equal(toolPreview('write_stdin', { session_id: 42, chars: 'yes' }), 'session 42: yes')
  assert.equal(toolPreview('wait', { cell_id: 'abc' }), 'abc')
  assert.equal(toolPreview('interrupt_agent', { target: '/root/reviewer' }), '/root/reviewer')
  assert.equal(toolPreview('Read', { file_path: 'C:\\project\\main.go' }), 'main.go')
})
