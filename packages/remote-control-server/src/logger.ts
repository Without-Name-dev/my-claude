/** Thin logging wrapper — silent in test environment, uses console in production. */
const isTest =
  process.env.NODE_ENV === 'test' ||
  (typeof Bun !== 'undefined' && !!Bun.env.BUN_TEST)

export function log(...args: unknown[]): void {
  if (!isTest) console.log(...args)
}

/** Opt-in transport metadata only; never pass message bodies or credentials. */
export function debug(...args: unknown[]): void {
  if (!isTest && process.env.RCS_DEBUG === '1') console.log(...args)
}

export function error(...args: unknown[]): void {
  if (!isTest) console.error(...args)
}
