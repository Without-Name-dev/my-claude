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
  // 手势的起点和累计拉动量放在 ref 里：流式回复期间 ChatView 每来一个数据块
  // 就重渲染，onLoad 的新身份会重建监听器；闭包里的局部变量随重绑归零，
  // 下一次 touchmove 拿 clientY-0 当位移，拉动量瞬间跳满并误触发加载。
  const gesture = useRef({ startY: 0, pulled: 0 });
  // 回调同样走 ref，让监听器只在开关变化时重绑，而不是每次渲染都拆装一遍。
  const onLoadRef = useRef(onLoad);
  onLoadRef.current = onLoad;

  useLayoutEffect(() => {
    const wrapper = containerRef.current;
    if (!wrapper) return;
    // The forwarded ref is StickToBottom's outer wrapper, which never scrolls
    // (overflow-y-hidden). Watching it made scrollTop always read 0, so every
    // downward drag anywhere in the transcript fired a history load. The real
    // scroller is the .rcs-scroller div rendered by StickToBottom.Content.
    const el = (wrapper.querySelector('.rcs-scroller') as HTMLElement | null) ?? wrapper;
    const onStart = (event: TouchEvent) => {
      if (!enabled || loading) {
        armed.current = false;
        return;
      }
      armed.current = el.scrollTop <= 0;
      gesture.current.startY = event.touches[0]?.clientY ?? 0;
      gesture.current.pulled = 0;
    };
    const onMove = (event: TouchEvent) => {
      if (!armed.current) return;
      if (el.scrollTop > 0) {
        armed.current = false;
        setPull(0);
        return;
      }
      const delta = (event.touches[0]?.clientY ?? gesture.current.startY) - gesture.current.startY;
      if (delta <= 0) {
        setPull(0);
        return;
      }
      // Damped so a short pull still reads as a pull, not a jump.
      gesture.current.pulled = Math.min(delta * 0.5, 72);
      setPull(gesture.current.pulled);
    };
    const onEnd = () => {
      if (!armed.current) return;
      armed.current = false;
      if (gesture.current.pulled > 56) onLoadRef.current();
      gesture.current.pulled = 0;
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
  }, [enabled, loading]);

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
          className="min-h-11 rounded-full px-3 text-xs text-text-muted transition-colors active:bg-surface-1"
        >
          查看更早的消息
        </button>
      )}
    </div>
  );
}
