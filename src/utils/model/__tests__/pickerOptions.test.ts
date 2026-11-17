import { expect, test } from 'bun:test'
import { composePickerOptions } from '../pickerOptions'

test('a server-managed model picker hides built-in Claude models', () => {
  const builtin = [
    { value: null, label: 'Default', description: '' },
    { value: 'opus', label: 'Opus', description: '' },
  ]
  const server = [
    {
      value: 'rcs-provider:p:model',
      label: 'Provider / model',
      description: '',
    },
  ]
  expect(composePickerOptions(builtin, server, true)).toEqual(server)
  expect(composePickerOptions(builtin, [], false)).toEqual(builtin)
})
