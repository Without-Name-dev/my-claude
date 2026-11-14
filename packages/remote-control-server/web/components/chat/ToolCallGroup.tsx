import { memo, useMemo, useState } from 'react';
import type { ToolCallEntry, ToolCallData } from '../../src/lib/types';
import { cn, stripAnsi } from '../../src/lib/utils';
import { ToolPermissionButtons } from '../ai-elements/permission-request';
import { Bot, Check, ChevronRight, Clock, Loader2, Minus, Workflow, X } from 'lucide-react';

// =============================================================================
// 工具调用折叠组 — 一次连续的工具活动收敛成一张安静的小卡片
// =============================================================================

interface ToolCallGroupProps {
  entries: ToolCallEntry[];
  onPermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
}

export function ToolCallGroup({ entries, onPermissionRespond }: ToolCallGroupProps) {
  const [expanded, setExpanded] = useState(false);

  if (entries.length === 0) return null;

  const hasWaiting = entries.some(entry => entry.toolCall.status === 'waiting_for_confirmation');
  const hasRunning = entries.some(entry => entry.toolCall.status === 'running');

  // 单个工具 — 一行，点开看参数和输出
  if (entries.length === 1) {
    return (
      <div className="overflow-hidden rounded-lg border border-border/50 bg-surface-1/30">
        <SingleToolCard tool={entries[0]!.toolCall} onPermissionRespond={onPermissionRespond} />
      </div>
    );
  }

  // 多个工具 — 折叠成一句摘要；有权限等待或正在运行时直接展开
  const open = expanded || hasWaiting;

  return (
    <div className="overflow-hidden rounded-lg border border-border/50 bg-surface-1/30">
      <button
        type="button"
        className="flex w-full items-center gap-2 px-2.5 py-1.5 text-left transition-colors active:bg-surface-1/60"
        onClick={() => setExpanded(value => !value)}
        aria-expanded={open}
      >
        <ChevronRight className={cn('h-3 w-3 shrink-0 text-text-muted/70 transition-transform', open && 'rotate-90')} />
        <span className="min-w-0 flex-1 truncate text-[12px] text-text-muted">{buildSummary(entries)}</span>
        {hasWaiting ? (
          <span className="flex shrink-0 items-center gap-1 text-[11px] text-brand-dark dark:text-brand">
            <Clock className="h-3 w-3" /> 等待确认
          </span>
        ) : hasRunning ? (
          <Loader2 className="h-3 w-3 shrink-0 animate-spin text-status-running" aria-label="运行中" />
        ) : null}
      </button>

      {open && (
        <div className="divide-y divide-border/40 border-t border-border/50">
          {entries.map((entry, index) => (
            <SingleToolCard
              key={entry.toolCall.id || index}
              tool={entry.toolCall}
              onPermissionRespond={onPermissionRespond}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// =============================================================================
// 单个工具 — 状态图标 + 名称 + 关键参数（命令 / 文件路径…）
// =============================================================================

interface SingleToolCardProps {
  tool: ToolCallData;
  onPermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
}

const SingleToolCard = memo(function SingleToolCard({ tool, onPermissionRespond }: SingleToolCardProps) {
  const [expanded, setExpanded] = useState(false);
  const isAgent = /^(agent|task)$/i.test(tool.title);
  const isWorkflow = /^workflow$/i.test(tool.title);
  const isWorkUnit = isAgent || isWorkflow;
  const hint = useMemo(() => toolHint(tool), [tool.rawInput, tool.title]);
  // 大参数只在展开时序列化，流式期间不做无用功。
  const inputJson = useMemo(() => (expanded ? JSON.stringify(tool.rawInput, null, 2) : ''), [expanded, tool.rawInput]);

  const statusIcon = (() => {
    switch (tool.status) {
      case 'running':
        return <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-status-running" aria-label="运行中" />;
      case 'unknown':
        return <Clock className="h-3.5 w-3.5 shrink-0 text-text-muted" aria-label="状态未知" />;
      case 'complete':
        return <Check className="h-3.5 w-3.5 shrink-0 text-status-success" aria-label="已完成" />;
      case 'error':
        return <X className="h-3.5 w-3.5 shrink-0 text-status-error" aria-label="失败" />;
      case 'waiting_for_confirmation':
        return <Clock className="h-3.5 w-3.5 shrink-0 text-brand-dark dark:text-brand" aria-label="等待确认" />;
      case 'canceled':
        return <Minus className="h-3.5 w-3.5 shrink-0 text-text-muted" aria-label="已取消" />;
      case 'rejected':
        return <X className="h-3.5 w-3.5 shrink-0 text-status-error" aria-label="已拒绝" />;
      default:
        return null;
    }
  })();

  const hasOutput =
    tool.status !== 'running' && tool.status !== 'waiting_for_confirmation' && (tool.rawOutput || tool.content);
  const hasInput = Boolean(tool.rawInput && Object.keys(tool.rawInput).length > 0);
  const canExpand = hasInput || Boolean(hasOutput) || Boolean(tool.agentActivity?.length);

  return (
    <div className={cn('px-2.5 py-1.5', isWorkUnit && 'border-l-2 border-brand/50 bg-brand/5 px-3 py-2')}>
      <button
        type="button"
        className="flex w-full min-w-0 items-center gap-2 text-left"
        onClick={() => canExpand && setExpanded(value => !value)}
        aria-expanded={canExpand ? expanded : undefined}
      >
        {statusIcon}
        {isAgent ? (
          <Bot aria-hidden="true" className="h-4 w-4 shrink-0 text-brand" />
        ) : isWorkflow ? (
          <Workflow aria-hidden="true" className="h-4 w-4 shrink-0 text-brand" />
        ) : null}
        <span className="shrink-0 text-[13px] font-medium leading-5 text-text-secondary">
          {isAgent ? '子代理' : isWorkflow ? '工作流' : tool.title}
        </span>
        {hint && <span className="min-w-0 flex-1 truncate text-[12px] leading-5 text-text-muted">{hint}</span>}
        {isWorkUnit && <span className="shrink-0 text-[11px] text-text-muted">{statusLabel(tool.status)}</span>}
        {!isWorkUnit && tool.status === 'running' && (
          <span className="shrink-0 text-[11px] text-status-running">运行中</span>
        )}
        {canExpand && (
          <ChevronRight
            className={cn(
              'h-3 w-3 shrink-0 text-text-muted/60 transition-transform',
              !hint && 'ml-auto',
              expanded && 'rotate-90',
            )}
          />
        )}
      </button>

      {isAgent && typeof tool.rawInput?.subagent_type === 'string' && (
        <div className="mt-1 pl-6 text-[11px] text-text-muted">类型：{tool.rawInput.subagent_type}</div>
      )}
      {isAgent && tool.agentActivity && tool.agentActivity.length > 0 && (
        <div className="mt-1.5 space-y-1 border-l border-border/70 pl-3 ml-1.5">
          {(expanded ? tool.agentActivity : tool.agentActivity.slice(-3)).map(activity => (
            <div key={activity.id} className="min-w-0 text-[12px] leading-5 text-text-secondary">
              <div className="flex items-start gap-1.5">
                {activity.status === 'running' ? (
                  <Loader2 aria-label="运行中" className="mt-1 h-3 w-3 shrink-0 animate-spin text-status-running" />
                ) : activity.status === 'error' ? (
                  <X aria-label="失败" className="mt-1 h-3 w-3 shrink-0 text-status-error" />
                ) : (
                  <Check aria-label="已完成" className="mt-1 h-3 w-3 shrink-0 text-status-success" />
                )}
                <span className="shrink-0 font-medium">{activity.name || '工具'}</span>
                {activity.hint && <span className="min-w-0 truncate text-text-muted">{activity.hint}</span>}
              </div>
              {expanded && activity.output && (
                <pre className="mt-1 max-h-28 overflow-auto whitespace-pre-wrap break-all rounded bg-surface-1 p-2 text-[11px] text-text-muted">
                  {stripAnsi(activity.output)}
                </pre>
              )}
            </div>
          ))}
          {!expanded && tool.agentActivity.length > 3 && (
            <span className="text-[11px] text-text-muted">还有 {tool.agentActivity.length - 3} 条活动，展开查看</span>
          )}
        </div>
      )}

      {/* 权限请求按钮 — 只有这个视图真的能回答时才显示 */}
      {tool.status === 'waiting_for_confirmation' && tool.permissionRequest && onPermissionRespond && (
        <div className="mt-1.5 ml-4">
          <ToolPermissionButtons
            requestId={tool.permissionRequest.requestId}
            options={tool.permissionRequest.options}
            onRespond={onPermissionRespond}
          />
        </div>
      )}

      {expanded && (
        <div className="mt-1.5 ml-4 space-y-1.5">
          {hasInput && (
            <pre className="max-h-40 overflow-x-auto rounded-md bg-surface-1 p-2 font-mono text-[11px] text-text-secondary">
              {truncate(inputJson, 2000)}
            </pre>
          )}
          {hasOutput && (
            <pre
              className={cn(
                'max-h-40 overflow-x-auto rounded-md p-2 font-mono text-[11px]',
                tool.status === 'error' ? 'bg-status-error/10 text-status-error' : 'bg-surface-1 text-text-secondary',
              )}
            >
              {formatOutput(tool)}
            </pre>
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
  if (parts.length === 1) return parts[0]!;
  return `${entries.length} 个工具: ${parts.join('、')}`;
}

/** 简化工具名称 */
function simplifyToolName(title: string): string {
  const match = title.match(/^(\w+)/);
  return match ? match[1]! : title;
}

function statusLabel(status: ToolCallData['status']): string {
  return {
    running: '运行中',
    unknown: '状态未知',
    complete: '已完成',
    error: '失败',
    waiting_for_confirmation: '等待确认',
    rejected: '已拒绝',
    canceled: '已取消',
  }[status];
}

/** 从工具入参里取一条最能说明问题的一行摘要 */
function toolHint(tool: ToolCallData): string {
  const input = tool.rawInput;
  if (!input) return '';

  const pick = (...keys: string[]): string => {
    for (const key of keys) {
      const value = input[key];
      if (typeof value === 'string' && value.trim()) return value;
      if (Array.isArray(value)) {
        const strings = value.filter((item): item is string => typeof item === 'string');
        if (strings.length > 0) return strings.join(' ');
      }
    }
    return '';
  };

  const name = simplifyToolName(tool.title).toLowerCase();
  let hint = '';
  if (name === 'bash' || name === 'shell') hint = pick('command');
  else if (name === 'read' || name === 'edit' || name === 'write' || name === 'multiedit') {
    hint = pick('file_path', 'path', 'notebook_path');
  } else if (name === 'grep' || name === 'glob') hint = pick('pattern');
  else if (name === 'webfetch') hint = pick('url');
  else if (name === 'websearch') hint = pick('query');
  else if (name === 'task' || name === 'agent') hint = pick('description', 'prompt');
  else if (name === 'workflow') hint = pick('title', 'name', 'description', 'scriptPath');

  if (!hint) hint = pick('command', 'file_path', 'path', 'pattern', 'query', 'url', 'description');
  return hint.replace(/\s+/g, ' ').trim();
}

/** 格式化工具输出（去掉终端控制符） */
function formatOutput(tool: ToolCallData): string {
  if (tool.content && tool.content.length > 0) {
    const texts = tool.content
      .filter((c): c is Extract<typeof c, { type: 'content' }> => c.type === 'content')
      .filter(c => c.content.type === 'text' && 'text' in c.content)
      .map(c => (c.content as { text: string }).text);
    if (texts.length > 0) return truncate(stripAnsi(texts.join('\n')), 2000);
  }
  if (tool.rawOutput && Object.keys(tool.rawOutput).length > 0) {
    return truncate(stripAnsi(JSON.stringify(tool.rawOutput, null, 2)), 2000);
  }
  return '';
}

function truncate(str: string, max: number): string {
  return str.length > max ? str.slice(0, max) + '...' : str;
}
