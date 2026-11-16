/** Split terminal-dropped absolute paths without splitting spaces inside them. */
export function splitPastedPaths(text: string): string[] {
  return text
    .split(/ (?=["']?(?:\/|[A-Za-z]:[\\/]))/)
    .flatMap(part => part.split(/\r?\n/))
    .filter(line => line.trim().length > 0)
}
