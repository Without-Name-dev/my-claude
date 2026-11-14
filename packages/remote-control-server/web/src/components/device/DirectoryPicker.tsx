import { useCallback, useEffect, useMemo, useState, type RefObject } from 'react';
import {
  AlertCircle,
  ArrowUp,
  Check,
  ChevronRight,
  File as FileIcon,
  Folder,
  HardDrive,
  Home,
  Loader2,
  X,
} from 'lucide-react';
import { cn } from '../../lib/utils';
import { haptic } from '../../lib/view-transition';
import { fetchDirectory, type DirectoryEntry, type DirectoryListing } from '../../api/devices';
import { ModalSheet } from './ModalSheet';

export type DirectoryPickerMode = 'directory' | 'file';

type DirectoryPickerProps = {
  open: boolean;
  deviceId: string;
  deviceName?: string;
  /**
   * `directory` picks one working directory (files are visible but cannot be
   * chosen); `file` picks one or more files to attach.
   */
  mode: DirectoryPickerMode;
  title?: string;
  /** Footer label in directory mode; defaults to the new-session wording. */
  directoryConfirmLabel?: string;
  onClose: () => void;
  onPickDirectory?: (path: string) => void;
  onPickFiles?: (paths: string[]) => void;
  returnFocusRef?: RefObject<HTMLElement | null>;
};

function formatSize(bytes: number | undefined): string {
  if (bytes === undefined) return '';
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GB`;
}

/** Last path segment, for the compact crumb line. */
function leafName(path: string): string {
  const trimmed = path.replace(/[\\/]+$/, '');
  const index = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf('\\'));
  return index >= 0 ? trimmed.slice(index + 1) || trimmed : trimmed;
}

/** Client-side parent, so the up button survives a failed listing. */
function parentOfPath(path: string): string | undefined {
  const trimmed = path.replace(/[\\/]+$/, '');
  const index = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf('\\'));
  if (index < 0) return undefined;
  const parent = trimmed.slice(0, index);
  if (parent.length === 0) return '/';
  // "C:" alone is drive-relative on Windows; make it the drive root.
  if (/^[a-zA-Z]:$/.test(parent)) return `${parent}\\`;
  return parent;
}

/**
 * Browses the operator machine's filesystem.
 *
 * Deliberately one component for both jobs: the new-session picker only needs
 * directories, while the attachment picker also selects files. Files always
 * stay visible in directory mode — hiding them made the listing look empty in
 * folders that only contain documents.
 */
export function DirectoryPicker({
  open,
  deviceId,
  deviceName,
  mode,
  title,
  directoryConfirmLabel,
  onClose,
  onPickDirectory,
  onPickFiles,
  returnFocusRef,
}: DirectoryPickerProps) {
  const [listing, setListing] = useState<DirectoryListing | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [selected, setSelected] = useState<string[]>([]);
  const [reloadKey, setReloadKey] = useState(0);
  const [path, setPath] = useState<string | undefined>(undefined);

  // Every open starts at the roots: a stale path from last time is jarring.
  useEffect(() => {
    if (!open) return;
    setPath(undefined);
    setListing(null);
    setError('');
    setSelected([]);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setError('');
    void (async () => {
      try {
        const result = await fetchDirectory(deviceId, path);
        if (cancelled) return;
        setListing(result);
      } catch (err) {
        if (cancelled) return;
        setListing(null);
        setError(err instanceof Error ? err.message : '无法读取目录');
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [open, deviceId, path, reloadKey]);

  // Show where we are — or where we tried to go. Rendering only listing.path
  // collapsed the breadcrumb to "选择一个目录" the moment a navigation failed,
  // losing the folder context the person was browsing.
  const currentPath = path ?? listing?.path;
  const parentTarget =
    currentPath === undefined ? undefined : listing?.path === currentPath ? listing.parent : parentOfPath(currentPath);

  const enter = useCallback((entry: DirectoryEntry) => {
    haptic('light');
    setSelected([]);
    setPath(entry.path);
  }, []);

  const goParent = useCallback(() => {
    if (!parentTarget) return;
    haptic('light');
    setSelected([]);
    setPath(parentTarget);
  }, [parentTarget]);

  const toggleFile = useCallback((entry: DirectoryEntry) => {
    haptic('light');
    setSelected(current =>
      current.includes(entry.path) ? current.filter(item => item !== entry.path) : [...current, entry.path],
    );
  }, []);

  const confirm = useCallback(() => {
    haptic('light');
    if (mode === 'directory') {
      if (listing?.path) onPickDirectory?.(listing.path);
    } else if (selected.length > 0) {
      onPickFiles?.(selected);
    }
  }, [listing?.path, mode, onPickDirectory, onPickFiles, selected]);

  const entries = listing?.entries ?? [];
  const dirCount = useMemo(() => entries.filter(entry => entry.type === 'dir').length, [entries]);
  const fileCount = entries.length - dirCount;
  const heading = title ?? (mode === 'directory' ? '选择目录' : '选择文件');
  const canConfirm = mode === 'directory' ? Boolean(listing?.path) : selected.length > 0;

  if (!open || typeof document === 'undefined') return null;

  return (
    <ModalSheet
      open={open}
      onClose={onClose}
      label={heading}
      returnFocusRef={returnFocusRef}
      className="flex h-[calc(var(--app-height,100dvh)*0.88)] flex-col overflow-hidden rounded-t-[20px] border border-border bg-surface-0 shadow-2xl sm:max-h-[42rem] sm:max-w-lg sm:rounded-2xl"
    >
      {/* grabber — the sheet is dismissible by the backdrop as well */}
      <div className="flex justify-center pt-2 sm:hidden">
        <span className="h-1 w-9 rounded-full bg-border" />
      </div>

      <div className="flex items-start justify-between gap-3 px-4 pb-2 pt-3 sm:px-5 sm:pt-4">
        <div className="min-w-0">
          <h2 aria-hidden="true" className="text-[17px] font-semibold tracking-[-0.01em] text-text-primary">
            {heading}
          </h2>
          <p className="mt-0.5 truncate text-[12px] text-text-muted">{deviceName || '这台电脑'}</p>
        </div>
        <button
          type="button"
          onClick={onClose}
          aria-label="关闭"
          className="-mr-1 flex h-9 w-9 shrink-0 items-center justify-center rounded-full text-text-muted transition-colors hover:bg-surface-1 hover:text-text-primary"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      {/* Current folder: one line, always visible, never wraps the layout. */}
      <div className="mx-4 mb-2 rounded-xl bg-surface-1 px-3 py-2 sm:mx-5">
        {currentPath ? (
          <div className="flex items-center gap-2">
            <Folder className="h-3.5 w-3.5 shrink-0 text-brand" />
            <span className="min-w-0 flex-1 truncate text-[12px] text-text-secondary" title={currentPath}>
              {currentPath}
            </span>
            {parentTarget && (
              <button
                type="button"
                onClick={goParent}
                aria-label="上一级"
                className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full text-text-muted transition-colors hover:bg-surface-2 hover:text-text-primary"
              >
                <ArrowUp className="h-3.5 w-3.5" />
              </button>
            )}
          </div>
        ) : (
          <span className="text-[12px] text-text-muted">选择一个目录</span>
        )}
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain border-y border-border/60 pb-2">
        {loading && (
          <div className="space-y-1.5 px-3 py-3">
            {[0, 1, 2, 3, 4].map(index => (
              <div key={index} className="h-11 animate-pulse rounded-xl bg-surface-1" />
            ))}
          </div>
        )}

        {!loading && error && (
          <div className="mx-3 my-3 flex items-start gap-2 rounded-xl border border-destructive/25 bg-destructive/10 px-3 py-2.5 text-[13px] text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
            <div className="min-w-0 flex-1">
              <p className="break-words">{error}</p>
              <button
                type="button"
                onClick={() => setReloadKey(key => key + 1)}
                className="mt-1.5 text-[12px] font-medium underline underline-offset-2"
              >
                重试
              </button>
            </div>
          </div>
        )}

        {!loading && !error && listing?.roots && (
          <ul className="px-2 py-2">
            {listing.roots.map(root => (
              <li key={root.path}>
                <button
                  type="button"
                  onClick={() => {
                    haptic('light');
                    setPath(root.path);
                  }}
                  className="flex min-h-12 w-full items-center gap-3 rounded-xl px-3 text-left transition-colors active:bg-surface-1 sm:hover:bg-surface-1"
                >
                  <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-surface-2 text-text-secondary">
                    {root.kind === 'home' ? <Home className="h-4 w-4" /> : <HardDrive className="h-4 w-4" />}
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-[14px] text-text-primary">{root.name}</span>
                    <span className="block truncate text-[11px] text-text-muted">{root.path}</span>
                  </span>
                  <ChevronRight className="h-4 w-4 shrink-0 text-text-muted" />
                </button>
              </li>
            ))}
          </ul>
        )}

        {!loading && !error && listing?.path && (
          <ul className="px-2 py-1.5">
            {entries.map(entry => {
              const isDir = entry.type === 'dir';
              const picked = selected.includes(entry.path);
              const hidden = entry.name.startsWith('.');
              if (isDir) {
                return (
                  <li key={entry.path}>
                    <button
                      type="button"
                      onClick={() => enter(entry)}
                      className="flex min-h-11 w-full items-center gap-3 rounded-xl px-3 text-left transition-colors active:bg-surface-1 sm:hover:bg-surface-1"
                    >
                      <Folder className="h-[18px] w-[18px] shrink-0 text-brand/80" strokeWidth={1.75} />
                      <span
                        className={cn(
                          'min-w-0 flex-1 truncate text-[14px]',
                          hidden ? 'text-text-muted' : 'text-text-primary',
                        )}
                      >
                        {entry.name}
                      </span>
                      <ChevronRight className="h-4 w-4 shrink-0 text-text-muted/60" />
                    </button>
                  </li>
                );
              }
              return (
                <li key={entry.path}>
                  <button
                    type="button"
                    disabled={mode === 'directory'}
                    onClick={() => toggleFile(entry)}
                    aria-pressed={mode === 'file' ? picked : undefined}
                    className={cn(
                      'flex min-h-11 w-full items-center gap-3 rounded-xl px-3 text-left transition-colors',
                      mode === 'file' ? 'active:bg-surface-1 sm:hover:bg-surface-1' : 'cursor-default',
                      picked && 'bg-brand/10',
                    )}
                  >
                    <FileIcon
                      className={cn(
                        'h-[18px] w-[18px] shrink-0',
                        mode === 'directory' ? 'text-text-muted/35' : 'text-text-muted',
                      )}
                      strokeWidth={1.75}
                    />
                    <span
                      className={cn(
                        'min-w-0 flex-1 truncate text-[14px]',
                        mode === 'directory'
                          ? 'text-text-muted/50'
                          : hidden
                            ? 'text-text-muted'
                            : 'text-text-secondary',
                      )}
                    >
                      {entry.name}
                    </span>
                    {mode === 'file' ? (
                      <span
                        className={cn(
                          'flex h-5 w-5 shrink-0 items-center justify-center rounded-full border transition-colors',
                          picked ? 'border-brand bg-brand text-white' : 'border-border text-transparent',
                        )}
                      >
                        <Check className="h-3 w-3" strokeWidth={3} />
                      </span>
                    ) : (
                      <span className="shrink-0 text-[11px] tabular-nums text-text-muted/60">
                        {formatSize(entry.size)}
                      </span>
                    )}
                  </button>
                </li>
              );
            })}
            {entries.length === 0 && (
              <li className="px-4 py-8 text-center text-[13px] text-text-muted">这个目录是空的</li>
            )}
            {listing.truncated && (
              <li className="px-4 py-3 text-center text-[11px] text-text-muted">条目太多，只显示前 500 项</li>
            )}
          </ul>
        )}
      </div>

      <div className="px-4 pt-2.5 pb-[calc(0.75rem+env(safe-area-inset-bottom))] sm:px-5 sm:pb-4">
        <div className="mb-2 flex items-center justify-between text-[11px] text-text-muted">
          <span>
            {listing?.path
              ? `${dirCount} 个文件夹${fileCount > 0 ? ` · ${fileCount} 个文件` : ''}`
              : '选择盘符或用户目录'}
          </span>
          {mode === 'directory' && listing?.path && (
            <span className="truncate pl-3 text-text-secondary">{leafName(listing.path)}</span>
          )}
        </div>
        <button
          type="button"
          onClick={confirm}
          disabled={!canConfirm || loading}
          className={cn(
            'flex h-11 w-full items-center justify-center gap-2 rounded-xl bg-brand text-[14px] font-medium text-white transition-all active:scale-[0.99] disabled:opacity-45',
          )}
        >
          {loading && <Loader2 className="h-4 w-4 animate-spin" />}
          {mode === 'directory'
            ? listing?.path
              ? directoryConfirmLabel || '在此目录新建会话'
              : '先选择一个目录'
            : selected.length > 0
              ? `添加 ${selected.length} 个文件`
              : '选择文件'}
        </button>
      </div>
    </ModalSheet>
  );
}
