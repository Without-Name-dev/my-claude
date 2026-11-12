import { useEffect } from 'react';
import { createPortal } from 'react-dom';
import { Check, X } from 'lucide-react';
import { cn } from '../../lib/utils';

/** Mirrors the CLI's effort levels; 'auto' clears the pinned level. */
const EFFORT_LEVELS = ['auto', 'low', 'medium', 'high', 'xhigh', 'max'];

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
  useEffect(() => {
    if (!open) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [open, onClose]);

  if (!open || typeof document === 'undefined') return null;

  const rowClass = (active: boolean) =>
    cn(
      'flex w-full items-center justify-between rounded-xl border px-3 py-2.5 text-sm transition-colors disabled:opacity-60',
      active
        ? 'border-brand/40 bg-brand/10 text-brand'
        : 'border-transparent bg-surface-1 text-text-primary hover:bg-surface-2',
    );

  return createPortal(
    <div className="fixed inset-0 z-[120] flex items-end justify-center sm:items-center">
      <div className="absolute inset-0 bg-black/40" onClick={onClose} aria-hidden="true" />
      <div
        role="dialog"
        aria-modal="true"
        aria-label="会话设置"
        className="relative max-h-[80vh] w-full overflow-y-auto rounded-2xl border border-border bg-surface-0 px-4 pt-4 pb-[calc(1rem+env(safe-area-inset-bottom))] shadow-xl sm:max-w-md sm:px-5 sm:pt-5"
      >
        <div className="mb-4 flex items-center justify-between">
          <h2 className="text-base font-medium text-text-primary">会话设置</h2>
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
          <div className="mb-2 px-1 text-xs font-medium uppercase tracking-wide text-text-muted">思考强度</div>
          <div role="radiogroup" aria-label="思考强度" className="space-y-1.5">
            {EFFORT_LEVELS.map(level => {
              const active = effort === level;
              return (
                <button
                  key={level}
                  type="button"
                  role="radio"
                  aria-checked={active}
                  disabled={pending}
                  onClick={() => void onChangeEffort(level)}
                  className={rowClass(active)}
                >
                  <span>{level}</span>
                  {active && <Check className="h-4 w-4 shrink-0" />}
                </button>
              );
            })}
          </div>

          {/* Ultracode is a separate switch: selecting a level never toggles it
              and toggling it never changes the level. */}
          <div
            className={cn(
              'mt-3 flex items-center gap-3 rounded-xl border px-3 py-2.5',
              ultracode ? 'border-brand/40 bg-brand/10' : 'border-border bg-surface-1',
              !ultracodeUsable && 'opacity-50',
            )}
          >
            <div className="min-w-0 flex-1">
              <div className="text-sm font-medium text-text-primary">Ultracode</div>
              <div className="text-xs text-text-muted">仅本会话 · 每个任务启用动态工作流</div>
            </div>
            <button
              type="button"
              role="switch"
              aria-checked={ultracode}
              aria-label="Ultracode"
              disabled={pending || !ultracodeUsable}
              onClick={() => void onToggleUltracode()}
              className={cn(
                'relative h-6 w-11 shrink-0 rounded-full transition-colors disabled:cursor-not-allowed',
                ultracode ? 'bg-brand' : 'border border-border bg-surface-2',
              )}
            >
              <span
                className={cn(
                  'absolute top-0.5 h-5 w-5 rounded-full bg-white shadow transition-transform',
                  ultracode ? 'translate-x-[1.375rem]' : 'translate-x-0.5',
                )}
              />
            </button>
          </div>
        </section>

        <section className="mt-5">
          <div className="mb-2 px-1 text-xs font-medium uppercase tracking-wide text-text-muted">权限模式</div>
          <div className="space-y-1.5">
            {permissionOptions.map(option => {
              const active = permissionMode === option.id;
              return (
                <button
                  key={option.id}
                  type="button"
                  aria-pressed={active}
                  disabled={pending}
                  onClick={() => void onChangePermissionMode(option.id)}
                  className={rowClass(active)}
                >
                  <span>{option.label}</span>
                  {active && <Check className="h-4 w-4 shrink-0" />}
                </button>
              );
            })}
          </div>
        </section>

        {models.length > 0 && (
          <section className="mt-5">
            <div className="mb-2 px-1 text-xs font-medium uppercase tracking-wide text-text-muted">模型</div>
            <div className="space-y-1.5">
              {models.map(model => {
                const active = modelId === model.id;
                return (
                  <button
                    key={model.id}
                    type="button"
                    aria-pressed={active}
                    title={model.title}
                    disabled={pending}
                    onClick={() => void onChangeModel(model.id)}
                    className={rowClass(active)}
                  >
                    <span className="min-w-0 truncate">{model.label}</span>
                    {active && <Check className="h-4 w-4 shrink-0" />}
                  </button>
                );
              })}
            </div>
          </section>
        )}

        {onCloseTerminal && (
          <button
            type="button"
            onClick={onCloseTerminal}
            className="mt-5 w-full rounded-xl border border-destructive/30 px-3 py-2.5 text-sm text-destructive transition-colors hover:bg-destructive/10"
          >
            关闭电脑上的终端
          </button>
        )}

        <button
          type="button"
          onClick={onClose}
          className="mt-3 h-11 w-full rounded-xl bg-brand text-sm font-medium text-white transition-transform active:scale-[0.99]"
        >
          完成
        </button>
      </div>
    </div>,
    document.body,
  );
}
