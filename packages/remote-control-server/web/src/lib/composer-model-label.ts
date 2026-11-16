/** Show the model ID, without the server provider or session settings. */
export function composerModelLabel(modelId: string, fallback?: string): string {
  if (modelId.startsWith('rcs-provider:')) {
    const match = /^rcs-provider:[^:]+:(.+)$/.exec(modelId)
    if (match) {
      try {
        return decodeURIComponent(match[1]!) || fallback || '设置'
      } catch {
        return fallback || '设置'
      }
    }
    return fallback || '设置'
  }
  return modelId || fallback || '设置'
}
