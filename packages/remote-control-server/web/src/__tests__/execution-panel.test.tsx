import { expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ExecutionPanel } from '../components/device/ExecutionPanel';
import type { ExecutionTask, WorkflowRun } from '../lib/execution-state';

const complete: ExecutionTask = { id: '1', subject: 'completed work', status: 'completed', blockedBy: [] };
const pending: ExecutionTask = { id: '2', subject: 'unfinished work', status: 'in_progress', blockedBy: [] };

test('completed task records do not keep the current work panel visible', () => {
  expect(renderToStaticMarkup(createElement(ExecutionPanel, { tasks: [complete], workflows: [] }))).toBe('');
});

test('unresolved tasks remain visible until actually updated', () => {
  const html = renderToStaticMarkup(createElement(ExecutionPanel, { tasks: [complete, pending], workflows: [] }));
  expect(html).toContain('1/2 任务');
});

test('running workflow remains visible when task records are complete', () => {
  const workflow: WorkflowRun = {
    runId: 'run-1',
    workflowName: 'work',
    status: 'running',
    declaredPhases: [],
    phases: [],
    agents: [],
  };
  const html = renderToStaticMarkup(createElement(ExecutionPanel, { tasks: [complete], workflows: [workflow] }));
  expect(html).toContain('1 个工作流运行中');
  expect(html).not.toContain('1/1 任务');
});
