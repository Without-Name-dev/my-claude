import { useEffect, useState } from 'react';
import { Bug, X } from 'lucide-react';
import { debugLog, enableDebug, getDebugEntries, isDebugEnabled, subscribeDebug } from '../lib/debug-log';

/**
 * Header log button + slide-up panel. Hidden unless ?debug=1 (or the button
 * was enabled this session). Shows the client-side debug ring buffer so
 * errors can be read straight off the failing machine/phone.
 */
export function DebugButton() {
  const [enabled, setEnabled] = useState(isDebugEnabled());
  useEffect(() => subscribeDebug(() => setEnabled(isDebugEnabled())), []);
  if (!enabled) return null;
  return (
    <button
      type="button"
      onClick={() => {
        window.dispatchEvent(new CustomEvent('cc:toggle-debug-panel'));
      }}
      className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full text-text-muted transition-colors hover:bg-surface-2 hover:text-text-secondary active:bg-surface-2 active:text-text-secondary"
      aria-label="打开日志"
      title="客户端日志"
    >
      <Bug className="h-4 w-4" />
    </button>
  );
}

export function DebugPanel() {
  const [open, setOpen] = useState(false);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    const toggle = () => setOpen(previous => !previous);
    window.addEventListener('cc:toggle-debug-panel', toggle);
    return () => window.removeEventListener('cc:toggle-debug-panel', toggle);
  }, []);

  useEffect(() => subscribeDebug(() => setTick(value => value + 1)), []);

  if (!open || !isDebugEnabled()) return null;
  const entries = getDebugEntries();

  return (
    <div className="fixed bottom-0 left-0 right-0 z-50 flex h-72 flex-col border-t border-border bg-surface-1 shadow-2xl">
      <div className="flex items-center justify-between border-b border-border px-3 py-1.5 text-xs text-text-secondary">
        <span className="font-medium">客户端日志（最近 {entries.length} 条）</span>
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="text-brand-text hover:underline"
            onClick={() => {
              void navigator.clipboard
                ?.writeText(entries.map(e => `${new Date(e.at).toISOString()} [${e.tag}] ${e.message}`).join('\n'))
                .then(() => debugLog('debug', '已复制到剪贴板'));
            }}
          >
            复制全部
          </button>
          <button type="button" onClick={() => setOpen(false)} aria-label="关闭">
            <X className="h-3.5 w-3.5" />
          </button>
        </div>
      </div>
      <div className="flex-1 overflow-y-auto px-3 py-2 font-mono text-[11px] leading-relaxed">
        {entries.length === 0 && <p className="text-text-muted">暂无日志</p>}
        {entries.map((entry, index) => (
          <div key={index} className="whitespace-pre-wrap break-all">
            <span className="text-text-muted">
              {new Date(entry.at).toLocaleTimeString()} [{entry.tag}]
            </span>{' '}
            <span className={entry.tag === 'window' || entry.tag === 'promise' ? 'text-status-error' : ''}>
              {entry.message}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
