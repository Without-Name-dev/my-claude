import React, { useEffect, useState, useSyncExternalStore } from 'react';
import { Box, Text } from '@anthropic/ink';
import { getWorkflowService } from '../service.js';
import type { RunProgress } from '../progress/store.js';

const EMPTY_RUNS: RunProgress[] = [];

/** Live progress belongs to the input footer so background workflows stay visible. */
export function WorkflowFooter(): React.ReactNode {
  const service = getWorkflowService();
  const runs = useSyncExternalStore(service.subscribe, service.listRuns, () => EMPTY_RUNS);
  const active = runs.filter(run => run.status === 'running');
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    if (active.length === 0) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [active.length]);

  if (active.length === 0) return null;
  return (
    <Box flexDirection="column" paddingX={2}>
      {active.slice(0, 3).map(run => {
        const done = run.agents.filter(agent => agent.status === 'done').length;
        const seconds = Math.max(0, Math.floor((now - run.startedAt) / 1000));
        return (
          <Text key={run.runId} dimColor wrap="truncate">
            ◌ {run.workflowName} · {done}/{run.agentCount} agents · {seconds}s
            {run.currentPhase ? ` · ${run.currentPhase}` : ''}
          </Text>
        );
      })}
      {active.length > 3 && <Text dimColor>+{active.length - 3} workflows</Text>}
    </Box>
  );
}
