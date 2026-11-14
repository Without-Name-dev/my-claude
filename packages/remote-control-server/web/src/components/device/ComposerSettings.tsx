import { Check, X } from 'lucide-react';
import { cn } from '../../lib/utils';
import { ModalSheet } from './ModalSheet';

/** Mirrors the CLI's effort levels; 'auto' clears the pinned level. */
const EFFORT_LEVELS = ['auto', 'low', 'medium', 'high', 'xhigh', 'max'];

const EFFORT_LABELS: Record<string, string> = {
  auto: '自动',
  low: '低',
  medium: '中',
  high: '高',
  xhigh: '超高',
  max: '最高',
};

export type ComposerSettingsProps = {
  open: boolean;
  onClose: () => void;
  /** A control request is in flight — every row is single-flight, like the old selects. */
  pending: boolean;
  /** Last control failure; shown inside the sheet because the sheet covers the banner. */
  controlError?: string;
  effort: string;
  ultracode: boolean;
  ultracodeUsable: boolean;
  permissionMode: string;
  permissionOptions: { id: string; label: string }[];
  modelId: string;
  models: { id: string; label: string; title?: string }[];
  /** Present when the remote terminal can be closed (bridge connected). */
  onCloseTerminal?: () => void;
  onChangeEffort: (value: string) => void | Promise<void>;
  onToggleUltracode: () => void | Promise<void>;
  onChangePermissionMode: (value: string) => void | Promise<void>;
  onChangeModel: (value: string) => void | Promise<void>;
};

/**
 * Session settings sheet for the mobile composer. Holds the thinking level,
 * the Ultracode switch (orthogonal to the level), the permission mode and the
 * model picker so the composer toolbar only needs one button.
 *
 * Rendered through a portal: the BridgeChat root runs a page-enter transform,
 * which would otherwise become the containing block for `fixed` and clip the
 * overlay to the chat area (the backdrop would miss the safe-area strip).
 */
export function ComposerSettings({
  open,
  onClose,
  pending,
  controlError,
  effort,
  ultracode,
  ultracodeUsable,
  permissionMode,
  permissionOptions,
  modelId,
  models,
  onCloseTerminal,
  onChangeEffort,
  onToggleUltracode,
  onChangePermissionMode,
  onChangeModel,
}: ComposerSettingsProps) {
  if (!open || typeof document === 'undefined') return null;

  const sectionLabel = 'mb-1.5 px-1 text-[11px] font-medium uppercase tracking-[0.08em] text-text-muted';

  const optionRow = (key: string, label: string, active: boolean, onSelect: () => void, title?: string) => (
    <button
      key={key}
      type="button"
      aria-pressed={active}
      title={title}
      disabled={pending}
      onClick={onSelect}
      className={cn(
        'flex min-h-11 w-full items-center gap-3 px-3.5 text-left text-[14px] transition-colors disabled:opacity-60',
        active ? 'text-text-primary' : 'text-text-secondary',
        'active:bg-surface-2 sm:hover:bg-surface-2',
      )}
    >
      <span className="min-w-0 flex-1 truncate">{label}</span>
      <Check
        className={cn('h-4 w-4 shrink-0 transition-opacity', active ? 'text-brand opacity-100' : 'opacity-0')}
        strokeWidth={2.5}
      />
    </button>
  );

  return (
    <ModalSheet
      open={open}
      onClose={onClose}
      label="会话设置"
      className="max-h-[calc(var(--app-height,100dvh)*0.86)] overflow-y-auto overscroll-contain rounded-t-[20px] border border-border bg-surface-0 px-4 pb-[calc(0.75rem+env(safe-area-inset-bottom))] pt-2 shadow-2xl sm:max-w-md sm:rounded-2xl sm:px-5 sm:pt-4 sm:pb-4"
    >
      <div className="flex justify-center pb-1 sm:hidden">
        <span className="h-1 w-9 rounded-full bg-border" />
      </div>
      <div className="mb-3 flex items-center justify-between">
        <h2 aria-hidden="true" className="text-[17px] font-semibold tracking-[-0.01em] text-text-primary">
          会话设置
        </h2>
        <button
          type="button"
          onClick={onClose}
          aria-label="关闭"
          className="-mr-1 flex h-8 w-8 items-center justify-center rounded-full text-text-muted transition-colors hover:bg-surface-1 hover:text-text-primary"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      {controlError && (
        <div
          role="alert"
          className="mb-3 rounded-xl border border-destructive/30 bg-destructive/10 px-3 py-2 text-[12px] leading-relaxed text-destructive"
        >
          {controlError}
        </div>
      )}

      <section>
        <div className={sectionLabel}>思考强度</div>
        {!effort && <p className="mb-2 px-1 text-xs text-text-muted">终端尚未报告当前强度，可手动选择。</p>}
        <div role="radiogroup" aria-label="思考强度" className="grid grid-cols-3 gap-1.5">
          {EFFORT_LEVELS.map(level => {
            const active = effort === level;
            return (
              <button
                key={level}
                type="button"
                role="radio"
                aria-checked={active}
                aria-label={`${EFFORT_LABELS[level] ?? level}（${level}）`}
                disabled={pending}
                onClick={() => void onChangeEffort(level)}
                className={cn(
                  'flex h-9 items-center justify-center rounded-[10px] border text-[13px] font-medium transition-colors disabled:opacity-60',
                  active
                    ? 'border-brand/50 bg-brand/10 text-brand-dark dark:text-brand'
                    : 'border-transparent bg-surface-1 text-text-secondary active:bg-surface-2 sm:hover:bg-surface-2',
                )}
              >
                {EFFORT_LABELS[level] ?? level}
              </button>
            );
          })}
        </div>
      </section>

      {/* Ultracode is a switch, not a level: toggling it never changes the
            thinking level above, and choosing a level never toggles it. */}
      <div
        className={cn(
          'mt-2 flex items-center gap-3 rounded-[12px] border px-3.5 py-2.5',
          ultracode ? 'border-brand/35 bg-brand/5' : 'border-border/70 bg-surface-1',
          !ultracodeUsable && 'opacity-60',
        )}
      >
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5 text-[14px] font-medium text-text-primary">
            Ultracode
            {ultracode && (
              <span className="rounded-full bg-brand/15 px-1.5 py-0.5 text-[10px] font-medium text-brand-dark dark:text-brand">
                已开启
              </span>
            )}
          </div>
          <div className="mt-0.5 text-[11.5px] text-text-muted">
            {ultracodeUsable ? '仅本会话 · 每个任务启用动态工作流' : '当前终端不可用或尚未报告支持状态'}
          </div>
        </div>
        <button
          type="button"
          role="switch"
          aria-checked={ultracode}
          aria-label="Ultracode"
          disabled={pending || !ultracodeUsable}
          onClick={() => void onToggleUltracode()}
          className={cn(
            'relative h-[26px] w-[46px] shrink-0 rounded-full transition-colors disabled:cursor-not-allowed',
            ultracode ? 'bg-brand' : 'bg-surface-3',
          )}
        >
          {/* Explicit left + transform: a bare translate-x utility
                depends on the button's static position, which centered the
                knob even when the switch was off. */}
          <span
            className="absolute left-[3px] top-[3px] h-5 w-5 rounded-full bg-white shadow-sm transition-transform"
            style={{ transform: ultracode ? 'translateX(20px)' : 'translateX(0px)' }}
          />
        </button>
      </div>

      <section className="mt-4">
        <div className={sectionLabel}>权限模式</div>
        {!permissionMode && <p className="mb-2 px-1 text-xs text-text-muted">终端尚未报告当前权限模式。</p>}
        <div className="overflow-hidden rounded-[12px] border border-border/70 bg-surface-1 divide-y divide-border/50">
          {permissionOptions.map(option =>
            optionRow(
              option.id,
              option.label,
              permissionMode === option.id,
              () => void onChangePermissionMode(option.id),
            ),
          )}
        </div>
      </section>

      <section className="mt-4">
        <div className={sectionLabel}>模型</div>
        {models.length > 0 ? (
          <div className="overflow-hidden rounded-[12px] border border-border/70 bg-surface-1 divide-y divide-border/50">
            {models.map(model =>
              optionRow(model.id, model.label, modelId === model.id, () => void onChangeModel(model.id), model.title),
            )}
          </div>
        ) : (
          <p className="px-1 text-xs text-text-muted">终端尚未提供模型信息。</p>
        )}
      </section>

      {onCloseTerminal && (
        <button
          type="button"
          onClick={onCloseTerminal}
          className="mt-4 w-full rounded-[12px] border border-destructive/25 bg-destructive/5 px-3 py-2.5 text-[13px] text-destructive transition-colors active:bg-destructive/10 sm:hover:bg-destructive/10"
        >
          关闭电脑上的终端
        </button>
      )}

      <button
        type="button"
        onClick={onClose}
        className="mt-3 h-11 w-full rounded-[12px] bg-brand text-[14px] font-medium text-white transition-transform active:scale-[0.99]"
      >
        完成
      </button>
    </ModalSheet>
  );
}
