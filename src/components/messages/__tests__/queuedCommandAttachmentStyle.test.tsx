/**
 * Regression test: mid-turn-drained queued_command attachments must render
 * with the same full-width userMessageBackground band as real user messages.
 *
 * Bug: queued messages consumed while a turn is running become
 * queued_command attachments (see getQueuedCommandAttachments), rendered via
 * AttachmentMessage → UserTextMessage → UserPromptMessage. The band only
 * paints full-width when an ancestor Box has an explicit width: MessageRow
 * wraps every row in OffscreenFreeze's plain <Box>, whose default
 * flexDirection is "row" — children fit-content, no stretch. Without an
 * explicit width the band shrink-wraps to the text width ("chip" style,
 * ~7 columns), while idle-consumed queue messages (real user messages,
 * width={columns} from Message.tsx case 'user') render the full-width gray
 * band.
 *
 * Provenance note: mock.module('bun:bundle', ...) only affects RUNTIME
 * feature() calls; if/ternary positions are compile-time folded by Bun's
 * transpiler. This test therefore asserts the non-brief render path
 * (useBriefLayout folded false), which is the default for all users.
 */
import { afterAll, expect, mock, test } from 'bun:test';
import { PassThrough } from 'node:stream';
import * as React from 'react';
import { logMock } from '../../../../tests/mocks/log';
import { debugMock } from '../../../../tests/mocks/debug';

mock.module('src/utils/log.ts', logMock);
mock.module('src/utils/debug.ts', debugMock);
mock.module('bun:bundle', () => ({ feature: () => false }));

const { wrappedRender: render, Box, Text, ThemeProvider } = await import('@anthropic/ink');
// Non-TTY stdout → chalk level 0 → no SGR emitted. The chalk singleton's
// level is mutable and read at styling time (colorize.ts's own xterm/tmux
// boost/clamp relies on this), so force truecolor directly — without SGR the
// background band's padding spaces are not emitted at all, and no width
// assertion is possible. Restore the previous level in afterAll so later test
// files in the same process keep their expected color behavior.
const chalk = (await import('chalk')) as unknown as { default: { level: number } };
const _prevChalkLevel = chalk.default.level;
chalk.default.level = 3;
afterAll(() => {
  chalk.default.level = _prevChalkLevel;
});
// Not AppStateProvider: under plain `bun test`, feature('VOICE_MODE') folds to
// false at transpile time (mock.module can't reach if/ternary positions), and
// its noop VoiceProvider destructures an unexported VoiceContext → crash.
// The render path only needs the store context (UserPromptMessage selectors).
const { AppStoreContext } = await import('../../../state/AppState.js');
const { getDefaultAppState } = await import('../../../state/AppStateStore.js');
const { createStore } = await import('../../../state/store.js');
const { AttachmentMessage } = await import('../AttachmentMessage.js');

const TERMINAL_COLUMNS = 80;

function Harness({ children }: { children: React.ReactNode }): React.ReactNode {
  return (
    <ThemeProvider>
      <AppStoreContext.Provider value={createStore(getDefaultAppState())}>{children}</AppStoreContext.Provider>
    </ThemeProvider>
  );
}

const queuedCommand = {
  type: 'queued_command',
  prompt: '测试',
  source_uuid: '11111111-2222-3333-4444-555555555555',
  commandMode: 'prompt',
} as never;

// Fail loudly if an earlier test file leaked a stubbed '@anthropic/ink' into
// this process (mock.module is process-global; onboarding/ultrareview
// re-register pristine snapshots in their afterAll — see those files).
// String stubs (ultrareview: Box: 'Box') fail the typeof checks; function
// stubs rendering pseudo-Elements (onboarding: createElement('box', ...))
// fail the source check.
test('guard: @anthropic/ink must not be stub-polluted by earlier test files', () => {
  expect(typeof Box).toBe('function');
  expect(typeof Text).toBe('function');
  const boxSource = String(Box);
  const isStub = boxSource.includes("createElement('box'") || boxSource.includes('createElement("box"');
  expect(isStub).toBe(false);
});

async function renderAndGetFrame(element: React.ReactNode): Promise<string> {
  const stdout = new PassThrough();
  let all = '';
  stdout.on('data', (d: Buffer) => {
    all += d.toString();
  });
  const instance = await render(element, {
    stdout: stdout as unknown as NodeJS.WriteStream,
    patchConsole: false,
  });
  await new Promise(r => setTimeout(r, 100));
  instance.unmount();
  await new Promise(r => setTimeout(r, 20));
  return all;
}

// The full-width band shows up as a long run of background-styled trailing
// spaces before the bg-reset SGR. A shrink-wrapped "chip" band only carries
// the single paddingRight space.
const FULL_WIDTH_BAND = new RegExp(` {${TERMINAL_COLUMNS - 12},}\\x1b\\[49m`);

test('queued_command attachment paints a full-width user band under OffscreenFreeze-like parent', async () => {
  // MessageRow wraps rows in OffscreenFreeze's plain <Box> — default
  // flexDirection "row", the shape attachments actually render under.
  const frame = await renderAndGetFrame(
    <Harness>
      <Box>
        <AttachmentMessage attachment={queuedCommand} addMargin={false} verbose={false} />
      </Box>
    </Harness>,
  );

  // Sanity: the message actually rendered (guards against silent render crash).
  expect(frame).toContain('测试');
  // The prompt uses an ASCII fallback on Windows terminals.
  expect(/[❯>]/.test(frame)).toBe(true);

  // Regression assertion: band spans (nearly) the whole terminal width.
  expect(FULL_WIDTH_BAND.test(frame)).toBe(true);
});

test('control: same attachment under an explicit-width column Box also paints full-width', async () => {
  const frame = await renderAndGetFrame(
    <Harness>
      <Box flexDirection="column" width={TERMINAL_COLUMNS}>
        <AttachmentMessage attachment={queuedCommand} addMargin={false} verbose={false} />
      </Box>
    </Harness>,
  );

  expect(frame).toContain('测试');
  expect(FULL_WIDTH_BAND.test(frame)).toBe(true);
});
