import { useLayoutEffect, useRef, useState } from 'react';

/**
 * "加载更早的消息" as a row at the top of the transcript plus a pull-down
 * trigger: the gesture iOS users expect from a message list. Wrapped in a
 * scroller, reaching the top and dragging further fires `onLoad`.
 */
export function usePullToLoadOlder({
  enabled,
  loading,
  onLoad,
}: {
  enabled: boolean;
  loading: boolean;
  onLoad: () => void;
}) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [pull, setPull] = useState(0);
  const armed = useRef(false);

  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    let startY = 0;
    let pulled = 0;
    const onStart = (event: TouchEvent) => {
      if (!enabled || loading) {
        armed.current = false;
        return;
      }
      armed.current = el.scrollTop <= 0;
      startY = event.touches[0]?.clientY ?? 0;
      pulled = 0;
    };
    const onMove = (event: TouchEvent) => {
      if (!armed.current) return;
      if (el.scrollTop > 0) {
        armed.current = false;
        setPull(0);
        return;
      }
      const delta = (event.touches[0]?.clientY ?? startY) - startY;
      if (delta <= 0) {
        setPull(0);
        return;
      }
      // Damped so a short pull still reads as a pull, not a jump.
      pulled = Math.min(delta * 0.5, 72);
      setPull(pulled);
    };
    const onEnd = () => {
      if (!armed.current) return;
      armed.current = false;
      if (pulled > 56) onLoad();
      pulled = 0;
      setPull(0);
    };
    el.addEventListener('touchstart', onStart, { passive: true });
    el.addEventListener('touchmove', onMove, { passive: true });
    el.addEventListener('touchend', onEnd);
    el.addEventListener('touchcancel', onEnd);
    return () => {
      el.removeEventListener('touchstart', onStart);
      el.removeEventListener('touchmove', onMove);
      el.removeEventListener('touchend', onEnd);
      el.removeEventListener('touchcancel', onEnd);
    };
  }, [enabled, loading, onLoad]);

  const hint = loading ? '正在加载更早的消息…' : pull > 0 ? (pull > 56 ? '松开加载' : '下拉加载更早的消息') : '';

  return {
    containerRef,
    pull,
    hint,
  };
}

/** Row rendered at the very top of the transcript. */
export function OlderMessagesRow({
  hasMore,
  loading,
  hint,
  pull,
  onLoad,
}: {
  hasMore: boolean;
  loading: boolean;
  hint: string;
  pull: number;
  onLoad: () => void;
}) {
  if (!hasMore && !loading && pull === 0) return null;
  return (
    <div
      className="flex items-center justify-center overflow-hidden pb-1 text-xs text-text-muted"
      style={{ height: pull > 0 ? pull : undefined }}
      aria-live="polite"
    >
      {pull > 0 || loading ? (
        <span>{hint}</span>
      ) : (
        <button
          type="button"
          onClick={onLoad}
          className="min-h-9 rounded-full px-3 text-xs text-text-muted transition-colors active:bg-surface-1"
        >
          查看更早的消息
        </button>
      )}
    </div>
  );
}
