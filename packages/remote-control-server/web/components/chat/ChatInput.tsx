import {
  useState,
  useRef,
  useEffect,
  useCallback,
  useMemo,
  useLayoutEffect,
  type KeyboardEvent,
  type ClipboardEvent,
  type ReactNode,
} from 'react';
import { cn } from '../../src/lib/utils';
import { ArrowUp, Square, Plus, Loader2 } from 'lucide-react';
import type { ChatInputMessage, UserMessageImage } from '../../src/lib/types';
import type { AvailableCommand } from '../../src/acp/types';
import { CommandMenu } from './CommandMenu';
import imageCompression from 'browser-image-compression';
import { haptic } from '../../src/lib/view-transition';
import { uploadToDevice } from '../../src/api/devices';
import { AttachmentMenu, type AttachmentChoice } from '../../src/components/device/AttachmentMenu';
import { DirectoryPicker } from '../../src/components/device/DirectoryPicker';

// 图片压缩配置
const IMAGE_COMPRESSION_OPTIONS = {
  maxSizeMB: 2,
  maxWidthOrHeight: 2048,
  useWebWorker: true,
  fileType: 'image/jpeg' as const,
};

/** More than this in one message freezes WKWebView long before the server cares. */
const MAX_IMAGES = 9;
/** Refuse absurd files before decoding; the canvas copy would be worse. */
const MAX_IMAGE_BYTES = 25 * 1024 * 1024;

// =============================================================================
// Anthropic 风格聊天输入框 — 底部居中浮动卡片，橙色焦点环
// =============================================================================

interface ChatInputProps {
  onSubmit: (message: ChatInputMessage) => void | Promise<void>;
  isLoading?: boolean;
  onInterrupt?: () => void;
  disabled?: boolean;
  placeholder?: string;
  /** 是否支持图片上传 */
  supportsImages?: boolean;
  /**
   * Stable id for this conversation. When set, an unsent draft is kept in
   * localStorage so switching sessions (or iOS reclaiming the web view) does
   * not silently throw away what the user typed.
   */
  draftKey?: string;
  /** Agent 提供的可用 slash 命令 */
  commands?: AvailableCommand[];
  /** Controls rendered inside the composer footer, left of the attachments. */
  toolbarLeft?: ReactNode;
  /** Controls rendered inside the composer footer, right of the textarea. */
  toolbarRight?: ReactNode;
  /**
   * When set, the attachment sheet also offers the machine this device runs
   * on: local files are uploaded there, machine files are referenced by path.
   */
  deviceId?: string;
  className?: string;
}

export function ChatInput({
  onSubmit,
  isLoading = false,
  onInterrupt,
  disabled = false,
  placeholder = '给 Claude 发送消息…',
  supportsImages = false,
  draftKey,
  commands,
  toolbarLeft,
  toolbarRight,
  deviceId,
  className,
}: ChatInputProps) {
  const [text, setText] = useState(() => readDraft(draftKey));
  const [images, setImages] = useState<UserMessageImage[]>([]);
  const [showCommandMenu, setShowCommandMenu] = useState(false);
  const [commandFilter, setCommandFilter] = useState('');
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const filesInputRef = useRef<HTMLInputElement>(null);
  const folderInputRef = useRef<HTMLInputElement>(null);

  const [sending, setSending] = useState(false);
  // State alone is not enough: two taps in the same tick both read the old
  // closure and sent the prompt twice. The ref closes that window.
  const sendingRef = useRef(false);
  const [notice, setNotice] = useState('');
  const [attachOpen, setAttachOpen] = useState(false);
  const [pickerMode, setPickerMode] = useState<'file' | 'directory' | null>(null);
  const [uploading, setUploading] = useState(false);

  // Keep the draft across a session switch and an iOS web-view reclaim. Images
  // are deliberately not persisted: base64 attachments blow past the
  // localStorage quota and are not worth the fragility.
  const latestText = useRef(text);
  useEffect(() => {
    latestText.current = text;
  }, [text]);
  useEffect(() => {
    if (!draftKey) return;
    const timer = window.setTimeout(() => writeDraft(draftKey, text), 400);
    return () => window.clearTimeout(timer);
  }, [text, draftKey]);
  // The debounce is cancelled on unmount; flush the newest value here.
  useEffect(() => {
    if (!draftKey) return;
    return () => writeDraft(draftKey, latestText.current);
  }, [draftKey]);

  const handleSubmit = useCallback(async () => {
    const trimmed = text.trim();
    // Explain instead of ignoring the tap: a disabled-looking composer that
    // swallows input reads as a broken app.
    if (disabled) {
      setNotice('正在加载，请稍候…');
      return;
    }
    if (!trimmed && images.length === 0) return;
    if (sendingRef.current) return;
    const sentText = text;
    const sentImages = images;
    setNotice('');

    haptic('light');
    setShowCommandMenu(false);
    setCommandFilter('');
    sendingRef.current = true;
    setSending(true);
    // Clear before the request settles: the bubble is already on screen, and
    // leaving the text in the box invited a second tap on the same prompt.
    setText('');
    setImages([]);
    try {
      await onSubmit({ text: trimmed, images: images.length > 0 ? images : undefined });
    } catch (error) {
      // A failed send must not eat the draft; put it back if it is untouched.
      setText(current => (current === '' ? sentText : current));
      setImages(current => (current.length === 0 ? sentImages : current));
      setNotice(error instanceof Error ? error.message : '发送失败，请重试');
    } finally {
      sendingRef.current = false;
      setSending(false);
    }
  }, [text, images, disabled, onSubmit]);

  // The menu only owns the keyboard while it actually has matches; otherwise
  // Enter must submit normally.
  const commandMatches = useMemo(() => {
    if (!commands) return [];
    const filter = commandFilter.toLowerCase();
    if (!filter) return commands;
    return commands.filter(command => command.name.toLowerCase().startsWith(filter));
  }, [commands, commandFilter]);

  // Closing without choosing used to leave "/mod" in the box, which Enter
  // then sent to Claude as a chat message. Only drop it when the whole draft
  // is still a bare slash token (never when the user kept typing arguments).
  const closeCommandMenu = useCallback((clearDetached: boolean) => {
    setShowCommandMenu(false);
    setCommandFilter('');
    if (clearDetached) {
      setText(previous => (previous.startsWith('/') && !/\s/.test(previous) ? '' : previous));
    }
  }, []);

  const handleKeyDown = useCallback(
    (e: KeyboardEvent<HTMLTextAreaElement>) => {
      if (showCommandMenu) {
        if (e.key === 'Escape') {
          e.preventDefault();
          closeCommandMenu(true);
          return;
        }
        // Arrow keys and Enter are handled by CommandMenu via document-level
        // listener — but only while there is something to select.
        if (commandMatches.length > 0 && (e.key === 'ArrowUp' || e.key === 'ArrowDown' || e.key === 'Enter')) {
          e.preventDefault();
          return;
        }
        if (e.key === 'Tab') {
          e.preventDefault();
          // Tab moves focus intentionally; keep what was typed.
          closeCommandMenu(false);
          return;
        }
      }
      if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
        // Soft keyboards label this key "换行/newline" and have no Shift, so
        // Enter inserts a line break there; the send button submits. Hardware
        // keyboards keep the desktop behaviour.
        if (window.matchMedia?.('(pointer: coarse)').matches) return;
        e.preventDefault();
        if (text.trim() || images.length > 0) {
          void handleSubmit();
        } else if (isLoading) {
          onInterrupt?.();
        }
      }
    },
    [
      closeCommandMenu,
      commandMatches.length,
      handleSubmit,
      images.length,
      isLoading,
      onInterrupt,
      showCommandMenu,
      text,
    ],
  );

  const handleInput = useCallback(
    (e: React.ChangeEvent<HTMLTextAreaElement>) => {
      const value = e.target.value;
      setText(value);

      // Slash mode is only for the command token; once the user types a space
      // (arguments), close the menu so Enter submits the command.
      if (value.startsWith('/') && !/\s/.test(value) && commands && commands.length > 0) {
        setShowCommandMenu(true);
        setCommandFilter(value.slice(1));
      } else if (showCommandMenu) {
        setShowCommandMenu(false);
        setCommandFilter('');
      }

      // 自动调整高度
      const el = e.target;
      el.style.height = 'auto';
      el.style.height = Math.min(el.scrollHeight, 200) + 'px';
    },
    [commands, showCommandMenu],
  );

  // 粘贴图片
  const handlePaste = useCallback(
    async (e: ClipboardEvent) => {
      if (!supportsImages) return;
      const files = Array.from(e.clipboardData.files).filter(f => f.type.startsWith('image/'));
      if (files.length === 0) return;

      e.preventDefault();
      const room = Math.max(0, MAX_IMAGES - images.length);
      if (room === 0) {
        setNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
        return;
      }
      const { images: newImages, failed } = await processImageFiles(files);
      const accepted = newImages.slice(0, room);
      setImages(prev => [...prev, ...accepted].slice(0, MAX_IMAGES));
      if (failed > 0) {
        setNotice(`${failed} 张图片无法读取，已跳过`);
      } else if (newImages.length > room) {
        setNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
      }
    },
    [images.length, supportsImages],
  );

  // 选择文件
  const handleFileSelect = useCallback(async () => {
    if (!fileInputRef.current) return;
    const files = fileInputRef.current.files;
    if (!files || files.length === 0) return;

    const imagesOnly = Array.from(files).filter(file => file.type.startsWith('image/'));
    if (imagesOnly.length !== files.length) {
      setNotice('只能发送图片附件');
    }
    if (imagesOnly.length === 0) {
      fileInputRef.current.value = '';
      return;
    }
    const accepted = imagesOnly.filter(file => file.size <= MAX_IMAGE_BYTES);
    if (accepted.length !== imagesOnly.length) {
      setNotice('图片超过 25MB，已跳过');
    }
    if (accepted.length === 0) {
      fileInputRef.current.value = '';
      return;
    }
    const room = Math.max(0, MAX_IMAGES - images.length);
    if (room === 0) {
      setNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
      fileInputRef.current.value = '';
      return;
    }
    const { images: newImages, failed } = await processImageFiles(accepted);
    const kept = newImages.slice(0, room);
    setImages(prev => [...prev, ...kept].slice(0, MAX_IMAGES));
    if (failed > 0) {
      setNotice(`${failed} 张图片无法读取，已跳过`);
    } else if (newImages.length > room) {
      setNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
    }
    // 清空 input 以便重复选择
    fileInputRef.current.value = '';
  }, [images.length]);

  const removeImage = useCallback((index: number) => {
    setImages(prev => prev.filter((_, i) => i !== index));
  }, []);

  /** Drop absolute paths into the draft; Claude reads them with its tools. */
  const insertReferences = useCallback((paths: string[]) => {
    if (paths.length === 0) return;
    const lines = paths.map(path => formatMention(path)).join('\n');
    setText(previous => (previous.trim() ? `${previous.replace(/\s+$/, '')}\n${lines}` : lines));
    textareaRef.current?.focus();
  }, []);

  /**
   * Push phone files to the device and reference the landing path. Batches
   * keep each request small enough for a phone uplink; one group id keeps a
   * folder's batches in the same folder on the PC.
   */
  const uploadLocalFiles = useCallback(
    async (files: File[], asFolder: boolean) => {
      if (!deviceId) return;
      const accepted: File[] = [];
      let bytes = 0;
      let skipped = 0;
      for (const file of files) {
        if (accepted.length >= MAX_UPLOAD_FILES) {
          skipped += 1;
          continue;
        }
        if (file.size > MAX_UPLOAD_FILE_BYTES || bytes + file.size > MAX_UPLOAD_TOTAL_BYTES) {
          skipped += 1;
          continue;
        }
        bytes += file.size;
        accepted.push(file);
      }
      if (accepted.length === 0) {
        setNotice(`没有可上传的文件（单个最大 20MB，总共 30MB）`);
        return;
      }

      setUploading(true);
      setNotice(`正在上传 ${accepted.length} 个文件到电脑…`);
      try {
        const group = generateGroupId();
        let root = '';
        let done = 0;
        const uploadedPaths: string[] = [];
        let batch: { name: string; data: string }[] = [];
        let batchBytes = 0;

        const flush = async () => {
          if (batch.length === 0) return;
          const result = await uploadToDevice(deviceId, batch, group);
          if (!root) root = result.root;
          uploadedPaths.push(...result.files.map(file => file.path));
          done += batch.length;
          setNotice(`正在上传 ${done}/${accepted.length} 个文件…`);
          batch = [];
          batchBytes = 0;
        };

        for (const file of accepted) {
          const data = await fileToBase64(file);
          if (batch.length > 0 && batchBytes + file.size > UPLOAD_BATCH_BYTES) {
            await flush();
          }
          batch.push({ name: file.webkitRelativePath || file.name, data });
          batchBytes += file.size;
        }
        await flush();

        if (asFolder && root) {
          // The browser keeps the chosen folder's own name in the relative
          // paths; pointing at it means Claude sees the tree, not one flat bag.
          const top = accepted[0]?.webkitRelativePath?.split('/')[0];
          const separator = root.includes('\\') ? '\\' : '/';
          insertReferences([top ? `${root}${separator}${top}` : root]);
        } else {
          insertReferences(uploadedPaths);
        }
        setNotice(
          skipped > 0
            ? `已上传 ${accepted.length} 个文件，${skipped} 个因大小超限已跳过`
            : `已上传 ${accepted.length} 个文件`,
        );
        window.setTimeout(() => setNotice(''), 4000);
      } catch (error) {
        setNotice(error instanceof Error ? error.message : '上传失败，请重试');
      } finally {
        setUploading(false);
      }
    },
    [deviceId, insertReferences],
  );

  const handleLocalFiles = useCallback(
    async (event: React.ChangeEvent<HTMLInputElement>, asFolder: boolean) => {
      const list = event.target.files;
      if (!list || list.length === 0) return;
      const files = Array.from(list);
      event.target.value = '';
      const allImages = !asFolder && files.every(file => file.type.startsWith('image/'));
      if (allImages) {
        // Images stay inline: Claude sees them directly, no upload needed.
        const room = Math.max(0, MAX_IMAGES - images.length);
        if (room === 0) {
          setNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
          return;
        }
        const { images: newImages, failed } = await processImageFiles(files.slice(0, room));
        setImages(previous => [...previous, ...newImages].slice(0, MAX_IMAGES));
        if (failed > 0) setNotice(`${failed} 张图片无法读取，已跳过`);
        return;
      }
      await uploadLocalFiles(files, asFolder);
    },
    [images.length, uploadLocalFiles],
  );

  const handleAttachChoice = useCallback(async (choice: AttachmentChoice) => {
    setAttachOpen(false);
    setNotice('');
    switch (choice) {
      case 'photos':
        fileInputRef.current?.click();
        break;
      case 'files':
        filesInputRef.current?.click();
        break;
      case 'folder':
        folderInputRef.current?.click();
        break;
      case 'device-files':
        setPickerMode('file');
        break;
      case 'device-folder':
        setPickerMode('directory');
        break;
    }
  }, []);

  const handleCommandSelect = useCallback(
    (command: AvailableCommand) => {
      // Keep anything the user already typed instead of throwing the draft
      // away: "/plan 帮我重构这个函数".
      const draft = text.startsWith('/') ? '' : text.trim();
      setText(`/${command.name} ${draft}`);
      setShowCommandMenu(false);
      setCommandFilter('');
      textareaRef.current?.focus();
    },
    [text],
  );

  // The textarea grows with the text — including text set programmatically by
  // the command menu, which used to leave a one-line command in a 200px box.
  useLayoutEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
  }, [text]);

  const hasDraft = Boolean(text.trim() || images.length > 0);
  const canSend = hasDraft && !disabled;
  // A stop button with no handler is a lie: only show it when interrupting is
  // actually wired up (e.g. the device-open flow cannot interrupt).
  const canInterrupt = isLoading && Boolean(onInterrupt);
  // While a draft exists the stop control steps aside: the row is already tight
  // with two selects, and the user is about to send, not interrupt.
  const showStop = canInterrupt && !hasDraft;
  // Interrupting is destructive, so it gets its own control and a deliberate
  // second tap. The send slot must stay "send": while a turn runs, the user
  // can still type and send a follow-up, and that tap used to stop the turn.
  const [confirmStop, setConfirmStop] = useState(false);
  useEffect(() => {
    if (!showStop) setConfirmStop(false);
  }, [showStop]);
  const handleStop = () => {
    if (!confirmStop) {
      setConfirmStop(true);
      haptic('warning');
      window.setTimeout(() => setConfirmStop(false), 2500);
      return;
    }
    setConfirmStop(false);
    onInterrupt?.();
  };

  return (
    <div
      className={cn(
        'composer-safe w-full max-w-3xl mx-auto px-3 sm:px-8 pb-[calc(0.75rem+env(safe-area-inset-bottom))] pt-2',
        className,
      )}
    >
      {notice && (
        <div role="status" className="mb-1.5 px-2 text-xs text-text-secondary">
          {notice}
        </div>
      )}
      <div className="relative">
        {/* Slash command menu — floating above input */}
        {showCommandMenu && commands && commands.length > 0 && (
          <CommandMenu
            commands={commands}
            filter={commandFilter}
            onSelect={handleCommandSelect}
            onClose={() => closeCommandMenu(false)}
            className="absolute bottom-full left-0 right-0 mb-1 z-50"
          />
        )}
        <div
          className={cn(
            'overflow-hidden rounded-[22px] border border-border bg-surface-2',
            'shadow-[0_2px_10px_rgba(16,18,22,0.05)] transition-all',
            'focus-within:border-brand/50 focus-within:shadow-[0_2px_14px_rgba(217,119,87,0.12)]',
          )}
        >
          {/* 图片预览 */}
          {images.length > 0 && (
            <div className="flex flex-wrap gap-2 px-3 pt-3">
              {images.map((img, i) => (
                <div key={i} className="relative group">
                  <img
                    src={`data:${img.mimeType};base64,${img.data}`}
                    alt={`附件图片 ${i + 1}`}
                    className="h-14 w-14 rounded-lg border border-border object-cover"
                  />
                  <button
                    type="button"
                    onClick={() => removeImage(i)}
                    className="absolute -right-1.5 -top-1.5 flex h-8 w-8 items-center justify-center rounded-full border border-border bg-surface-2 text-xs text-text-muted opacity-100 transition-opacity hover:text-text-primary sm:opacity-0 sm:group-hover:opacity-100"
                    aria-label={`移除附件 ${i + 1}`}
                  >
                    {'×'}
                  </button>
                </div>
              ))}
            </div>
          )}

          <div className="px-3 pt-3">
            <textarea
              ref={textareaRef}
              value={text}
              onChange={handleInput}
              onKeyDown={handleKeyDown}
              onPaste={handlePaste}
              placeholder={placeholder}
              disabled={disabled}
              rows={1}
              aria-label="消息输入框"
              className={cn(
                'w-full resize-none border-none bg-transparent outline-none',
                'text-base text-text-primary placeholder:text-text-muted sm:text-[15px]',
                // Never taller than the visible shell minus the footer, so a
                // long draft cannot push the send row off a small screen.
                'min-h-[24px] leading-6',
                'max-h-[min(200px,calc(var(--app-height,100dvh)-220px))]',
              )}
            />
          </div>

          {/* Footer toolbar — attachments and mode on the left, model and send
              on the right. Wraps on narrow screens (iPhone) instead of
              crushing or overflowing the controls. */}
          <div className="flex flex-wrap items-center gap-1 gap-y-1 px-1.5 pb-1.5 pt-1">
            <div className="flex min-w-0 items-center gap-0.5">
              {toolbarLeft}
              {supportsImages && (
                <>
                  <button
                    type="button"
                    onClick={() => {
                      setNotice('');
                      setAttachOpen(true);
                    }}
                    className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full text-text-secondary transition-colors hover:bg-surface-1 hover:text-text-primary disabled:opacity-50"
                    disabled={disabled || uploading}
                    aria-label={uploading ? '正在上传附件' : '添加附件'}
                  >
                    {uploading ? (
                      <Loader2 className="h-[18px] w-[18px] animate-spin" strokeWidth={1.75} />
                    ) : (
                      <Plus className="h-5 w-5" strokeWidth={1.75} />
                    )}
                  </button>
                  <input
                    ref={fileInputRef}
                    type="file"
                    accept="image/*"
                    multiple
                    className="hidden"
                    onChange={handleFileSelect}
                  />
                  {deviceId && (
                    <>
                      <input
                        ref={filesInputRef}
                        type="file"
                        multiple
                        className="hidden"
                        onChange={event => void handleLocalFiles(event, false)}
                      />
                      <input
                        ref={folderInputRef}
                        type="file"
                        multiple
                        className="hidden"
                        {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
                        onChange={event => void handleLocalFiles(event, true)}
                      />
                    </>
                  )}
                </>
              )}
              {/* The "/" trigger button was removed: the command list still
                  opens from typing "/" in the composer, and the icon added
                  one more control to a row that was already busy. */}
            </div>

            <div className="ml-auto flex min-w-0 items-center gap-0.5">
              {toolbarRight}
              {showStop && (
                <button
                  type="button"
                  onClick={handleStop}
                  aria-label={confirmStop ? '再点一次停止生成' : '停止生成'}
                  className={cn(
                    'ml-1 flex h-8 shrink-0 items-center justify-center rounded-full transition-all',
                    confirmStop
                      ? 'gap-1 bg-destructive/15 px-2.5 text-status-error'
                      : 'w-8 bg-surface-3 text-text-primary active:scale-95',
                  )}
                >
                  <Square className="h-2.5 w-2.5" fill="currentColor" strokeWidth={0} />
                  {confirmStop && <span className="text-xs font-medium">确认停止</span>}
                </button>
              )}
              <button
                type="button"
                onClick={() => void handleSubmit()}
                disabled={!canSend || sending}
                aria-label="发送消息"
                className={cn(
                  'ml-1 flex h-10 w-10 shrink-0 items-center justify-center rounded-full transition-all',
                  canSend ? 'bg-brand text-white active:scale-95' : 'bg-surface-1 text-text-muted',
                  sending && 'opacity-70',
                )}
              >
                {sending ? (
                  <span className="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white" />
                ) : (
                  <ArrowUp className="h-4 w-4" strokeWidth={2.25} />
                )}
              </button>
            </div>
          </div>
        </div>
      </div>

      <AttachmentMenu
        open={attachOpen}
        onClose={() => setAttachOpen(false)}
        onChoose={choice => void handleAttachChoice(choice)}
        showDeviceOptions={Boolean(deviceId)}
      />
      {deviceId && pickerMode && (
        <DirectoryPicker
          open
          deviceId={deviceId}
          mode={pickerMode}
          title={pickerMode === 'file' ? '添加电脑上的文件' : '添加电脑上的目录'}
          directoryConfirmLabel="添加这个目录"
          onClose={() => setPickerMode(null)}
          onPickFiles={paths => {
            setPickerMode(null);
            insertReferences(paths);
          }}
          onPickDirectory={path => {
            setPickerMode(null);
            insertReferences([path]);
          }}
        />
      )}
    </div>
  );
}

// =============================================================================
// 草稿持久化
// =============================================================================

/** Comfortably under the 5 MB quota, and far more than a chat message needs. */
const DRAFT_MAX_CHARS = 20_000;

function draftStorageKey(key: string): string {
  return `rcs.draft.${key}`;
}

function readDraft(key: string | undefined): string {
  if (!key) return '';
  try {
    return (localStorage.getItem(draftStorageKey(key)) ?? '').slice(0, DRAFT_MAX_CHARS);
  } catch {
    // Private mode / disabled storage: a draft is a convenience, not a right.
    return '';
  }
}

function writeDraft(key: string | undefined, text: string): void {
  if (!key) return;
  try {
    if (!text.trim()) localStorage.removeItem(draftStorageKey(key));
    else localStorage.setItem(draftStorageKey(key), text.slice(0, DRAFT_MAX_CHARS));
  } catch {
    // Quota or disabled storage: keep typing, just do not persist.
  }
}

// =============================================================================
// 上传到电脑（本机文件/文件夹）
// =============================================================================

/** Ceilings that keep one message from melting a WKWebView or the uplink. */
const MAX_UPLOAD_FILES = 100;
const MAX_UPLOAD_FILE_BYTES = 20 * 1024 * 1024;
const MAX_UPLOAD_TOTAL_BYTES = 30 * 1024 * 1024;
/** One request stays around 8 MB once base64-encoded. */
const UPLOAD_BATCH_BYTES = 6 * 1024 * 1024;

function generateGroupId(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
}

/** `@path` is how the terminal names a file; quote paths that contain spaces. */
function formatMention(path: string): string {
  return /[\s"'()]/.test(path) ? `@"${path}"` : `@${path}`;
}

function fileToBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onloadend = () => {
      const result = reader.result as string;
      const comma = result.indexOf(',');
      resolve(comma >= 0 ? result.slice(comma + 1) : result);
    };
    reader.onerror = () => reject(new Error(`无法读取文件：${file.name}`));
    reader.readAsDataURL(file);
  });
}

// =============================================================================
// 图片处理工具
// =============================================================================

async function processImageFiles(files: File[]): Promise<{ images: UserMessageImage[]; failed: number }> {
  const results: UserMessageImage[] = [];
  let failed = 0;

  for (const file of files) {
    try {
      let blob: Blob = file;
      let mimeType = file.type;

      // A 1.9 MB JPEG can still be 8000×6000 pixels: size alone does not
      // decide whether the WebView can hold it in memory. Decode the header
      // first (also rejects HEIC/invalid files instead of dropping them
      // silently) and compress anything with a large pixel footprint.
      let width = 0;
      let height = 0;
      if (typeof createImageBitmap === 'function') {
        const bitmap = await createImageBitmap(file).catch(() => null);
        if (!bitmap) throw new Error('无法读取图片');
        width = bitmap.width;
        height = bitmap.height;
        bitmap.close?.();
      }

      if (file.size > 2 * 1024 * 1024 || width > 2048 || height > 2048) {
        const compressed = await imageCompression(file, IMAGE_COMPRESSION_OPTIONS);
        blob = compressed;
        mimeType = 'image/jpeg';
      }

      const base64 = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onloadend = () => {
          const result = reader.result as string;
          const commaIdx = result.indexOf(',');
          resolve(commaIdx >= 0 ? result.slice(commaIdx + 1) : result);
        };
        reader.onerror = () => reject(new Error('FileReader error'));
        reader.readAsDataURL(blob);
      });

      results.push({ mimeType, data: base64 });
    } catch (err) {
      failed += 1;
      console.error('Failed to process image:', err);
    }
  }

  return { images: results, failed };
}
