import { useState, useEffect, useCallback } from 'react';
import { QueuedMessage, getQueueMessages, enqueueMessage, cancelQueueMessage } from '@/api/queue-api';

export function useMessageQueue(sessionId: string) {
  const [messages, setMessages] = useState<QueuedMessage[]>([]);
  const [isLoading, setIsLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [lastSeq, setLastSeq] = useState(0);

  // Fetch initial messages
  const fetchMessages = useCallback(async () => {
    if (!sessionId) return;

    setIsLoading(true);
    setError(null);

    try {
      const msgs = await getQueueMessages(sessionId, lastSeq);

      setMessages(prev => {
        const merged = [...prev];
        for (const newMsg of msgs) {
          const index = merged.findIndex(m => m.id === newMsg.id);
          if (index >= 0) {
            merged[index] = newMsg;
          } else {
            merged.push(newMsg);
          }
          setLastSeq(seq => Math.max(seq, newMsg.seq));
        }
        return merged.sort((a, b) => a.seq - b.seq);
      });
    } catch (err) {
      setError(err as Error);
    } finally {
      setIsLoading(false);
    }
  }, [sessionId, lastSeq]);

  // Enqueue new message
  const enqueue = useCallback(async (content: string, attachments?: string[]) => {
    if (!sessionId) return;

    try {
      const msg = await enqueueMessage(sessionId, { content, attachments, source: 'web' });
      setMessages(prev => [...prev, msg].sort((a, b) => a.seq - b.seq));
      setLastSeq(seq => Math.max(seq, msg.seq));
      return msg;
    } catch (err) {
      setError(err as Error);
      throw err;
    }
  }, [sessionId]);

  // Cancel message
  const cancel = useCallback(async (messageId: string) => {
    if (!sessionId) return;

    try {
      await cancelQueueMessage(sessionId, messageId);
      setMessages(prev => prev.map(msg =>
        msg.id === messageId
          ? { ...msg, status: 'cancelled' as const, completedAt: new Date().toISOString() }
          : msg
      ));
    } catch (err) {
      setError(err as Error);
      throw err;
    }
  }, [sessionId]);

  // Handle WebSocket events
  const handleQueueEvent = useCallback((event: any) => {
    if (event.type === 'queue.message_added' && event.message) {
      setMessages(prev => {
        const exists = prev.find(m => m.id === event.message.id);
        if (exists) return prev;
        return [...prev, event.message].sort((a, b) => a.seq - b.seq);
      });
      setLastSeq(seq => Math.max(seq, event.message.seq));
    } else if (event.type === 'queue.message_status_changed' || event.type === 'queue.message_completed') {
      setMessages(prev => prev.map(msg =>
        msg.id === event.messageId
          ? {
              ...msg,
              status: event.status || msg.status,
              errorMessage: event.errorMessage,
              completedAt: event.status === 'completed' ? new Date().toISOString() : msg.completedAt,
            }
          : msg
      ));
    } else if (event.type === 'queue.message_cancelled') {
      setMessages(prev => prev.map(msg =>
        msg.id === event.messageId
          ? { ...msg, status: 'cancelled' as const, completedAt: new Date().toISOString() }
          : msg
      ));
    }
  }, []);

  // Fetch on mount
  useEffect(() => {
    fetchMessages();
  }, [fetchMessages]);

  return {
    messages,
    isLoading,
    error,
    enqueue,
    cancel,
    refresh: fetchMessages,
    handleQueueEvent,
  };
}
