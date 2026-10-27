import React from 'react';
import { AgentExecution, WorkflowRun } from '@/api/config-api';

interface AgentTreeProps {
  agents: AgentExecution[];
}

export function AgentTree({ agents }: AgentTreeProps) {
  const [expandedIds, setExpandedIds] = React.useState<Set<string>>(new Set());

  const rootAgents = agents.filter(a => !a.parentId);

  const getChildren = (parentId: string) => {
    return agents.filter(a => a.parentId === parentId);
  };

  const toggleExpand = (id: string) => {
    setExpandedIds(prev => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  if (agents.length === 0) {
    return (
      <div className="text-center py-12 text-gray-500">
        <svg className="w-16 h-16 mx-auto mb-4 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
        </svg>
        <p>没有 Agent 执行记录</p>
      </div>
    );
  }

  return (
    <div className="space-y-2">
      {rootAgents.map(agent => (
        <AgentNode
          key={agent.id}
          agent={agent}
          children={getChildren(agent.id)}
          level={0}
          expandedIds={expandedIds}
          onToggle={toggleExpand}
          getChildren={getChildren}
        />
      ))}
    </div>
  );
}

interface AgentNodeProps {
  agent: AgentExecution;
  children: AgentExecution[];
  level: number;
  expandedIds: Set<string>;
  onToggle: (id: string) => void;
  getChildren: (parentId: string) => AgentExecution[];
}

function AgentNode({ agent, children, level, expandedIds, onToggle, getChildren }: AgentNodeProps) {
  const isExpanded = expandedIds.has(agent.id);
  const hasChildren = children.length > 0;

  const statusConfig = {
    pending: { color: 'text-gray-500', icon: '⏳' },
    running: { color: 'text-blue-500', icon: '▶️' },
    completed: { color: 'text-green-500', icon: '✅' },
    error: { color: 'text-red-500', icon: '❌' },
    cancelled: { color: 'text-orange-500', icon: '⛔' },
  };

  const config = statusConfig[agent.status];

  return (
    <div>
      <div
        className={`flex items-start gap-3 p-3 rounded-lg transition-colors ${
          level === 0 ? 'bg-gray-50' : 'hover:bg-gray-50'
        }`}
        style={{ marginLeft: `${level * 32}px` }}
      >
        {hasChildren && (
          <button
            onClick={() => onToggle(agent.id)}
            className="mt-1 text-gray-400 hover:text-gray-600"
          >
            <svg
              className={`w-4 h-4 transition-transform ${isExpanded ? 'rotate-90' : ''}`}
              fill="none"
              stroke="currentColor"
              viewBox="0 0 24 24"
            >
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
            </svg>
          </button>
        )}

        <span className={`text-lg ${config.color}`}>{config.icon}</span>

        <div className="flex-1">
          <div className="flex items-center gap-2 mb-1">
            <span className={`font-medium ${level === 0 ? 'text-base' : 'text-sm'}`}>
              {agent.label}
            </span>
            {agent.phase && (
              <span className="px-2 py-0.5 text-xs bg-purple-100 text-purple-700 rounded">
                {agent.phase}
              </span>
            )}
          </div>

          <div className="flex items-center gap-3 text-xs text-gray-500">
            {agent.tokenCount > 0 && (
              <span>{agent.tokenCount.toLocaleString()} tokens</span>
            )}
            {agent.startedAt && agent.finishedAt && (
              <span>
                {formatDuration(
                  new Date(agent.finishedAt).getTime() - new Date(agent.startedAt).getTime()
                )}
              </span>
            )}
            {agent.status === 'running' && (
              <div className="flex items-center gap-1">
                <div className="animate-spin h-3 w-3 border-2 border-blue-500 border-t-transparent rounded-full" />
                <span>运行中...</span>
              </div>
            )}
          </div>

          {agent.errorMessage && (
            <p className="mt-2 text-xs text-red-600 bg-red-50 p-2 rounded">
              {agent.errorMessage}
            </p>
          )}
        </div>
      </div>

      {isExpanded && hasChildren && (
        <div>
          {children.map(child => (
            <AgentNode
              key={child.id}
              agent={child}
              children={getChildren(child.id)}
              level={level + 1}
              expandedIds={expandedIds}
              onToggle={onToggle}
              getChildren={getChildren}
            />
          ))}
        </div>
      )}
    </div>
  );
}

interface WorkflowDashboardProps {
  workflows: WorkflowRun[];
}

export function WorkflowDashboard({ workflows }: WorkflowDashboardProps) {
  if (workflows.length === 0) {
    return (
      <div className="text-center py-12 text-gray-500">
        <svg className="w-16 h-16 mx-auto mb-4 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" />
        </svg>
        <p>没有工作流运行记录</p>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      {workflows.map(workflow => (
        <WorkflowCard key={workflow.id} workflow={workflow} />
      ))}
    </div>
  );
}

interface WorkflowCardProps {
  workflow: WorkflowRun;
}

function WorkflowCard({ workflow }: WorkflowCardProps) {
  const statusConfig = {
    running: { color: 'bg-blue-100 text-blue-800', label: '运行中' },
    completed: { color: 'bg-green-100 text-green-800', label: '已完成' },
    error: { color: 'bg-red-100 text-red-800', label: '失败' },
    cancelled: { color: 'bg-gray-100 text-gray-800', label: '已取消' },
  };

  const config = statusConfig[workflow.status];

  return (
    <div className="border rounded-lg p-4 space-y-4">
      <div className="flex items-start justify-between">
        <div>
          <h3 className="font-semibold text-lg">{workflow.name}</h3>
          <p className="text-sm text-gray-500">
            {new Date(workflow.startedAt).toLocaleString('zh-CN')}
          </p>
        </div>
        <span className={`px-3 py-1 text-sm font-medium rounded ${config.color}`}>
          {config.label}
        </span>
      </div>

      {workflow.currentPhase && (
        <div className="space-y-2">
          <div className="flex items-center justify-between text-sm">
            <span className="text-gray-600">当前阶段:</span>
            <span className="font-medium">{workflow.currentPhase}</span>
          </div>
          {workflow.phaseCount > 0 && (
            <div className="w-full bg-gray-200 rounded-full h-2">
              <div
                className="bg-blue-500 h-2 rounded-full transition-all"
                style={{ width: `${(workflow.agentCount / (workflow.phaseCount * 10)) * 100}%` }}
              />
            </div>
          )}
        </div>
      )}

      <div className="grid grid-cols-3 gap-4 text-center">
        <div>
          <div className="text-2xl font-bold text-gray-900">{workflow.agentCount}</div>
          <div className="text-xs text-gray-500">Agents</div>
        </div>
        <div>
          <div className="text-2xl font-bold text-gray-900">{workflow.phaseCount}</div>
          <div className="text-xs text-gray-500">Phases</div>
        </div>
        <div>
          <div className="text-2xl font-bold text-gray-900">
            {formatNumber(workflow.totalTokens)}
          </div>
          <div className="text-xs text-gray-500">Tokens</div>
        </div>
      </div>

      {workflow.errorMessage && (
        <p className="text-sm text-red-600 bg-red-50 p-3 rounded">
          {workflow.errorMessage}
        </p>
      )}
    </div>
  );
}

function formatDuration(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const secs = seconds % 60;
  return `${minutes}m ${secs}s`;
}

function formatNumber(num: number): string {
  if (num >= 1000000) return `${(num / 1000000).toFixed(1)}M`;
  if (num >= 1000) return `${(num / 1000).toFixed(1)}K`;
  return num.toString();
}
