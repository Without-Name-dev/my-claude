import { useRef, type ReactNode, type RefObject } from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import { cn } from '../../lib/utils';

/** Shared, keyboard-accessible phone sheet / desktop dialog. */
export function ModalSheet({
  open,
  onClose,
  label,
  className,
  children,
  returnFocusRef,
}: {
  open: boolean;
  onClose: () => void;
  label: string;
  className?: string;
  children: ReactNode;
  returnFocusRef?: RefObject<HTMLElement | null>;
}) {
  const previousFocus = useRef<HTMLElement | null>(null);
  return (
    <Dialog.Root
      open={open}
      onOpenChange={next => {
        if (!next) onClose();
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-[125] bg-black/45 backdrop-blur-[2px]" />
        <Dialog.Content
          data-modal-sheet=""
          aria-label={label}
          aria-describedby={undefined}
          onOpenAutoFocus={() => {
            previousFocus.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
          }}
          onCloseAutoFocus={event => {
            event.preventDefault();
            const target = returnFocusRef?.current ?? previousFocus.current;
            const activeSheet = document.querySelector('[data-modal-sheet][data-state="open"]');
            if (target?.isConnected && (!activeSheet || activeSheet.contains(target))) target.focus();
          }}
          className={cn(
            'fixed inset-x-0 bottom-0 z-[126] w-full outline-none sm:inset-x-auto sm:bottom-auto sm:left-1/2 sm:top-1/2 sm:-translate-x-1/2 sm:-translate-y-1/2',
            className,
          )}
        >
          <Dialog.Title className="sr-only">{label}</Dialog.Title>
          {children}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
