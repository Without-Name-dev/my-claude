import * as React from "react"

export type IncrementalHistoryState<T> = {
  items: T[]
  hasMore: boolean
  isLoading: boolean
  error: string | null
}

export type IncrementalHistoryActions = {
  loadMore: () => Promise<void>
  reset: () => void
  prependItems: (items: any[], hasMore: boolean) => void
}

export type UseIncrementalHistoryOptions<T> = {
  initialItems?: T[]
  initialHasMore?: boolean
  pageSize?: number
  onLoadMore: (oldestItem: T, pageSize: number) => Promise<{ items: T[]; hasMore: boolean }>
  getItemKey: (item: T) => string
  compareItems?: (a: T, b: T) => number
}

export function useIncrementalHistory<T>(
  options: UseIncrementalHistoryOptions<T>
): [IncrementalHistoryState<T>, IncrementalHistoryActions] {
  const {
    initialItems = [],
    initialHasMore = false,
    pageSize = 100,
    onLoadMore,
    getItemKey,
    compareItems,
  } = options

  const [items, setItems] = React.useState<T[]>(initialItems)
  const [hasMore, setHasMore] = React.useState(initialHasMore)
  const [isLoading, setIsLoading] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const loadingRef = React.useRef(false)

  const loadMore = React.useCallback(async () => {
    if (loadingRef.current || isLoading || !hasMore) return
    const oldestItem = items[0]
    if (!oldestItem) return

    loadingRef.current = true
    setIsLoading(true)
    setError(null)

    try {
      const result = await onLoadMore(oldestItem, pageSize)

      setItems((current) => {
        const existingKeys = new Set(current.map(getItemKey))
        const newItems = result.items.filter((item) => !existingKeys.has(getItemKey(item)))

        const merged = [...newItems, ...current]
        return compareItems ? merged.sort(compareItems) : merged
      })

      setHasMore(result.hasMore)
    } catch (err) {
      const message = err instanceof Error ? err.message : "Failed to load more items"
      setError(message)
    } finally {
      loadingRef.current = false
      setIsLoading(false)
    }
  }, [isLoading, hasMore, items, onLoadMore, pageSize, getItemKey, compareItems])

  const reset = React.useCallback(() => {
    setItems(initialItems)
    setHasMore(initialHasMore)
    setIsLoading(false)
    setError(null)
    loadingRef.current = false
  }, [initialItems, initialHasMore])

  const prependItems = React.useCallback(
    (newItems: T[], newHasMore: boolean) => {
      setItems((current) => {
        const existingKeys = new Set(current.map(getItemKey))
        const uniqueNewItems = newItems.filter((item) => !existingKeys.has(getItemKey(item)))

        const merged = [...uniqueNewItems, ...current]
        return compareItems ? merged.sort(compareItems) : merged
      })
      setHasMore(newHasMore)
    },
    [getItemKey, compareItems]
  )

  return [
    { items, hasMore, isLoading, error },
    { loadMore, reset, prependItems },
  ]
}
