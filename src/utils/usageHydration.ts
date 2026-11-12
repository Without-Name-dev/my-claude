import { statSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import type { ModelUsage } from 'src/entrypoints/agentSdkTypes.js'
// eslint-disable-next-line custom-rules/bootstrap-isolation
import { addToTotalCostState, getUsageForModel } from '../bootstrap/state.js'
import {
  MAX_TRANSCRIPT_READ_BYTES,
  getTranscriptPath,
} from './sessionStorage.js'

/**
 * Seed the process's token counters from the session transcript.
 *
 * On `--resume` (and on any `/resume`-style switch) the counters start at zero
 * because they only accumulate responses this process has seen. The status
 * line then showed "Tokens 0 (in 0, out 0)" and no cache rate until the first
 * new message, even though the conversation already had thousands of tokens.
 *
 * Display-only: costs are not recomputed (the transcript carries no price),
 * and a model that has already recorded live usage is left alone so a request
 * that lands during startup is never clobbered.
 */
const hydrated = new Set<string>()

function emptyUsage(): ModelUsage {
  return {
    inputTokens: 0,
    outputTokens: 0,
    cacheReadInputTokens: 0,
    cacheCreationInputTokens: 0,
    webSearchRequests: 0,
    costUSD: 0,
    contextWindow: 0,
    maxOutputTokens: 0,
  }
}

/**
 * Sum billed usage per model from raw session-JSONL text. Exported for tests:
 * the parsing rules (assistant usage only, synthetic models skipped, duplicate
 * uuids counted once) are the part that can silently over-count.
 */
export function sumUsageFromTranscriptText(
  text: string,
): Map<string, ModelUsage> {
  const totals = new Map<string, ModelUsage>()
  // Resume can leave the same assistant entry in the file twice; usage is
  // billed once, so dedupe by the entry uuid.
  const seen = new Set<string>()
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed) continue
    let entry: Record<string, unknown>
    try {
      entry = JSON.parse(trimmed) as Record<string, unknown>
    } catch {
      continue
    }
    const message = entry.message as Record<string, unknown> | undefined
    const usage = message?.usage as Record<string, unknown> | undefined
    const model = message?.model
    if (!usage || typeof model !== 'string' || model === '<synthetic>') {
      continue
    }
    const uuid = typeof entry.uuid === 'string' ? entry.uuid : undefined
    if (uuid) {
      if (seen.has(uuid)) continue
      seen.add(uuid)
    }
    const current = totals.get(model) ?? emptyUsage()
    current.inputTokens += Number(usage.input_tokens) || 0
    current.outputTokens += Number(usage.output_tokens) || 0
    current.cacheReadInputTokens += Number(usage.cache_read_input_tokens) || 0
    current.cacheCreationInputTokens +=
      Number(usage.cache_creation_input_tokens) || 0
    totals.set(model, current)
  }
  return totals
}

/**
 * Async so a large transcript never blocks the first frame. Resolves true when
 * counters were seeded (the caller re-renders then).
 */
export async function hydrateUsageFromTranscript(
  sessionId: string,
): Promise<boolean> {
  if (hydrated.has(sessionId)) return false
  hydrated.add(sessionId)
  try {
    const path = getTranscriptPath()
    const stat = statSync(path)
    if (!stat.isFile() || stat.size === 0) return false
    // A multi-GB transcript must not be read on every launch; the counters
    // simply start at zero for this session, as before.
    if (stat.size > MAX_TRANSCRIPT_READ_BYTES) return false

    const totals = sumUsageFromTranscriptText(await readFile(path, 'utf8'))
    let applied = false
    for (const [model, usage] of totals) {
      // A live response may have landed since mount; never overwrite it.
      if (getUsageForModel(model)) continue
      if (
        usage.inputTokens === 0 &&
        usage.outputTokens === 0 &&
        usage.cacheReadInputTokens === 0 &&
        usage.cacheCreationInputTokens === 0
      ) {
        continue
      }
      addToTotalCostState(0, usage, model)
      applied = true
    }
    return applied
  } catch {
    // A missing/unreadable transcript is not an error: the row just starts at
    // zero, which is what it did before.
    return false
  }
}
