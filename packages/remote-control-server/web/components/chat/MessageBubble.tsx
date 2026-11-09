import { memo, useState, useRef, useEffect, useCallback } from 'react';
import type { UserMessageEntry, AssistantMessageEntry, UserMessageImage } from '../../src/lib/types';
import { cn } from '../../src/lib/utils';
import { MessageResponse } from '../ai-elements/message';
import { Reasoning, ReasoningTrigger, ReasoningContent } from '../ai-elements/reasoning';
import { ChevronDown } from 'lucide-react';

// 用户消息折叠最大高度（px）
const COLLAPSED_MAX_HEIGHT = 200;

// =============================================================================
// 用户消息 — 右对齐，品牌色淡底，可折叠
// =============================================================================

interface UserBubbleProps {
  entry: UserMessageEntry;
}

export const UserBubble = memo(function UserBubble({ entry }: UserBubbleProps) {
  const [expanded, setExpanded] = useState(false);
  const [overflowing, setOverflowing] = useState(false);
  const contentRef = useRef<HTMLDivElement>(null);

  const checkOverflow = useCallback(() => {
    const el = contentRef.current;
    if (!el) return;
    setOverflowing(el.scrollHeight > COLLAPSED_MAX_HEIGHT + 4);
  }, []);

  useEffect(() => {
    checkOverflow();
  }, [checkOverflow, entry.content]);

  return (
    <div className="flex justify-end">
      <div className="max-w-[85%] sm:max-w-[70%]">
        {/* 图片附件 */}
        {entry.images && entry.images.length > 0 && (
          <div className="flex flex-wrap gap-2 mb-2 justify-end">
            {entry.images.map((img, i) => (
              <ImageThumbnail key={i} image={img} />
            ))}
          </div>
        )}
        {/* 文本内容 — 品牌色淡底 + 折叠 */}
        {entry.content && (
          <div className="relative overflow-hidden rounded-[18px] rounded-br-[6px] border border-user-bubble-border bg-user-bubble">
            <div
              ref={contentRef}
              className={cn(
                'px-4 py-2.5 text-[15px] leading-[1.65] text-text-primary whitespace-pre-wrap break-words',
                !expanded && overflowing && `max-h-[${COLLAPSED_MAX_HEIGHT}px]`,
              )}
              style={!expanded && overflowing ? { maxHeight: `${COLLAPSED_MAX_HEIGHT}px` } : undefined}
            >
              {entry.content}
            </div>
            {/* 折叠渐变遮罩 + 展开按钮 */}
            {!expanded && overflowing && (
              <div className="absolute bottom-0 inset-x-0 flex flex-col items-center pt-8 bg-gradient-to-t from-user-bubble via-user-bubble/80 to-transparent">
                <button
                  type="button"
                  onClick={() => setExpanded(true)}
                  className="flex items-center gap-1 rounded-full px-3 py-1 text-xs font-medium text-text-secondary transition-colors hover:bg-surface-1 hover:text-text-primary"
                >
                  <span>展开</span>
                  <ChevronDown className="h-3 w-3" />
                </button>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
});

// =============================================================================
// 助手消息 — 左对齐，无背景卡片，编辑式排版
// =============================================================================

interface AssistantBubbleProps {
  entry: AssistantMessageEntry;
  isStreaming?: boolean;
}

export const AssistantBubble = memo(function AssistantBubble({ entry, isStreaming }: AssistantBubbleProps) {
  return (
    <div className="flex items-start">
      {/* 内容 — 无头像、无卡片，干净的回答流 */}
      <div className="min-w-0 flex-1 space-y-3">
        {entry.chunks.map((chunk, i) => {
          if (chunk.type === 'thought') {
            const isLastChunk = i === entry.chunks.length - 1;
            const isThoughtStreaming = isStreaming && isLastChunk;
            return (
              <Reasoning key={i} isStreaming={isThoughtStreaming}>
                <ReasoningTrigger />
                <ReasoningContent>
                  <div className="text-sm text-text-secondary leading-relaxed">{chunk.text}</div>
                </ReasoningContent>
              </Reasoning>
            );
          }
          // 普通消息块 — 直接输出，无包裹卡片
          return (
            <div key={i} className="message-content text-[15px] leading-[1.72] text-text-primary">
              <MessageResponse>{chunk.text}</MessageResponse>
            </div>
          );
        })}
      </div>
    </div>
  );
});

// =============================================================================
// 图片缩略图 — 点击放大
// =============================================================================

function ImageThumbnail({ image }: { image: UserMessageImage }) {
  // A WKWebView without a WKUIDelegate returns null from window.open(), so
  // tapping used to do nothing. Preview in-app instead.
  const [open, setOpen] = useState(false);
  const dataUrl = `data:${image.mimeType};base64,${image.data}`;
  return (
    <>
      <button
        type="button"
        className="rounded-lg overflow-hidden border border-border hover:border-brand/40 transition-colors cursor-pointer"
        onClick={() => setOpen(true)}
        aria-label="查看大图"
      >
        <img src={dataUrl} alt="Uploaded image" className="h-20 w-20 object-cover" />
      </button>
      {open && (
        <div
          role="dialog"
          aria-modal="true"
          aria-label="图片预览"
          className="fixed inset-0 z-[120] flex items-center justify-center bg-black/90 p-4"
          onClick={() => setOpen(false)}
        >
          <img src={dataUrl} alt="Uploaded image" className="max-h-full max-w-full rounded-lg object-contain" />
          <button
            type="button"
            onClick={() => setOpen(false)}
            aria-label="关闭"
            className="absolute right-3 flex h-10 w-10 items-center justify-center rounded-full bg-white/15 text-2xl leading-none text-white backdrop-blur"
            style={{ top: 'calc(0.75rem + env(safe-area-inset-top))' }}
          >
            {'×'}
          </button>
        </div>
      )}
    </>
  );
}
