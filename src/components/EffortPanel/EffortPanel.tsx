import * as React from 'react';
import { Box, Text, useTerminalSize } from '@anthropic/ink';
import { useKeybindings } from '../../keybindings/useKeybinding.js';
import { type EffortValue, getDisplayedEffortLevel, getEffortEnvOverride } from '../../utils/effort.js';
import {
  type PanelPosition,
  CANCEL_MESSAGE,
  computeConfirmOutcome,
  getInitialCursor,
  moveLeft,
  moveRight,
  PANEL_POSITIONS,
} from './effortPanelState.js';
import { executeEffort, setUltracode } from '../../commands/effort/effort.js';
import { useMainLoopModel } from '../../hooks/useMainLoopModel.js';
import { useAppState, useSetAppState } from '../../state/AppState.js';
import { isUltracodeAvailable, ultracodeUnavailableReason } from '../../utils/ultracode.js';

/**
 * 每档最小宽度（保证档位名可读）。
 */
const MIN_SEGMENT = 12;

/**
 * 根据终端宽度计算每档实际宽度（SEGMENT）。
 *
 * 规则：
 * - 留出 paddingX={1} 的左右各 1 列 → 可用宽度 = columns - 2
 * - 若可用宽度 <= MIN_SEGMENT * 5，用 MIN_SEGMENT
 * - 否则铺满：floor(可用宽度 / 5)
 */
function computeSegment(terminalColumns: number): number {
  const available = terminalColumns - 2; // paddingX={1} 两侧
  const minNeeded = MIN_SEGMENT * PANEL_POSITIONS.length;
  if (available <= minNeeded) return MIN_SEGMENT;
  return Math.floor(available / PANEL_POSITIONS.length);
}

type Props = {
  appStateEffort: EffortValue | undefined;
  onDone: (message: string) => void;
};

export function EffortPanel({ appStateEffort, onDone }: Props): React.ReactNode {
  const setAppState = useSetAppState();
  const model = useMainLoopModel();
  const ultracodeActive = useAppState(s => s.ultracodeActive) ?? false;
  const ultracodeUsable = isUltracodeAvailable(model);
  const { columns } = useTerminalSize();
  // 自适应宽度：根据终端列数计算每档宽度。
  // 终端变化（resize）时 columns 改变 → 重新计算 → 重渲染。
  const segment = React.useMemo(() => computeSegment(columns), [columns]);
  const panelWidth = segment * PANEL_POSITIONS.length;

  const envOverride = getEffortEnvOverride();
  const displayed = getDisplayedEffortLevel(model, appStateEffort);
  const initialCursor = getInitialCursor({ envOverride, appStateEffort, displayed });

  const [cursor, setCursor] = React.useState<PanelPosition>(initialCursor);
  const [done, setDone] = React.useState(false);

  const handleConfirm = React.useCallback(() => {
    if (done) return;
    setDone(true);
    const outcome = computeConfirmOutcome(cursor, executeEffort);
    if (outcome.kind === 'apply' && outcome.effortUpdate) {
      setAppState(prev => ({
        ...prev,
        effortValue: outcome.effortUpdate!.value,
      }));
    }
    onDone(outcome.message);
  }, [cursor, done, onDone, setAppState]);

  // Tab toggles ultracode — a session-only switch, exactly like the official
  // CLI: effort stays untouched, nothing is persisted.
  const handleToggleUltracode = React.useCallback(() => {
    if (done) return;
    if (!ultracodeUsable) {
      onDone(ultracodeUnavailableReason(model));
      setDone(true);
      return;
    }
    const next = !ultracodeActive;
    setAppState(prev => ({ ...prev, ultracodeActive: next }));
    const result = setUltracode(next, model, appStateEffort);
    // setUltracode repeats the state for the message; it is already applied.
    setDone(true);
    onDone(result.message);
  }, [done, ultracodeUsable, ultracodeActive, model, appStateEffort, onDone, setAppState]);

  const handleCancel = React.useCallback(() => {
    if (done) return;
    setDone(true);
    onDone(CANCEL_MESSAGE);
  }, [done, onDone]);

  useKeybindings(
    {
      'effortPanel:decrease': () => setCursor(c => moveLeft(c)),
      'effortPanel:increase': () => setCursor(c => moveRight(c)),
      'effortPanel:home': () => setCursor('low'),
      'effortPanel:end': () => setCursor('max'),
      'effortPanel:confirm': handleConfirm,
      'effortPanel:cancel': handleCancel,
      'effortPanel:toggleUltracode': handleToggleUltracode,
    },
    { context: 'EffortPanel' },
  );

  const envActive = envOverride !== null && envOverride !== undefined;
  const envRaw = process.env.CLAUDE_CODE_EFFORT_LEVEL;

  return (
    <Box flexDirection="column" paddingX={1} width={panelWidth + 2}>
      <Text bold color="suggestion">
        Effort
      </Text>
      {envActive && <Text color="warning">{`⚠ CLAUDE_CODE_EFFORT_LEVEL=${envRaw} overrides this session`}</Text>}
      <PlainContent cursor={cursor} segment={segment} panelWidth={panelWidth} />
      <Box marginTop={1}>
        <Text bold={ultracodeActive} color={ultracodeActive ? 'suggestion' : 'subtle'} dimColor={!ultracodeUsable}>
          {`Ultracode  ${ultracodeActive ? '● on' : ultracodeUsable ? '○ off' : '✕ unavailable'}`}
        </Text>
      </Box>
      <Box marginTop={1}>
        <Text color="subtle">
          {ultracodeUsable
            ? '←/→ adjust · Tab toggle ultracode · Enter confirm · Esc cancel'
            : '←/→ adjust · Enter confirm · Esc cancel'}
        </Text>
      </Box>
    </Box>
  );
}

// ---- 普通模式（无波纹）----

function PlainContent({
  cursor,
  segment,
  panelWidth,
}: {
  cursor: PanelPosition;
  segment: number;
  panelWidth: number;
}): React.ReactNode {
  return (
    <>
      <Box marginTop={1} flexDirection="row" justifyContent="space-between">
        <Text color="suggestion">Faster</Text>
        <Text color="suggestion">Smarter</Text>
      </Box>
      <Text color="subtle">{'─'.repeat(panelWidth)}</Text>
      <Box flexDirection="row">
        {PANEL_POSITIONS.map(p => (
          <Box key={`cursor-${p}`} width={segment} justifyContent="center">
            <Text bold color={cursor === p ? 'suggestion' : 'subtle'}>
              {cursor === p ? '▲' : ' '}
            </Text>
          </Box>
        ))}
      </Box>
      <Box flexDirection="row">
        {PANEL_POSITIONS.map(p => (
          <Box key={`label-${p}`} width={segment} justifyContent="center">
            <Text bold={cursor === p} color={cursor === p ? 'suggestion' : 'subtle'}>
              {p}
            </Text>
          </Box>
        ))}
      </Box>
    </>
  );
}
