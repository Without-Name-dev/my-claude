// Session Configuration API Client

export interface SessionConfig {
  sessionId: string;
  effortLevel: 'low' | 'medium' | 'high' | 'xhigh' | 'max' | 'auto';
  modelName: string;
  fastMode: boolean;
  autoMode: boolean;
  updatedAt: string;
}

export interface UpdateConfigRequest {
  effortLevel?: string;
  modelName?: string;
  fastMode?: boolean;
  autoMode?: boolean;
}

export interface CommandRequest {
  command: string;
}

export interface CommandResult {
  command: string;
  result: Record<string, any>;
}

/**
 * Get session configuration
 */
export async function getSessionConfig(sessionId: string): Promise<SessionConfig> {
  const response = await fetch(`/api/v2/sessions/${sessionId}/config`, {
    method: 'GET',
  });

  if (!response.ok) {
    throw new Error(`Failed to get config: ${response.statusText}`);
  }

  return response.json();
}

/**
 * Update session configuration
 */
export async function updateSessionConfig(
  sessionId: string,
  updates: UpdateConfigRequest
): Promise<SessionConfig> {
  const response = await fetch(`/api/v2/sessions/${sessionId}/config`, {
    method: 'PATCH',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify(updates),
  });

  if (!response.ok) {
    throw new Error(`Failed to update config: ${response.statusText}`);
  }

  return response.json();
}

/**
 * Execute a slash command
 */
export async function executeCommand(
  sessionId: string,
  command: string
): Promise<CommandResult> {
  const response = await fetch(`/api/v2/sessions/${sessionId}/commands/execute`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ command }),
  });

  if (!response.ok) {
    throw new Error(`Failed to execute command: ${response.statusText}`);
  }

  return response.json();
}

// Agent and Workflow API

export interface AgentExecution {
  id: string;
  sessionId: string;
  parentId?: string;
  label: string;
  phase?: string;
  status: 'pending' | 'running' | 'completed' | 'error' | 'cancelled';
  startedAt?: string;
  finishedAt?: string;
  tokenCount: number;
  errorMessage?: string;
}

export interface WorkflowRun {
  id: string;
  sessionId: string;
  name: string;
  status: 'running' | 'completed' | 'error' | 'cancelled';
  currentPhase?: string;
  phaseCount: number;
  agentCount: number;
  totalTokens: number;
  startedAt: string;
  finishedAt?: string;
  errorMessage?: string;
}

/**
 * Get agent executions for a session
 */
export async function getAgentExecutions(sessionId: string): Promise<AgentExecution[]> {
  const response = await fetch(`/api/v2/sessions/${sessionId}/agents`, {
    method: 'GET',
  });

  if (!response.ok) {
    throw new Error(`Failed to get agents: ${response.statusText}`);
  }

  const data = await response.json();
  return data.agents;
}

/**
 * Get workflow runs for a session
 */
export async function getWorkflowRuns(sessionId: string): Promise<WorkflowRun[]> {
  const response = await fetch(`/api/v2/sessions/${sessionId}/workflows`, {
    method: 'GET',
  });

  if (!response.ok) {
    throw new Error(`Failed to get workflows: ${response.statusText}`);
  }

  const data = await response.json();
  return data.workflows;
}
