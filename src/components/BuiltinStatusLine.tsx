import React, { useEffect, useState } from 'react';
import { Text, stringWidth } from '@anthropic/ink';
import { formatTokens, truncate } from '../utils/format.js';
import { useTerminalSize } from '../hooks/useTerminalSize.js';

type RateLimitBucket = {
  utilization: number;
  resets_at: number;
};

type BuiltinStatusLineProps = {
  modelName: string;
  /** Effective effort level for this model (always shown). */
  effortLevel?: string;
  /** Ultracode is a session-only mode; render it next to the level. */
  ultracodeActive?: boolean;
  contextUsedPct: number;
  usedTokens: number;
  contextWindowSize: number;
  /** Cumulative session input tokens (all requests so far). */
  totalInputTokens: number;
  /** Cumulative session output tokens (all requests so far). */
  totalOutputTokens: number;
  /**
   * Conversation-average cache hit rate: total cache reads over every prompt
   * token billed this session (input + cache write + cache read).
   */
  cacheHitRate: number | null;
  rateLimits: {
    five_hour?: RateLimitBucket;
    seven_day?: RateLimitBucket;
  };
};

/** 10-cell usage bar: ███░░░░░░░ */
function contextBar(pct: number): string {
  const filled = Math.min(10, Math.max(0, Math.round(pct / 10)));
  return '█'.repeat(filled) + '░'.repeat(10 - filled);
}

/**
 * Format a countdown from now until the given epoch time (in seconds).
 * Returns a compact human-readable string like "3h12m", "5d20h", "45m", or "now".
 */
export function formatCountdown(epochSeconds: number): string {
  const diff = epochSeconds - Date.now() / 1000;
  if (diff <= 0) return 'now';

  const days = Math.floor(diff / 86400);
  const hours = Math.floor((diff % 86400) / 3600);
  const minutes = Math.floor((diff % 3600) / 60);

  if (days >= 1) return `${days}d${hours}h`;
  if (hours >= 1) return `${hours}h${minutes}m`;
  return `${minutes}m`;
}

const SEP = ' │ ';

/**
 * Model segment of the status row: `model · xhigh`, or
 * `model ● ultracode(xhigh)` when the session-only mode is on. Effort is
 * always shown (the row never scrolls away) and the level always survives
 * truncation because callers measure this whole string.
 */
export function formatModelWithMode(
  modelName: string,
  effortLevel: string | undefined,
  ultracodeActive: boolean | undefined,
): string {
  if (effortLevel === undefined) return modelName;
  return ultracodeActive ? `${modelName} ● ultracode(${effortLevel})` : `${modelName} · ${effortLevel}`;
}

export type StatusRowInput = {
  modelName: string;
  effortLevel?: string;
  ultracodeActive?: boolean;
  contextUsedPct: number;
  usedTokens: number;
  contextWindowSize: number;
  totalInputTokens: number;
  totalOutputTokens: number;
  cacheHitRate: number | null;
  /** Pre-formatted rate-limit segments, joined with SEP by the caller. */
  limitText?: string;
};

/**
 * Compose the whole status row into ONE string that fits `columns`.
 *
 * The row used to be a set of flex children. When any of them was narrowed
 * (a footer notification competing for space, a resize) Ink wrapped each child
 * inside its own column, interleaving the row into fragments like
 * "cac" / "76%)" on the next line. A single measured, truncating string can
 * only ever lose tail characters — it cannot wrap.
 *
 * Pure and exported so the fitting rules are unit-testable.
 */
export function composeStatusRow(input: StatusRowInput, columns: number): string {
  // The row owns a full-width line; only the footer's paddingX={2} (both
  // sides) is taken. Measure with stringWidth: `.length` undercounts CJK.
  const budget = Math.max(20, columns - 4);

  // Model display: first two words (e.g. "Opus 4.6"). The effort level rides
  // along — this row is always visible, unlike the logo that scrolls away.
  const modelParts = input.modelName.split(' ');
  const shortModel = modelParts.length >= 2 ? `${modelParts[0]} ${modelParts[1]}` : input.modelName;
  const modeSuffix =
    input.effortLevel === undefined
      ? ''
      : input.ultracodeActive
        ? ` ● ultracode(${input.effortLevel})`
        : ` · ${input.effortLevel}`;
  let line = shortModel + modeSuffix;

  const contextCore = `${contextBar(input.contextUsedPct)} ${input.contextUsedPct}%`;
  let ctx = `${contextCore} (${formatTokens(input.usedTokens)}/${formatTokens(input.contextWindowSize)})`;

  // Tokens: session total plus the conversation-average cache hit rate. The
  // in/out split was dropped — rarely actionable, and it doubled the width.
  const tokenCore = `Tokens ${formatTokens(input.totalInputTokens + input.totalOutputTokens)}`;
  let tokens = tokenCore + (input.cacheHitRate !== null ? ` (cache ${input.cacheHitRate}%)` : '');

  let limits = input.limitText ?? '';

  const sepWidth = stringWidth(SEP);
  const fits = (candidate: string) => stringWidth(candidate) <= budget;
  const join = () => [line, ctx, tokens, limits].filter(Boolean).join(SEP);

  // Shed detail right-to-left until the row fits.
  if (!fits(join())) limits = ''; // claude.ai rate limits first (absent self-hosted)
  if (!fits(join())) tokens = tokenCore; // token breakdown parens
  if (!fits(join())) ctx = contextCore; // context used/window numbers
  if (!fits(join())) tokens = '';
  if (!fits(join())) ctx = `${input.contextUsedPct}%`; // drop the bar itself
  if (!fits(join())) {
    // Shrink the model name last, keeping the effort/ultracode suffix intact —
    // this row is the only place the effective level is visible.
    const nameBudget = Math.max(1, budget - sepWidth - stringWidth(ctx) - stringWidth(modeSuffix));
    line = truncate(shortModel, nameBudget) + modeSuffix;
  }
  if (!fits(join())) {
    // Absurdly narrow terminal: model + mode is the floor.
    const nameBudget = Math.max(1, budget - stringWidth(modeSuffix));
    return truncate(shortModel, nameBudget) + modeSuffix;
  }

  return join();
}

function BuiltinStatusLineInner({
  modelName,
  effortLevel,
  ultracodeActive,
  contextUsedPct,
  usedTokens,
  contextWindowSize,
  totalInputTokens,
  totalOutputTokens,
  cacheHitRate,
  rateLimits,
}: BuiltinStatusLineProps) {
  const { columns } = useTerminalSize();

  // Force re-render every 60s so countdowns stay current
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const hasResetTime = (rateLimits.five_hour?.resets_at ?? 0) || (rateLimits.seven_day?.resets_at ?? 0);
    if (!hasResetTime) return;
    const id = setInterval(() => setTick(t => t + 1), 60_000);
    return () => clearInterval(id);
  }, [rateLimits.five_hour?.resets_at, rateLimits.seven_day?.resets_at]);

  // Suppress unused-variable lint for tick (it exists only to trigger re-renders)
  void tick;

  const limits: string[] = [];
  if (rateLimits.five_hour != null) {
    limits.push(
      `Session ${Math.round(rateLimits.five_hour.utilization * 100)}%${
        rateLimits.five_hour.resets_at > 0 ? ` ${formatCountdown(rateLimits.five_hour.resets_at)}` : ''
      }`,
    );
  }
  if (rateLimits.seven_day != null) {
    limits.push(
      `Weekly ${Math.round(rateLimits.seven_day.utilization * 100)}%${
        rateLimits.seven_day.resets_at > 0 ? ` ${formatCountdown(rateLimits.seven_day.resets_at)}` : ''
      }`,
    );
  }

  const row = composeStatusRow(
    {
      modelName,
      effortLevel,
      ultracodeActive,
      contextUsedPct,
      usedTokens,
      contextWindowSize,
      totalInputTokens,
      totalOutputTokens,
      cacheHitRate,
      limitText: limits.join(SEP),
    },
    columns,
  );

  return <Text wrap="truncate">{row}</Text>;
}

export const BuiltinStatusLine = React.memo(BuiltinStatusLineInner);
