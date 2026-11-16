import { Database } from 'bun:sqlite'
import { randomUUID } from 'node:crypto'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { dataDir } from './state'

export type ProviderProtocol = 'anthropic' | 'openai' | 'gemini' | 'grok'
export type ProviderModelDetail = {
  model: string
  displayName: string
  context1m: boolean
}
export type ProviderInput = {
  name: string
  protocol: ProviderProtocol
  baseUrl: string
  authMode?: 'api-key' | 'bearer'
  apiKey?: string
  models: string[]
  modelDetails?: ProviderModelDetail[]
}
export type ProviderInfo = Omit<ProviderInput, 'apiKey'> & {
  id: string
  hasApiKey: boolean
  revision: number
}
export type ProviderCredentials = ProviderInfo & { apiKey: string }

type Row = {
  id: string
  name: string
  protocol: ProviderProtocol
  base_url: string
  auth_mode: 'api-key' | 'bearer'
  api_key: string
  models: string
  model_details: string
  revision: number
}
let database: Database | undefined

function db(): Database {
  if (database) return database
  const dir = dataDir()
  mkdirSync(dir, { recursive: true })
  database = new Database(join(dir, 'providers.db'), { create: true })
  database.exec(`CREATE TABLE IF NOT EXISTS providers (
    id TEXT PRIMARY KEY, name TEXT NOT NULL, protocol TEXT NOT NULL,
    base_url TEXT NOT NULL, api_key TEXT NOT NULL, models TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1, auth_mode TEXT NOT NULL DEFAULT 'api-key'
  )`)
  database.exec(`CREATE TABLE IF NOT EXISTS provider_preferences (
    key TEXT PRIMARY KEY, provider_id TEXT NOT NULL, model TEXT NOT NULL
  )`)
  const columns = database.query('PRAGMA table_info(providers)').all() as {
    name: string
  }[]
  if (!columns.some(column => column.name === 'auth_mode')) {
    database.exec(
      "ALTER TABLE providers ADD COLUMN auth_mode TEXT NOT NULL DEFAULT 'api-key'",
    )
  }
  if (!columns.some(column => column.name === 'model_details')) {
    database.exec(
      "ALTER TABLE providers ADD COLUMN model_details TEXT NOT NULL DEFAULT '[]'",
    )
  }
  return database
}

function publicRow(row: Row): ProviderInfo {
  return {
    id: row.id,
    name: row.name,
    protocol: row.protocol,
    baseUrl: row.base_url,
    authMode: row.auth_mode,
    models: JSON.parse(row.models) as string[],
    modelDetails: JSON.parse(
      row.model_details || '[]',
    ) as ProviderModelDetail[],
    revision: row.revision,
    hasApiKey: row.api_key.length > 0,
  }
}

export function listProviders(): ProviderInfo[] {
  return (
    db()
      .query('SELECT * FROM providers ORDER BY name COLLATE NOCASE, id')
      .all() as Row[]
  ).map(publicRow)
}

export function readProvider(id: string): ProviderCredentials | undefined {
  const row = db()
    .query('SELECT * FROM providers WHERE id = ?')
    .get(id) as Row | null
  return row ? { ...publicRow(row), apiKey: row.api_key } : undefined
}

/** Preferences contain identifiers only, never provider credentials. */
export function readLastProviderSelection(): {
  id: string
  model: string
} | null {
  const row = db()
    .query(
      "SELECT provider_id, model FROM provider_preferences WHERE key = 'last'",
    )
    .get() as { provider_id: string; model: string } | null
  return row ? { id: row.provider_id, model: row.model } : null
}

export function saveLastProviderSelection(id: string, model: string): void {
  const provider = readProvider(id)
  if (!provider || !provider.models.includes(model))
    throw new Error('供应商或模型不存在')
  db()
    .query(
      "INSERT INTO provider_preferences (key, provider_id, model) VALUES ('last', ?, ?) ON CONFLICT(key) DO UPDATE SET provider_id=excluded.provider_id, model=excluded.model",
    )
    .run(id, model)
}

export function saveProvider(input: ProviderInput, id?: string): ProviderInfo {
  const previous = id ? readProvider(id) : undefined
  if (id && !previous) throw new Error('provider_not_found')
  const providerId = id ?? randomUUID()
  const key =
    input.apiKey === undefined ? (previous?.apiKey ?? '') : input.apiKey
  db()
    .query(`INSERT INTO providers (id, name, protocol, base_url, api_key, models, revision, auth_mode, model_details)
    VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
    ON CONFLICT(id) DO UPDATE SET name=excluded.name, protocol=excluded.protocol,
      base_url=excluded.base_url, api_key=excluded.api_key, models=excluded.models, auth_mode=excluded.auth_mode, model_details=excluded.model_details, revision=revision+1`)
    .run(
      providerId,
      input.name,
      input.protocol,
      input.baseUrl,
      key,
      JSON.stringify(input.models),
      input.authMode ?? 'api-key',
      JSON.stringify(
        input.modelDetails ??
          previous?.modelDetails?.filter(detail =>
            input.models.includes(detail.model),
          ) ??
          [],
      ),
    )
  return publicRow(
    db().query('SELECT * FROM providers WHERE id = ?').get(providerId) as Row,
  )
}

export function deleteProvider(id: string): boolean {
  return db().query('DELETE FROM providers WHERE id = ?').run(id).changes > 0
}

export function resetProviderStoreForTest(): void {
  database?.close()
  database = undefined
}
