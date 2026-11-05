'use client';

import { Button } from '../ui/button';
import { cn } from '../../src/lib/utils';
import { ArrowDownIcon, UserIcon } from 'lucide-react';
import type { ComponentProps } from 'react';
import { useCallback, useEffect, useState } from 'react';
import { StickToBottom, useStickToBottomContext } from 'use-stick-to-bottom';

export type ConversationProps = ComponentProps<typeof StickToBottom>;

const reduceMotion =
  typeof window !== 'undefined' && Boolean(window.matchMedia?.('(prefers-reduced-motion: reduce)').matches);

export const Conversation = ({ className, ...props }: ConversationProps) => {
  // The first history page lands a beat after mount. Letting the scroller
  // "smoothly" travel that distance animated the whole transcript past the
  // eye (and dropped frames); keep it instant during startup, then animate
  // small growth (streaming text) afterwards.
  const [settled, setSettled] = useState(false);
  useEffect(() => {
    const timer = window.setTimeout(() => setSettled(true), 1500);
    return () => window.clearTimeout(timer);
  }, []);

  return (
    <StickToBottom
      className={cn('relative min-h-0 flex-1 overflow-y-hidden overflow-x-hidden', className)}
      initial="instant"
      resize={settled && !reduceMotion ? 'smooth' : 'instant'}
      role="log"
      {...props}
    />
  );
};

export type ConversationContentProps = ComponentProps<typeof StickToBottom.Content>;

export const ConversationContent = ({ className, scrollClassName, ...props }: ConversationContentProps) => (
  <StickToBottom.Content
    className={cn('mx-auto flex min-h-full max-w-3xl flex-col gap-2 px-4 py-8 sm:px-8 sm:py-12 min-w-0', className)}
    // Stops a drag at the end of the transcript from chaining to the page;
    // the WKWebView rubber-banding is what made the screen shake.
    scrollClassName={cn('rcs-scroller', scrollClassName)}
    {...props}
  />
);

export type ConversationEmptyStateProps = ComponentProps<'div'> & {
  title?: string;
  description?: string;
  icon?: React.ReactNode;
};

export const ConversationEmptyState = ({
  className,
  title = 'No messages yet',
  description = 'Start a conversation to see messages here',
  icon,
  children,
  ...props
}: ConversationEmptyStateProps) => (
  <div
    className={cn('flex size-full flex-col items-center justify-center gap-4 p-8 text-center', className)}
    {...props}
  >
    {children ?? (
      <>
        {icon && <div className="text-text-muted">{icon}</div>}
        <div className="space-y-2">
          <h3 className="font-semibold text-base font-display text-text-primary">{title}</h3>
          {description && <p className="text-text-muted text-sm leading-relaxed max-w-xs">{description}</p>}
        </div>
      </>
    )}
  </div>
);

export type ConversationScrollButtonProps = ComponentProps<typeof Button>;

/**
 * Button to scroll to the bottom of the conversation.
 * Can be used standalone or within ConversationScrollButtons container.
 * When used standalone, it handles its own visibility based on isAtBottom.
 * When used in ConversationScrollButtons, the container manages visibility.
 */
export const ConversationScrollButton = ({ className, ...props }: ConversationScrollButtonProps) => {
  const { scrollToBottom } = useStickToBottomContext();

  const handleScrollToBottom = useCallback(() => {
    scrollToBottom();
  }, [scrollToBottom]);

  return (
    <Button
      className={cn('rounded-full', className)}
      onClick={handleScrollToBottom}
      size="icon"
      type="button"
      variant="outline"
      title="Scroll to bottom"
      {...props}
    >
      <ArrowDownIcon className="size-4" />
    </Button>
  );
};

/**
 * Data attribute used to mark the last user message element.
 * ChatInterface adds this attribute to the last user message for scroll targeting.
 */
export const LAST_USER_MESSAGE_ATTR = 'data-last-user-message';

export type ConversationScrollToLastUserMessageButtonProps = ComponentProps<typeof Button>;

/**
 * Button to scroll to the last user message in the conversation.
 * Reference: Issue #3 - Provide a feature to locate the last human message
 */
export const ConversationScrollToLastUserMessageButton = ({
  className,
  ...props
}: ConversationScrollToLastUserMessageButtonProps) => {
  const handleScrollToLastUserMessage = useCallback(() => {
    // Find the last user message element by data attribute
    const lastUserMessage = document.querySelector(`[${LAST_USER_MESSAGE_ATTR}="true"]`);
    if (lastUserMessage) {
      lastUserMessage.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  }, []);

  return (
    <Button
      className={cn('rounded-full', className)}
      onClick={handleScrollToLastUserMessage}
      size="icon"
      type="button"
      variant="outline"
      title="Scroll to last user message"
      {...props}
    >
      <UserIcon className="size-4" />
    </Button>
  );
};

export type ConversationScrollButtonsProps = ComponentProps<'div'> & {
  /** Whether there are user messages to scroll to */
  hasUserMessages?: boolean;
};

/**
 * Container for scroll navigation buttons.
 * Renders scroll-to-last-user-message and scroll-to-bottom buttons side by side.
 * Reference: Issue #3 - Provide a feature to locate the last human message
 */
export const ConversationScrollButtons = ({
  className,
  hasUserMessages = false,
  ...props
}: ConversationScrollButtonsProps) => {
  const { isAtBottom, scrollToBottom } = useStickToBottomContext();

  if (isAtBottom) return null;

  return (
    <div className={cn('absolute bottom-3 right-3 sm:bottom-4 sm:right-4', className)} {...props}>
      <button
        type="button"
        onClick={() => scrollToBottom()}
        className="flex h-9 w-9 items-center justify-center rounded-full border border-border/70 bg-surface-2/95 text-text-secondary shadow-sm backdrop-blur transition-colors hover:bg-surface-1 hover:text-text-primary"
        title="回到底部"
        aria-label="回到底部"
      >
        <ArrowDownIcon className="size-4" />
      </button>
    </div>
  );
};
