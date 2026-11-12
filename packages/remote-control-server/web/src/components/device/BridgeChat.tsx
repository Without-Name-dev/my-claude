import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type Dispatch,
  type ReactNode,
  type SetStateAction,
} from 'react';
import { RCSChatAdapter } from '../../lib/rcs-chat-adapter';
import type { BridgeSessionInfo, SessionUsage } from '../../lib/rcs-chat-adapter';
import { mergeThreadEntries } from '../../lib/device-history';
import type { PendingPermission, ThreadEntry } from '../../lib/types';
import type { ChatInputMessage } from '../../lib/types';
import { apiSendControl } from '../../api/client';
import { ChatView } from '../../../components/chat/ChatView';
import { ChatInput } from '../../../components/chat/ChatInput';
import { ComposerSettings } from './ComposerSettings';
import { TooltipProvider } from '../../../components/ui/tooltip';
import { cn } from '../../lib/utils';
import { haptic } from '../../lib/view-transition';
import { turnFinished } from '../../lib/turn-notice';
import { SlidersHorizontal } from 'lucide-react';
import { PermissionPromptView, AskUserPanelView, PlanPanelView } from '../../components/PermissionViews';
import type { Question } from '../../types';
import type { AvailableCommand } from '../../acp/types';

const PERMISSION_MODES = [
  { id: 'default', label: '默认' },
  { id: 'acceptEdits', label: '自动接受编辑' },
  { id: 'plan', label: '计划模式' },
  { id: 'auto', label: '自动' },
];

/**
 * Live chat for a running bridge session. Same adapter and components as the
 * legacy SessionDetail page, plus model/permission pickers and slash commands
 * fed by the CLI's system/init message.
 */
export function BridgeChat({
  sessionId,
  onBack,
  header,
  initialEntries,
  entries: controlledEntries,
  onEntriesChange,
  onTerminalUnavailable,
  onBridgeDisconnected,
  onBridgeReady,
  toolbarExtraLeft,
  onCloseTerminal,
  deviceId,
  cwd,
}: {
  sessionId: string;
  onBack?: () => void;
  header?: ReactNode;
  /** Server-cached transcript, so switching to the live view keeps history. */
  initialEntries?: ThreadEntry[];
  /**
   * Controlled conversation. The device view owns the entries and passes them
   * in, so connecting/disconnecting a terminal never reloads the transcript.
   */
  entries?: ThreadEntry[];
  onEntriesChange?: Dispatch<SetStateAction<ThreadEntry[]>>;
  onTerminalUnavailable?: (text: string) => void;
  onBridgeDisconnected?: () => void;
  /** The SSE stream is open; catch up on anything produced before it. */
  onBridgeReady?: () => void;
  /** Extra controls at the far left of the composer (e.g. close-terminal). */
  toolbarExtraLeft?: ReactNode;
  /** Close the PC terminal; offered inside the settings sheet. */
  onCloseTerminal?: () => void;
  /** Enables attaching files from this phone or from the PC. */
  deviceId?: string;
  /** The terminal's working directory; attachments land inside it. */
  cwd?: string;
}) {
  const [internalEntries, setInternalEntries] = useState<ThreadEntry[]>(() => initialEntries ?? []);
  const entries = controlledEntries ?? internalEntries;
  const setEntries = onEntriesChange ?? setInternalEntries;
  const controlled = controlledEntries !== undefined;
  const [isLoading, setIsLoading] = useState(false);
  const [permissions, setPermissions] = useState<PendingPermission[]>([]);
  // Request ids whose answer is on the wire; the card shows "处理中…".
  const [answering, setAnswering] = useState<Set<string>>(() => new Set());
  const withAnswering = useCallback(async (requestId: string, action: () => Promise<void>) => {
    setAnswering(previous => new Set(previous).add(requestId));
    try {
      await action();
    } finally {
      setAnswering(previous => {
        const next = new Set(previous);
        next.delete(requestId);
        return next;
      });
    }
  }, []);
  const [error, setError] = useState('');
  const [sessionInfo, setSessionInfo] = useState<BridgeSessionInfo | null>(null);
  const [modelId, setModelId] = useState('');
  const [permissionMode, setPermissionMode] = useState('');
  const [effort, setEffort] = useState('auto');
  const [ultracode, setUltracode] = useState(false);
  const [ultracodeUsable, setUltracodeUsable] = useState(false);
  const [usage, setUsage] = useState<SessionUsage | null>(null);
  const [controlError, setControlError] = useState('');
  /** False until the bridge handshake (init) returns; sending is held until then. */
  const [ready, setReady] = useState(false);
  const [controlPending, setControlPending] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  /** Single-flight for the actual send; duplicate taps must not run a prompt twice. */
  const sendInFlight = useRef(false);
  const [connected, setConnected] = useState(true);
  const adapterRef = useRef<RCSChatAdapter | null>(null);
  const disconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Parent callbacks must not be memo dependencies: Workspace re-renders every
  // 10s (device refresh) and used to rebuild the adapter, dropping the SSE.
  const onBridgeDisconnectedRef = useRef(onBridgeDisconnected);
  useEffect(() => {
    onBridgeDisconnectedRef.current = onBridgeDisconnected;
  }, [onBridgeDisconnected]);
  const onBridgeReadyRef = useRef(onBridgeReady);
  useEffect(() => {
    onBridgeReadyRef.current = onBridgeReady;
  }, [onBridgeReady]);
  const userChangedModel = useRef(false);
  const userChangedMode = useRef(false);
  /**
   * Rollbacks keyed by control request id. Keying by id (instead of "whatever
   * is pending") means two controls in flight can no longer roll back each
   * other, which used to revert the model when a permission answer landed.
   */
  const pendingControls = useRef(new Map<string, () => void>());
  const controlTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  /** When the current turn began, so a finished long turn can ping quietly. */
  const turnStartedAt = useRef(0);

  useEffect(() => {
    if (isLoading && !turnStartedAt.current) turnStartedAt.current = Date.now();
  }, [isLoading]);

  /**
   * A CLI build that ignores a control request must not leave the pickers
   * disabled forever — and the optimistic value in the <select> must roll
   * back, or the user believes a switch happened that never did.
   */
  const armControlTimeout = useCallback(() => {
    if (controlTimer.current) clearTimeout(controlTimer.current);
    controlTimer.current = setTimeout(() => {
      controlTimer.current = null;
      setControlPending(false);
      const rollbacks = [...pendingControls.current.values()];
      pendingControls.current.clear();
      for (const rollback of rollbacks) rollback();
      if (rollbacks.length > 0) setControlError('切换超时，已保持原来的设置');
    }, 8000);
  }, []);

  useEffect(() => {
    return () => {
      if (controlTimer.current) clearTimeout(controlTimer.current);
    };
  }, []);

  const adapter = useMemo(
    () =>
      new RCSChatAdapter(sessionId, setEntries, {
        onError: message => setError(message),
        // Answering on the PC (or another device) resolves the request there;
        // the prompt must not linger on this phone.
        onPermissionResolved: requestId => {
          setPermissions(previous => previous.filter(item => item.requestId !== requestId));
        },
        onPermissionRequest: permission => {
          setPermissions(previous =>
            previous.some(item => item.requestId === permission.requestId) ? previous : [...previous, permission],
          );
        },
        onSessionInfo: info => {
          setSessionInfo(info);
          // A replayed system/init must not undo a model the user just chose.
          if (info.model && !userChangedModel.current) setModelId(info.model);
          if (info.permissionMode && !userChangedMode.current) setPermissionMode(info.permissionMode);
          // Effort/ultracode are republished by the CLI whenever they change —
          // including changes made in the terminal — so these are authoritative.
          if (typeof info.effort === 'string' && info.effort) setEffort(info.effort);
          if (typeof info.ultracodeAvailable === 'boolean') setUltracodeUsable(info.ultracodeAvailable);
          if (typeof info.ultracodeActive === 'boolean') setUltracode(info.ultracodeActive);
        },
        onControlResponse: response => {
          if (response.requestId) {
            const rollback = pendingControls.current.get(response.requestId);
            pendingControls.current.delete(response.requestId);
            if (response.error) rollback?.();
          }
          if (pendingControls.current.size === 0) {
            if (controlTimer.current) {
              clearTimeout(controlTimer.current);
              controlTimer.current = null;
            }
            setControlPending(false);
          }
          setControlError(response.error ?? '');
        },
        onBridgeState: next => {
          setConnected(next);
          // A disconnect is not a finished turn; do not let the elapsed time
          // accumulate into one.
          if (!next) turnStartedAt.current = 0;
          if (!next) setIsLoading(false);
          if (disconnectTimer.current) {
            clearTimeout(disconnectTimer.current);
            disconnectTimer.current = null;
          }
          if (!next) {
            // Give a transient reconnect a moment before switching the view.
            disconnectTimer.current = setTimeout(() => {
              disconnectTimer.current = null;
              onBridgeDisconnectedRef.current?.();
            }, 1500);
          }
        },
        onTurnComplete: () => {
          // Only a real turn end pings: a bridge disconnect also clears the
          // spinner, but that is not something to celebrate.
          const startedAt = turnStartedAt.current;
          turnStartedAt.current = 0;
          if (startedAt) turnFinished(Date.now() - startedAt);
          setIsLoading(false);
        },
        onUsage: next => setUsage(next),
        // The CLI reports the authoritative turn state. Without this, a turn
        // started in the terminal looked idle in the browser, and the first
        // streamed assistant chunk cleared the spinner prematurely.
        onStatusChange: status => {
          setIsLoading(status === 'running' || status === 'requires_action');
        },
        onStreamOpen: controlled ? () => onBridgeReadyRef.current?.() : undefined,
        // The controlled (device) view already has the authoritative
        // transcript. Re-reading bridge history there is what made the chat
        // flash/reload on every connect; only its init metadata is needed.
        historyMode: controlled ? 'metadata' : 'full',
      }),
    [sessionId, setEntries, controlled],
  );

  useEffect(() => {
    return () => {
      if (disconnectTimer.current) clearTimeout(disconnectTimer.current);
    };
  }, []);

  // The device view may finish loading history after this component mounts
  // (it has to ask the server first). Merge that older transcript underneath
  // whatever the live bridge already rendered.
  useEffect(() => {
    if (controlled) return;
    if (!initialEntries || initialEntries.length === 0) return;
    setInternalEntries(previous => mergeThreadEntries(initialEntries, previous));
  }, [controlled, initialEntries]);

  useEffect(() => {
    adapterRef.current = adapter;
    setReady(false);
    void adapter
      .init()
      .catch(err => {
        setError(err instanceof Error ? err.message : '无法连接会话');
      })
      .finally(() => {
        // The banner above tells the user what went wrong; the composer stays
        // usable either way so typing is never silently swallowed.
        setReady(true);
      });
    return () => {
      adapter.disconnect();
      adapterRef.current = null;
    };
  }, [adapter]);

  const appendCommandOutput = useCallback(
    (text: string) => {
      const id = `command-${Date.now()}-${Math.random().toString(16).slice(2, 6)}`;
      setEntries(prev => [...prev, { type: 'command_output', id, text }]);
    },
    [setEntries],
  );

  const submit = useCallback(
    async (message: ChatInputMessage) => {
      const text = message.text.trim();
      if (!text) return;
      setError('');

      // /effort is a local-jsx command in the CLI, so it cannot be sent as a
      // chat turn. Route the useful text form through the bridge control
      // channel instead.
      const effortMatch = text.match(/^\/effort(?:\s+(.*))?$/i);
      if (effortMatch) {
        const arg = (effortMatch[1] ?? '').trim().toLowerCase();
        if (!arg || arg === 'status' || arg === 'current') {
          appendCommandOutput('effort 由终端会话管理。可用 /effort low|medium|high|xhigh|max|auto 设置。');
          return;
        }
        const levels = ['low', 'medium', 'high', 'xhigh', 'max', 'auto'];
        if (!levels.includes(arg)) {
          appendCommandOutput('用法：/effort low|medium|high|xhigh|max|auto');
          return;
        }
        if (!connected) {
          throw new Error('终端已关闭，请重新打开终端');
        }
        setControlPending(true);
        setControlError('');
        try {
          await adapter.setEffort(arg);
          appendCommandOutput(`已设置 effort：${arg}`);
        } catch (err) {
          const message = err instanceof Error ? err.message : '切换 effort 失败';
          setControlError(message);
          throw new Error(message);
        } finally {
          setControlPending(false);
        }
        return;
      }
      // `connected` is maintained by the server's bridge_state SSE event, so
      // sending costs no extra request. If the terminal is gone, hand the
      // text to the parent, which queues it and reopens the terminal.
      if (!connected) {
        if (onTerminalUnavailable) onTerminalUnavailable(text);
        else throw new Error('终端已关闭，请重新打开终端');
        return;
      }
      if (!ready) {
        throw new Error('正在连接终端，请稍候…');
      }
      if (sendInFlight.current) return;
      sendInFlight.current = true;
      setIsLoading(true);
      try {
        await adapter.sendMessage(text, message.images);
      } catch (err) {
        setIsLoading(false);
        const message = err instanceof Error ? err.message : '发送失败';
        // The terminal vanished between the last bridge_state and this send:
        // hand it to the parent, which reopens the terminal and queues it.
        if (/terminal|终端/i.test(message) && onTerminalUnavailable) {
          onTerminalUnavailable(text);
          return;
        }
        setError(message);
        // Let the composer restore the draft the user just typed.
        throw err;
      } finally {
        sendInFlight.current = false;
      }
    },
    [adapter, appendCommandOutput, connected, onTerminalUnavailable, ready],
  );

  /** Send a control request and remember how to undo its optimistic value. */
  const runControl = useCallback(
    async (send: () => Promise<string>, rollback: () => void, failure: string) => {
      setControlError('');
      setControlPending(true);
      armControlTimeout();
      try {
        const requestId = await send();
        pendingControls.current.set(requestId, rollback);
      } catch (err) {
        if (controlTimer.current) {
          clearTimeout(controlTimer.current);
          controlTimer.current = null;
        }
        rollback();
        pendingControls.current.clear();
        setControlPending(false);
        setControlError(`${failure}：${err instanceof Error ? err.message : '未知原因'}`);
      }
    },
    [armControlTimeout],
  );

  const changeModel = useCallback(
    async (id: string) => {
      const previous = modelId;
      userChangedModel.current = true;
      setModelId(id);
      await runControl(
        () => adapter.setModel(id),
        () => setModelId(previous),
        '切换模型失败，已保持原模型',
      );
    },
    [adapter, modelId, runControl],
  );

  const changePermissionMode = useCallback(
    async (mode: string) => {
      const previous = permissionMode;
      userChangedMode.current = true;
      setPermissionMode(mode);
      await runControl(
        () => adapter.setPermissionMode(mode),
        () => setPermissionMode(previous),
        '切换权限模式失败，已保持原模式',
      );
    },
    [adapter, permissionMode, runControl],
  );

  const changeEffort = useCallback(
    async (value: string) => {
      const previous = effort;
      setEffort(value);
      await runControl(
        () => adapter.setEffort(value),
        () => setEffort(previous),
        '切换思考等级失败，已保持原等级',
      );
    },
    [adapter, effort, runControl],
  );

  const toggleUltracode = useCallback(async () => {
    const next = !ultracode;
    setUltracode(next);
    await runControl(
      () => adapter.setUltracode(next),
      () => setUltracode(!next),
      '切换 Ultracode 失败',
    );
  }, [adapter, runControl, ultracode]);

  const interrupt = useCallback(async () => {
    try {
      await adapter.interrupt();
      // Only claim idle once the interrupt really landed. The old `finally`
      // cleared the spinner after a failed POST, so the UI looked idle while
      // the turn was still running.
      setIsLoading(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : '中断失败');
    }
  }, [adapter]);

  const approve = useCallback(
    async (requestId: string) => {
      try {
        await adapter.respondPermission(requestId, true);
        setPermissions(prev => prev.filter(item => item.requestId !== requestId));
      } catch (err) {
        // Keep the prompt: the terminal is still waiting for an answer.
        setError(err instanceof Error ? err.message : '操作失败');
      }
    },
    [adapter],
  );

  const reject = useCallback(
    async (requestId: string) => {
      try {
        await adapter.respondPermission(requestId, false);
        setPermissions(prev => prev.filter(item => item.requestId !== requestId));
      } catch (err) {
        setError(err instanceof Error ? err.message : '操作失败');
      }
    },
    [adapter],
  );

  /** Answer with one of the CLI's own permission options (allow always…). */
  const respondOption = useCallback(
    async (requestId: string, optionId: string, kind: string) => {
      const rejected = kind === 'reject_once' || kind === 'reject_always';
      try {
        await adapter.respondPermission(requestId, !rejected, { option_id: optionId });
        setPermissions(prev => prev.filter(item => item.requestId !== requestId));
      } catch (err) {
        setError(err instanceof Error ? err.message : '操作失败');
      }
    },
    [adapter],
  );

  const submitAnswers = useCallback(
    async (requestId: string, answers: Record<string, unknown>, questions: Question[]) => {
      try {
        await apiSendControl(sessionId, {
          type: 'permission_response',
          approved: true,
          request_id: requestId,
          updated_input: { questions, answers },
        });
        setPermissions(prev => prev.filter(item => item.requestId !== requestId));
      } catch (err) {
        setError(err instanceof Error ? err.message : '操作失败');
      }
    },
    [sessionId],
  );

  // Tool cards in the transcript carry their own permission buttons. Without
  // a handler they rendered as dead controls; route them through the same
  // control channel as the cards below the composer.
  const respondFromCard = useCallback(
    (requestId: string, optionId: string | null, optionKind: string | null) => {
      const rejected = optionKind === 'reject_once' || optionKind === 'reject_always';
      const approved = !optionId || !rejected;
      void adapter.respondPermission(requestId, approved, optionId ? { option_id: optionId } : undefined).catch(err => {
        setError(err instanceof Error ? err.message : '处理权限请求失败');
      });
    },
    [adapter],
  );

  const submitPlan = useCallback(
    async (requestId: string, value: string, feedback?: string) => {
      try {
        if (value === 'no') {
          await apiSendControl(sessionId, {
            type: 'permission_response',
            approved: false,
            request_id: requestId,
            ...(feedback ? { message: feedback } : {}),
          });
        } else {
          const mode = value === 'yes-accept-edits' ? 'acceptEdits' : 'default';
          await apiSendControl(sessionId, {
            type: 'permission_response',
            approved: true,
            request_id: requestId,
            updated_permissions: [{ type: 'setMode', mode, destination: 'session' }],
          });
        }
        setPermissions(prev => prev.filter(item => item.requestId !== requestId));
      } catch (err) {
        setError(err instanceof Error ? err.message : '操作失败');
      }
    },
    [sessionId],
  );

  const commands: AvailableCommand[] = (sessionInfo?.commands ?? []).map(command => ({
    name: command.name,
    description: command.description || '命令',
  }));
  const models =
    sessionInfo && sessionInfo.models.length > 0 ? sessionInfo.models : modelId ? [{ id: modelId, name: modelId }] : [];
  const permissionOptions = PERMISSION_MODES.some(option => option.id === permissionMode)
    ? PERMISSION_MODES
    : permissionMode
      ? [...PERMISSION_MODES, { id: permissionMode, label: permissionMode }]
      : PERMISSION_MODES;
  const activeModelName = shortModelName(models.find(model => model.id === modelId)?.name ?? '');

  return (
    <TooltipProvider>
      <div className="flex min-h-0 flex-1 flex-col overflow-hidden rcs-page-enter">
        {header}
        {controlError && (
          <div
            role="alert"
            className="flex items-start gap-3 border-b bg-destructive/10 px-4 py-2 text-sm text-destructive"
          >
            <span className="min-w-0 flex-1">{controlError}</span>
            <button
              type="button"
              onClick={() => setControlError('')}
              aria-label="关闭提示"
              className="-mr-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-destructive/70 active:bg-destructive/10"
            >
              ×
            </button>
          </div>
        )}
        {error && (
          <div
            role="alert"
            className="flex items-start gap-3 border-b bg-destructive/10 px-4 py-2 text-sm text-destructive"
          >
            <span className="min-w-0 flex-1">{error}</span>
            <button
              type="button"
              onClick={() => setError('')}
              aria-label="关闭提示"
              className="-mr-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-destructive/70 active:bg-destructive/10"
            >
              ×
            </button>
          </div>
        )}
        <ChatView
          entries={entries}
          isLoading={isLoading}
          onPermissionRespond={respondFromCard}
          emptyTitle="开始对话"
          emptyDescription="输入消息继续这个会话"
        />
        {permissions.length > 0 && (
          /* A long AskUserQuestion/plan card used to squeeze the transcript and
             could push the composer off a phone screen; scroll it instead. */
          <div className="max-h-[50%] overflow-y-auto overscroll-contain border-t bg-surface-1 px-4 py-3">
            <div className="mx-auto max-w-3xl space-y-3">
              {permissions.map(request => (
                <PermissionEventView
                  key={request.requestId}
                  request={request}
                  onApprove={() => withAnswering(request.requestId, () => approve(request.requestId))}
                  onReject={() => withAnswering(request.requestId, () => reject(request.requestId))}
                  onRespondOption={(requestId, optionId, kind) =>
                    void withAnswering(requestId, () => respondOption(requestId, optionId, kind))
                  }
                  onSubmitAnswers={(requestId, answers, questions) =>
                    void withAnswering(requestId, () => submitAnswers(requestId, answers, questions))
                  }
                  onSubmitPlan={(requestId, value, feedback) =>
                    void withAnswering(requestId, () => submitPlan(requestId, value, feedback))
                  }
                />
              ))}
            </div>
          </div>
        )}
        {usage && usage.current && <ContextStats usage={usage} contextWindow={sessionInfo?.contextWindow} />}
        <ChatInput
          key={sessionId}
          draftKey={`bridge:${sessionId}`}
          onSubmit={submit}
          isLoading={isLoading}
          onInterrupt={interrupt}
          placeholder="和 Claude 一起工作"
          supportsImages
          deviceId={deviceId}
          cwd={cwd}
          commands={commands}
          toolbarLeft={
            <>
              {toolbarExtraLeft}
              {/* One settings entry point: the thinking level, Ultracode,
                  permission mode and model live in the settings sheet so the
                  composer row stays readable on a phone. */}
              <button
                type="button"
                onClick={() => setSettingsOpen(true)}
                disabled={controlPending}
                aria-label="会话设置"
                className="flex h-8 min-w-0 shrink items-center gap-1 rounded-full px-1.5 text-text-secondary transition-colors hover:bg-surface-1 hover:text-text-primary disabled:opacity-60 sm:h-9 sm:px-2"
              >
                <SlidersHorizontal className="h-3.5 w-3.5 shrink-0 sm:h-4 sm:w-4" />
                <span
                  className={cn(
                    'min-w-0 truncate text-[11px] sm:text-xs',
                    ultracode || (effort && effort !== 'auto') ? 'text-brand' : 'text-text-secondary',
                  )}
                >
                  {effort}
                  {ultracode ? ' · Ultra' : ''}
                  {activeModelName && <span className="hidden sm:inline">{` · ${activeModelName}`}</span>}
                </span>
              </button>
            </>
          }
        />
        <ComposerSettings
          open={settingsOpen}
          onClose={() => setSettingsOpen(false)}
          pending={controlPending}
          controlError={controlError}
          effort={effort}
          ultracode={ultracode}
          ultracodeUsable={ultracodeUsable}
          permissionMode={permissionMode}
          permissionOptions={permissionOptions}
          modelId={modelId}
          models={models.map(model => ({
            id: model.id,
            label: shortModelName(model.name),
            title: model.name,
          }))}
          onCloseTerminal={onCloseTerminal}
          onChangeEffort={changeEffort}
          onToggleUltracode={toggleUltracode}
          onChangePermissionMode={changePermissionMode}
          onChangeModel={changeModel}
        />
      </div>
    </TooltipProvider>
  );
}

/** "deepseek/deepseek-flash[1m]" -> "deepseek-flash[1m]" for narrow pickers. */
function shortModelName(name: string): string {
  const tail = name.includes('/') ? name.slice(name.lastIndexOf('/') + 1) : name;
  return tail || name;
}

/** 1234567 -> "1.2M"; 45600 -> "45.6K". */
function formatTokenCount(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`;
  return String(value);
}

/**
 * One-line session stats above the composer, mirroring the terminal's status
 * line: context fill, accumulated tokens and cache hit rate. Shown from the
 * moment the session opens — zeros are honest, an empty row that fills in
 * after the first reply reads as a missing feature.
 */
function ContextStats({ usage, contextWindow }: { usage: SessionUsage | null; contextWindow?: number }) {
  const current = usage?.current ?? { input: 0, output: 0, cacheCreation: 0, cacheRead: 0 };
  const totals = usage?.totals ?? { input: 0, output: 0 };
  const contextTokens = current.input + current.cacheCreation + current.cacheRead;
  const pct =
    contextWindow && contextWindow > 0 ? Math.min(100, Math.round((contextTokens / contextWindow) * 100)) : null;
  // 10 cells, matching the terminal's ███░░░░░░░ bar.
  const filled = pct === null ? 0 : Math.round(pct / 10);
  // Conversation average (cumulative cache reads / all prompt tokens), the
  // same definition the terminal status line uses.
  const cache = usage && usage.cacheHitRate !== null ? `${Math.round(usage.cacheHitRate * 100)}%` : null;
  return (
    <div
      role="status"
      className="mx-auto flex w-full max-w-3xl flex-wrap items-center gap-x-2 gap-y-0.5 px-4 pb-0.5 text-[11px] leading-4 text-text-muted"
    >
      {pct !== null && (
        <span className="font-mono tracking-tight" title={`上下文 ${contextWindow?.toLocaleString()} tokens`}>
          <span className="text-text-secondary">{'█'.repeat(filled)}</span>
          {'░'.repeat(10 - filled)} {pct}%
        </span>
      )}
      <span>
        Tokens {formatTokenCount(totals.input + totals.output)}
        {cache !== null ? ` (cache ${cache})` : ''}
      </span>
    </div>
  );
}

function PermissionEventView({
  request,
  onApprove,
  onReject,
  onRespondOption,
  onSubmitAnswers,
  onSubmitPlan,
}: {
  request: PendingPermission;
  onApprove: () => void;
  onReject: () => void;
  onRespondOption: (requestId: string, optionId: string, kind: string) => void;
  onSubmitAnswers: (requestId: string, answers: Record<string, unknown>, questions: Question[]) => void;
  onSubmitPlan: (requestId: string, value: string, feedback?: string) => void;
}) {
  const toolName = request.toolName;
  const toolInput = request.toolInput;
  const description = request.description || '';

  if (toolName === 'AskUserQuestion') {
    const questions = (toolInput.questions as Question[]) || [];
    return (
      <AskUserPanelView
        requestId={request.requestId}
        questions={questions}
        description={description}
        onSubmit={answers => onSubmitAnswers(request.requestId, answers, questions)}
        onSkip={onReject}
      />
    );
  }

  if (toolName === 'ExitPlanMode') {
    const planContent = (toolInput.plan as string) || '';
    return (
      <PlanPanelView
        requestId={request.requestId}
        planContent={planContent}
        description={description}
        onSubmit={(value, feedback) => onSubmitPlan(request.requestId, value, feedback)}
      />
    );
  }

  return (
    <PermissionPromptView
      requestId={request.requestId}
      toolName={toolName}
      toolInput={toolInput}
      description={description}
      options={request.options}
      onApprove={onApprove}
      onReject={onReject}
      onRespond={(optionId, kind) => onRespondOption(request.requestId, optionId, kind)}
    />
  );
}
