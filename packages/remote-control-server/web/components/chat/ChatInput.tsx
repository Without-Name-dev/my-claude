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
import { copyToDevice, uploadToDevice } from '../../src/api/devices';
import { AttachmentMenu, type AttachmentChoice } from '../../src/components/device/AttachmentMenu';
import { DirectoryPicker } from '../../src/components/device/DirectoryPicker';

// 图片压缩配置
const IMAGE_COMPRESSION_OPTIONS = {
  maxSizeMB: 2,
  maxWidthOrHeight: 2048,
  useWebWorker: true,
  fileType: 'image/jpeg' as const,
};
const SUPPORTED_IMAGE_TYPES = new Set(['image/jpeg', 'image/png', 'image/gif', 'image/webp']);

/** More than this in one message freezes WKWebView long before the server cares. */
const MAX_IMAGES = 8;

// On touch devices Enter deliberately inserts a newline (the send button
// submits); labelling that key "send" made phones show a send key that
// could not send.
const coarsePointer = typeof window !== 'undefined' && Boolean(window.matchMedia?.('(pointer: coarse)').matches);
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
  /**
   * The session's working directory. Uploads and copies land here so the
   * terminal sees them as project files.
   */
  cwd?: string;
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
  cwd,
  className,
}: ChatInputProps) {
  /** Attaching from the PC needs a device; uploads also need its cwd. */
  const deviceReady = Boolean(deviceId && cwd);
  /**
   * The "+" is about attachments, not about inline images: a chat that can
   * upload to the session directory must offer the button even when the model
   * cannot take image blocks.
   */
  const canAttach = supportsImages || deviceReady;
  const [text, setText] = useState(() => readDraft(draftKey));
  const [images, setImages] = useState<UserMessageImage[]>([]);
  const [showCommandMenu, setShowCommandMenu] = useState(false);
  const [commandFilter, setCommandFilter] = useState('');
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const attachmentButtonRef = useRef<HTMLButtonElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const filesInputRef = useRef<HTMLInputElement>(null);
  const folderInputRef = useRef<HTMLInputElement>(null);
  // handleFileSelect is defined before uploadLocalFiles; a ref bridges the two.
  const uploadLocalFilesRef = useRef<(files: File[], asFolder: boolean) => Promise<void>>(async () => {});

  const [sending, setSending] = useState(false);
  // State alone is not enough: two taps in the same tick both read the old
  // closure and sent the prompt twice. The ref closes that window.
  const sendingRef = useRef(false);
  const [notice, setNotice] = useState('');
  // Every notice self-clears: error/limit texts used to sit above the composer
  // forever, contradicting state the user had long since changed.
  const noticeTimer = useRef<number | null>(null);
  useEffect(
    () => () => {
      if (noticeTimer.current) window.clearTimeout(noticeTimer.current);
    },
    [],
  );
  const showNotice = useCallback((text: string, autoClearMs = 4000) => {
    setNotice(text);
    if (noticeTimer.current) window.clearTimeout(noticeTimer.current);
    if (text && autoClearMs > 0) {
      noticeTimer.current = window.setTimeout(() => setNotice(''), autoClearMs);
    }
  }, []);
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
      showNotice('正在加载，请稍候…');
      return;
    }
    if (!trimmed && images.length === 0) return;
    if (sendingRef.current) return;
    const sentText = text;
    const sentImages = images;
    if (noticeTimer.current) window.clearTimeout(noticeTimer.current);
    setNotice('');

    haptic('light');
    setShowCommandMenu(false);
    setCommandFilter('');
    sendingRef.current = true;
    setSending(true);
    // Clear before the request settles: the bubble is already on screen, and
    // leaving the text in the box invited a second tap on the same prompt.
    // The history composer can unmount as soon as the terminal connects.
    // Clear both the cleanup ref and persisted draft synchronously, before the
    // replacement live composer reads them during the same React commit.
    latestText.current = '';
    writeDraft(draftKey, '');
    setText('');
    setImages([]);
    try {
      await onSubmit({ text: trimmed, images: images.length > 0 ? images : undefined });
    } catch (error) {
      // A failed send must not eat the draft; put it back if it is untouched.
      setText(current => (!current ? sentText : sentText ? `${sentText}\n\n${current}` : current));
      setImages(current => [...sentImages, ...current]);
      showNotice(error instanceof Error ? error.message : '发送失败，请重试');
    } finally {
      sendingRef.current = false;
      setSending(false);
    }
  }, [text, images, disabled, onSubmit, showNotice, draftKey]);

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
      // Enter can confirm a Chinese/Japanese IME candidate, including in
      // WebKit's legacy 229 composition sequence; it must not send a message.
      if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return;
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
        // keyboards keep the desktop behaviour. An empty composer must never
        // interrupt a running turn from the keyboard: stopping is the explicit
        // two-tap control's job.
        if (coarsePointer) return;
        e.preventDefault();
        if (text.trim() || images.length > 0) {
          void handleSubmit();
        }
      }
    },
    [closeCommandMenu, commandMatches.length, handleSubmit, images.length, showCommandMenu, text],
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
      const files = Array.from(e.clipboardData.files).filter(f => f.type.startsWith('image/'));
      if (files.length === 0) return;
      // A chat without inline image support swallowed the paste in silence;
      // say where attachments go instead of letting the clipboard vanish.
      if (!supportsImages) {
        if (deviceReady) {
          e.preventDefault();
          await uploadLocalFilesRef.current(files, false);
        } else {
          showNotice('这个会话不能直接发送图片，请用 + 上传附件');
        }
        return;
      }

      e.preventDefault();
      const room = Math.max(0, MAX_IMAGES - images.length);
      if (room === 0) {
        showNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
        return;
      }
      const { images: newImages, failed } = await processImageFiles(files);
      const accepted = newImages.slice(0, room);
      setImages(prev => [...prev, ...accepted].slice(0, MAX_IMAGES));
      if (failed > 0) {
        showNotice(`${failed} 张图片无法读取，已跳过`);
      } else if (newImages.length > room) {
        showNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
      }
    },
    [deviceReady, images.length, showNotice, supportsImages],
  );

  // 选择文件
  const handleFileSelect = useCallback(async () => {
    if (!fileInputRef.current) return;
    const files = fileInputRef.current.files;
    if (!files || files.length === 0) return;

    // No inline image support (the history composer, before a terminal is up):
    // the photo is still useful as a session file, so upload it and reference
    // the path instead of silently dropping the message's image blocks.
    if (!supportsImages) {
      const list = Array.from(files);
      fileInputRef.current.value = '';
      if (deviceReady) await uploadLocalFilesRef.current(list, false);
      else showNotice('这个会话还不能添加附件');
      return;
    }

    const imagesOnly = Array.from(files).filter(file => file.type.startsWith('image/'));
    if (imagesOnly.length !== files.length) {
      showNotice('只能发送图片附件');
    }
    if (imagesOnly.length === 0) {
      fileInputRef.current.value = '';
      return;
    }
    const accepted = imagesOnly.filter(file => file.size <= MAX_IMAGE_BYTES);
    if (accepted.length !== imagesOnly.length) {
      showNotice('图片超过 25MB，已跳过');
    }
    if (accepted.length === 0) {
      fileInputRef.current.value = '';
      return;
    }
    const room = Math.max(0, MAX_IMAGES - images.length);
    if (room === 0) {
      showNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
      fileInputRef.current.value = '';
      return;
    }
    const { images: newImages, failed } = await processImageFiles(accepted);
    const kept = newImages.slice(0, room);
    setImages(prev => [...prev, ...kept].slice(0, MAX_IMAGES));
    if (failed > 0) {
      showNotice(`${failed} 张图片无法读取，已跳过`);
    } else if (newImages.length > room) {
      showNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
    }
    // 清空 input 以便重复选择
    fileInputRef.current.value = '';
  }, [deviceReady, images.length, showNotice, supportsImages]);

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
   * Push phone files into the session's working directory and reference the
   * landing paths. Batches keep each request small enough for a phone uplink.
   */
  const uploadLocalFiles = useCallback(
    async (files: File[], asFolder: boolean) => {
      if (!deviceId || !cwd) return;
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
        showNotice(`没有可上传的文件（单个最大 20MB，总共 30MB）`);
        return;
      }

      setUploading(true);
      showNotice(`正在上传 ${accepted.length} 个文件到会话目录…`, 0);
      const uploadedPaths: string[] = [];
      try {
        let done = 0;
        let batch: { name: string; data: string }[] = [];
        let batchBytes = 0;

        const flush = async () => {
          if (batch.length === 0) return;
          const result = await uploadToDevice(deviceId, cwd, batch);
          uploadedPaths.push(...result.files.map(file => file.path));
          done += batch.length;
          showNotice(`正在上传 ${done}/${accepted.length} 个文件…`, 0);
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

        if (asFolder) {
          // Point at the uploaded folder itself, not each file: the browser
          // keeps the folder name in the relative path. Derive it from the
          // landed path because a name clash may have added a suffix.
          const first = uploadedPaths[0];
          const separator = cwd.includes('\\') ? '\\' : '/';
          const prefix = cwd.endsWith(separator) ? cwd : `${cwd}${separator}`;
          if (first && first.startsWith(prefix)) {
            const top = first.slice(prefix.length).split(separator)[0];
            insertReferences([`${prefix}${top}`]);
          } else {
            insertReferences(uploadedPaths);
          }
        } else {
          insertReferences(uploadedPaths);
        }
        showNotice(
          skipped > 0
            ? `已上传 ${accepted.length} 个文件到会话目录，${skipped} 个因大小超限已跳过`
            : `已上传 ${accepted.length} 个文件到会话目录`,
        );
      } catch (error) {
        const detail = error instanceof Error ? error.message : '上传失败';
        if (uploadedPaths.length > 0) {
          insertReferences(uploadedPaths);
          showNotice(`已上传 ${uploadedPaths.length} 个文件并添加引用；后续上传失败：${detail}`);
        } else {
          showNotice(`${detail}；请检查会话目录后重试`);
        }
      } finally {
        setUploading(false);
      }
    },
    [cwd, deviceId, insertReferences, showNotice],
  );

  // handleFileSelect may need the upload path defined above.
  useEffect(() => {
    uploadLocalFilesRef.current = uploadLocalFiles;
  }, [uploadLocalFiles]);

  /** Copy PC files/folders into the session directory, then reference them. */
  const addDevicePaths = useCallback(
    async (paths: string[]) => {
      if (!deviceId || !cwd || paths.length === 0) return;
      setUploading(true);
      showNotice(paths.length > 1 ? `正在添加到会话目录（${paths.length} 项）…` : '正在添加到会话目录…', 0);
      try {
        const result = await copyToDevice(deviceId, cwd, paths);
        insertReferences(result.entries.map(entry => entry.path));
        const copied = result.entries.filter(entry => entry.copied).length;
        showNotice(copied > 0 ? `已添加 ${result.entries.length} 项到会话目录` : '这些文件已经在会话目录里');
      } catch (error) {
        showNotice(error instanceof Error ? error.message : '添加失败，请重试');
      } finally {
        setUploading(false);
      }
    },
    [cwd, deviceId, insertReferences, showNotice],
  );

  const handleLocalFiles = useCallback(
    async (event: React.ChangeEvent<HTMLInputElement>, asFolder: boolean) => {
      const list = event.target.files;
      if (!list || list.length === 0) return;
      const files = Array.from(list);
      event.target.value = '';
      // Only inline images when the send path can carry image blocks; the
      // history composer (no bridge yet) must upload them as session files.
      const allImages = supportsImages && !asFolder && files.every(file => file.type.startsWith('image/'));
      if (allImages) {
        // Images stay inline: Claude sees them directly, no upload needed.
        const room = Math.max(0, MAX_IMAGES - images.length);
        if (room === 0) {
          showNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
          return;
        }
        const { images: newImages, failed } = await processImageFiles(files.slice(0, room));
        setImages(previous => [...previous, ...newImages].slice(0, MAX_IMAGES));
        if (failed > 0) showNotice(`${failed} 张图片无法读取，已跳过`);
        // Mirrors the picker path: never drop photos past the cap in silence.
        if (files.length > room) showNotice(`最多只能添加 ${MAX_IMAGES} 张图片`);
        return;
      }
      await uploadLocalFiles(files, asFolder);
    },
    [images.length, showNotice, supportsImages, uploadLocalFiles],
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
        'composer-safe w-full max-w-[56rem] mx-auto px-4 sm:px-8 pb-[calc(0.75rem+env(safe-area-inset-bottom))] pt-2',
        className,
      )}
    >
      {notice && (
        <div role="status" className="mb-1.5 px-2 text-xs text-text-secondary">
          {notice}
        </div>
      )}
      <div className="relative" data-composer-root>
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
              enterKeyHint={coarsePointer ? 'enter' : 'send'}
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
              {canAttach && (
                <button
                  ref={attachmentButtonRef}
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
              )}
              {canAttach && (
                <input
                  ref={fileInputRef}
                  type="file"
                  accept="image/*"
                  multiple
                  className="hidden"
                  onChange={handleFileSelect}
                />
              )}
              {deviceReady && (
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
        showDeviceOptions={deviceReady}
        imagesInline={supportsImages}
      />
      {deviceId && cwd && pickerMode && (
        <DirectoryPicker
          open
          deviceId={deviceId}
          returnFocusRef={attachmentButtonRef}
          mode={pickerMode}
          title={pickerMode === 'file' ? '添加电脑上的文件' : '添加电脑上的目录'}
          directoryConfirmLabel="添加这个目录"
          onClose={() => setPickerMode(null)}
          onPickFiles={paths => {
            setPickerMode(null);
            void addDevicePaths(paths);
          }}
          onPickDirectory={path => {
            setPickerMode(null);
            void addDevicePaths([path]);
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

export async function processImageFiles(
  files: File[],
  compress: typeof imageCompression = imageCompression,
): Promise<{ images: UserMessageImage[]; failed: number }> {
  const results: UserMessageImage[] = [];
  let failed = 0;

  for (const file of files) {
    try {
      let blob: Blob = file;
      let mimeType = file.type.toLowerCase();

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

      // A small iPhone HEIC can decode successfully and still be unsupported
      // by model APIs; convert it even when no resizing is needed.
      if (!SUPPORTED_IMAGE_TYPES.has(mimeType) || file.size > 2 * 1024 * 1024 || width > 2048 || height > 2048) {
        const compressed = await compress(file, IMAGE_COMPRESSION_OPTIONS);
        blob = compressed;
        mimeType = compressed.type.toLowerCase();
        if (!SUPPORTED_IMAGE_TYPES.has(mimeType)) throw new Error('图片格式不受支持');
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
