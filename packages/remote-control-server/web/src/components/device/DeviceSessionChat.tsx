import { ChevronLeft } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { ChatInput } from '../../../components/chat/ChatInput';
import { ChatView } from '../../../components/chat/ChatView';
import { TooltipProvider } from '../../../components/ui/tooltip';
import type { ChatInputMessage, ThreadEntry } from '../../lib/types';
import { mergeHistoryPage, mergeThreadEntries, transcriptToEntries } from '../../lib/device-history';
import {
  fetchHistory,
  fetchOperation,
  fetchSession,
  openSession,
  sendDeviceMessage,
  type DeviceSessionDetail,
  type OpenOperation,
} from '../../api/devices';
import { BridgeChat } from './BridgeChat';

/** One screenful plus context; older pages are fetched on demand. */
const HISTORY_PAGE = 30;

function isBridgeFresh(detail: DeviceSessionDetail): boolean {
  if (!detail.bridgeSessionId) return false;
  // A stale mapping must never mount the live chat: the SSE endpoint answers
  // 409 for closed sessions and would error-loop.
  if (detail.bridgeConnected === false) return false;
  const runtime = detail.runtime;
  if (!runtime?.bridgeSessionId) return false;
  if (runtime.bridgeSessionId !== detail.bridgeSessionId) return false;
  const at = runtime.reportedAt ?? runtime.verifiedAt;
  return Date.now() - at < 120_000;
}

export function DeviceSessionChat({
  deviceId,
  localSessionId,
  onBack,
}: {
  deviceId: string;
  localSessionId: string;
  deviceName?: string;
  onBack: () => void;
}) {
  const [detail, setDetail] = useState<DeviceSessionDetail | null>(null);
  const [entries, setEntries] = useState<ThreadEntry[]>([]);
  const [historyCursor, setHistoryCursor] = useState<string | undefined>();
  const [historyHasMore, setHistoryHasMore] = useState(false);
  const [historyError, setHistoryError] = useState('');
  const [operation, setOperation] = useState<OpenOperation | null>(null);
  const [operationId, setOperationId] = useState<string | null>(null);
  const [forcedBridgeId, setForcedBridgeId] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const [loadingDetail, setLoadingDetail] = useState(false);
  const [loadingEarlier, setLoadingEarlier] = useState(false);
  const [blockedSession, setBlockedSession] = useState(false);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const loadedKey = useRef('');

  const loadDetail = useCallback(async () => {
    const next = await fetchSession(deviceId, localSessionId);
    setDetail(next);
    if (isBridgeFresh(next)) setForcedBridgeId(next.bridgeSessionId);
    if (next.operation && next.operation.state !== 'ready' && next.operation.state !== 'failed') {
      // Includes waiting_for_user: the terminal is open on the PC and the
      // user needs to see its instruction, not a generic spinner.
      setOperation(next.operation);
      setOperationId(next.operation.id);
    }
    return next;
  }, [deviceId, localSessionId]);

  const loadHistory = useCallback(
    async (before?: string) => {
      const page = await fetchHistory(deviceId, localSessionId, {
        // A phone screen shows ~3 messages. Pulling 100 and rendering them all
        // made the first paint expensive; older pages load on demand.
        limit: HISTORY_PAGE,
        ...(before ? { before } : {}),
      });
      const converted = transcriptToEntries(page.messages);
      setEntries(previous => {
        if (!before) {
          // A live bridge may have appended entries while this async load was
          // in flight. Merge instead of replacing, keeping history first.
          return previous.length === 0 ? converted : mergeHistoryPage(previous, converted);
        }
        // Guard against a double-click sending the same cursor twice.
        const key = (entry: ThreadEntry) => (entry.type === 'tool_call' ? `tool-${entry.toolCall.id}` : entry.id);
        const seen = new Set(previous.map(key));
        return [...converted.filter(entry => !seen.has(key(entry))), ...previous];
      });
      setHistoryCursor(page.before);
      setHistoryHasMore(page.hasMore);
      setHistoryError('');
      // The server serves its own cache when the machine is unreachable or
      // the terminal is closed; say so instead of pretending it is live.
      if (page.warning) {
        setNotice(previous => previous || page.warning!);
      } else if (page.cached) {
        setNotice(previous => previous || '设备离线，显示服务端缓存的消息');
      }
    },
    [deviceId, localSessionId],
  );

  /**
   * Merge the newest server rows into the on-screen transcript without
   * replacing it. Used across bridge connect/disconnect so messages produced
   * while the live stream was down are picked up, but nothing visibly reloads.
   */
  const syncHistory = useCallback(async () => {
    try {
      const page = await fetchHistory(deviceId, localSessionId, { limit: HISTORY_PAGE });
      const converted = transcriptToEntries(page.messages);
      setEntries(previous => mergeHistoryPage(previous, converted));
    } catch {
      // Silent: this is a background catch-up, not the primary load.
    }
  }, [deviceId, localSessionId]);

  const loadEarlier = useCallback(async () => {
    if (!historyCursor || loadingEarlier) return;
    setLoadingEarlier(true);
    try {
      await loadHistory(historyCursor);
    } catch (err) {
      setHistoryError(err instanceof Error ? err.message : '加载失败');
    } finally {
      setLoadingEarlier(false);
    }
  }, [historyCursor, loadingEarlier, loadHistory]);

  const reload = useCallback(async () => {
    setError('');
    setHistoryError('');
    setLoadingDetail(true);
    try {
      try {
        await loadDetail();
      } catch (err) {
        setError(err instanceof Error ? err.message : '加载会话失败');
      }
      // History comes from the server cache, so a detail failure must not
      // hide a transcript that is still readable.
      await loadHistory();
    } catch (err) {
      setHistoryError(err instanceof Error ? err.message : '加载历史失败');
    } finally {
      setLoadingDetail(false);
    }
  }, [loadDetail, loadHistory]);

  useEffect(() => {
    const key = `${deviceId}:${localSessionId}`;
    if (loadedKey.current === key) return;
    loadedKey.current = key;
    setDetail(null);
    setEntries([]);
    setHistoryCursor(undefined);
    setHistoryHasMore(false);
    setHistoryError('');
    setOperation(null);
    setOperationId(null);
    setForcedBridgeId(null);
    setNotice('');
    setError('');
    setLoadingDetail(true);

    void (async () => {
      try {
        try {
          await loadDetail();
        } catch (err) {
          setError(err instanceof Error ? err.message : '加载会话失败');
        }
        // Always load: the server has a durable conversation cache, so the
        // terminal being closed (or the machine offline) no longer blanks out
        // the conversation.
        await loadHistory();
      } catch (err) {
        setHistoryError(err instanceof Error ? err.message : '加载历史失败');
      } finally {
        setLoadingDetail(false);
      }
    })();
  }, [deviceId, localSessionId, loadDetail, loadHistory]);

  useEffect(() => {
    if (!operationId) return;
    let cancelled = false;
    const tick = async () => {
      try {
        const next = await fetchOperation(operationId);
        if (cancelled) return;
        setOperation(next);
        if (next.state === 'ready') {
          setOperationId(null);
          setBlockedSession(false);
          if (next.bridgeSessionId) {
            setForcedBridgeId(next.bridgeSessionId);
            void syncHistory();
          }
          await loadDetail();
        } else if (next.state === 'failed') {
          setOperationId(null);
          setError(next.detail || '终端启动失败');
          // A terminal that is running without `/remote-control` cannot be
          // reused; offer the explicit (destructive) restart instead.
          setBlockedSession(next.failureCode === 'already_running_without_bridge');
        } else if (next.state === 'waiting_for_user') {
          // Keep polling, but surface the actionable instruction instead of a
          // generic spinner (the terminal is waiting on the PC).
          setOperation(next);
        }
      } catch (err) {
        if (cancelled) return;
        setOperationId(null);
        setError(err instanceof Error ? err.message : '无法读取启动进度');
      }
    };
    void tick();
    const timer = setInterval(tick, 1500);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [operationId, loadDetail, syncHistory]);

  const bridgeId = forcedBridgeId;

  const forceRestart = useCallback(async () => {
    if (!window.confirm('重启会中断该终端里正在执行的任务，但会话记录会保留。确定重启并启用远程控制吗？')) {
      return;
    }
    setBlockedSession(false);
    setError('');
    setNotice('正在重启终端并启用远程控制…');
    try {
      const result = await openSession(deviceId, localSessionId, true);
      if (result.operationId) {
        setOperationId(result.operationId);
      } else if (result.bridgeSessionId) {
        setForcedBridgeId(result.bridgeSessionId);
        await loadDetail();
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : '重启终端失败');
    }
  }, [deviceId, localSessionId, loadDetail]);

  const send = useCallback(
    async (message: ChatInputMessage) => {
      const text = message.text.trim();
      if (!text || sending) return;
      setSending(true);
      setError('');
      setNotice('');
      const pendingId = `pending-${Date.now()}`;
      setEntries(previous => [...previous, { type: 'user_message', id: pendingId, content: text }]);
      try {
        const result = await sendDeviceMessage(deviceId, localSessionId, text);
        if (result.bridgeSessionId) {
          setForcedBridgeId(result.bridgeSessionId);
          await loadDetail();
        } else if (result.operationId) {
          setOperationId(result.operationId);
        }
        if (result.state === 'unconfirmed') {
          setNotice(result.detail || '消息已保存，等待终端确认。');
        } else if (result.state === 'queued') {
          setNotice(result.detail || '终端尚未就绪，消息会在连接后发送。');
        }
      } catch (err) {
        setEntries(previous => previous.filter(entry => !(entry.type === 'user_message' && entry.id === pendingId)));
        setError(err instanceof Error ? err.message : '发送失败');
      } finally {
        setSending(false);
      }
    },
    [deviceId, localSessionId, sending, loadDetail],
  );

  // Always keep the session state fresh, not only while the live view is up.
  // This is what re-enters the bridge after an RCS restart or a transient SSE
  // failure, and what updates device/terminal state in the transcript view.
  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      try {
        const next = await fetchSession(deviceId, localSessionId);
        if (cancelled) return;
        setDetail(next);
        if (next.bridgeConnected && next.bridgeSessionId) {
          if (forcedBridgeId !== next.bridgeSessionId) {
            // Entering the live view after a reconnect: catch up on anything
            // produced while the SSE was down before switching.
            void syncHistory();
            setForcedBridgeId(next.bridgeSessionId);
          }
          return;
        }
        if (forcedBridgeId) {
          // The bridge went away. Keep the transcript on screen and just
          // switch back to the (sendable) history view.
          setForcedBridgeId(null);
          setNotice('终端已关闭，发送消息会重新打开终端。');
          void syncHistory();
        } else {
          // Transcript view: pick up rows the server cached in the
          // background (terminal output while no live stream was open).
          void syncHistory();
        }
      } catch {
        // Keep the current view; the next tick retries.
      }
    };
    const timer = setInterval(() => void tick(), 10_000);
    void tick();
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [forcedBridgeId, deviceId, localSessionId, syncHistory]);

  const leaveBridge = useCallback(
    (message: string) => {
      setForcedBridgeId(null);
      setNotice(message);
      // Keep the transcript that is already on screen; only the connection
      // state changed. Re-reading history here is what caused the visible
      // reload on every disconnect.
      void fetchSession(deviceId, localSessionId)
        .then(next => {
          setDetail(next);
          void syncHistory();
        })
        .catch(() => {});
    },
    [deviceId, localSessionId, syncHistory],
  );

  if (bridgeId) {
    return (
      <BridgeChat
        key={bridgeId}
        sessionId={bridgeId}
        entries={entries}
        onEntriesChange={setEntries}
        onBridgeReady={() => void syncHistory()}
        onBridgeDisconnected={() => leaveBridge('终端已关闭，发送消息会重新打开终端。')}
        onTerminalUnavailable={text => {
          setForcedBridgeId(null);
          setNotice('终端已关闭，正在重新打开…');
          void send({ text });
        }}
        onBack={onBack}
        header={<MobileChatHeader title={detail?.session.title || localSessionId} onBack={onBack} />}
      />
    );
  }

  const starting = Boolean(operationId);
  const statusText = starting
    ? operation?.state === 'waiting_for_user'
      ? operation.detail || '终端已打开，等待你在电脑上确认（信任目录、权限或模型配置）。'
      : '正在启动终端…'
    : historyError || notice;

  return (
    <TooltipProvider>
      <div className="flex min-h-0 flex-1 flex-col overflow-hidden rcs-page-enter">
        <MobileChatHeader title={detail?.session.title || localSessionId} onBack={onBack} />
        {error && (
          <div
            role="alert"
            className="flex items-center gap-3 border-b bg-destructive/10 px-4 py-2 text-sm text-destructive"
          >
            <span className="min-w-0 flex-1">{error}</span>
            <button type="button" onClick={() => void reload()} className="shrink-0 text-xs text-brand hover:underline">
              重试
            </button>
          </div>
        )}
        {historyHasMore && (
          <div className="py-1 text-center">
            <button
              type="button"
              className="text-[11px] text-text-muted transition-colors hover:text-brand disabled:opacity-50"
              disabled={loadingEarlier}
              onClick={() => void loadEarlier()}
            >
              {loadingEarlier ? '加载中…' : '加载更早的消息'}
            </button>
          </div>
        )}
        <ChatView
          entries={entries}
          isLoading={sending || starting || loadingDetail}
          emptyTitle={loadingDetail ? '正在加载会话…' : '暂无消息'}
          emptyDescription={
            loadingDetail ? '' : detail?.online ? '发送消息后会自动打开终端' : '设备离线，发送的消息会在上线后发出'
          }
        />
        {blockedSession && (
          <div className="mx-auto w-full max-w-3xl px-4 pt-2">
            <div className="flex items-center gap-3 rounded-md border border-border bg-surface-1 px-3 py-2 text-xs">
              <span className="min-w-0 flex-1 text-text-secondary">这个会话在终端里运行，但没有启用远程控制。</span>
              <button type="button" onClick={() => void forceRestart()} className="shrink-0 text-brand hover:underline">
                重启终端并启用
              </button>
            </div>
          </div>
        )}
        {statusText && (
          <div aria-live="polite" className="mx-auto w-full max-w-3xl px-4 pt-1 text-xs leading-snug text-text-muted">
            {statusText}
          </div>
        )}
        <ChatInput
          onSubmit={send}
          isLoading={sending}
          disabled={loadingDetail}
          placeholder={loadingDetail ? '正在加载…' : detail?.online ? '输入消息…' : '设备离线，消息会在上线后发送'}
        />
      </div>
    </TooltipProvider>
  );
}

/** Mobile-only way back; the desktop sidebar already shows the selection. */
function MobileChatHeader({ title, onBack }: { title: string; onBack: () => void }) {
  return (
    <div className="flex h-12 items-center gap-0.5 border-b border-border/70 bg-surface-0 px-1.5 md:hidden">
      <button
        type="button"
        onClick={onBack}
        className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full text-text-secondary transition-colors active:bg-surface-1"
        aria-label="返回列表"
      >
        <ChevronLeft className="h-5 w-5" />
      </button>
      <span className="truncate text-[15px] font-medium text-text-primary">{title}</span>
    </div>
  );
}
