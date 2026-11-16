import { RefreshCw } from 'lucide-react';

export type QuotaWindow = { label: string; remaining: number; resetAt?: number };
export type QuotaState = { status: 'loading' | 'ready' | 'error'; windows: QuotaWindow[] };

export function formatQuotaReset(resetAt: number, now = Date.now()): string {
  const minutes = Math.max(0, Math.ceil((resetAt - now) / 60_000));
  if (minutes >= 1_440) return `${Math.floor(minutes / 1_440)}天${Math.floor((minutes % 1_440) / 60)}小时`;
  if (minutes >= 60) return `${Math.floor(minutes / 60)}小时${minutes % 60}分钟`;
  return `${minutes}分钟`;
}

export function ProviderQuota({ state, onRefresh }: { state?: QuotaState; onRefresh: () => void }) {
  const loading = !state || state.status === 'loading';
  return (
    <div className="mt-3 rounded-lg border border-border bg-surface-0 px-3 py-2">
      <div className="flex items-center justify-between gap-3">
        <span className="text-xs font-medium text-text-secondary">额度</span>
        <button
          type="button"
          aria-label="刷新额度"
          disabled={loading}
          onClick={onRefresh}
          className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg text-text-muted disabled:opacity-50 active:bg-surface-1"
        >
          <RefreshCw size={14} className={loading ? 'animate-spin' : undefined} />
        </button>
      </div>
      {loading ? (
        <p role="status" className="text-xs text-text-muted">
          正在查询额度…
        </p>
      ) : state.status === 'error' ? (
        <p className="text-xs text-text-muted">额度查询失败，请重试</p>
      ) : state.windows.length === 0 ? (
        <p className="text-xs text-text-muted">暂无可用额度数据</p>
      ) : (
        <div className="space-y-1 text-xs text-text-secondary">
          {state.windows.map((window, index) => (
            <div
              key={`${window.label}-${index}`}
              className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-0.5"
            >
              <span>
                {window.label}剩余{' '}
                <span className="font-medium tabular-nums text-text-primary">{window.remaining}%</span>
              </span>
              {window.resetAt && <span className="text-text-muted">{formatQuotaReset(window.resetAt)}后重置</span>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
