import type { ThreadEntry, ToolCallEntry, PlanDisplayEntry } from '../../src/lib/types';
import { cn } from '../../src/lib/utils';
import { UserBubble, AssistantBubble } from './MessageBubble';
import { ToolCallGroup } from './ToolCallGroup';
import { PlanDisplay } from './PlanView';
import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
  ConversationScrollButtons,
} from '../ai-elements/conversation';

// =============================================================================
// 统一聊天视图 — Anthropic 编辑式排版
// 无气泡间距，用垂直 rhythm 区分消息块
// =============================================================================

interface ChatViewProps {
  entries: ThreadEntry[];
  isLoading?: boolean;
  onPermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
  emptyTitle?: string;
  emptyDescription?: string;
}

export function ChatView({
  entries,
  isLoading = false,
  onPermissionRespond,
  emptyTitle = '开始对话',
  emptyDescription = '输入消息开始聊天',
}: ChatViewProps) {
  // 将相邻的 ToolCallEntry 合并为一组
  const grouped = groupToolCalls(entries);
  const hasMessages = entries.length > 0;

  // 检查是否正在加载（最后一个条目是用户消息）
  // Keep the indicator visible for every busy turn, including ones started
  // in the terminal and turns currently running a tool.
  const showThinking = isLoading && entries.length > 0;

  return (
    <Conversation className="flex-1">
      <ConversationContent>
        {!hasMessages ? (
          isLoading ? (
            <div role="status" className="flex flex-1 flex-col items-center justify-center gap-4 py-24 text-center">
              <span className="h-6 w-6 animate-spin rounded-full border-2 border-border border-t-brand" />
              <p className="text-sm text-text-muted">{emptyTitle}</p>
            </div>
          ) : (
            <ConversationEmptyState title={emptyTitle} description={emptyDescription} />
          )
        ) : (
          <>
            {grouped.map(item => {
              if (item.type === 'single') {
                return (
                  <div
                    key={item.entry.type === 'tool_call' ? `tool-${item.entry.toolCall.id}` : item.entry.id}
                    className={cn(entrySpacing(entries, item.index), 'rcs-enter')}
                  >
                    <EntryRenderer entry={item.entry} isLoading={isLoading} onPermissionRespond={onPermissionRespond} />
                  </div>
                );
              }
              // 工具调用组 — 紧贴在助手消息下方
              return (
                <div key={`group-${item.entries[0]?.toolCall.id ?? item.index}`} className="-mt-2 rcs-enter">
                  <ToolCallGroup entries={item.entries} onPermissionRespond={onPermissionRespond} />
                </div>
              );
            })}

            {/* 思考指示器 — 三个点 */}
            {showThinking && (
              <div className="flex items-center gap-2 py-1" aria-label="Claude 正在回复">
                <span className="chat-typing-indicator" aria-hidden="true">
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
// 间距逻辑 — 用户消息前后间距大，工具调用紧贴
// =============================================================================

function entrySpacing(entries: ThreadEntry[], index: number): string {
  const entry = entries[index];
  // 用户消息前后大留白 — Claude.ai 式宽松间距
  if (entry?.type === 'user_message') {
    return 'pt-5 pb-1 sm:pt-8';
  }
  // 助手消息 — 工具调用紧贴，否则多留白
  if (entry?.type === 'assistant_message') {
    const next = entries[index + 1];
    if (next?.type === 'tool_call') {
      return 'pt-1 pb-1';
    }
    return 'pt-1 pb-5 sm:pb-7';
  }
  // Plan 条目
  if (entry?.type === 'plan' || entry?.type === 'command_output') {
    return 'pt-1 pb-1';
  }
  return 'py-2';
}

// =============================================================================
// 单条目渲染器
// =============================================================================

function CommandOutput({ text }: { text: string }) {
  return (
    <div className="rounded-lg border border-border/60 bg-surface-1/50 px-3 py-2">
      <div className="mb-1 text-[10px] uppercase tracking-wide text-text-muted">命令输出</div>
      <div className="whitespace-pre-wrap font-mono text-xs leading-relaxed text-text-secondary">{text}</div>
    </div>
  );
}

function EntryRenderer({
  entry,
  isLoading,
  onPermissionRespond,
}: {
  entry: ThreadEntry;
  isLoading: boolean;
  onPermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
}) {
  switch (entry.type) {
    case 'user_message':
      return <UserBubble entry={entry} />;
    case 'assistant_message':
      return <AssistantBubble entry={entry} isStreaming={isLoading} />;
    case 'tool_call':
      return <ToolCallGroup entries={[entry as ToolCallEntry]} onPermissionRespond={onPermissionRespond} />;
    case 'command_output':
      return <CommandOutput text={entry.text} />;
    case 'plan':
      return <PlanDisplay entry={entry as PlanDisplayEntry} />;
    default:
      return null;
  }
}

// =============================================================================
// 工具调用分组逻辑
// =============================================================================

type GroupedItem =
  | { type: 'single'; entry: ThreadEntry; index: number }
  | { type: 'tool_group'; entries: ToolCallEntry[]; index: number };

function groupToolCalls(entries: ThreadEntry[]): GroupedItem[] {
  const result: GroupedItem[] = [];
  let currentToolGroup: ToolCallEntry[] = [];
  let groupStart = 0;

  const flushToolGroup = () => {
    if (currentToolGroup.length === 1) {
      result.push({
        type: 'single',
        entry: currentToolGroup[0]!,
        index: groupStart,
      });
    } else if (currentToolGroup.length > 1) {
      result.push({
        type: 'tool_group',
        entries: currentToolGroup,
        index: groupStart,
      });
    }
    currentToolGroup = [];
  };

  entries.forEach((entry, index) => {
    if (entry.type === 'tool_call') {
      if (currentToolGroup.length === 0) groupStart = index;
      currentToolGroup.push(entry);
    } else {
      flushToolGroup();
      result.push({ type: 'single', entry, index });
    }
  });
  flushToolGroup();

  return result;
}
