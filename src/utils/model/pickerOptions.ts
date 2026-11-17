import type { ModelOption } from './modelOptions.js'

/** An active server catalogue is the authoritative set of selectable models. */
export function composePickerOptions(
  builtin: readonly ModelOption[],
  server: readonly ModelOption[],
  serverOnly: boolean,
): ModelOption[] {
  return serverOnly ? [...server] : [...builtin, ...server]
}
