import { ChevronLeft } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { ChatInput } from '../../../components/chat/ChatInput';
import { ChatView } from '../../../components/chat/ChatView';
import { TooltipProvider } from '../../../components/ui/tooltip';
import type { ChatInputMessage, ThreadEntry } from '../../lib/types';
import { mergeHistoryPage, mergeThreadEntries, transcriptToEntries } from '../../lib/device-history';
import { cn } from '../../lib/utils';
import { createSerialQueue } from '../../lib/serial-queue';
import {
  closeSession,
  fetchHistory,
  fetchOperation,
  fetchSession,
  openSession,
  reviewUnconfirmedDelivery,
  sendDeviceMessage,
  type DeviceSessionDetail,
  type OpenOperation,
  type TranscriptUsage,
} from '../../api/devices';
import { BridgeChat } from './BridgeChat';
import { apiSendControl } from '../../api/client';
import { ModalSheet } from './ModalSheet';

/**
 * The 10s poll rebuilds the detail object on every tick; pushing an identical
 * value into state re-rendered the whole chat twice a minute for nothing.
 */
function shallowEqualSession(a: DeviceSessionDetail | null, b: DeviceSessionDetail): boolean {
  if (!a) return false;
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  for (const key of keys) {
    const left = (a as Record<string, unknown>)[key];
    const right = (b as Record<string, unknown>)[key];
    if (left === right) continue;
    if (
      left &&
      right &&
      typeof left === 'object' &&
      typeof right === 'object' &&
      JSON.stringify(left) === JSON.stringify(right)
    ) {
      continue;
    }
    return false;
  }
  return true;
}

/** One screenful plus context; older pages are fetched on demand. */
const HISTORY_PAGE = 30;

/**
 * After a long agent turn the newest 30 rows can hold nothing but tool calls,
 * which used to make the chat open on a single anonymous tool group that
 * looked like "all my messages are gone". Keep paging back until the window
 * contains at least one user message (bounded so a transcript with none —
 * impossible today — cannot loop).
 */
const INITIAL_MAX_PAGES = 6;

const OFFLINE_CACHE_NOTICE = '设备离线，显示服务端缓存的消息';

const FAILURE_TEXT: Record<string, string> = {
  busy: '该会话已有一个启动请求在进行中',
  config_dir_mismatch: '配置目录不匹配，无法打开终端',
  session_not_found: '找不到该会话的记录文件',
  cwd_mismatch: '会话工作目录不在允许范围内',
  spawn_failed: '终端启动失败',
  bridge_timeout: '终端已打开，但 90 秒内没有连上远程控制',
  process_exited: '终端在连接远程控制之前退出了',
};

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
  deviceName,
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
  /** True until the first history page has been painted (never an empty flash). */
  const [historyLoading, setHistoryLoading] = useState(true);
  const [loadingEarlier, setLoadingEarlier] = useState(false);
  const [blockedSession, setBlockedSession] = useState(false);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [confirmTerminalAction, setConfirmTerminalAction] = useState<'restart' | 'close' | null>(null);
  const [terminalActionPending, setTerminalActionPending] = useState(false);
  const terminalActionInFlight = useRef(false);
  /** Assistant history rows with billed usage; seeds the live token ledger. */
  const [usageSeed, setUsageSeed] = useState<{ id: string; usage: TranscriptUsage }[]>([]);
  const loadedKey = useRef('');
  // The 10s poll's closure captures operationId from mount (null); a plain
  // state read made the poll decide "no operation" forever and flip the
  // blocked card while a terminal was legitimately being opened.
  const operationIdRef = useRef<string | null>(null);
  // Suppresses the poll's mount-time tick while the initial detail+history
  // load is still on the wire (it duplicated both requests).
  const initialLoadDoneRef = useRef(false);
  useEffect(() => {
    operationIdRef.current = operationId;
  }, [operationId]);
  // Each caller waits its turn and receives its own result. A single queued
  // message ref overwrote earlier sends and silently lost them on a slow link.
  const sendQueue = useRef(createSerialQueue());

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
      // Assistant rows that carry billed usage seed the live ledger, so the
      // token/cache-rate row matches the terminal immediately after a reload.
      const usageRows = page.messages.filter(message => message.role === 'assistant' && message.usage) as {
        id: string;
        usage: TranscriptUsage;
      }[];
      if (usageRows.length > 0) {
        setUsageSeed(previous => {
          const byId = new Map(previous.map(row => [row.id, row]));
          for (const row of usageRows) byId.set(row.id, row);
          return [...byId.values()];
        });
      }
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
        setNotice(previous => previous || OFFLINE_CACHE_NOTICE);
      }
      return page;
    },
    [deviceId, localSessionId],
  );

  /**
   * First load: page back until the window shows a user message so the chat
   * never opens on a wall of tool cards.
   */
  const loadInitialHistory = useCallback(async () => {
    let page = await loadHistory();
    let combined = transcriptToEntries(page.messages);
    let pages = 1;
    while (
      page.hasMore &&
      page.before &&
      !combined.some(entry => entry.type === 'user_message') &&
      pages < INITIAL_MAX_PAGES
    ) {
      page = await loadHistory(page.before);
      combined = [...transcriptToEntries(page.messages), ...combined];
      pages += 1;
    }
  }, [loadHistory]);

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
      // Recovery: a background sync that succeeds means history is reachable
      // again — a stale load error would otherwise stick above a transcript
      // that visibly recovered. Same for the offline notice once the server
      // stops serving its cache.
      setHistoryError('');
      if (!page.cached && !page.warning) {
        setNotice(previous => (previous === OFFLINE_CACHE_NOTICE ? '' : previous));
      }
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
    setNotice('');
    setLoadingDetail(true);
    try {
      try {
        await loadDetail();
      } catch (err) {
        setError(err instanceof Error ? err.message : '加载会话失败');
      }
      // History comes from the server cache, so a detail failure must not
      // hide a transcript that is still readable.
      await loadInitialHistory();
    } catch (err) {
      setHistoryError(err instanceof Error ? err.message : '加载历史失败');
    } finally {
      setLoadingDetail(false);
    }
  }, [loadDetail, loadInitialHistory]);

  useEffect(() => {
    const key = `${deviceId}:${localSessionId}`;
    if (loadedKey.current === key) return;
    loadedKey.current = key;
    initialLoadDoneRef.current = false;
    setDetail(null);
    setEntries([]);
    setUsageSeed([]);
    setHistoryCursor(undefined);
    setHistoryHasMore(false);
    setHistoryError('');
    setOperation(null);
    setOperationId(null);
    setForcedBridgeId(null);
    setBlockedSession(false);
    setNotice('');
    setError('');
    setLoadingDetail(true);
    setHistoryLoading(true);

    void (async () => {
      try {
        try {
          const next = await loadDetail();
          // The poll's mount-time tick used to detect this; it is skipped
          // while the initial load runs, so evaluate it here instead.
          if (!next.operation && next.runtime?.processAlive && !next.bridgeSessionId) {
            setBlockedSession(true);
          }
        } catch (err) {
          setError(err instanceof Error ? err.message : '加载会话失败');
        }
        // Always load: the server has a durable conversation cache, so the
        // terminal being closed (or the machine offline) no longer blanks out
        // the conversation.
        await loadInitialHistory();
      } catch (err) {
        setHistoryError(err instanceof Error ? err.message : '加载历史失败');
      } finally {
        initialLoadDoneRef.current = true;
        setLoadingDetail(false);
        setHistoryLoading(false);
      }
    })();
  }, [deviceId, localSessionId, loadDetail, loadInitialHistory]);

  useEffect(() => {
    if (!operationId) return;
    let cancelled = false;
    let failures = 0;
    let running = false;
    // The spawner gives up after 90s; if its terminal event never arrives
    // (restart, dropped SSE, spawner offline) the spinner used to spin
    // forever. Give the client its own deadline and tell the user they can
    // retry.
    let startedAt = Date.now();
    const tick = async () => {
      // A slow request used to overlap the next tick; one hung request also
      // ended the loop for good. Serialise ticks and give up only after a
      // run of consecutive failures.
      if (running) return;
      running = true;
      try {
        if (Date.now() - startedAt > 100_000) {
          setOperationId(null);
          setError('等待终端就绪超时。可以点“重试”再打开一次，或检查电脑上的终端是否停在确认提示。');
          return;
        }
        const next = await fetchOperation(operationId);
        failures = 0;
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
          // A terminal that is running without `/remote-control` cannot be
          // reused. That is a known state with its own inline explanation and
          // restart action — surfacing it as a red error banner too showed the
          // same sentence twice and made a normal situation look broken.
          if (next.failureCode === 'already_running_without_bridge') {
            setBlockedSession(true);
            setError('');
          } else {
            setError(next.detail || FAILURE_TEXT[next.failureCode ?? ''] || '终端启动失败');
          }
        } else if (next.state === 'waiting_for_user') {
          // Keep polling, but surface the actionable instruction instead of a
          // generic spinner (the terminal is waiting on the PC). A human
          // staring at a confirmation prompt is not a stuck spawner: restart
          // the deadline so the 100s budget only measures actual progress.
          startedAt = Date.now();
          setOperation(next);
        }
      } catch (err) {
        if (cancelled) return;
        failures += 1;
        if (failures < 5) return;
        setOperationId(null);
        setError(err instanceof Error ? err.message : '无法读取启动进度');
      } finally {
        running = false;
      }
    };
    void tick();
    const timer = setInterval(() => void tick(), 1500);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [operationId, loadDetail, syncHistory]);

  const bridgeId = forcedBridgeId;

  // Tool cards can ask for permission while the terminal is live but the view
  // is showing cached history. Route the answer through the bridge control
  // channel; without a live bridge there is nothing to answer.
  const respondToolPermission = useCallback(
    (requestId: string, optionId: string | null, optionKind: string | null) => {
      if (!forcedBridgeId) {
        setNotice('终端未连接，无法在这里处理权限请求，请发送一条消息重新打开终端。');
        return;
      }
      const approved = !optionId || (optionKind !== 'reject_once' && optionKind !== 'reject_always');
      void apiSendControl(forcedBridgeId, {
        type: 'permission_response',
        approved,
        request_id: requestId,
        ...(optionId ? { option_id: optionId } : {}),
      }).catch(err => {
        setError(err instanceof Error ? err.message : '处理权限请求失败');
      });
    },
    [forcedBridgeId],
  );

  const forceRestart = useCallback(async () => {
    setError('');
    setNotice('正在重启终端并启用远程控制…');
    try {
      const result = await openSession(deviceId, localSessionId, true);
      if (result.operationId) {
        setBlockedSession(false);
        setOperationId(result.operationId);
      } else if (result.bridgeSessionId) {
        setBlockedSession(false);
        setForcedBridgeId(result.bridgeSessionId);
        await loadDetail();
      } else {
        setNotice('');
        setError(result.detail || '无法启动终端，请重试');
      }
    } catch (err) {
      setNotice('');
      setError(err instanceof Error ? err.message : '重启终端失败');
    }
  }, [deviceId, localSessionId, loadDetail]);

  const send = useCallback(
    async (message: ChatInputMessage, transitionalNotice?: string) => {
      const text = message.text.trim();
      const images = message.images;
      if (!text && (!images || images.length === 0)) return;
      return sendQueue.current(async () => {
        setSending(true);
        setError('');
        // React batches the caller's setNotice with this one; only clobber the
        // notice when no transitional message was handed in, or the
        // "正在重新打开…" hint could never paint.
        setNotice(transitionalNotice ?? '');
        const pendingId = `pending-${Date.now()}`;
        // pending: the muted bubble + 排队中 label — a queued/offline send must
        // not look delivered. The history echo adopts this row by content match.
        setEntries(previous => [
          ...previous,
          {
            type: 'user_message',
            id: pendingId,
            content: text,
            images: images && images.length > 0 ? images : undefined,
            pending: true,
          },
        ]);
        try {
          const result = await sendDeviceMessage(
            deviceId,
            localSessionId,
            text,
            images && images.length > 0 ? images : undefined,
          );
          if (result.bridgeSessionId) {
            setForcedBridgeId(result.bridgeSessionId);
            // The message has already been accepted. A subsequent status refresh
            // must not be treated as a failed send, remove its bubble, or put
            // the draft back where the user could send the same prompt again.
            try {
              await loadDetail();
            } catch {
              setNotice('消息已提交，正在重新同步终端状态。');
            }
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
          // The composer puts the text back, so a failed send is never silent.
          throw err;
        } finally {
          setSending(false);
        }
      });
    },
    [deviceId, localSessionId, loadDetail],
  );

  // Always keep the session state fresh, not only while the live view is up.
  // This is what re-enters the bridge after an RCS restart or a transient SSE
  // failure, and what updates device/terminal state in the transcript view.
  useEffect(() => {
    let cancelled = false;
    let running = false;
    const tick = async () => {
      // The initial load already fetched detail and history; a mount-time tick
      // on top of it tripled cold-open latency and raced the history pager.
      // Checked before `running` is armed — an early return below would leave
      // it stuck true and kill the poll for good.
      if (!initialLoadDoneRef.current) return;
      // Skip while the previous tick is still in flight: a slow request used
      // to stack up on a flaky network.
      if (running) return;
      running = true;
      // Nothing to report while the app is in the background; the visibility
      // handler below catches up as soon as it returns.
      if (document.hidden) {
        running = false;
        return;
      }
      try {
        const next = await fetchSession(deviceId, localSessionId);
        if (cancelled) return;
        setDetail(previous => (shallowEqualSession(previous, next) ? previous : next));
        if (next.bridgeConnected && next.bridgeSessionId) {
          // The user ran /remote-control in the terminal — the blocked card
          // and disabled composer must go away on their own.
          setBlockedSession(false);
          if (forcedBridgeId !== next.bridgeSessionId) {
            // Entering the live view after a reconnect: catch up on anything
            // produced while the SSE was down before switching.
            void syncHistory();
            setForcedBridgeId(next.bridgeSessionId);
          }
          // While the bridge is live the SSE is the source of truth; re-reading
          // history every 10s just re-parsed and re-merged the transcript.
          return;
        }
        // Running in a terminal with no bridge at all: sending cannot reach it.
        // Say so up front instead of letting the first message queue into the
        // void and only then explaining why. Read the ref, not the closure:
        // this effect is not re-created when an operation starts, and a stale
        // null showed the blocked card while a terminal was being opened.
        if (!operationIdRef.current && next.runtime?.processAlive && !next.bridgeSessionId) {
          setBlockedSession(true);
        } else if (next.runtime && next.runtime.processAlive === false) {
          // The terminal exited; a send will open a fresh bridged one.
          setBlockedSession(false);
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
      } finally {
        running = false;
      }
    };
    const timer = setInterval(() => void tick(), 10_000);
    const onVisible = () => {
      if (!document.hidden) void tick();
    };
    document.addEventListener('visibilitychange', onVisible);
    void tick();
    return () => {
      cancelled = true;
      clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [forcedBridgeId, deviceId, localSessionId, syncHistory]);

  /**
   * Stop the terminal on the PC. Destructive: the user confirms here, and the
   * spawner independently re-verifies the PID before killing it. The transcript
   * stays; sending a message will open a fresh terminal again.
   */
  const closeTerminal = useCallback(async () => {
    setNotice('正在关闭终端…');
    try {
      const result = await closeSession(deviceId, localSessionId);
      if (result.closed) {
        setForcedBridgeId(null);
        setNotice(result.detail || '终端已关闭，发送消息会重新打开终端。');
        void loadDetail();
      } else {
        setNotice('');
        setError(result.detail || '无法关闭终端');
      }
    } catch (err) {
      setNotice('');
      setError(err instanceof Error ? err.message : '关闭终端失败');
    }
  }, [deviceId, localSessionId, loadDetail]);

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

  const terminalStatus = !detail
    ? '状态加载中'
    : !detail.online
      ? '设备离线'
      : bridgeId
        ? '终端已连接'
        : operation?.state === 'waiting_for_user'
          ? '等待电脑确认'
          : operationId
            ? '终端启动中'
            : detail.runtime?.processAlive === false
              ? '终端未运行'
              : '终端未连接';

  const terminalConfirm = (
    <ModalSheet
      open={confirmTerminalAction !== null}
      onClose={() => {
        if (!terminalActionPending) setConfirmTerminalAction(null);
      }}
      label={confirmTerminalAction === 'restart' ? '重启终端' : '关闭终端'}
      className="rounded-t-[20px] border border-border bg-surface-0 p-5 pb-[calc(1.25rem+env(safe-area-inset-bottom))] shadow-2xl sm:max-w-md sm:rounded-2xl sm:pb-5"
    >
      <h2 aria-hidden="true" className="text-base font-semibold text-text-primary">
        {confirmTerminalAction === 'restart' ? '重启电脑上的终端？' : '关闭电脑上的终端？'}
      </h2>
      <p className="mt-2 text-sm leading-relaxed text-text-secondary">
        正在执行的任务会中断，会话记录会保留。
        {confirmTerminalAction === 'restart' && ' 重启后会尝试开启远程控制。'}
      </p>
      <div className="mt-5 flex gap-2">
        <button
          type="button"
          disabled={terminalActionPending}
          onClick={() => setConfirmTerminalAction(null)}
          className="h-11 flex-1 rounded-xl border border-border bg-surface-1 text-sm text-text-primary disabled:opacity-50"
        >
          取消
        </button>
        <button
          type="button"
          disabled={terminalActionPending}
          onClick={() => {
            if (terminalActionInFlight.current) return;
            terminalActionInFlight.current = true;
            setTerminalActionPending(true);
            void (confirmTerminalAction === 'restart' ? forceRestart() : closeTerminal()).finally(() => {
              terminalActionInFlight.current = false;
              setTerminalActionPending(false);
              setConfirmTerminalAction(null);
            });
          }}
          className="h-11 flex-1 rounded-xl bg-destructive px-4 text-sm font-medium text-white disabled:opacity-50"
        >
          {terminalActionPending ? '处理中…' : confirmTerminalAction === 'restart' ? '重启终端' : '关闭终端'}
        </button>
      </div>
    </ModalSheet>
  );

  if (bridgeId) {
    return (
      <>
        <BridgeChat
          key={bridgeId}
          sessionId={bridgeId}
          entries={entries}
          onEntriesChange={setEntries}
          onBridgeReady={() => void syncHistory()}
          onBridgeDisconnected={() => leaveBridge('终端已关闭，发送消息会重新打开终端。')}
          onTerminalUnavailable={async text => {
            await send({ text }, '终端已关闭，正在重新打开…');
            setForcedBridgeId(current => (current === bridgeId ? null : current));
          }}
          onCloseTerminal={detail?.bridgeConnected ? () => setConfirmTerminalAction('close') : undefined}
          deviceId={deviceId}
          cwd={detail?.session.cwd}
          hasOlder={historyHasMore}
          loadingOlder={loadingEarlier}
          onLoadOlder={() => void loadEarlier()}
          usageSeed={usageSeed}
          draftKey={`device:${deviceId}:${localSessionId}`}
          onBack={onBack}
          header={
            <>
              <MobileChatHeader
                title={detail?.session.title || localSessionId}
                subtitle={detail?.session.cwd}
                deviceName={deviceName}
                online={detail?.online}
                terminalStatus={terminalStatus}
                onBack={onBack}
              />
              <DeliveryReviewNotice
                count={detail?.deliveryReviewRequired ?? 0}
                deviceId={deviceId}
                localSessionId={localSessionId}
                onReviewed={() => setDetail(current => (current ? { ...current, deliveryReviewRequired: 0 } : current))}
              />
            </>
          }
        />
        {terminalConfirm}
      </>
    );
  }

  const starting = Boolean(operationId);
  const statusText = starting
    ? operation?.state === 'waiting_for_user'
      ? operation.detail || '终端已打开，等待你在电脑上确认（信任目录、权限或模型配置）。'
      : '正在电脑上打开终端…'
    : historyError || notice;

  return (
    <TooltipProvider>
      <div className="flex min-h-0 flex-1 flex-col overflow-hidden rcs-page-enter">
        <MobileChatHeader
          title={detail?.session.title || localSessionId}
          subtitle={detail?.session.cwd}
          deviceName={deviceName}
          online={detail?.online}
          terminalStatus={terminalStatus}
          onBack={onBack}
        />
        <DeliveryReviewNotice
          count={detail?.deliveryReviewRequired ?? 0}
          deviceId={deviceId}
          localSessionId={localSessionId}
          onReviewed={() => setDetail(current => (current ? { ...current, deliveryReviewRequired: 0 } : current))}
        />
        {error && (
          <div className="mx-auto w-full px-4 pt-2">
            <div
              role="alert"
              className="flex items-start gap-3 rounded-xl border border-destructive/30 bg-destructive/10 px-3 py-2 text-[13px] text-destructive"
            >
              <span className="min-w-0 flex-1 leading-relaxed">{error}</span>
              <button
                type="button"
                onClick={() => void reload()}
                className="shrink-0 text-xs font-medium text-brand-dark hover:underline dark:text-brand"
              >
                刷新状态
              </button>
              <button
                type="button"
                onClick={() => setError('')}
                aria-label="关闭提示"
                className="-mr-1 -mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-destructive/70 active:bg-destructive/10"
              >
                ×
              </button>
            </div>
          </div>
        )}
        {/* "Starting the terminal" is not Claude typing: the dots made it look
            like a reply was already streaming. The status row explains it. */}
        <ChatView
          entries={entries}
          onPermissionRespond={respondToolPermission}
          isHistoryLoading={historyLoading}
          hasOlder={historyHasMore}
          loadingOlder={loadingEarlier}
          onLoadOlder={() => void loadEarlier()}
          emptyTitle={loadingDetail || historyLoading ? '正在加载会话…' : '暂无消息'}
          emptyDescription={
            loadingDetail || historyLoading
              ? ''
              : detail?.online
                ? '发送消息后会自动打开终端'
                : '设备离线，发送的消息会在上线后发出'
          }
        />
        {blockedSession && (
          <div className="mx-auto w-full px-4 pt-2">
            <div className="rounded-xl border border-border bg-surface-1 p-3">
              <p className="text-[13px] font-medium text-text-primary">
                这个会话正在电脑的终端里运行，但没有开启远程控制
              </p>
              <p className="mt-1 text-[12px] leading-relaxed text-text-muted">
                在电脑那个终端里输入{' '}
                <code className="rounded bg-surface-2 px-1 py-0.5 font-mono text-[11px]">/remote-control</code>{' '}
                就能把这里连上（推荐，不打断它）；或者让这里重启终端并自动开启，终端里正在跑的任务会中断。
              </p>
              <div className="mt-2 flex items-center gap-3">
                <button
                  type="button"
                  onClick={() => setConfirmTerminalAction('restart')}
                  className="rounded-lg bg-brand px-3 py-1.5 text-xs font-medium text-white active:scale-[0.98]"
                >
                  重启终端并启用
                </button>
                <span className="text-[11px] text-text-muted">在终端执行后，这里会自动连上</span>
              </div>
            </div>
          </div>
        )}
        {statusText && (
          <div
            role="status"
            aria-live="polite"
            className="mx-auto w-full px-4 pt-1 text-[13px] leading-snug text-text-secondary"
          >
            {statusText}
          </div>
        )}
        <ChatInput
          key={localSessionId}
          draftKey={`device:${deviceId}:${localSessionId}`}
          onSubmit={send}
          isLoading={sending}
          disabled={loadingDetail || blockedSession}
          supportsImages
          deviceId={deviceId}
          cwd={detail?.session.cwd}
          placeholder={
            loadingDetail
              ? '正在加载…'
              : blockedSession
                ? '终端未开启远程控制，暂时无法发送'
                : detail?.online
                  ? '输入消息，发送后连接终端…'
                  : '设备离线，消息会在上线后发送'
          }
        />
        {terminalConfirm}
      </div>
    </TooltipProvider>
  );
}

function DeliveryReviewNotice({
  count,
  deviceId,
  localSessionId,
  onReviewed,
}: {
  count: number;
  deviceId: string;
  localSessionId: string;
  onReviewed: () => void;
}) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  if (!count) return null;
  return (
    <div
      role="alert"
      className="border-b border-warning-border/30 bg-warning-bg/50 px-4 py-2 text-sm text-warning-text"
    >
      <span>服务重启前有 {count} 条消息的发送状态无法确认，请检查终端记录后再决定是否重新发送。</span>
      <button
        type="button"
        className="ml-3 font-medium underline disabled:opacity-50"
        disabled={pending}
        onClick={() => {
          setPending(true);
          setError('');
          void reviewUnconfirmedDelivery(deviceId, localSessionId)
            .then(onReviewed)
            .catch(() => setError('保存核对状态失败，请重试'))
            .finally(() => setPending(false));
        }}
      >
        已核对
      </button>
      {error && <span className="ml-2">{error}</span>}
    </div>
  );
}

/** Session context on both layouts, with a back button on phones. */
function MobileChatHeader({
  title,
  subtitle,
  deviceName,
  online,
  terminalStatus,
  onBack,
}: {
  title: string;
  subtitle?: string;
  deviceName?: string;
  online?: boolean;
  terminalStatus: string;
  onBack: () => void;
}) {
  return (
    <div className="flex h-14 shrink-0 items-center gap-0.5 border-b border-border/70 bg-surface-0 px-1.5 md:px-6">
      <button
        type="button"
        onClick={onBack}
        className="session-back flex h-11 w-11 shrink-0 items-center justify-center rounded-full text-text-secondary transition-colors active:bg-surface-1 md:hidden"
        aria-label="返回列表"
      >
        <ChevronLeft className="h-5 w-5" />
      </button>
      <div className="min-w-0 flex-1">
        <h2 className="truncate text-[15px] font-medium leading-tight text-text-primary">{title}</h2>
        {(deviceName || subtitle || terminalStatus) && (
          <p className="mt-0.5 flex items-center gap-1.5 text-[11px] leading-tight text-text-muted">
            <span
              aria-hidden="true"
              className={cn('h-1.5 w-1.5 shrink-0 rounded-full', online ? 'bg-status-success' : 'bg-text-muted/40')}
            />
            <span className="min-w-0 truncate" title={[deviceName, subtitle].filter(Boolean).join(' · ')}>
              {[deviceName, subtitle].filter(Boolean).join(' · ')}
            </span>
            {online !== undefined && <span className="sr-only">{online ? '设备在线' : '设备离线'}</span>}
            <span aria-hidden="true" className="shrink-0 text-text-secondary">
              · {terminalStatus}
            </span>
            <span className="sr-only">{terminalStatus}</span>
          </p>
        )}
      </div>
    </div>
  );
}
