/**
 * Minimal typings for Bun's built-in SQLite driver.
 *
 * The server only uses the synchronous query surface; declaring it here keeps
 * `tsc --noEmit` working without pulling all of `bun-types` into the package.
 */
declare module 'bun:sqlite' {
  export type SqlValue = string | number | boolean | null | Uint8Array

  export class Statement {
    all(...params: SqlValue[]): unknown[]
    get(...params: SqlValue[]): unknown
    run(...params: SqlValue[]): {
      changes: number
      lastInsertRowid: number | bigint
    }
  }

  export class Database {
    constructor(
      filename?: string,
      options?: {
        create?: boolean
        readwrite?: boolean
        readonly?: boolean
        strict?: boolean
      },
    )
    exec(sql: string): void
    query(sql: string): Statement
    prepare(sql: string): Statement
    run(
      sql: string,
      ...params: SqlValue[]
    ): { changes: number; lastInsertRowid: number | bigint }
    close(): void
  }
}
