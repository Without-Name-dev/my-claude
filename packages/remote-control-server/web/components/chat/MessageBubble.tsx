import { memo, useState, useRef, useEffect, useCallback, useMemo, Fragment } from 'react';
import type {
  AssistantChunk,
  AssistantMessageEntry,
  ToolCallEntry,
  UserMessageEntry,
  UserMessageImage,
} from '../../src/lib/types';
import { cn } from '../../src/lib/utils';
import { copyText } from '../../src/lib/clipboard';
import { MessageResponse } from '../ai-elements/message';
import { Shimmer } from '../ai-elements/shimmer';
import { ToolCallGroup } from './ToolCallGroup';
import { ModalSheet } from '../../src/components/device/ModalSheet';
import { BrainIcon, Check, ChevronDown, Copy } from 'lucide-react';

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
          <div className="mb-2 flex flex-wrap justify-end gap-2">
            {entry.images.map((img, i) => (
              <ImageThumbnail key={i} image={img} />
            ))}
          </div>
        )}
        {/* 文本内容 — 品牌色淡底 + 折叠；排队中的消息用灰色表示还没被终端取走 */}
        {entry.content && (
          <div
            className={cn(
              'relative overflow-hidden rounded-[18px] rounded-br-[6px] border',
              entry.pending ? 'border-border bg-surface-2' : 'border-user-bubble-border bg-user-bubble',
            )}
          >
            <div
              ref={contentRef}
              className={cn(
                'whitespace-pre-wrap break-words px-4 py-2.5 text-[15px] leading-[1.65]',
                entry.pending ? 'text-text-muted' : 'text-text-primary',
              )}
              style={!expanded && overflowing ? { maxHeight: `${COLLAPSED_MAX_HEIGHT}px` } : undefined}
            >
              {entry.content}
            </div>
            {/* 折叠渐变遮罩 + 展开按钮 */}
            {!expanded && overflowing && (
              <div
                className={cn(
                  'absolute inset-x-0 bottom-0 flex flex-col items-center bg-gradient-to-t to-transparent pt-8',
                  entry.pending ? 'from-surface-2 via-surface-2/80' : 'from-user-bubble via-user-bubble/80',
                )}
              >
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
        {entry.pending && <div className="mt-1 pr-1 text-right text-[11px] text-text-muted">排队中 · 等待终端处理</div>}
      </div>
    </div>
  );
});

// =============================================================================
// 助手回合 — 把一次回答里的思考、工具、正文收进同一个块
// 思考只出现一次；工具按顺序成组；正文保持原始顺序。
// =============================================================================

type TurnEntry = AssistantMessageEntry | ToolCallEntry;

type TurnSegment =
  | { kind: 'text'; key: string; text: string }
  | { kind: 'tools'; key: string; entries: ToolCallEntry[] };

interface AssistantTurnProps {
  entries: TurnEntry[];
  isStreaming?: boolean;
  onPermissionRespond?: (requestId: string, optionId: string | null, optionKind: string | null) => void;
}

export const AssistantTurn = memo(function AssistantTurn({
  entries,
  isStreaming,
  onPermissionRespond,
}: AssistantTurnProps) {
  const { thoughtText, segments, streamingThought } = useMemo(() => {
    const thoughtParts: string[] = [];
    const segments: TurnSegment[] = [];
    let toolRun: ToolCallEntry[] = [];

    const flushTools = () => {
      if (toolRun.length === 0) return;
      segments.push({ kind: 'tools', key: `tools-${toolRun[0]!.toolCall.id}`, entries: toolRun });
      toolRun = [];
    };

    for (const entry of entries) {
      if (entry.type === 'tool_call') {
        // Agent and workflow calls are meaningful work units, not just one
        // line in a collapsed batch of file/shell operations.
        if (/^(agent|task|workflow)$/i.test(entry.toolCall.title)) {
          flushTools();
          segments.push({ kind: 'tools', key: `tools-${entry.toolCall.id}`, entries: [entry] });
          continue;
        }
        toolRun.push(entry);
        continue;
      }
      const prose = entry.chunks.filter(
        (chunk): chunk is Extract<AssistantChunk, { type: 'message' }> =>
          chunk.type === 'message' && Boolean(chunk.text.trim()),
      );
      for (const chunk of entry.chunks) {
        if (chunk.type === 'thought' && chunk.text.trim()) thoughtParts.push(chunk.text);
      }
      // 只有思考、没有正文的消息不打断工具分组
      if (prose.length === 0) continue;
      flushTools();
      prose.forEach((chunk, index) => {
        segments.push({ kind: 'text', key: `${entry.id}-text-${index}`, text: chunk.text });
      });
    }
    flushTools();

    const last = entries[entries.length - 1];
    const lastChunk = last?.type === 'assistant_message' ? last.chunks[last.chunks.length - 1] : undefined;

    return {
      thoughtText: thoughtParts.join('\n\n').trim(),
      segments,
      streamingThought: Boolean(isStreaming) && lastChunk?.type === 'thought',
    };
  }, [entries, isStreaming]);

  const proseText = segments
    .filter((segment): segment is Extract<TurnSegment, { kind: 'text' }> => segment.kind === 'text')
    .map(segment => segment.text)
    .join('\n\n')
    .trim();

  if (!thoughtText && segments.length === 0) return null;

  // The copy affordance belongs to the prose, not the turn: rendering it after
  // every segment left it stranded below trailing tool cards, far from the
  // text it copies.
  const lastTextIndex = segments.reduce((acc, segment, index) => (segment.kind === 'text' ? index : acc), -1);

  return (
    <div className="min-w-0">
      <div className="space-y-2">
        {thoughtText && <ThinkingRow text={thoughtText} streaming={streamingThought} />}
        {segments.map((segment, index) =>
          segment.kind === 'text' ? (
            <Fragment key={segment.key}>
              <div className="message-content text-[15px] leading-[1.72] text-text-primary">
                <MessageResponse>{segment.text}</MessageResponse>
              </div>
              {!isStreaming && index === lastTextIndex && (
                <div className="flex justify-start">
                  <CopyButton text={proseText} />
                </div>
              )}
            </Fragment>
          ) : (
            <ToolCallGroup key={segment.key} entries={segment.entries} onPermissionRespond={onPermissionRespond} />
          ),
        )}
      </div>
    </div>
  );
});

// =============================================================================
// 思考 — 整个回合只留一行，默认收起
// =============================================================================

function ThinkingRow({ text, streaming }: { text: string; streaming: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen(value => !value)}
        aria-expanded={open}
        className="flex items-center gap-1.5 rounded-md py-0.5 text-[12px] font-medium text-text-muted transition-colors hover:text-text-primary"
      >
        <BrainIcon className="h-3.5 w-3.5 shrink-0 text-text-muted/80" />
        {streaming ? (
          <Shimmer as="span" duration={1} className="text-[12px] text-text-muted">
            思考中…
          </Shimmer>
        ) : (
          <span>思考过程</span>
        )}
        <ChevronDown className={cn('h-3 w-3 shrink-0 text-text-muted/70 transition-transform', open && 'rotate-180')} />
      </button>
      {open && (
        <div className="mt-1.5 max-h-[40vh] overflow-y-auto overscroll-contain whitespace-pre-wrap break-words border-l border-border/70 pl-3 text-[13px] leading-[1.65] text-text-secondary">
          {text}
        </div>
      )}
    </div>
  );
}

// =============================================================================
// 复制 — 一次回答只放一个按钮
// =============================================================================

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | null>(null);
  useEffect(
    () => () => {
      if (timer.current) window.clearTimeout(timer.current);
    },
    [],
  );

  const onCopy = useCallback(async () => {
    if (!(await copyText(text))) return;
    setCopied(true);
    if (timer.current) window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setCopied(false), 1600);
  }, [text]);

  return (
    <button
      type="button"
      onClick={() => void onCopy()}
      aria-label={copied ? '已复制' : '复制回答'}
      title={copied ? '已复制' : '复制'}
      className="-ml-1 flex h-6 w-6 items-center justify-center rounded-full text-text-muted/60 transition-colors hover:bg-surface-1 hover:text-text-primary active:bg-surface-1"
    >
      {copied ? <Check className="h-3.5 w-3.5 text-status-success" /> : <Copy className="h-3.5 w-3.5" />}
    </button>
  );
}

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
        className="cursor-pointer overflow-hidden rounded-lg border border-border transition-colors hover:border-brand/40"
        onClick={() => setOpen(true)}
        aria-label="查看大图"
      >
        <img src={dataUrl} alt="会话图片" className="h-20 w-20 object-cover" />
      </button>
      {open && (
        <ModalSheet
          open
          onClose={() => setOpen(false)}
          label="图片预览"
          className="!inset-0 !h-[var(--app-height,100dvh)] !w-full !translate-x-0 !translate-y-0 bg-black/90 p-4"
        >
          <div
            className="flex h-full items-center justify-center"
            onClick={event => {
              if (event.target === event.currentTarget) setOpen(false);
            }}
          >
            <img src={dataUrl} alt="会话图片" className="max-h-full max-w-full rounded-lg object-contain" />
          </div>
          <button
            type="button"
            onClick={() => setOpen(false)}
            aria-label="关闭"
            className="absolute right-3 flex h-10 w-10 items-center justify-center rounded-full bg-white/15 text-2xl leading-none text-white backdrop-blur"
            style={{ top: 'calc(0.75rem + env(safe-area-inset-top))' }}
          >
            {'×'}
          </button>
        </ModalSheet>
      )}
    </>
  );
}
