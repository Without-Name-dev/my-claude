import { memo, useMemo, useState } from 'react';
import type { ToolCallEntry, ToolCallData } from '../../src/lib/types';
import { cn } from '../../src/lib/utils';
import { ToolPermissionButtons } from '../ai-elements/permission-request';
import { Check, Clock, Loader2, X } from 'lucide-react';

// =============================================================================
// 工具调用折叠组 — Anthropic: subtle card, left-border accent, compact layout
// =============================================================================

interface ToolCallGroupProps {
  entries: ToolCallEntry[];
  onPermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
}

export function ToolCallGroup({ entries, onPermissionRespond }: ToolCallGroupProps) {
  const [expanded, setExpanded] = useState(false);

  if (entries.length === 0) return null;

  // 单个工具调用 — 默认折叠，不展开内容详情
  if (entries.length === 1) {
    return (
      <div>
        <SingleToolCard tool={entries[0].toolCall} compact onPermissionRespond={onPermissionRespond} />
      </div>
    );
  }

  // 多个工具调用 — 折叠组
  const summary = buildSummary(entries);

  return (
    <div>
      <div className="rounded-xl border border-border/70 bg-surface-1/60 overflow-hidden">
        {/* 折叠头 */}
        <button
          type="button"
          className="w-full flex items-center gap-2 px-3 py-2 text-sm text-text-secondary hover:bg-surface-1/50 transition-colors"
          onClick={() => setExpanded(!expanded)}
        >
          <svg
            width="12"
            height="12"
            viewBox="0 0 12 12"
            fill="none"
            className={cn('transition-transform text-text-muted', expanded && 'rotate-90')}
          >
            <path d="M4 2L8 6L4 10" stroke="currentColor" strokeWidth="1.5" fill="none" />
          </svg>
          <span className="text-xs text-text-muted font-display">{summary}</span>
        </button>

        {/* 展开内容 */}
        {expanded && (
          <div className="border-t border-border divide-y divide-border">
            {entries.map((entry, i) => (
              <SingleToolCard
                key={entry.toolCall.id || i}
                tool={entry.toolCall}
                compact
                onPermissionRespond={onPermissionRespond}
              />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// =============================================================================
// 单个工具卡片 — compact, left-accent, inline status
// =============================================================================

interface SingleToolCardProps {
  tool: ToolCallData;
  compact?: boolean;
  onPermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
}

const SingleToolCard = memo(function SingleToolCard({ tool, compact, onPermissionRespond }: SingleToolCardProps) {
  const [expanded, setExpanded] = useState(!compact);
  // Stringifying a large tool payload on every streamed token was pure waste;
  // the body only renders when the card is open.
  const inputJson = useMemo(() => (expanded ? JSON.stringify(tool.rawInput, null, 2) : ''), [expanded, tool.rawInput]);

  const statusIcon = (() => {
    switch (tool.status) {
      case 'running':
        return <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-status-running" aria-label="运行中" />;
      case 'complete':
        return <Check className="h-3.5 w-3.5 shrink-0 text-status-success" aria-label="已完成" />;
      case 'error':
        return <X className="h-3.5 w-3.5 shrink-0 text-status-error" aria-label="失败" />;
      case 'waiting_for_confirmation':
        return <Clock className="h-3.5 w-3.5 shrink-0 text-brand" aria-label="等待确认" />;
      case 'canceled':
        return <span className="text-text-muted text-[10px]">&#8212;</span>;
      case 'rejected':
        return <span className="text-status-error text-[10px]">&#10005;</span>;
      default:
        return null;
    }
  })();

  const hasOutput =
    tool.status !== 'running' && tool.status !== 'waiting_for_confirmation' && (tool.rawOutput || tool.content);

  return (
    <div className={cn('px-3 py-2', compact && 'py-1.5')}>
      {/* 标题行 — 单行紧凑 */}
      <button
        type="button"
        className="flex w-full items-center gap-1.5 text-left group"
        onClick={() => setExpanded(!expanded)}
      >
        {statusIcon}
        <span className="text-xs font-display font-medium text-text-secondary group-hover:text-text-primary transition-colors truncate">
          {tool.title}
        </span>
        {tool.status === 'running' && <span className="text-[10px] text-status-running animate-pulse">运行中</span>}
      </button>

      {/* 权限请求按钮 */}
      {tool.status === 'waiting_for_confirmation' && tool.permissionRequest && (
        <div className="mt-1.5 ml-4">
          <ToolPermissionButtons
            requestId={tool.permissionRequest.requestId}
            options={tool.permissionRequest.options}
            onRespond={onPermissionRespond || (() => {})}
          />
        </div>
      )}

      {/* 展开详情 */}
      {expanded && (
        <div className="mt-1.5 ml-4 space-y-1.5">
          {tool.rawInput && Object.keys(tool.rawInput).length > 0 && (
            <div>
              <pre className="text-[11px] bg-surface-1 rounded-md p-2 overflow-x-auto font-mono max-h-36 text-text-secondary">
                {truncate(inputJson, 2000)}
              </pre>
            </div>
          )}
          {hasOutput && (
            <div>
              <pre
                className={cn(
                  'text-[11px] rounded-md p-2 overflow-x-auto font-mono max-h-36',
                  tool.status === 'error' ? 'bg-status-error/10 text-status-error' : 'bg-surface-1 text-text-secondary',
                )}
              >
                {formatOutput(tool)}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
});

// =============================================================================
// 工具函数
// =============================================================================

/** 构建统计摘要 */
function buildSummary(entries: ToolCallEntry[]): string {
  const toolCounts = new Map<string, number>();
  for (const entry of entries) {
    const name = simplifyToolName(entry.toolCall.title);
    toolCounts.set(name, (toolCounts.get(name) || 0) + 1);
  }

  const parts: string[] = [];
  for (const [name, count] of toolCounts) {
    parts.push(count === 1 ? name : `${count} 次${name}`);
  }

  if (parts.length === 0) return `${entries.length} 个工具调用`;
  if (parts.length === 1) return parts[0];
  return `${entries.length} 个工具: ${parts.join('、')}`;
}

/** 简化工具名称 */
function simplifyToolName(title: string): string {
  const match = title.match(/^(\w+)/);
  return match ? match[1] : title;
}

/** 格式化工具输出 */
function formatOutput(tool: ToolCallData): string {
  if (tool.content && tool.content.length > 0) {
    const texts = tool.content
      .filter((c): c is Extract<typeof c, { type: 'content' }> => c.type === 'content')
      .filter(c => c.content.type === 'text' && 'text' in c.content)
      .map(c => (c.content as { text: string }).text);
    if (texts.length > 0) return truncate(texts.join('\n'), 2000);
  }
  if (tool.rawOutput && Object.keys(tool.rawOutput).length > 0) {
    return truncate(JSON.stringify(tool.rawOutput, null, 2), 2000);
  }
  return '';
}

function truncate(str: string, max: number): string {
  return str.length > max ? str.slice(0, max) + '...' : str;
}
