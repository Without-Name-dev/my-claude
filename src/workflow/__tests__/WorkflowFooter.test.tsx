import { expect, test } from 'bun:test';
import { PassThrough } from 'node:stream';
import React from 'react';
import { wrappedRender as render } from '@anthropic/ink';
import { WorkflowFooter } from '../panel/WorkflowFooter.js';
import type { RunProgress } from '../progress/store.js';
import { __resetWorkflowServiceForTests, getWorkflowService } from '../service.js';

test('footer follows running workflow progress and disappears on completion', async () => {
  __resetWorkflowServiceForTests();
  const service = getWorkflowService();
  const originalList = service.listRuns;
  const originalSubscribe = service.subscribe;
  let runs: RunProgress[] = [];
  let notify: (() => void) | undefined;
  service.listRuns = () => runs;
  service.subscribe = listener => {
    notify = listener;
    return () => {
      notify = undefined;
    };
  };
  const stdout = new PassThrough();
  let output = '';
  stdout.on('data', data => {
    output += data.toString();
  });
  let instance: { unmount: () => void } | undefined;
  try {
    instance = await render(React.createElement(WorkflowFooter), {
      stdout: stdout as unknown as NodeJS.WriteStream,
      patchConsole: false,
    });
    await new Promise(resolve => setTimeout(resolve, 40));
    runs = [
      {
        runId: 'test-run',
        workflowName: 'dynamic-test',
        status: 'running',
        phases: [],
        declaredPhases: [],
        currentPhase: 'review',
        agents: [
          { id: 1, status: 'done' },
          { id: 2, status: 'running' },
        ],
        agentCount: 2,
        startedAt: Date.now(),
        updatedAt: Date.now(),
      },
    ];
    notify?.();
    await new Promise(resolve => setTimeout(resolve, 80));
    expect(output).toContain('dynamic-test');
    expect(output).toContain('1/2 agents');
    expect(output).toContain('review');
    output = '';
    runs = [{ ...runs[0]!, status: 'completed' }];
    notify?.();
    await new Promise(resolve => setTimeout(resolve, 80));
    expect(output).not.toContain('dynamic-test');
  } finally {
    instance?.unmount();
    service.listRuns = originalList;
    service.subscribe = originalSubscribe;
    __resetWorkflowServiceForTests();
  }
});
