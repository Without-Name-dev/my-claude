import { useEffect, useMemo, useRef, useState } from 'react';
import { OlderMessagesRow, usePullToLoadOlder } from './PullOlder';
import type {
  AssistantMessageEntry,
  CommandOutputEntry,
  PlanDisplayEntry,
  ThreadEntry,
  ToolCallEntry,
  UserMessageEntry,
} from '../../src/lib/types';
import { cn, stripAnsi } from '../../src/lib/utils';
import { UserBubble, AssistantTurn } from './MessageBubble';
import { PlanDisplay } from './PlanView';
import { Conversation, ConversationContent, ConversationScrollButtons } from '../ai-elements/conversation';

// =============================================================================
// 统一聊天视图 — Anthropic 编辑式排版
//
// 上游把一次回答拆成很多条 JSONL 记录（思考 / 工具 / 正文各一条）。逐条渲染
// 会得到满屏"思考了几秒 + 一个工具"的日志。这里先按用户消息切回合，再把同一
// 回合里的思考合并成一条、连续工具合并成一组，正文保持原顺序。
// =============================================================================

interface ChatViewProps {
  entries: ThreadEntry[];
  isLoading?: boolean;
  isHistoryLoading?: boolean;
  onPermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
  emptyTitle?: string;
  emptyDescription?: string;
  /** Older transcript pages (history view only). */
  hasOlder?: boolean;
  loadingOlder?: boolean;
  onLoadOlder?: () => void;
}

export function ChatView({
  entries,
  isLoading = false,
  isHistoryLoading = false,
  onPermissionRespond,
  emptyTitle = '开始对话',
  emptyDescription = '输入消息开始聊天',
  hasOlder = false,
  loadingOlder = false,
  onLoadOlder,
}: ChatViewProps) {
  const items = useMemo(() => buildChatItems(entries), [entries]);
  const hasMessages = entries.length > 0;

  // Entrance animation belongs to messages that arrive while you are watching.
  // A history page can drop 30-100 entries in at once; animating all of them
  // turned the first screen into a frame-dropping wall of motion.
  const animatedKeys = useRef<Set<string>>(new Set());
  const previousKeys = useRef<Set<string>>(new Set());
  const currentKeys = items.map(item => item.key);
  const freshKeys = currentKeys.filter(key => !previousKeys.current.has(key));
  if (freshKeys.length > 0 && freshKeys.length <= 2) {
    for (const key of freshKeys) animatedKeys.current.add(key);
  }
  previousKeys.current = new Set(currentKeys);

  // Paint the newest few messages first, then fill the rest of the page in on
  // the next tick. The transcript is pinned to the bottom either way, so the
  // extra rows appear above the thumb instead of flashing a wall of text.
  const [revealAll, setRevealAll] = useState(false);
  useEffect(() => {
    if (revealAll || entries.length === 0) return;
    const timer = window.setTimeout(() => setRevealAll(true), 80);
    return () => window.clearTimeout(timer);
  }, [entries.length, revealAll]);

  // Warm the lazily loaded markdown chunk while the transcript is still
  // small: otherwise the first streamed reply swaps from raw text to
  // rendered markdown mid-stream, reflowing the whole bubble.
  useEffect(() => {
    void import('streamdown').catch(() => {});
  }, []);
  const visibleItems = revealAll ? items : items.slice(-6);
  const lastItemKey = items.length > 0 ? items[items.length - 1]!.key : '';

  // Dots only mean "sent, nothing back yet". Once a turn has started, the
  // thinking shimmer / running tool / streaming prose already say it.
  const showThinking = isLoading && entries.length > 0 && entries[entries.length - 1]?.type === 'user_message';

  // Pull down at the top of the transcript to fetch the previous page.
  const { containerRef, pull, hint } = usePullToLoadOlder({
    enabled: hasOlder && Boolean(onLoadOlder),
    loading: loadingOlder,
    onLoad: () => onLoadOlder?.(),
  });

  return (
    <Conversation className="flex-1" outerRef={containerRef}>
      <ConversationContent>
        {/* Always mounted: a live region inserted at the same moment the text
            appears is unreliable in VoiceOver. Only its content changes. */}
        <span role="status" aria-live="polite" className="sr-only">
          {showThinking ? 'Claude 正在回复' : ''}
        </span>
        {hasOlder && onLoadOlder ? (
          <OlderMessagesRow
            hasMore={hasOlder}
            loading={loadingOlder}
            hint={hint}
            pull={pull}
            onLoad={() => onLoadOlder()}
          />
        ) : null}
        {!hasMessages ? (
          isHistoryLoading || isLoading ? (
            <div role="status" aria-live="polite" className="flex w-full flex-col gap-3 py-6">
              <span className="sr-only">{emptyTitle}</span>
              {[72, 55, 84].map((width, i) => (
                <div key={i} className="flex flex-col gap-2">
                  <div className="h-3 animate-pulse rounded-full bg-surface-1" style={{ width: `${width}%` }} />
                  <div
                    className="h-3 animate-pulse rounded-full bg-surface-1"
                    style={{ width: `${Math.max(28, width - 22)}%`, animationDelay: `${i * 120}ms` }}
                  />
                </div>
              ))}
            </div>
          ) : (
            <div className="flex flex-1 flex-col items-center justify-center gap-2 py-20 text-center">
              <span className="flex h-11 w-11 items-center justify-center rounded-2xl bg-surface-1 text-text-muted">
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true">
                  <path
                    d="M4 6.5A2.5 2.5 0 0 1 6.5 4h11A2.5 2.5 0 0 1 20 6.5v8A2.5 2.5 0 0 1 17.5 17H9l-5 3v-3.5A2.5 2.5 0 0 1 4 14.5z"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinejoin="round"
                  />
                </svg>
              </span>
              <p className="text-[15px] font-medium text-text-primary">{emptyTitle}</p>
              {emptyDescription ? (
                <p className="max-w-xs text-[13px] leading-relaxed text-text-muted">{emptyDescription}</p>
              ) : null}
            </div>
          )
        ) : (
          <>
            {visibleItems.map(item => {
              const animate = animatedKeys.current.has(item.key);
              switch (item.kind) {
                case 'user':
                  return (
                    <div key={item.key} className={cn('pt-7 pb-1', animate && 'rcs-enter')}>
                      <UserBubble entry={item.entry} />
                    </div>
                  );
                case 'turn':
                  return (
                    <div key={item.key} className={cn('pt-1 pb-2', animate && 'rcs-enter')}>
                      <AssistantTurn
                        entries={item.entries}
                        isStreaming={isLoading && item.key === lastItemKey}
                        onPermissionRespond={onPermissionRespond}
                      />
                    </div>
                  );
                case 'plan':
                  return (
                    <div key={item.key} className={cn('py-1', animate && 'rcs-enter')}>
                      <PlanDisplay entry={item.entry} />
                    </div>
                  );
                case 'output':
                  return (
                    <div key={item.key} className={cn('py-1', animate && 'rcs-enter')}>
                      <CommandOutput text={item.entry.text} />
                    </div>
                  );
                default:
                  return null;
              }
            })}

            {/* 思考指示器 — 三个点（仅等待第一条输出时出现） */}
            {showThinking && (
              <div className="flex items-center gap-2 py-1" aria-hidden="true">
                <span className="chat-typing-indicator">
                  <span></span>
                  <span></span>
                  <span></span>
                </span>
              </div>
            )}
          </>
        )}
        <ConversationScrollButtons />
      </ConversationContent>
    </Conversation>
  );
}

// =============================================================================
// 条目分组 — 用户消息之间的一切助手活动是一个回合
// =============================================================================

type ChatItem =
  | { kind: 'user'; key: string; entry: UserMessageEntry }
  | { kind: 'turn'; key: string; entries: Array<AssistantMessageEntry | ToolCallEntry> }
  | { kind: 'plan'; key: string; entry: PlanDisplayEntry }
  | { kind: 'output'; key: string; entry: CommandOutputEntry };

function buildChatItems(entries: ThreadEntry[]): ChatItem[] {
  const items: ChatItem[] = [];
  let turn: Array<AssistantMessageEntry | ToolCallEntry> = [];

  const flushTurn = () => {
    if (turn.length === 0) return;
    items.push({ kind: 'turn', key: itemKey(turn[0]!), entries: turn });
    turn = [];
  };

  for (const entry of entries) {
    if (entry.type === 'assistant_message' || entry.type === 'tool_call') {
      turn.push(entry);
      continue;
    }
    flushTurn();
    switch (entry.type) {
      case 'user_message':
        items.push({ kind: 'user', key: itemKey(entry), entry });
        break;
      case 'plan':
        items.push({ kind: 'plan', key: itemKey(entry), entry });
        break;
      case 'command_output':
        items.push({ kind: 'output', key: itemKey(entry), entry });
        break;
      default:
        break;
    }
  }
  flushTurn();

  return items;
}

/** Stable key shared by the React list and the entrance-animation ledger. */
function itemKey(entry: ThreadEntry): string {
  if (entry.type === 'user_message') return `user-${entry.id}`;
  if (entry.type === 'assistant_message') return `assistant-${entry.id}`;
  if (entry.type === 'tool_call') return `tool-${entry.toolCall.id}`;
  if (entry.type === 'command_output') return `output-${entry.id}`;
  return `plan-${entry.id}`;
}

// =============================================================================
// 命令输出（/compact 等）— 去掉 ANSI 控制符再显示
// =============================================================================

function CommandOutput({ text }: { text: string }) {
  return (
    <div className="rounded-lg border border-border/60 bg-surface-1/50 px-3 py-2">
      <div className="mb-1 text-[11px] uppercase tracking-wide text-text-muted">命令输出</div>
      <div className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed text-text-secondary">
        {stripAnsi(text)}
      </div>
    </div>
  );
}
