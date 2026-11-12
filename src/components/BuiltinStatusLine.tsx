import React, { useEffect, useState } from 'react';
import { Box, Text } from '@anthropic/ink';
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

/**
 * One status row, laid out as a single pre-measured line.
 *
 * The previous version rendered every segment as a flex child; when the total
 * exceeded the terminal width Ink wrapped mid-segment and the row collapsed
 * into intercut fragments ("Tokens33.3k", "(25.8k/1m │"). Segments are now
 * strings measured against the available columns and dropped whole — widest
 * tier shows everything, narrower tiers shed detail right-to-left.
 */
function BuiltinStatusLineInner({
  modelName,
  effortLevel,
  ultracodeActive,
  contextUsedPct,
  usedTokens,
  contextWindowSize,
  totalInputTokens,
  totalOutputTokens,
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

  // Model display: use first two words (e.g. "Opus 4.6") instead of just first
  // word. The effort level rides along — this row is always visible, unlike the
  // logo that scrolls away with the transcript.
  const modelParts = modelName.split(' ');
  const shortModel = modelParts.length >= 2 ? `${modelParts[0]} ${modelParts[1]}` : modelName;
  const modelWithMode = formatModelWithMode(shortModel, effortLevel, ultracodeActive);

  // Context: bar + percent, optionally with used/window.
  const contextCore = `${contextBar(contextUsedPct)} ${contextUsedPct}%`;
  const contextDetail = ` (${formatTokens(usedTokens)}/${formatTokens(contextWindowSize)})`;

  // Tokens: session totals plus the in/out breakdown. No cache percentage —
  // it would be a per-request rate sitting among session totals, which reads
  // as an average of the conversation.
  const tokenTotal = formatTokens(totalInputTokens + totalOutputTokens);
  const tokenCore = `Tokens ${tokenTotal}`;
  const tokenDetail = ` (in ${formatTokens(totalInputTokens)}, out ${formatTokens(totalOutputTokens)})`;

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

  // Assemble: shed detail right-to-left until the line fits the terminal.
  const budget = Math.max(20, columns - 4);
  let line = modelWithMode;
  let ctx = contextCore + contextDetail;
  let tokens = tokenCore + tokenDetail;
  let limitText = limits.join(SEP);
  const fits = (candidate: string) => candidate.length <= budget;
  const joined = () => [line, ctx, tokens, limitText].filter(Boolean).join(SEP);

  if (!fits(joined())) {
    // Drop the claude.ai rate-limit segments first (absent on self-hosted anyway).
    limitText = '';
  }
  if (!fits(joined())) {
    // Then the token breakdown parens.
    tokens = tokenCore;
  }
  if (!fits(joined())) {
    ctx = contextCore;
  }
  if (!fits(joined())) {
    tokens = '';
  }
  if (!fits([line, ctx].join(SEP))) {
    // Nothing left to drop but the bar itself.
    ctx = `${contextUsedPct}%`;
  }
  if (!fits(line)) {
    // A very long model id plus mode still must not wrap the row.
    line = truncate(line, budget);
  }

  const parts = [line, ctx, tokens, limitText].filter(Boolean);

  return (
    <Box>
      {parts.map((part, index) => (
        <React.Fragment key={index}>
          {index > 0 && <Text dimColor>{SEP}</Text>}
          <Text>{part}</Text>
        </React.Fragment>
      ))}
    </Box>
  );
}

export const BuiltinStatusLine = React.memo(BuiltinStatusLineInner);
