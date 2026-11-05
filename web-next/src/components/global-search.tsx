import React, { useState, useCallback, useEffect, useRef } from 'react';
import { Search, X, Loader2, MessageSquare } from 'lucide-react';
import { Dialog, DialogContent } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { ScrollArea } from '@/components/ui/scroll-area';

interface SearchResult {
  session_id: string;
  session_title: string;
  item_id: string;
  item_type: string;
  role?: string;
  content: string;
  snippet: string;
  item_time?: number;
  rank: number;
}

interface SearchResponse {
  results: SearchResult[];
  total: number;
  limit: number;
  offset: number;
}

interface GlobalSearchProps {
  isOpen: boolean;
  onClose: () => void;
  onSelectResult?: (sessionId: string, itemId: string) => void;
}

export function GlobalSearch({ isOpen, onClose, onSelectResult }: GlobalSearchProps) {
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<SearchResult[]>([]);
  const [isLoading, setIsLoading] = useState(false);
  const [total, setTotal] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const abortControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    if (isOpen) {
      setTimeout(() => inputRef.current?.focus(), 100);
    } else {
      setQuery('');
      setResults([]);
      setTotal(0);
    }
  }, [isOpen]);

  const performSearch = useCallback(async (searchQuery: string) => {
    if (!searchQuery.trim()) {
      setResults([]);
      setTotal(0);
      return;
    }

    abortControllerRef.current?.abort();
    abortControllerRef.current = new AbortController();

    setIsLoading(true);

    try {
      const params = new URLSearchParams({
        q: searchQuery,
        limit: '20',
        offset: '0',
      });

      const response = await fetch(`/api/v2/search?${params}`, {
        signal: abortControllerRef.current.signal,
      });

      if (!response.ok) {
        throw new Error('Search failed');
      }

      const data: SearchResponse = await response.json();
      setResults(data.results);
      setTotal(data.total);
    } catch (error) {
      if (error instanceof Error && error.name !== 'AbortError') {
        console.error('Search error:', error);
      }
    } finally {
      setIsLoading(false);
    }
  }, []);

  useEffect(() => {
    const timeoutId = setTimeout(() => {
      performSearch(query);
    }, 300);

    return () => clearTimeout(timeoutId);
  }, [query, performSearch]);

  const handleResultClick = (result: SearchResult) => {
    onSelectResult?.(result.session_id, result.item_id);
    onClose();
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      onClose();
    }
  };

  const highlightSnippet = (snippet: string) => {
    return snippet
      .split(/(<mark>|<\/mark>)/)
      .map((part, index) => {
        if (part === '<mark>') return null;
        if (part === '</mark>') return null;
        const prevPart = snippet.split(/(<mark>|<\/mark>)/)[index - 1];
        if (prevPart === '<mark>') {
          return (
            <mark key={index} className="bg-yellow-200 dark:bg-yellow-800 font-semibold">
              {part}
            </mark>
          );
        }
        return <span key={index}>{part}</span>;
      })
      .filter(Boolean);
  };

  const formatTimestamp = (timestamp?: number) => {
    if (!timestamp) return '';
    const date = new Date(timestamp);
    const now = new Date();
    const diffMs = now.getTime() - date.getTime();
    const diffMins = Math.floor(diffMs / 60000);
    const diffHours = Math.floor(diffMs / 3600000);
    const diffDays = Math.floor(diffMs / 86400000);

    if (diffMins < 1) return 'just now';
    if (diffMins < 60) return `${diffMins}m ago`;
    if (diffHours < 24) return `${diffHours}h ago`;
    if (diffDays < 7) return `${diffDays}d ago`;
    return date.toLocaleDateString();
  };

  return (
    <Dialog open={isOpen} onOpenChange={onClose}>
      <DialogContent className="max-w-3xl max-h-[80vh] p-0" onKeyDown={handleKeyDown}>
        <div className="flex flex-col h-full">
          <div className="flex items-center gap-3 p-4 border-b">
            <Search className="w-5 h-5 text-muted-foreground" />
            <Input
              ref={inputRef}
              type="text"
              placeholder="Search messages across all conversations..."
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              className="border-0 focus-visible:ring-0 focus-visible:ring-offset-0 text-base"
            />
            {isLoading && <Loader2 className="w-5 h-5 animate-spin text-muted-foreground" />}
            {query && (
              <button
                onClick={() => setQuery('')}
                className="p-1 hover:bg-muted rounded-sm transition-colors"
              >
                <X className="w-4 h-4" />
              </button>
            )}
          </div>

          <ScrollArea className="flex-1">
            {!query ? (
              <div className="flex flex-col items-center justify-center h-64 text-center px-4">
                <Search className="w-12 h-12 text-muted-foreground mb-4" />
                <h3 className="text-lg font-semibold mb-2">Search across all conversations</h3>
                <p className="text-sm text-muted-foreground">
                  Find messages, code snippets, and insights
                </p>
              </div>
            ) : results.length === 0 && !isLoading ? (
              <div className="flex flex-col items-center justify-center h-64 text-center px-4">
                <MessageSquare className="w-12 h-12 text-muted-foreground mb-4" />
                <h3 className="text-lg font-semibold mb-2">No results found</h3>
                <p className="text-sm text-muted-foreground">
                  Try different keywords or check your spelling
                </p>
              </div>
            ) : (
              <div className="p-4 space-y-3">
                {total > 0 && (
                  <p className="text-sm text-muted-foreground mb-4">
                    Found {total} {total === 1 ? 'result' : 'results'}
                  </p>
                )}
                {results.map((result) => (
                  <button
                    key={`${result.session_id}-${result.item_id}`}
                    onClick={() => handleResultClick(result)}
                    className="w-full text-left p-4 rounded-lg border hover:bg-muted/50 transition-colors"
                  >
                    <div className="flex items-center justify-between mb-2">
                      <span className="text-sm font-medium text-muted-foreground">
                        {result.session_title || 'Untitled conversation'}
                      </span>
                      <div className="flex items-center gap-2">
                        {result.role && (
                          <span
                            className={`text-xs px-2 py-1 rounded ${
                              result.role === 'user'
                                ? 'bg-blue-100 text-blue-700 dark:bg-blue-900 dark:text-blue-300'
                                : 'bg-purple-100 text-purple-700 dark:bg-purple-900 dark:text-purple-300'
                            }`}
                          >
                            {result.role}
                          </span>
                        )}
                        <span className="text-xs text-muted-foreground">
                          {formatTimestamp(result.item_time)}
                        </span>
                      </div>
                    </div>
                    <div className="text-sm line-clamp-3">{highlightSnippet(result.snippet)}</div>
                  </button>
                ))}
              </div>
            )}
          </ScrollArea>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function useGlobalSearch() {
  const [isOpen, setIsOpen] = useState(false);

  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault();
        setIsOpen(true);
      }
    };

    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, []);

  return {
    isOpen,
    open: () => setIsOpen(true),
    close: () => setIsOpen(false),
  };
}
