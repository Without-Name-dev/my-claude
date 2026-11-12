import * as React from 'react';
import { EffortPanel } from '../../components/EffortPanel/EffortPanel.js';
import { useMainLoopModel } from '../../hooks/useMainLoopModel.js';
import {
  type AnalyticsMetadata_I_VERIFIED_THIS_IS_NOT_CODE_OR_FILEPATHS,
  logEvent,
} from '../../services/analytics/index.js';
import { useAppState, useSetAppState } from '../../state/AppState.js';
import type { LocalJSXCommandOnDone } from '../../types/command.js';
import {
  type EffortValue,
  getDisplayedEffortLevel,
  getEffortEnvOverride,
  getEffortValueDescription,
  isEffortLevel,
  toPersistableEffort,
} from '../../utils/effort.js';
import { updateSettingsForSource } from '../../utils/settings/settings.js';
import { isUltracodeAvailable, ultracodeUnavailableReason } from '../../utils/ultracode.js';

const COMMON_HELP_ARGS = ['help', '-h', '--help'];

type EffortCommandResult = {
  message: string;
  effortUpdate?: { value: EffortValue | undefined };
  /** Session-only: official semantics — ultracode never persists and never
   *  changes effortValue. */
  ultracodeUpdate?: boolean;
};

function setEffortValue(effortValue: EffortValue): EffortCommandResult {
  const persistable = toPersistableEffort(effortValue);
  if (persistable !== undefined) {
    const result = updateSettingsForSource('userSettings', {
      effortLevel: persistable,
    });
    if (result.error) {
      return {
        message: `Failed to set effort level: ${result.error.message}`,
      };
    }
  }
  logEvent('tengu_effort_command', {
    effort: effortValue as AnalyticsMetadata_I_VERIFIED_THIS_IS_NOT_CODE_OR_FILEPATHS,
  });

  // Env var wins at resolveAppliedEffort time. Only flag it when it actually
  // conflicts — if env matches what the user just asked for, the outcome is
  // the same, so "Set effort to X" is true and the note is noise.
  const envOverride = getEffortEnvOverride();
  if (envOverride !== undefined && envOverride !== effortValue) {
    const envRaw = process.env.CLAUDE_CODE_EFFORT_LEVEL;
    if (persistable === undefined) {
      return {
        message: `Not applied: CLAUDE_CODE_EFFORT_LEVEL=${envRaw} overrides effort this session, and ${effortValue} is session-only (nothing saved)`,
        effortUpdate: { value: effortValue },
      };
    }
    return {
      message: `CLAUDE_CODE_EFFORT_LEVEL=${envRaw} overrides this session — clear it and ${effortValue} takes over`,
      effortUpdate: { value: effortValue },
    };
  }

  const description = getEffortValueDescription(effortValue);
  const suffix = persistable !== undefined ? '' : ' (this session only)';
  return {
    message: `Set effort level to ${effortValue}${suffix}: ${description}`,
    effortUpdate: { value: effortValue },
  };
}

export function showCurrentEffort(appStateEffort: EffortValue | undefined, model: string): EffortCommandResult {
  const envOverride = getEffortEnvOverride();
  const effectiveValue = envOverride === null ? undefined : (envOverride ?? appStateEffort);
  if (effectiveValue === undefined) {
    const level = getDisplayedEffortLevel(model, appStateEffort);
    return { message: `Effort level: auto (currently ${level})` };
  }
  const description = getEffortValueDescription(effectiveValue);
  return {
    message: `Current effort level: ${effectiveValue} (${description})`,
  };
}

function unsetEffortLevel(): EffortCommandResult {
  const result = updateSettingsForSource('userSettings', {
    effortLevel: undefined,
  });
  if (result.error) {
    return {
      message: `Failed to set effort level: ${result.error.message}`,
    };
  }
  logEvent('tengu_effort_command', {
    effort: 'auto' as AnalyticsMetadata_I_VERIFIED_THIS_IS_NOT_CODE_OR_FILEPATHS,
  });
  // env=auto/unset (null) matches what /effort auto asks for, so only warn
  // when env is pinning a specific level that will keep overriding.
  const envOverride = getEffortEnvOverride();
  if (envOverride !== undefined && envOverride !== null) {
    const envRaw = process.env.CLAUDE_CODE_EFFORT_LEVEL;
    return {
      message: `Cleared effort from settings, but CLAUDE_CODE_EFFORT_LEVEL=${envRaw} still controls this session`,
      effortUpdate: { value: undefined },
    };
  }
  return {
    message: 'Effort level set to auto',
    effortUpdate: { value: undefined },
  };
}

/**
 * `ultracode`, `ultracode on`, `ultracode off` — session-only toggle with
 * official semantics: availability gated on dynamic workflows + model xhigh
 * support, never persisted, never changes effortValue.
 */
export function setUltracode(
  enabled: boolean | undefined,
  model: string,
  currentEffort: EffortValue | undefined,
): EffortCommandResult {
  if (enabled !== undefined && !isUltracodeAvailable(model)) {
    return { message: ultracodeUnavailableReason(model) };
  }
  const next = enabled ?? true;
  logEvent('tengu_effort_command', {
    effort: (next ? 'ultracode' : 'ultracode_off') as AnalyticsMetadata_I_VERIFIED_THIS_IS_NOT_CODE_OR_FILEPATHS,
  });
  const effortText = getDisplayedEffortLevel(model, currentEffort);
  return {
    message: next
      ? `Ultracode on (this session only): dynamic workflows on every task. Effort stays ${effortText}.`
      : `Ultracode off. Effort stays ${effortText}.`,
    ultracodeUpdate: next,
  };
}

export function executeEffort(
  args: string,
  options?: { model?: string; currentEffort?: EffortValue },
): EffortCommandResult {
  const normalized = args.toLowerCase();
  if (normalized === 'auto' || normalized === 'unset') {
    return unsetEffortLevel();
  }

  // `ultracode` / `ultracode on` / `ultracode off`
  if (normalized === 'ultracode' || normalized.startsWith('ultracode ')) {
    const rest = normalized.slice('ultracode'.length).trim();
    if (rest === '' || rest === 'on') {
      return setUltracode(true, options?.model ?? '', options?.currentEffort);
    }
    if (rest === 'off') {
      return setUltracode(false, options?.model ?? '', options?.currentEffort);
    }
    return {
      message: `Invalid argument: ${args}. Use: ultracode [on|off]`,
    };
  }

  if (!isEffortLevel(normalized)) {
    return {
      message: `Invalid argument: ${args}. Valid options are: low, medium, high, max, auto, ultracode [on|off]`,
    };
  }

  return setEffortValue(normalized);
}

function ShowCurrentEffort({ onDone }: { onDone: (result: string) => void }): React.ReactNode {
  const effortValue = useAppState(s => s.effortValue);
  const model = useMainLoopModel();
  const { message } = showCurrentEffort(effortValue, model);
  onDone(message);
  return null;
}

function ApplyEffortAndClose({
  result,
  onDone,
}: {
  result: EffortCommandResult;
  onDone: (result: string) => void;
}): React.ReactNode {
  const setAppState = useSetAppState();
  const { effortUpdate, ultracodeUpdate, message } = result;
  React.useEffect(() => {
    if (effortUpdate) {
      setAppState(prev => ({
        ...prev,
        effortValue: effortUpdate.value,
      }));
    }
    if (ultracodeUpdate !== undefined) {
      setAppState(prev => ({
        ...prev,
        ultracodeActive: ultracodeUpdate,
      }));
    }
    onDone(message);
  }, [setAppState, effortUpdate, ultracodeUpdate, message, onDone]);
  return null;
}

export async function call(onDone: LocalJSXCommandOnDone, _context: unknown, args?: string): Promise<React.ReactNode> {
  args = args?.trim() || '';

  if (COMMON_HELP_ARGS.includes(args)) {
    onDone(
      'Usage: /effort [low|medium|high|xhigh|max|auto|ultracode [on|off]]\n\nEffort levels:\n- low: Quick, straightforward implementation\n- medium: Balanced approach with standard testing\n- high: Comprehensive implementation with extensive testing\n- xhigh: Extended reasoning beyond high, short of max; including ChatGPT Codex models\n- max: Maximum capability with deepest reasoning\n- auto: Use the default effort level for your model\n- ultracode [on|off]: dynamic workflows on every task (this session only; effort stays unchanged)',
    );
    return;
  }

  if (!args || args === 'current' || args === 'status') {
    if (args === 'current' || args === 'status') {
      return <ShowCurrentEffort onDone={onDone} />;
    }
    // 完全无参 → 打开交互面板
    return <EffortPanelWrapper onDone={onDone} />;
  }

  return <EffortWithModel args={args} onDone={onDone} />;
}

function EffortWithModel({ args, onDone }: { args: string; onDone: (result: string) => void }): React.ReactNode {
  const effortValue = useAppState(s => s.effortValue);
  const model = useMainLoopModel();
  const result = executeEffort(args, { model, currentEffort: effortValue });
  return <ApplyEffortAndClose result={result} onDone={onDone} />;
}

function EffortPanelWrapper({ onDone }: { onDone: (result: string) => void }): React.ReactNode {
  const effortValue = useAppState(s => s.effortValue);
  return <EffortPanel appStateEffort={effortValue} onDone={onDone} />;
}
