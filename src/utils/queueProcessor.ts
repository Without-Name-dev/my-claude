import type { QueuedCommand } from '../types/textInputTypes.js'
import {
  dequeue,
  getCommandQueue,
  hasCommandsInQueue,
  peek,
  remove,
} from './messageQueueManager.js'

type ProcessQueueParams = {
  executeInput: (commands: QueuedCommand[]) => Promise<void>
}

type ProcessQueueResult = {
  processed: boolean
  /** Rejects if the executed turn throws; the commands are already dequeued. */
  execution?: Promise<void>
}

/**
 * Check if a queued command is a slash command (value starts with '/').
 */
function isSlashCommand(cmd: QueuedCommand): boolean {
  if (typeof cmd.value === 'string') {
    return (
      cmd.value.trim().startsWith('/') &&
      (!cmd.skipSlashCommands || cmd.bridgeOrigin === true)
    )
  }
  // A bridge client can hand us content that is neither a string nor an array
  // (a stray object). Iterating it threw inside processQueueIfReady *before*
  // dequeueing, permanently wedging the drain; treat it as plain text.
  if (!Array.isArray(cmd.value)) return false
  // For ContentBlockParam[], check the first text block
  for (const block of cmd.value) {
    if (block.type === 'text') {
      return (
        block.text.trim().startsWith('/') &&
        (!cmd.skipSlashCommands || cmd.bridgeOrigin === true)
      )
    }
  }
  return false
}

/**
 * Processes commands from the queue.
 *
 * Slash commands (starting with '/') and bash-mode commands are processed
 * one at a time so each goes through the executeInput path individually.
 * Bash commands need individual processing to preserve per-command error
 * isolation, exit codes, and progress UI. Other non-slash commands are
 * batched: all items **with the same mode** as the highest-priority item
 * are drained at once and passed as a single array to executeInput — each
 * becomes its own user message with its own UUID. Different modes
 * (e.g. prompt vs task-notification) are never mixed because they are
 * treated differently downstream.
 *
 * The caller is responsible for ensuring no query is currently running
 * and for calling this function again after each command completes
 * until the queue is empty.
 *
 * @returns result with processed status
 */
export function processQueueIfReady({
  executeInput,
}: ProcessQueueParams): ProcessQueueResult {
  // This processor runs on the REPL main thread between turns. Skip anything
  // addressed to a subagent — an unfiltered peek() returning a subagent
  // notification would set targetMode, dequeueAllMatching would find nothing
  // matching that mode with agentId===undefined, and we'd return processed:
  // false with the queue unchanged → the React effect never re-fires and any
  // queued user prompt stalls permanently.
  const isMainThread = (cmd: QueuedCommand) => cmd.agentId === undefined

  const next = peek(isMainThread)
  if (!next) {
    return { processed: false }
  }

  // Slash commands and bash-mode commands are processed individually.
  // Bash commands need per-command error isolation, exit codes, and progress UI.
  if (isSlashCommand(next) || next.mode === 'bash') {
    const cmd = dequeue(isMainThread)!
    // Hand the promise back: silently discarding it turned a turn error into
    // an unhandled rejection with the prompt already dequeued and lost.
    return { processed: true, execution: executeInput([cmd]) }
  }

  // Drain a contiguous FIFO run starting at the peeked head, same mode only.
  // The old dequeueAllMatching reached across an intervening slash/bash
  // command, so a later prompt executed before input queued earlier.
  // Subagent-scoped and differently-moded entries stay in the queue.
  const targetMode = next.mode
  const queue = getCommandQueue()
  const headIndex = queue.indexOf(next)
  const commands: QueuedCommand[] = []
  for (let index = headIndex; index < queue.length; index += 1) {
    const cmd = queue[index]!
    if (!isMainThread(cmd)) continue
    if (isSlashCommand(cmd) || cmd.mode === 'bash') break
    if (cmd.mode !== targetMode) continue
    commands.push(cmd)
  }
  if (commands.length === 0) {
    return { processed: false }
  }

  remove(commands)
  return { processed: true, execution: executeInput(commands) }
}

/**
 * Checks if the queue has pending commands.
 * Use this to determine if queue processing should be triggered.
 */
export function hasQueuedCommands(): boolean {
  return hasCommandsInQueue()
}
