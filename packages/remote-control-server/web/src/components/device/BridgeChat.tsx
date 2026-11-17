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
import { formatContextUsage } from '../../lib/context-usage';
import { composerModelLabel } from '../../lib/composer-model-label';
import { OfflineSessionSettings } from './OfflineSessionSettings';
import { mergeThreadEntries } from '../../lib/device-history';
import type { PendingPermission, ThreadEntry } from '../../lib/types';
import type { ChatInputMessage } from '../../lib/types';
import { apiFetchProviderQuota, apiFetchProviders, apiFetchSession, apiSendControl } from '../../api/client';
import {
  updateSessionSettings,
  fetchSession,
  type TranscriptUsage,
  type SessionSettings,
  type SessionSettingsValues,
} from '../../api/devices';
import { ChatView } from '../../../components/chat/ChatView';
import { ChatInput } from '../../../components/chat/ChatInput';
import { ComposerSettings } from './ComposerSettings';
import { ExecutionPanel } from './ExecutionPanel';
import {
  agentProgressParentId,
  applyAgentProgress,
  markTerminalQueuedMessages,
  type ExecutionTask,
  type BackgroundAgent,
  type TerminalQueueSnapshot,
  type WorkflowRun,
} from '../../lib/execution-state';
import { TooltipProvider } from '../../../components/ui/tooltip';
import { cn } from '../../lib/utils';
import { haptic } from '../../lib/view-transition';
import { turnFinished } from '../../lib/turn-notice';
import { SlidersHorizontal, TerminalSquare } from 'lucide-react';
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
  onSessionTitle,
  toolbarExtraLeft,
  onCloseTerminal,
  deviceId,
  localSessionId,
  savedSettings,
  onSavedSettingsChange,
  cwd,
  hasOlder,
  loadingOlder,
  onLoadOlder,
  usageSeed,
  draftKey: draftKeyProp,
  onOfflineSubmit,
  offlineDisabled,
  offlineLoading,
  offlineHistoryLoading,
  offlinePlaceholder,
  offlineContent,
  onOfflinePermissionRespond,
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
  onTerminalUnavailable?: (text: string) => void | Promise<void>;
  onBridgeDisconnected?: () => void;
  /** The SSE stream is open; catch up on anything produced before it. */
  onBridgeReady?: () => void;
  onSessionTitle?: (title: string) => void;
  /** Extra controls at the far left of the composer (e.g. close-terminal). */
  toolbarExtraLeft?: ReactNode;
  /** Close the PC terminal; offered inside the settings sheet. */
  onCloseTerminal?: () => void;
  /** Enables attaching files from this phone or from the PC. */
  deviceId?: string;
  localSessionId?: string;
  savedSettings?: SessionSettings | null;
  onSavedSettingsChange?: (settings: SessionSettings) => void;
  /** The terminal's working directory; attachments land inside it. */
  cwd?: string;
  /** Older server-cache pages; the device view owns the cursor. */
  hasOlder?: boolean;
  loadingOlder?: boolean;
  onLoadOlder?: () => void;
  /**
   * Assistant rows from the device-history cache that carry billed usage.
   * Seeds the token/cache-rate row so it matches the terminal right after a
   * reload instead of restarting from zero.
   */
  usageSeed?: { id: string; timestamp?: string; usage: TranscriptUsage }[];
  /**
   * Shared draft key. The device view passes its own key so an unsent draft
   * survives the switch from the history view to this live view; the default
   * keeps the legacy bridge-scoped key for standalone mounts.
   */
  draftKey?: string;
  onOfflineSubmit?: (message: ChatInputMessage) => Promise<void>;
  offlineDisabled?: boolean;
  offlineLoading?: boolean;
  offlineHistoryLoading?: boolean;
  offlinePlaceholder?: string;
  offlineContent?: ReactNode;
  onOfflinePermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
}) {
  const [internalEntries, setInternalEntries] = useState<ThreadEntry[]>(() => initialEntries ?? []);
  const entries = controlledEntries ?? internalEntries;
  const setEntries = onEntriesChange ?? setInternalEntries;
  const controlled = controlledEntries !== undefined;
  const [isLoading, setIsLoading] = useState(false);
  const [isCompacting, setIsCompacting] = useState(false);
  const [tasks, setTasks] = useState<ExecutionTask[]>([]);
  const [backgroundAgents, setBackgroundAgents] = useState<BackgroundAgent[]>([]);
  useEffect(() => {
    setBackgroundAgents([]);
  }, [sessionId]);
  const [workflows, setWorkflows] = useState<WorkflowRun[]>([]);
  /** Prompts typed at the terminal, waiting for the running turn to finish. */
  const [terminalQueue, setTerminalQueue] = useState<TerminalQueueSnapshot | null>(null);
  const visibleEntries = useMemo(() => markTerminalQueuedMessages(entries, terminalQueue), [entries, terminalQueue]);
  const agentProgressEvents = useRef<unknown[]>([]);
  const entriesRef = useRef(entries);
  entriesRef.current = entries;
  // Bridge metadata can arrive before the device transcript. Replay recent
  // child updates once the parent Agent card appears, without another fetch.
  useEffect(() => {
    if (!agentProgressEvents.current.length) return;
    const ids = new Set(entries.filter(entry => entry.type === 'tool_call').map(entry => entry.toolCall.id));
    const ready = agentProgressEvents.current.filter(event => ids.has(agentProgressParentId(event) ?? ''));
    if (!ready.length) return;
    agentProgressEvents.current = agentProgressEvents.current.filter(
      event => !ids.has(agentProgressParentId(event) ?? ''),
    );
    setEntries(previous => ready.reduce<ThreadEntry[]>((items, event) => applyAgentProgress(items, event), previous));
  }, [entries, setEntries]);
  const [permissions, setPermissions] = useState<PendingPermission[]>([]);
  // Request ids whose answer is on the wire; the card shows "处理中…".
  const [answering, setAnswering] = useState<Set<string>>(() => new Set());
  const answeringRef = useRef(new Set<string>());
  const withAnswering = useCallback(async (requestId: string, action: () => Promise<void>) => {
    if (answeringRef.current.has(requestId)) return;
    answeringRef.current.add(requestId);
    setAnswering(previous => new Set(previous).add(requestId));
    try {
      await action();
    } finally {
      answeringRef.current.delete(requestId);
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
  const [effort, setEffort] = useState('');
  const [ultracode, setUltracode] = useState(false);
  const [ultracodeUsable, setUltracodeUsable] = useState<boolean | null>(null);
  const [usageState, setUsageState] = useState<{ sessionId: string; usage: SessionUsage } | null>(null);
  const usage = usageState?.sessionId === sessionId ? usageState.usage : null;
  const [controlError, setControlError] = useState('');
  /** False until the bridge handshake (init) returns; sending is held until then. */
  const [ready, setReady] = useState(false);
  const [streamConnected, setStreamConnected] = useState(false);
  const [controlPending, setControlPending] = useState(false);
  const savingSettings = useRef(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [providersLoading, setProvidersLoading] = useState(false);
  const [providersError, setProvidersError] = useState(false);
  const [serverModels, setServerModels] = useState<
    {
      id: string;
      name: string;
      displayName: string;
      quota?: { label: string; remaining: number; resetAt?: number }[];
    }[]
  >([]);
  useEffect(() => {
    let cancelled = false;
    setProvidersLoading(true);
    setProvidersError(false);
    void apiFetchProviders()
      .then(async ({ providers }) => {
        if (cancelled) return;
        setServerModels(
          providers.flatMap(provider =>
            provider.models.map(model => ({
              id: `rcs-provider:${provider.id}:${encodeURIComponent(model)}`,
              name: `${provider.name} / ${provider.modelDetails?.find(detail => detail.model === model)?.displayName ?? model}`,
              displayName: provider.modelDetails?.find(detail => detail.model === model)?.displayName ?? model,
            })),
          ),
        );
        await Promise.all(
          providers.map(async provider => {
            try {
              const { windows } = await apiFetchProviderQuota(provider.id);
              if (!cancelled && windows.length)
                setServerModels(previous =>
                  previous.map(model =>
                    model.id.startsWith(`rcs-provider:${provider.id}:`) ? { ...model, quota: windows } : model,
                  ),
                );
            } catch {
              /* Quota is optional and must not affect model selection. */
            }
          }),
        );
      })
      .catch(() => {
        if (!cancelled) setProvidersError(true);
      })
      .finally(() => {
        if (!cancelled) setProvidersLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [settingsOpen]);
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
  const onSessionTitleRef = useRef(onSessionTitle);
  useEffect(() => {
    onSessionTitleRef.current = onSessionTitle;
  }, [onSessionTitle]);
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
  // Ref-stored so the adapter's memoized options keep a stable identity; the
  // logic itself needs the latest closure values.
  const applySessionStatusRef = useRef<(status: string, since?: number) => void>(() => {});
  applySessionStatusRef.current = (status, since) => {
    const running = status === 'running' || status === 'requires_action';
    if (running && !turnStartedAt.current) {
      // A device joining mid-turn gets the server's snapshot with the
      // time the status changed; the elapsed row then reads true
      // runtime instead of restarting from zero on every reconnect.
      turnStartedAt.current = since && since > 0 && since <= Date.now() ? since : Date.now();
    } else if (!running) {
      // A missed result event must not let the timer run into the next
      // turn; the bus delivers the result before its derived idle
      // status, so this cannot race onTurnComplete's own cleanup.
      turnStartedAt.current = 0;
    }
    setIsLoading(running);
  };

  useEffect(() => {
    if (isLoading && !turnStartedAt.current) turnStartedAt.current = Date.now();
  }, [isLoading]);

  /**
   * Long tool-heavy turns used to give no signal at all on the phone: the
   * tool cards completed one by one and nothing said the agent was still
   * working. A ticking status row with elapsed time is that signal.
   */
  const [statusNow, setStatusNow] = useState(() => Date.now());
  useEffect(() => {
    if (!isLoading) return;
    setStatusNow(Date.now());
    const timer = setInterval(() => setStatusNow(Date.now()), 1000);
    return () => clearInterval(timer);
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
      userChangedModel.current = false;
      userChangedMode.current = false;
      for (const rollback of rollbacks) rollback();
      if (rollbacks.length > 0) setControlError('切换超时，请重新确认终端当前设置');
    }, 12000);
  }, []);

  useEffect(() => {
    return () => {
      if (controlTimer.current) clearTimeout(controlTimer.current);
    };
  }, []);

  const adapter = useMemo(
    () =>
      new RCSChatAdapter(sessionId, setEntries, {
        onTasks: setTasks,
        onBackgroundAgents: setBackgroundAgents,
        onWorkflows: setWorkflows,
        // Mirrored from the CLI: prompts typed at the terminal while a turn
        // runs. Without this the phone sees nothing until the turn ends.
        onTerminalQueue: next => setTerminalQueue(next ?? null),
        onAgentProgress: payload => {
          const parentId = agentProgressParentId(payload);
          if (
            parentId &&
            !entriesRef.current.some(entry => entry.type === 'tool_call' && entry.toolCall.id === parentId)
          ) {
            agentProgressEvents.current.push(payload);
            if (agentProgressEvents.current.length > 500) agentProgressEvents.current.shift();
          }
        },
        onError: message => setError(message),
        // Answering on the PC (or another device) resolves the request there;
        // the prompt must not linger on this phone.
        onPermissionResolved: requestId => {
          setPermissions(previous => previous.filter(item => item.requestId !== requestId));
        },
        // The tool finished: its prompt is spent even if the resolving control
        // frame never arrived (interrupt or a transport gap drops those).
        onPermissionToolResult: toolUseId => {
          setPermissions(previous => previous.filter(item => item.toolUseId !== toolUseId));
        },
        onPermissionRequest: permission => {
          setPermissions(previous =>
            previous.some(item => item.requestId === permission.requestId) ? previous : [...previous, permission],
          );
        },
        onSessionInfo: info => {
          setSessionInfo(info);
          // A replayed system/init must not undo a model the user just chose.
          if (info.model && !userChangedModel.current) setModelId(info.modelSelection ?? info.model);
          if (info.permissionMode && !userChangedMode.current) setPermissionMode(info.permissionMode);
          // Effort/ultracode are republished by the CLI whenever they change —
          // including changes made in the terminal — so these are authoritative.
          if (typeof info.effort === 'string' && info.effort) setEffort(info.effort);
          if (typeof info.ultracodeAvailable === 'boolean') setUltracodeUsable(info.ultracodeAvailable);
          if (typeof info.ultracodeActive === 'boolean') setUltracode(info.ultracodeActive);
        },
        onControlResponse: response => {
          // Permission answers and controls from other clients share this
          // stream. They must not change this sheet's pending/error state.
          if (!response.requestId || !pendingControls.current.has(response.requestId)) return;
          const rollback = pendingControls.current.get(response.requestId);
          pendingControls.current.delete(response.requestId);
          if (response.error) rollback?.();
          if (pendingControls.current.size === 0) {
            userChangedModel.current = false;
            userChangedMode.current = false;
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
          if (!next) setIsCompacting(false);
          if (next) {
            // A bridge_state false→true flap mid-turn cleared the running row
            // above; the session_status snapshot that restores it only rides a
            // NEW stream open, which this reconnect never triggers. Re-ask.
            void apiFetchSession(sessionId)
              .then(session => {
                if (session.status) applySessionStatusRef.current(session.status);
              })
              .catch(() => {});
          }
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
        onCompaction: setIsCompacting,
        onSessionTitle: title => onSessionTitleRef.current?.(title),
        onLocalSessionChanged: next => {
          if (!deviceId || !localSessionId || next === localSessionId) return;
          // The event can be replayed after another resume; verify the current
          // mapping before detaching this view from its bridge.
          void fetchSession(deviceId, localSessionId)
            .then(detail => {
              if (detail.bridgeSessionId !== sessionId || !detail.bridgeConnected) onBridgeDisconnectedRef.current?.();
            })
            .catch(() => {});
        },
        onUsage: next => setUsageState({ sessionId, usage: next }),
        // The CLI reports the authoritative turn state. Without this, a turn
        // started in the terminal looked idle in the browser, and the first
        // streamed assistant chunk cleared the spinner prematurely.
        onStatusChange: (status, since) => applySessionStatusRef.current(status, since),
        onStreamState: setStreamConnected,
        onStreamOpen: controlled ? () => onBridgeReadyRef.current?.() : undefined,
        // The controlled (device) view already has the authoritative
        // transcript. Re-reading bridge history there is what made the chat
        // flash/reload on every connect; only its init metadata is needed.
        historyMode: controlled ? 'metadata' : 'full',
      }),
    [sessionId, setEntries, controlled, deviceId, localSessionId],
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

  // Seed the usage ledger whenever the device view has cached pages with
  // billed usage — on first mount and again for each "load older" page.
  // Ids collide with live events by design: same message, one ledger row.
  useEffect(() => {
    if (!usageSeed || usageSeed.length === 0) return;
    adapter.seedUsageFromHistory(usageSeed);
  }, [adapter, usageSeed]);

  useEffect(() => {
    if (!sessionId) {
      setReady(false);
      setConnected(false);
      setStreamConnected(false);
      setIsLoading(false);
      setIsCompacting(false);
      setPermissions([]);
      setTasks([]);
      setWorkflows([]);
      setTerminalQueue(null);
      setError('');
      setSettingsOpen(false);
      return;
    }
    adapterRef.current = adapter;
    let cancelled = false;
    setReady(false);
    void adapter
      .init()
      .catch(err => {
        if (!cancelled) setError(err instanceof Error ? err.message : '无法连接会话');
      })
      .finally(() => {
        // The banner above tells the user what went wrong; the composer stays
        // usable either way so typing is never silently swallowed.
        if (!cancelled) setReady(true);
      });
    return () => {
      cancelled = true;
      if (disconnectTimer.current) {
        clearTimeout(disconnectTimer.current);
        disconnectTimer.current = null;
      }
      adapter.disconnect();
      if (adapterRef.current === adapter) adapterRef.current = null;
    };
  }, [adapter, sessionId]);

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
      const hasImages = Boolean(message.images && message.images.length > 0);
      // Image-only sends are legal (the adapter builds image blocks); the old
      // text-only guard silently swallowed them — photos vanished with no
      // bubble, no error, nothing.
      if (!text && !hasImages) return;
      setError('');
      if (!sessionId && onOfflineSubmit) {
        await onOfflineSubmit(message);
        return;
      }
      if (!streamConnected) {
        throw new Error('消息连接已断开，正在重连，请稍候再发送');
      }

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
        armControlTimeout();
        try {
          // Register the request id: the CLI rejects invalid levels (e.g.
          // 'max' for non-ant users) asynchronously via control_response, and
          // an unregistered response was dropped instead of surfacing the error.
          await adapter.setEffort(arg, requestId => pendingControls.current.set(requestId, () => {}));
          appendCommandOutput(`已设置 effort：${arg}`);
        } catch (err) {
          if (controlTimer.current) {
            clearTimeout(controlTimer.current);
            controlTimer.current = null;
          }
          pendingControls.current.clear();
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
      // text to the parent, which queues it and reopens the terminal. The
      // queue only carries text: with photos attached, put the whole draft
      // back instead of silently dropping them.
      if (!connected) {
        if (hasImages) throw new Error('终端正在重连，图片还未发送，请稍候重试');
        if (onTerminalUnavailable) await onTerminalUnavailable(text);
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
        // Image-carrying drafts are restored to the composer instead — the
        // queue cannot carry photos.
        if (/terminal|终端/i.test(message) && onTerminalUnavailable && !hasImages) {
          await onTerminalUnavailable(text);
          return;
        }
        setError(message);
        // Let the composer restore the draft the user just typed.
        throw err;
      } finally {
        sendInFlight.current = false;
      }
    },
    [
      adapter,
      appendCommandOutput,
      connected,
      onTerminalUnavailable,
      ready,
      streamConnected,
      sessionId,
      onOfflineSubmit,
    ],
  );

  /** Send a control request and remember how to undo its optimistic value. */
  const runControl = useCallback(
    async (send: (register: (id: string) => void) => Promise<string>, rollback: () => void, failure: string) => {
      setControlError('');
      setControlPending(true);
      armControlTimeout();
      try {
        await send(requestId => pendingControls.current.set(requestId, rollback));
      } catch (err) {
        if (controlTimer.current) {
          clearTimeout(controlTimer.current);
          controlTimer.current = null;
        }
        rollback();
        pendingControls.current.clear();
        userChangedModel.current = false;
        userChangedMode.current = false;
        setControlPending(false);
        setControlError(`${failure}：${err instanceof Error ? err.message : '未知原因'}`);
      }
    },
    [armControlTimeout],
  );

  const saveDeviceSetting = useCallback(
    async (patch: SessionSettingsValues, rollback: () => void) => {
      if (!deviceId || !localSessionId || savingSettings.current) return;
      savingSettings.current = true;
      setControlPending(true);
      setControlError('');
      try {
        const result = await updateSessionSettings(deviceId, localSessionId, patch);
        onSavedSettingsChange?.(result.settings);
        if (result.settings.error) setControlError(`设置已保存，但终端未能应用：${result.settings.error}`);
      } catch (cause) {
        rollback();
        setControlError(cause instanceof Error ? cause.message : '保存设置失败');
      } finally {
        savingSettings.current = false;
        setControlPending(false);
      }
    },
    [deviceId, localSessionId, onSavedSettingsChange],
  );

  useEffect(() => {
    if (!savedSettings || savingSettings.current) return;
    const values = savedSettings.values;
    if (values.model !== undefined) setModelId(values.model);
    if (values.effort !== undefined) setEffort(values.effort);
    if (values.permissionMode !== undefined) setPermissionMode(values.permissionMode);
    if (values.ultracode !== undefined) setUltracode(values.ultracode);
    if (savedSettings.error) setControlError(`设置尚未生效：${savedSettings.error}`);
  }, [savedSettings]);

  const changeModel = useCallback(
    async (id: string) => {
      const previous = modelId;
      userChangedModel.current = true;
      setModelId(id);
      if (deviceId && localSessionId) {
        await saveDeviceSetting({ model: id }, () => setModelId(previous));
        userChangedModel.current = false;
        return;
      }
      await runControl(
        register => adapter.setModel(id, register),
        () => setModelId(previous),
        '切换模型失败，已保持原模型',
      );
    },
    [adapter, modelId, runControl, deviceId, localSessionId, saveDeviceSetting],
  );

  const changePermissionMode = useCallback(
    async (mode: string) => {
      const previous = permissionMode;
      userChangedMode.current = true;
      setPermissionMode(mode);
      if (deviceId && localSessionId) {
        await saveDeviceSetting({ permissionMode: mode }, () => setPermissionMode(previous));
        userChangedMode.current = false;
        return;
      }
      await runControl(
        register => adapter.setPermissionMode(mode, register),
        () => setPermissionMode(previous),
        '切换权限模式失败，已保持原模式',
      );
    },
    [adapter, permissionMode, runControl, deviceId, localSessionId, saveDeviceSetting],
  );

  const changeEffort = useCallback(
    async (value: string) => {
      const previous = effort;
      setEffort(value);
      if (deviceId && localSessionId) {
        await saveDeviceSetting({ effort: value }, () => setEffort(previous));
        return;
      }
      await runControl(
        register => adapter.setEffort(value, register),
        () => setEffort(previous),
        '切换思考等级失败，已保持原等级',
      );
    },
    [adapter, effort, runControl, deviceId, localSessionId, saveDeviceSetting],
  );

  const toggleUltracode = useCallback(async () => {
    const next = !ultracode;
    setUltracode(next);
    if (deviceId && localSessionId) {
      await saveDeviceSetting({ ultracode: next }, () => setUltracode(!next));
      return;
    }
    await runControl(
      register => adapter.setUltracode(next, register),
      () => setUltracode(!next),
      '切换 Ultracode 失败',
    );
  }, [adapter, runControl, ultracode, deviceId, localSessionId, saveDeviceSetting]);

  const interrupt = useCallback(async () => {
    try {
      await adapter.interrupt();
      // Only claim idle once the interrupt really landed. The old `finally`
      // cleared the spinner after a failed POST, so the UI looked idle while
      // the turn was still running.
      turnStartedAt.current = 0;
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
    async (requestId: string, answers: Record<string, unknown>, toolInput: Record<string, unknown>) => {
      try {
        await apiSendControl(sessionId, {
          type: 'permission_response',
          approved: true,
          request_id: requestId,
          updated_input: { ...toolInput, answers },
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
  const models = serverModels;
  const permissionOptions = PERMISSION_MODES.some(option => option.id === permissionMode)
    ? PERMISSION_MODES
    : permissionMode
      ? [...PERMISSION_MODES, { id: permissionMode, label: permissionMode }]
      : PERMISSION_MODES;
  const activeModelName =
    serverModels.find(model => model.id === modelId)?.displayName ?? composerModelLabel(modelId, sessionInfo?.model);

  return (
    <TooltipProvider>
      <div className="flex min-h-0 flex-1 flex-col overflow-hidden rcs-page-enter">
        {header}
        {sessionId && !streamConnected && (
          <div
            role="status"
            className="border-b border-warning-border/30 bg-warning-border/10 px-4 py-2 text-sm text-warning-text"
          >
            消息连接已断开，正在重连…
          </div>
        )}
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
          entries={visibleEntries}
          isLoading={Boolean(sessionId) && isLoading}
          isHistoryLoading={!sessionId && offlineHistoryLoading}
          onPermissionRespond={sessionId ? respondFromCard : onOfflinePermissionRespond}
          hasOlder={hasOlder}
          loadingOlder={loadingOlder}
          onLoadOlder={onLoadOlder}
          emptyTitle={!sessionId && offlineHistoryLoading ? '正在加载会话…' : '开始对话'}
          emptyDescription={!sessionId && offlineHistoryLoading ? '' : '输入消息继续这个会话'}
        />
        {!sessionId && offlineContent}
        {sessionId && permissions.length > 0 && (
          /* A long AskUserQuestion/plan card used to squeeze the transcript and
             could push the composer off a phone screen; scroll it instead. */
          <div className="max-h-[40%] overflow-y-auto overscroll-contain border-t bg-surface-1 px-4 py-3 sm:max-h-[50%]">
            <div className="mx-auto w-full space-y-3">
              {permissions.map(request => (
                <PermissionEventView
                  key={request.requestId}
                  request={request}
                  pending={answering.has(request.requestId)}
                  onApprove={() => withAnswering(request.requestId, () => approve(request.requestId))}
                  onReject={() => withAnswering(request.requestId, () => reject(request.requestId))}
                  onRespondOption={(requestId, optionId, kind) =>
                    void withAnswering(requestId, () => respondOption(requestId, optionId, kind))
                  }
                  onSubmitAnswers={(requestId, answers, toolInput) =>
                    void withAnswering(requestId, () => submitAnswers(requestId, answers, toolInput))
                  }
                  onSubmitPlan={(requestId, value, feedback) =>
                    void withAnswering(requestId, () => submitPlan(requestId, value, feedback))
                  }
                />
              ))}
            </div>
          </div>
        )}
        {/* Keep the expandable task details above the compact status row. */}
        {sessionId && (
          <ExecutionPanel
            tasks={tasks}
            workflows={workflows}
            agents={backgroundAgents.filter(
              agent => !entries.some(entry => entry.type === 'tool_call' && entry.toolCall.agentId === agent.id),
            )}
            connected={connected}
            compact={permissions.length > 0}
          />
        )}
        {sessionId && (isLoading || isCompacting) && (
          <div className="flex w-full flex-wrap items-center gap-x-3 gap-y-0.5 px-4 pb-1 text-[11px] leading-5 text-text-secondary sm:px-8">
            <span role="status" aria-live="polite" className="inline-flex items-center gap-1.5">
              <span className="h-1.5 w-1.5 shrink-0 rounded-full bg-brand" />
              {isCompacting ? '压缩中' : '执行中'}
              {turnStartedAt.current ? ` · ${formatElapsed(statusNow - turnStartedAt.current)}` : ''}
            </span>
          </div>
        )}
        {sessionId && terminalQueue && terminalQueue.count > 0 && (
          <div role="status" aria-label="终端排队消息" className="shrink-0 px-4 pb-1.5 sm:px-8">
            <div className="mx-auto flex w-full items-center gap-2 rounded-xl border border-border/60 bg-surface-1/60 px-3 py-2 text-[12px] leading-5 text-text-secondary">
              <TerminalSquare className="h-3.5 w-3.5 shrink-0 text-text-muted" />
              <span className="shrink-0 font-medium text-text-primary">
                终端排队中{terminalQueue.count > 1 ? `（${terminalQueue.count} 条）` : ''}
              </span>
              {terminalQueue.previews[0] && (
                <span className="min-w-0 flex-1 truncate text-text-muted">{terminalQueue.previews[0]}</span>
              )}
            </div>
          </div>
        )}
        <ChatInput
          key={draftKeyProp ?? sessionId}
          draftKey={draftKeyProp ?? `bridge:${sessionId}`}
          onSubmit={submit}
          isLoading={sessionId ? isLoading : offlineLoading}
          onInterrupt={sessionId ? interrupt : undefined}
          disabled={!sessionId && offlineDisabled}
          placeholder={sessionId ? '和 Claude 一起工作' : (offlinePlaceholder ?? '和 Claude 一起工作')}
          supportsImages
          deviceId={deviceId}
          cwd={cwd}
          commands={sessionId ? commands : []}
          toolbarAfterAttachments={<ContextStats usage={usage} contextWindow={sessionInfo?.contextWindow} />}
          toolbarLeft={
            <>
              {toolbarExtraLeft}
              {!sessionId && deviceId && localSessionId && onSavedSettingsChange ? (
                <OfflineSessionSettings
                  deviceId={deviceId}
                  sessionId={localSessionId}
                  settings={savedSettings}
                  onChange={onSavedSettingsChange}
                />
              ) : (
                <button
                  type="button"
                  onClick={() => setSettingsOpen(true)}
                  disabled={controlPending || !streamConnected}
                  aria-label="会话设置"
                  className="flex h-8 min-w-0 shrink items-center gap-1 rounded-full px-1.5 text-text-secondary transition-colors hover:bg-surface-1 hover:text-text-primary disabled:opacity-60 sm:h-9 sm:px-2"
                >
                  <SlidersHorizontal className="h-3.5 w-3.5 shrink-0 sm:h-4 sm:w-4" />
                  <span
                    className={cn(
                      'min-w-0 truncate text-[11px] sm:text-xs',
                      ultracode || (effort && effort !== 'auto')
                        ? 'text-brand-dark dark:text-brand'
                        : 'text-text-secondary',
                    )}
                  >
                    {activeModelName}
                  </span>
                </button>
              )}
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
          currentModelName={activeModelName}
          currentModelIsLocal={Boolean(modelId) && !modelId.startsWith('rcs-provider:')}
          providerSwitchAvailable={sessionInfo?.serverProviderSwitch === true}
          providersLoading={providersLoading}
          providersError={providersError}
          models={models.map(model => ({
            id: model.id,
            label: model.name,
            title: model.name,
            quota: model.quota,
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
/** 65s -> "1分05秒"; 6s -> "6秒". */
function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return minutes > 0 ? `${minutes}分${String(seconds).padStart(2, '0')}秒` : `${seconds}秒`;
}

/** Latest context use and configured limit, as reported by the terminal. */
function ContextStats({ usage, contextWindow }: { usage: SessionUsage | null; contextWindow?: number }) {
  const text = formatContextUsage(usage, contextWindow);
  if (!text) return null;
  return (
    <span
      role="status"
      aria-label={`上下文 ${text}`}
      title={
        usage?.estimated
          ? '上下文和缓存命中率根据会话记录估算'
          : usage?.contextEstimated
            ? '压缩后上下文为估算值，缓存命中率为会话实际值'
            : '当前上下文使用量 / 上限及会话缓存命中率'
      }
      className="min-w-0 truncate px-1 text-[11px] text-text-muted sm:text-xs"
    >
      <span aria-hidden="true" className="sm:hidden">
        {text.replace(' / ', '/').replace(' · ', ' ')}
      </span>
      <span aria-hidden="true" className="hidden sm:inline">
        {text}
      </span>
    </span>
  );
}

function PermissionEventView({
  request,
  pending,
  onApprove,
  onReject,
  onRespondOption,
  onSubmitAnswers,
  onSubmitPlan,
}: {
  request: PendingPermission;
  /** The answer POST is on the wire; the card must not accept another tap. */
  pending: boolean;
  onApprove: () => void;
  onReject: () => void;
  onRespondOption: (requestId: string, optionId: string, kind: string) => void;
  onSubmitAnswers: (requestId: string, answers: Record<string, unknown>, toolInput: Record<string, unknown>) => void;
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
        busy={pending}
        onSubmit={answers => onSubmitAnswers(request.requestId, answers, toolInput)}
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
        busy={pending}
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
      busy={pending}
      onApprove={onApprove}
      onReject={onReject}
      onRespond={(optionId, kind) => onRespondOption(request.requestId, optionId, kind)}
    />
  );
}
