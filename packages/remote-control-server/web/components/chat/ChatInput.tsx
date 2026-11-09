import {
  useState,
  useRef,
  useCallback,
  useMemo,
  useLayoutEffect,
  type KeyboardEvent,
  type ClipboardEvent,
  type ReactNode,
} from 'react';
import { cn } from '../../src/lib/utils';
import { ArrowUp, Square, Paperclip, Plus, Slash } from 'lucide-react';
import type { ChatInputMessage, UserMessageImage } from '../../src/lib/types';
import type { AvailableCommand } from '../../src/acp/types';
import { CommandMenu } from './CommandMenu';
import imageCompression from 'browser-image-compression';
import { haptic } from '../../src/lib/view-transition';

// 图片压缩配置
const IMAGE_COMPRESSION_OPTIONS = {
  maxSizeMB: 2,
  maxWidthOrHeight: 2048,
  useWebWorker: true,
  fileType: 'image/jpeg' as const,
};

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
  /** Agent 提供的可用 slash 命令 */
  commands?: AvailableCommand[];
  /** Controls rendered inside the composer footer, left of the attachments. */
  toolbarLeft?: ReactNode;
  /** Controls rendered inside the composer footer, right of the textarea. */
  toolbarRight?: ReactNode;
  className?: string;
}

export function ChatInput({
  onSubmit,
  isLoading = false,
  onInterrupt,
  disabled = false,
  placeholder = '给 Claude 发送消息…',
  supportsImages = false,
  commands,
  toolbarLeft,
  toolbarRight,
  className,
}: ChatInputProps) {
  const [text, setText] = useState('');
  const [images, setImages] = useState<UserMessageImage[]>([]);
  const [showCommandMenu, setShowCommandMenu] = useState(false);
  const [commandFilter, setCommandFilter] = useState('');
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleSubmit = useCallback(async () => {
    const trimmed = text.trim();
    if ((!trimmed && images.length === 0) || disabled) return;
    const sentText = text;
    const sentImages = images;

    haptic('light');
    setShowCommandMenu(false);
    setCommandFilter('');
    try {
      await onSubmit({ text: trimmed, images: images.length > 0 ? images : undefined });
      setText('');
      setImages([]);
    } catch {
      // A failed send must not eat the draft; put it back if it is untouched.
      setText(current => (current === '' ? sentText : current));
      setImages(current => (current.length === 0 ? sentImages : current));
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

  const handleKeyDown = useCallback(
    (e: KeyboardEvent<HTMLTextAreaElement>) => {
      if (showCommandMenu) {
        if (e.key === 'Escape') {
          e.preventDefault();
          setShowCommandMenu(false);
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
          setShowCommandMenu(false);
          return;
        }
      }
      if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
        // Soft keyboards label this key "换行/newline" and have no Shift, so
        // Enter inserts a line break there; the send button submits. Hardware
        // keyboards keep the desktop behaviour.
        if (window.matchMedia?.('(pointer: coarse)').matches) return;
        e.preventDefault();
        if (isLoading) {
          onInterrupt?.();
        } else {
          void handleSubmit();
        }
      }
    },
    [commandMatches.length, handleSubmit, isLoading, onInterrupt, showCommandMenu],
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
      const newImages = await processImageFiles(files);
      setImages(prev => [...prev, ...newImages]);
    },
    [supportsImages],
  );

  // 选择文件
  const handleFileSelect = useCallback(async () => {
    if (!fileInputRef.current) return;
    const files = fileInputRef.current.files;
    if (!files || files.length === 0) return;

    const newImages = await processImageFiles(Array.from(files));
    setImages(prev => [...prev, ...newImages]);
    // 清空 input 以便重复选择
    fileInputRef.current.value = '';
  }, []);

  const removeImage = useCallback((index: number) => {
    setImages(prev => prev.filter((_, i) => i !== index));
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

  const toggleCommandMenu = useCallback(() => {
    if (showCommandMenu) {
      setShowCommandMenu(false);
      setCommandFilter('');
      return;
    }
    const slashMode = text.startsWith('/');
    // Tapping "/" used to prefix the whole draft, turning "hello" into
    // "/hello". Only seed the slash when the composer is empty.
    if (!text) setText('/');
    setShowCommandMenu(true);
    // Filter on the text that will actually be in the input; using the old
    // value made a typed prefix invisible to the menu.
    setCommandFilter(slashMode ? text.slice(1) : '');
    textareaRef.current?.focus();
  }, [showCommandMenu, text]);

  // The textarea grows with the text — including text set programmatically by
  // the command menu, which used to leave a one-line command in a 200px box.
  useLayoutEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
  }, [text]);

  const canSend = (text.trim() || images.length > 0) && !disabled;
  // A stop button with no handler is a lie: only show it when interrupting is
  // actually wired up (e.g. the device-open flow cannot interrupt).
  const canInterrupt = isLoading && Boolean(onInterrupt);

  return (
    <div
      className={cn(
        'composer-safe w-full max-w-3xl mx-auto px-3 sm:px-8 pb-[calc(0.75rem+env(safe-area-inset-bottom))] pt-2',
        className,
      )}
    >
      <div className="relative">
        {/* Slash command menu — floating above input */}
        {showCommandMenu && commands && commands.length > 0 && (
          <CommandMenu
            commands={commands}
            filter={commandFilter}
            onSelect={handleCommandSelect}
            onClose={() => {
              setShowCommandMenu(false);
              setCommandFilter('');
            }}
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

          {/* Footer toolbar — attachments and mode on the left, model and send on the right. */}
          <div className="flex items-center gap-1 px-1.5 pb-1.5 pt-1">
            <div className="flex min-w-0 items-center gap-0.5">
              {toolbarLeft}
              {supportsImages && (
                <>
                  <button
                    type="button"
                    onClick={() => fileInputRef.current?.click()}
                    className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full text-text-secondary transition-colors hover:bg-surface-1 hover:text-text-primary disabled:opacity-50"
                    disabled={disabled}
                    aria-label="添加附件"
                  >
                    <Plus className="h-5 w-5" strokeWidth={1.75} />
                  </button>
                  <input
                    ref={fileInputRef}
                    type="file"
                    accept="image/*"
                    multiple
                    className="hidden"
                    onChange={handleFileSelect}
                  />
                </>
              )}
              {commands && commands.length > 0 && (
                <button
                  type="button"
                  onClick={toggleCommandMenu}
                  className={cn(
                    'flex h-10 w-10 shrink-0 items-center justify-center rounded-full transition-colors disabled:opacity-50',
                    showCommandMenu
                      ? 'bg-brand/15 text-brand'
                      : 'text-text-secondary hover:bg-surface-1 hover:text-text-primary',
                  )}
                  disabled={disabled}
                  title="命令列表"
                  aria-label="命令列表"
                >
                  <Slash className="h-4 w-4" />
                </button>
              )}
            </div>

            <div className="ml-auto flex min-w-0 items-center gap-0.5">
              {toolbarRight}
              <button
                type="button"
                onClick={() => {
                  if (canInterrupt) onInterrupt?.();
                  else void handleSubmit();
                }}
                disabled={canInterrupt ? false : !canSend}
                aria-label={canInterrupt ? '停止生成' : '发送消息'}
                className={cn(
                  'ml-1 flex h-10 w-10 shrink-0 items-center justify-center rounded-full transition-all',
                  canInterrupt
                    ? 'bg-text-primary text-surface-2 hover:opacity-90'
                    : canSend
                      ? 'bg-brand text-white hover:bg-brand-light active:scale-95'
                      : 'bg-surface-1 text-text-muted',
                )}
              >
                {canInterrupt ? (
                  <Square className="h-3 w-3" fill="currentColor" />
                ) : (
                  <ArrowUp className="h-4 w-4" strokeWidth={2.25} />
                )}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

// =============================================================================
// 图片处理工具
// =============================================================================

async function processImageFiles(files: File[]): Promise<UserMessageImage[]> {
  const results: UserMessageImage[] = [];

  for (const file of files) {
    try {
      let blob: Blob = file;
      let mimeType = file.type;

      if (file.size > 2 * 1024 * 1024) {
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
      console.error('Failed to process image:', err);
    }
  }

  return results;
}
