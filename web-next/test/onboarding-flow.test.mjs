import assert from 'node:assert/strict'
import test from 'node:test'
import { registerSource } from './helpers/onboarding-source.mjs'

const hooks = registerSource()
const { rememberAuthFlow, takeAuthFlow } = await import('../src/features/auth/continuation.ts')
hooks.deregister()

const storage = () => {
  const values = new Map()
  return { setItem: (key, value) => values.set(key, value), getItem: key => values.get(key) ?? null, removeItem: key => values.delete(key) }
}

test('login and registration preserve native OAuth context without allowing external redirects', () => {
  const store = storage()
  const hash = '#/mobile-oauth?client_id=agents-anywhere-mobile&redirect_uri=agents-anywhere%3A%2F%2Foauth%2Fcallback'
  rememberAuthFlow(hash, store)
  assert.equal(takeAuthFlow('#/register', store), hash)
  assert.equal(takeAuthFlow('#/login', store), null)
  rememberAuthFlow('https://evil.test/', store)
  assert.equal(takeAuthFlow('#/login', store), null)
  assert.equal(takeAuthFlow(hash, store), hash)
})

test('removed plugin and onboarding routes are no longer continuation targets', () => {
  const store = storage()
  rememberAuthFlow('#/plugin-oauth?client_id=agents-anywhere-dsh-plugin', store)
  assert.equal(takeAuthFlow('#/login', store), null)
  rememberAuthFlow('#/onboarding?source=dsh-plugin', store)
  assert.equal(takeAuthFlow('#/login', store), null)
})
