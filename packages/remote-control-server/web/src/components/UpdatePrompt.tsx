import { useEffect, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import { isUpdateAvailable } from '../lib/app-version';
import { haptic } from '../lib/view-transition';

const CHECK_INTERVAL_MS = 5 * 60 * 1000;

/**
 * A quiet pill that appears when the deployed UI no longer matches the running
 * one. Reloading is the user's choice: they may be mid-draft.
 */
export function UpdatePrompt() {
  const [available, setAvailable] = useState(false);

  useEffect(() => {
    let cancelled = false;
    const check = async () => {
      if (document.hidden) return;
      const next = await isUpdateAvailable();
      if (!cancelled && next) setAvailable(true);
    };
    void check();
    const timer = window.setInterval(() => void check(), CHECK_INTERVAL_MS);
    const onVisible = () => {
      if (!document.hidden) void check();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, []);

  if (!available) return null;

  return (
    <div className="pointer-events-none fixed inset-x-0 bottom-[calc(5rem+env(safe-area-inset-bottom))] z-40 flex justify-center px-4">
      <button
        type="button"
        onClick={() => {
          haptic('light');
          window.location.reload();
        }}
        className="pointer-events-auto flex items-center gap-1.5 rounded-full border border-border bg-surface-2 px-3.5 py-2 text-xs font-medium text-text-primary shadow-lg transition-transform active:scale-95"
      >
        <RefreshCw className="h-3.5 w-3.5" aria-hidden="true" />
        有新版本 · 点击刷新
      </button>
    </div>
  );
}
