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
import type { BridgeSessionInfo } from '../../lib/rcs-chat-adapter';
import { mergeThreadEntries } from '../../lib/device-history';
import type { PendingPermission, ThreadEntry } from '../../lib/types';
import type { ChatInputMessage } from '../../lib/types';
import { apiSendControl } from '../../api/client';
import { ChatView } from '../../../components/chat/ChatView';
import { ChatInput } from '../../../components/chat/ChatInput';
import { TooltipProvider } from '../../../components/ui/tooltip';
import { cn } from '../../lib/utils';
import { haptic } from '../../lib/view-transition';
import { ChevronDown, ShieldCheck } from 'lucide-react';
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
}) {
  const [internalEntries, setInternalEntries] = useState<ThreadEntry[]>(() => initialEntries ?? []);
  const entries = controlledEntries ?? internalEntries;
  const setEntries = onEntriesChange ?? setInternalEntries;
  const controlled = controlledEntries !== undefined;
  const [isLoading, setIsLoading] = useState(false);
  const [permissions, setPermissions] = useState<PendingPermission[]>([]);
  const [error, setError] = useState('');
  const [sessionInfo, setSessionInfo] = useState<BridgeSessionInfo | null>(null);
  const [modelId, setModelId] = useState('');
  const [permissionMode, setPermissionMode] = useState('');
  const [controlError, setControlError] = useState('');
  const [controlPending, setControlPending] = useState(false);
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
  const pendingModel = useRef<{ previous: string } | null>(null);
  const pendingMode = useRef<{ previous: string } | null>(null);

  const adapter = useMemo(
    () =>
      new RCSChatAdapter(sessionId, setEntries, {
        onError: message => setError(message),
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
        },
        onControlResponse: response => {
          setControlPending(false);
          setControlError(response.error ?? '');
          if (response.error) {
            // Roll the pickers back to what the CLI is actually using.
            if (pendingModel.current) {
              setModelId(pendingModel.current.previous);
              pendingModel.current = null;
            }
            if (pendingMode.current) {
              setPermissionMode(pendingMode.current.previous);
              pendingMode.current = null;
            }
          } else {
            pendingModel.current = null;
            pendingMode.current = null;
          }
        },
        onBridgeState: next => {
          setConnected(next);
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
        onTurnComplete: () => setIsLoading(false),
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
    void adapter.init().catch(err => {
      setError(err instanceof Error ? err.message : '无法连接会话');
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
      }
    },
    [adapter, appendCommandOutput, connected, onTerminalUnavailable],
  );

  const changeModel = useCallback(
    async (id: string) => {
      const previous = modelId;
      userChangedModel.current = true;
      pendingModel.current = { previous };
      setModelId(id);
      setControlError('');
      setControlPending(true);
      // A CLI build that ignores the control request must not leave the
      // pickers disabled forever.
      window.setTimeout(() => setControlPending(false), 8000);
      try {
        await adapter.setModel(id);
      } catch (err) {
        pendingModel.current = null;
        setModelId(previous);
        setControlPending(false);
        setControlError(err instanceof Error ? err.message : '切换模型失败');
      }
    },
    [adapter, modelId],
  );

  const changePermissionMode = useCallback(
    async (mode: string) => {
      const previous = permissionMode;
      userChangedMode.current = true;
      pendingMode.current = { previous };
      setPermissionMode(mode);
      setControlError('');
      setControlPending(true);
      window.setTimeout(() => setControlPending(false), 8000);
      try {
        await adapter.setPermissionMode(mode);
      } catch (err) {
        pendingMode.current = null;
        setPermissionMode(previous);
        setControlPending(false);
        setControlError(err instanceof Error ? err.message : '切换权限模式失败');
      }
    },
    [adapter, permissionMode],
  );

  const interrupt = useCallback(async () => {
    try {
      await adapter.interrupt();
    } catch (err) {
      setError(err instanceof Error ? err.message : '中断失败');
    } finally {
      setIsLoading(false);
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

  return (
    <TooltipProvider>
      <div className="flex min-h-0 flex-1 flex-col overflow-hidden rcs-page-enter">
        {header}
        {controlError && (
          <div role="alert" className="border-b bg-destructive/10 px-4 py-2 text-sm text-destructive">
            {controlError}
          </div>
        )}
        {error && (
          <div role="alert" className="border-b bg-destructive/10 px-4 py-2 text-sm text-destructive">
            {error}
          </div>
        )}
        <ChatView
          entries={entries}
          isLoading={isLoading}
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
                  onApprove={() => void approve(request.requestId)}
                  onReject={() => void reject(request.requestId)}
                  onSubmitAnswers={submitAnswers}
                  onSubmitPlan={submitPlan}
                />
              ))}
            </div>
          </div>
        )}
        <ChatInput
          onSubmit={submit}
          isLoading={isLoading}
          onInterrupt={interrupt}
          placeholder="和 Claude 一起工作"
          commands={commands}
          toolbarLeft={
            <>
              <span
                className={cn(
                  'ml-1 mr-0.5 h-1.5 w-1.5 shrink-0 rounded-full',
                  isLoading ? 'animate-pulse bg-brand' : connected ? 'bg-status-success' : 'bg-status-error',
                )}
                title={isLoading ? '处理中' : connected ? '已连接' : '连接断开'}
              />
              <label className="relative inline-flex items-center rounded-full transition-colors hover:bg-surface-1">
                <ShieldCheck
                  className={cn(
                    'ml-1.5 h-3.5 w-3.5 shrink-0',
                    permissionMode && permissionMode !== 'default' ? 'text-brand' : 'text-text-muted',
                  )}
                />
                <select
                  value={permissionMode}
                  onChange={event => void changePermissionMode(event.target.value)}
                  disabled={controlPending}
                  className={cn(
                    'h-9 max-w-[9.5rem] appearance-none truncate bg-transparent py-0 pl-1 pr-6 text-xs outline-none disabled:opacity-60',
                    permissionMode && permissionMode !== 'default' ? 'text-brand' : 'text-text-secondary',
                  )}
                  aria-label="切换权限模式"
                >
                  {permissionOptions.map(option => (
                    <option key={option.id} value={option.id}>
                      {option.label}
                    </option>
                  ))}
                </select>
                <ChevronDown className="pointer-events-none absolute right-1.5 h-3.5 w-3.5 text-text-muted" />
              </label>
            </>
          }
          toolbarRight={
            models.length > 0 ? (
              <label className="relative inline-flex min-w-0 items-center rounded-full transition-colors hover:bg-surface-1">
                <select
                  value={modelId}
                  onChange={event => void changeModel(event.target.value)}
                  disabled={controlPending}
                  className="h-9 w-[9.5rem] appearance-none truncate bg-transparent py-0 pl-2 pr-6 text-xs text-text-secondary outline-none disabled:opacity-60 sm:w-[11rem]"
                  aria-label="切换模型"
                >
                  {models.map(model => (
                    <option key={model.id} value={model.id} title={model.name}>
                      {/* Provider prefixes make the closed select truncate to
                          "deepseek/deeps…" on a phone; show the model itself. */}
                      {shortModelName(model.name)}
                    </option>
                  ))}
                </select>
                <ChevronDown className="pointer-events-none absolute right-1.5 h-3.5 w-3.5 text-text-muted" />
              </label>
            ) : null
          }
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

function PermissionEventView({
  request,
  onApprove,
  onReject,
  onSubmitAnswers,
  onSubmitPlan,
}: {
  request: PendingPermission;
  onApprove: () => void;
  onReject: () => void;
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
      onApprove={onApprove}
      onReject={onReject}
    />
  );
}
