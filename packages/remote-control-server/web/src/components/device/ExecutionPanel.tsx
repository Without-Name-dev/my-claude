import { memo, useEffect, useState } from 'react';
import { Check, ChevronDown, Circle, Loader2, Square, X } from 'lucide-react';
import type { BackgroundAgent, ExecutionTask, WorkflowRun } from '../../lib/execution-state';
import { splitWorkflowRuns } from '../../lib/execution-state';
import { cn } from '../../lib/utils';

/** Only render state received from the terminal's Bridge. */
export const ExecutionPanel = memo(function ExecutionPanel({
  tasks,
  workflows,
  agents = [],
  connected = true,
  compact = false,
}: {
  tasks: ExecutionTask[];
  workflows: WorkflowRun[];
  agents?: BackgroundAgent[];
  connected?: boolean;
  compact?: boolean;
}) {
  // Collapsed by default: the transcript owns the screen; this is a status
  // strip the user opens on demand, not a panel that pushes messages down.
  const [expanded, setExpanded] = useState(false);
  const [showHistory, setShowHistory] = useState(false);
  const [now, setNow] = useState(Date.now);
  const anyRunning = connected && agents.some(agent => agent.status === 'running' || agent.status === 'pending');
  useEffect(() => {
    if (!anyRunning) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [anyRunning]);
  const { active, history } = splitWorkflowRuns(workflows);
  // Completed task records persist on disk for history and must not keep the
  // current-work panel open forever. Never discard unresolved tasks.
  const visibleTasks = tasks.some(task => task.status !== 'completed') ? tasks : [];
  if (visibleTasks.length === 0 && active.length === 0 && agents.length === 0) return null;
  const done = tasks.filter(task => task.status === 'completed').length;
  const running = active.length;
  const unresolved = new Set(tasks.filter(task => task.status !== 'completed').map(task => task.id));
  const summary = [
    visibleTasks.length > 0 ? `${done}/${tasks.length} 任务` : null,
    running > 0 ? `${running} 个工作流运行中` : null,
  ]
    .filter(Boolean)
    .join(' · ');
  return (
    <section aria-label="任务与工作流" className="shrink-0 px-4 pb-1.5 sm:px-8">
      {agents.length > 0 && (
        <div
          aria-label="后台子代理"
          className="mx-auto mb-1 max-h-[min(20vh,8rem)] w-full space-y-1 overflow-y-auto overscroll-contain rounded-xl bg-surface-1/60 px-3 py-2 text-xs text-text-secondary"
        >
          {!connected && <div className="text-text-muted">终端未连接，显示上次状态</div>}
          {agents.map(agent => {
            const running = agent.status === 'running' || agent.status === 'pending';
            const elapsedMs = Math.max(
              0,
              (running ? now : (agent.endTime ?? agent.startTime)) - agent.startTime - (agent.totalPausedMs ?? 0),
            );
            const seconds = Math.floor(elapsedMs / 1000);
            const duration = seconds >= 60 ? `${Math.floor(seconds / 60)}分${seconds % 60}秒` : `${seconds}秒`;
            const state = running
              ? '运行中'
              : agent.status === 'completed'
                ? '已完成'
                : agent.status === 'failed'
                  ? '失败'
                  : '已停止';
            return (
              <div key={agent.id} className="flex min-w-0 flex-wrap items-start gap-x-2 leading-5">
                {running && connected ? (
                  <Loader2 aria-hidden="true" className="mt-0.5 h-4 w-4 shrink-0 animate-spin text-brand" />
                ) : (
                  <Circle aria-hidden="true" className="mt-1 h-3 w-3 shrink-0" />
                )}
                <div className="min-w-0 flex-1 break-words">
                  <span className="font-medium text-text-primary">{agent.name || '子代理'}</span>
                  {agent.description && ` ${agent.description}`}
                </div>
                <span className="shrink-0 text-right text-text-muted">
                  {connected ? state : '未知'} · {duration}
                  {typeof agent.tokenCount === 'number' && agent.tokenCount > 0
                    ? ` · ${agent.tokenCount.toLocaleString()} tokens`
                    : ''}
                  {agent.queuedCount > 0 ? ` · ${agent.queuedCount} 排队` : ''}
                </span>
              </div>
            );
          })}
        </div>
      )}
      {(visibleTasks.length > 0 || active.length > 0) && (
        <div className="mx-auto w-full overflow-hidden rounded-xl border border-border/60 bg-surface-1/60">
          <button
            type="button"
            onClick={() => setExpanded(value => !value)}
            aria-expanded={expanded}
            className="flex min-h-11 w-full items-center gap-2 px-3 text-left text-[12px] leading-5 text-text-secondary active:bg-surface-2/70 sm:min-h-9"
          >
            <ChevronDown
              className={cn('h-3.5 w-3.5 shrink-0 text-text-muted transition-transform', !expanded && '-rotate-90')}
            />
            <span className="shrink-0 font-medium text-text-primary">任务与工作流</span>
            <span className="min-w-0 flex-1 truncate text-text-muted">{summary}</span>
          </button>
          {expanded && (
            <div
              className={cn(
                'space-y-2.5 overflow-y-auto overscroll-contain border-t border-border/50 px-3 py-2.5',
                compact ? 'max-h-[min(16vh,7rem)] sm:max-h-[min(38vh,20rem)]' : 'max-h-[min(30vh,16rem)]',
              )}
            >
              {visibleTasks.length > 0 && (
                <div aria-label="任务列表" className="space-y-1">
                  {tasks.map(task => {
                    const blocked = task.blockedBy.filter(id => unresolved.has(id));
                    return (
                      <div key={task.id} className="flex items-start gap-2 py-0.5 text-[13px] leading-5">
                        <TaskIcon status={task.status} />
                        <div className="min-w-0 flex-1">
                          <span
                            className={cn(
                              'break-words',
                              task.status === 'completed' ? 'text-text-muted line-through' : 'text-text-primary',
                            )}
                          >
                            {task.subject}
                          </span>
                          {task.owner && <span className="ml-1.5 text-text-muted">@{task.owner}</span>}
                          {blocked.length > 0 && (
                            <span className="ml-1.5 text-text-muted">
                              等待 {blocked.map(id => `#${id}`).join('、')}
                            </span>
                          )}
                          {task.status === 'in_progress' && task.activeForm && task.activeForm !== task.subject && (
                            <div className="text-[12px] text-text-muted">{task.activeForm}</div>
                          )}
                        </div>
                        <span className="sr-only">
                          {task.status === 'completed' ? '已完成' : task.status === 'in_progress' ? '进行中' : '待处理'}
                        </span>
                      </div>
                    );
                  })}
                </div>
              )}
              {active.map(run => (
                <WorkflowCard key={run.runId} run={run} />
              ))}
              {history.length > 0 && (
                <div>
                  <button
                    type="button"
                    onClick={() => setShowHistory(value => !value)}
                    aria-expanded={showHistory}
                    className="min-h-10 w-full px-1 text-left text-xs text-text-muted"
                  >
                    {showHistory ? '隐藏' : '查看'}历史工作流（{history.length}）
                  </button>
                  {showHistory && (
                    <div className="space-y-2">
                      {history.map(run => (
                        <WorkflowCard key={run.runId} run={run} />
                      ))}
                    </div>
                  )}
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </section>
  );
});

function TaskIcon({ status }: { status: ExecutionTask['status'] }) {
  if (status === 'completed') return <Check aria-hidden="true" className="mt-1 h-4 w-4 shrink-0 text-status-success" />;
  if (status === 'in_progress')
    return <Square aria-hidden="true" fill="currentColor" className="mt-1 h-3.5 w-3.5 shrink-0 text-brand" />;
  return <Square aria-hidden="true" className="mt-1 h-3.5 w-3.5 shrink-0 text-text-muted" />;
}

const WorkflowCard = memo(function WorkflowCard({ run }: { run: WorkflowRun }) {
  const [expanded, setExpanded] = useState(false);
  // A script's meta.phases is user-authored and may repeat a title; verbatim
  // copying rendered duplicate blocks, duplicate agent rows and duplicate
  // React keys. The terminal's mergePhases dedupes — match it.
  const phases: string[] = [];
  for (const title of [...run.declaredPhases, ...run.phases.map(phase => phase.title)]) {
    if (!phases.includes(title)) phases.push(title);
  }
  const status = { running: '运行中', completed: '已完成', failed: '失败', killed: '已停止' }[run.status];
  return (
    <div className="rounded-xl border border-border/70 bg-surface-1/50">
      {/* A controlled <details open onToggle> re-applies `open` on every
          snapshot update; with workflow_state arriving continuously that
          showed up on the phone as a card that kept flickering open/closed.
          A button + conditional body keeps expansion purely in React state. */}
      <button
        type="button"
        onClick={() => setExpanded(value => !value)}
        aria-expanded={expanded}
        className="flex min-h-11 w-full items-center gap-2 px-3 text-left text-[13px] text-text-primary active:bg-surface-2/60"
      >
        {run.status === 'running' ? (
          <Loader2 className="h-4 w-4 shrink-0 animate-spin text-brand" />
        ) : run.status === 'completed' ? (
          <Check className="h-4 w-4 shrink-0 text-status-success" />
        ) : (
          <X className="h-4 w-4 shrink-0 text-status-error" />
        )}
        <span className="min-w-0 flex-1 break-words font-medium">{run.workflowName}</span>
        <span className="shrink-0 text-xs text-text-muted">{status}</span>
        <ChevronDown
          className={cn('h-3.5 w-3.5 shrink-0 text-text-muted transition-transform', !expanded && '-rotate-90')}
        />
      </button>
      {expanded && (
        <div className="space-y-2 border-t border-border/60 px-3 py-2 text-[12px] leading-5">
          {run.description && <p className="break-words text-text-secondary">{run.description}</p>}
          {run.error && <p className="break-words text-status-error">{run.error}</p>}
          {phases.map(phase => {
            const state = run.phases.find(item => item.title === phase)?.status;
            const agents = run.agents.filter(agent => agent.phase === phase);
            return (
              <div key={phase} className="space-y-1">
                <div className="flex items-center gap-1.5 text-text-secondary">
                  {state === 'done' ? (
                    <Check className="h-3.5 w-3.5 text-status-success" />
                  ) : state === 'running' ? (
                    <Loader2 className="h-3.5 w-3.5 animate-spin text-brand" />
                  ) : (
                    <Circle className="h-3.5 w-3.5 text-text-muted" />
                  )}
                  <span className="break-words">{phase}</span>
                  <span className="text-text-muted">
                    {state === 'done' ? '已完成' : state === 'running' ? '进行中' : '待开始'}
                  </span>
                </div>
                {agents.map(agent => (
                  <AgentRow key={agent.id} agent={agent} />
                ))}
              </div>
            );
          })}
          {run.agents
            .filter(agent => !agent.phase || !phases.includes(agent.phase))
            .map(agent => (
              <AgentRow key={agent.id} agent={agent} />
            ))}
        </div>
      )}
    </div>
  );
});

function AgentRow({ agent }: { agent: WorkflowRun['agents'][number] }) {
  const label = agent.label || `agent-${agent.id}`;
  const state = agent.status === 'running' ? '运行中' : agent.resultKind === 'dead' ? '失败' : '已完成';
  const meta = [
    agent.model,
    agent.toolCount !== undefined ? `${agent.toolCount} 工具` : undefined,
    agent.tokenCount !== undefined ? `${agent.tokenCount.toLocaleString()} tokens` : undefined,
  ].filter(Boolean);
  return (
    <div className="flex min-w-0 items-start gap-2 pl-5 text-text-muted">
      {agent.status === 'running' ? (
        <Loader2 className="mt-1 h-3 w-3 shrink-0 animate-spin text-brand" />
      ) : agent.resultKind === 'dead' ? (
        <X className="mt-1 h-3 w-3 shrink-0 text-status-error" />
      ) : (
        <Check className="mt-1 h-3 w-3 shrink-0 text-status-success" />
      )}
      <span className="min-w-0 flex-1 break-words text-text-secondary">{label}</span>
      <span className="max-w-[55%] shrink-0 text-right">{[state, ...meta].join(' · ')}</span>
    </div>
  );
}
