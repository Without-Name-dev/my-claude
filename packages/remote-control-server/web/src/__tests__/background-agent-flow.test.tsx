import { expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { buildTaskStateMessage } from '../../../../../src/utils/taskStateMessage';
import { buildTerminalQueueMessage, buildTerminalQueueSnapshot } from '../../../../../src/bridge/terminalQueueMessage';
import { EventBus } from '../../../src/transport/event-bus';
import { normalizePayload } from '../../../src/services/transport';
import { RCSChatAdapter } from '../lib/rcs-chat-adapter';
import { markTerminalQueuedMessages, type BackgroundAgent, type TerminalQueueSnapshot } from '../lib/execution-state';
import type { ThreadEntry } from '../lib/types';
import { ExecutionPanel } from '../components/device/ExecutionPanel';

test('terminal agent snapshot survives server normalization and reconnect replay', () => {
  const agent: BackgroundAgent = {
    id: 'a',
    name: 'verification',
    description: '验证排队消息样式修复',
    status: 'running',
    startTime: Date.now() - 90_000,
    queuedCount: 0,
    tokenCount: 21,
  };
  const message = buildTaskStateMessage('list', [], [agent]);
  const bus = new EventBus();
  bus.publish({
    id: 'event',
    sessionId: 'session',
    type: 'task_state',
    direction: 'inbound',
    payload: normalizePayload('task_state', message),
  });
  let current: BackgroundAgent[] = [];
  const adapter = new RCSChatAdapter('session', () => {}, {
    onBackgroundAgents: agents => {
      current = agents;
    },
  });
  const snapshot = bus.getStateSnapshots()[0]!;
  adapter.handleEvent({ type: snapshot.type, payload: snapshot.payload as Record<string, unknown> });
  expect(current).toEqual([agent]);
  const html = renderToStaticMarkup(createElement(ExecutionPanel, { tasks: [], workflows: [], agents: current }));
  expect(html).toContain('verification');
  expect(html).toContain('验证排队消息样式修复');
  expect(html).toContain('运行中');
  const cleared = buildTaskStateMessage('list', [], []);
  adapter.handleEvent({ type: 'task_state', payload: normalizePayload('task_state', cleared) });
  expect(current).toEqual([]);
});

test('remote queue identity survives CLI, server and web without duplicating a message', () => {
  const uuid = '12345678-1234-1234-1234-123456789abc';
  const message = buildTerminalQueueMessage(
    buildTerminalQueueSnapshot([{ mode: 'prompt', value: 'queued prompt', uuid, bridgeOrigin: true }]),
  );
  let queue: TerminalQueueSnapshot | undefined;
  const adapter = new RCSChatAdapter('session', () => {}, {
    onTerminalQueue: next => {
      queue = next;
    },
  });
  adapter.handleEvent({ type: 'terminal_queue', payload: normalizePayload('terminal_queue', message) });
  const entries: ThreadEntry[] = [{ type: 'user_message', id: `user-${uuid}`, content: 'queued prompt' }];
  expect(markTerminalQueuedMessages(entries, queue ?? null)).toEqual([{ ...entries[0], pending: true }]);
  const empty = buildTerminalQueueMessage(buildTerminalQueueSnapshot([]));
  adapter.handleEvent({ type: 'terminal_queue', payload: normalizePayload('terminal_queue', empty) });
  expect(markTerminalQueuedMessages(entries, queue ?? null)).toEqual(entries);
});
