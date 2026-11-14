import type { ReactNode } from 'react';
import { FileUp, FolderUp, Image as ImageIcon, Monitor, X } from 'lucide-react';
import { haptic } from '../../lib/view-transition';
import { ModalSheet } from './ModalSheet';

export type AttachmentChoice = 'photos' | 'files' | 'folder' | 'device-files' | 'device-folder';

/**
 * What the "+" button opens. Two sources — this phone and the machine Claude
 * runs on — because both mean something different: phone files must be pushed
 * across, machine files only need a path.
 */
export function AttachmentMenu({
  open,
  onClose,
  onChoose,
  showDeviceOptions,
  imagesInline = true,
}: {
  open: boolean;
  onClose: () => void;
  onChoose: (choice: AttachmentChoice) => void;
  /** Hidden in a bridge-only chat, where no device can receive an upload. */
  showDeviceOptions: boolean;
  /** False when photos are uploaded to the session directory instead of inline. */
  imagesInline?: boolean;
}) {
  if (!open || typeof document === 'undefined') return null;

  const pick = (choice: AttachmentChoice) => {
    haptic('light');
    onChoose(choice);
  };

  const row = (icon: ReactNode, title: string, subtitle: string, choice: AttachmentChoice) => (
    <button
      type="button"
      onClick={() => pick(choice)}
      className="flex w-full items-center gap-3 rounded-xl px-2.5 py-2.5 text-left transition-colors active:bg-surface-1 sm:hover:bg-surface-1"
    >
      <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-[10px] bg-surface-2 text-text-secondary">
        {icon}
      </span>
      <span className="min-w-0 flex-1">
        <span className="block truncate text-[14px] text-text-primary">{title}</span>
        <span className="block truncate text-[11.5px] text-text-muted">{subtitle}</span>
      </span>
    </button>
  );

  return (
    <ModalSheet
      open={open}
      onClose={onClose}
      label="添加附件"
      // 与设置/目录两个抽屉一致的高度上限：横屏手机或分屏下没有 max-h 时，
      // 这张底部抽屉会比视口还高，顶部行被推出屏幕外无法点到。
      className="max-h-[calc(var(--app-height,100dvh)*0.86)] overflow-y-auto overscroll-contain rounded-t-[20px] border border-border bg-surface-0 px-3 pt-2 pb-[calc(0.75rem+env(safe-area-inset-bottom))] shadow-2xl sm:max-w-sm sm:rounded-2xl sm:pb-3"
    >
      <div className="flex justify-center pt-0.5 sm:hidden">
        <span className="h-1 w-9 rounded-full bg-border" />
      </div>
      <div className="flex items-center justify-between px-2 pb-1 pt-2">
        <h2 aria-hidden="true" className="text-[15px] font-semibold text-text-primary">
          添加附件
        </h2>
        <button
          type="button"
          onClick={onClose}
          aria-label="关闭"
          className="flex h-8 w-8 items-center justify-center rounded-full text-text-muted transition-colors hover:bg-surface-1 hover:text-text-primary"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      <div className="px-0.5 pb-1">
        <p className="px-2.5 pb-1 pt-1.5 text-[11px] font-medium uppercase tracking-wide text-text-muted">当前设备</p>
        {row(
          <ImageIcon className="h-[18px] w-[18px]" strokeWidth={1.75} />,
          '照片',
          imagesInline ? '直接发送图片，Claude 能看' : '上传到会话目录，Claude 能看',
          'photos',
        )}
        {showDeviceOptions &&
          row(<FileUp className="h-[18px] w-[18px]" strokeWidth={1.75} />, '文件', '上传到会话目录', 'files')}
        {showDeviceOptions &&
          row(
            <FolderUp className="h-[18px] w-[18px]" strokeWidth={1.75} />,
            '文件夹',
            '整个目录上传到会话目录',
            'folder',
          )}
      </div>

      {showDeviceOptions && (
        <div className="mt-1 border-t border-border/60 px-0.5 pt-1">
          <p className="px-2.5 pb-1 pt-2 text-[11px] font-medium uppercase tracking-wide text-text-muted">
            电脑（Claude 所在的机器）
          </p>
          {row(<Monitor className="h-[18px] w-[18px]" strokeWidth={1.75} />, '文件…', '复制到会话目录', 'device-files')}
          {row(
            <Monitor className="h-[18px] w-[18px]" strokeWidth={1.75} />,
            '目录…',
            '把整个目录复制进来',
            'device-folder',
          )}
        </div>
      )}
    </ModalSheet>
  );
}
