import React from 'react';
import { QueuedMessage } from '@/api/queue-api';
import { useMessageQueue } from '@/hooks/use-message-queue';

interface MessageQueueBarProps {
  sessionId: string;
}

export function MessageQueueBar({ sessionId }: MessageQueueBarProps) {
  const { messages, cancel } = useMessageQueue(sessionId);
  const [showPanel, setShowPanel] = React.useState(false);

  const queuedMessages = messages.filter(m => m.status === 'queued' || m.status === 'running');
  const runningMessage = messages.find(m => m.status === 'running');

  if (queuedMessages.length === 0) return null;

  return (
    <>
      <button
        onClick={() => setShowPanel(true)}
        className="w-full px-4 py-3 bg-gray-50 hover:bg-gray-100 rounded-lg flex items-center gap-3 transition-colors"
      >
        {runningMessage ? (
          <>
            <div className="animate-spin h-4 w-4 border-2 border-blue-500 border-t-transparent rounded-full" />
            <span className="text-sm text-gray-700 truncate flex-1 text-left">
              执行中: {runningMessage.content.slice(0, 50)}...
            </span>
          </>
        ) : (
          <>
            <svg className="w-4 h-4 text-yellow-500" fill="currentColor" viewBox="0 0 20 20">
              <path fillRule="evenodd" d="M10 18a8 8 0 100-16 8 8 0 000 16zm1-12a1 1 0 10-2 0v4a1 1 0 00.293.707l2.828 2.829a1 1 0 101.415-1.415L11 9.586V6z" clipRule="evenodd" />
            </svg>
            <span className="text-sm text-gray-700">排队中 ({queuedMessages.length})</span>
          </>
        )}
        <svg className="w-4 h-4 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M5 15l7-7 7 7" />
        </svg>
      </button>

      {showPanel && (
        <MessageQueuePanel
          messages={messages}
          onClose={() => setShowPanel(false)}
          onCancel={cancel}
        />
      )}
    </>
  );
}

interface MessageQueuePanelProps {
  messages: QueuedMessage[];
  onClose: () => void;
  onCancel: (messageId: string) => Promise<void>;
}

function MessageQueuePanel({ messages, onClose, onCancel }: MessageQueuePanelProps) {
  return (
    <div className="fixed inset-0 z-50 bg-black/50 flex items-center justify-center p-4">
      <div className="bg-white rounded-lg shadow-xl max-w-2xl w-full max-h-[80vh] flex flex-col">
        <div className="flex items-center justify-between p-4 border-b">
          <h2 className="text-lg font-semibold">消息队列</h2>
          <button
            onClick={onClose}
            className="text-gray-400 hover:text-gray-600"
          >
            <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
            </svg>
          </button>
        </div>

        <div className="flex-1 overflow-y-auto p-4 space-y-3">
          {messages.length === 0 ? (
            <div className="text-center py-12 text-gray-500">
              <svg className="w-12 h-12 mx-auto mb-3 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M20 13V6a2 2 0 00-2-2H6a2 2 0 00-2 2v7m16 0v5a2 2 0 01-2 2H6a2 2 0 01-2-2v-5m16 0h-2.586a1 1 0 00-.707.293l-2.414 2.414a1 1 0 01-.707.293h-3.172a1 1 0 01-.707-.293l-2.414-2.414A1 1 0 006.586 13H4" />
              </svg>
              <p>队列为空</p>
            </div>
          ) : (
            messages.map(message => (
              <MessageQueueItem
                key={message.id}
                message={message}
                onCancel={onCancel}
              />
            ))
          )}
        </div>
      </div>
    </div>
  );
}

interface MessageQueueItemProps {
  message: QueuedMessage;
  onCancel: (messageId: string) => Promise<void>;
}

function MessageQueueItem({ message, onCancel }: MessageQueueItemProps) {
  const [cancelling, setCancelling] = React.useState(false);

  const handleCancel = async () => {
    setCancelling(true);
    try {
      await onCancel(message.id);
    } finally {
      setCancelling(false);
    }
  };

  const statusConfig = {
    queued: { color: 'bg-yellow-100 text-yellow-800', label: '排队中' },
    running: { color: 'bg-blue-100 text-blue-800', label: '执行中' },
    completed: { color: 'bg-green-100 text-green-800', label: '已完成' },
    error: { color: 'bg-red-100 text-red-800', label: '失败' },
    cancelled: { color: 'bg-gray-100 text-gray-800', label: '已取消' },
  };

  const config = statusConfig[message.status];

  return (
    <div className="border rounded-lg p-4 space-y-2">
      <div className="flex items-start gap-3">
        <span className={`px-2 py-1 text-xs font-medium rounded ${config.color}`}>
          {config.label}
        </span>

        <p className="flex-1 text-sm text-gray-700 line-clamp-2">
          {message.content}
        </p>

        {message.status === 'running' && (
          <div className="animate-spin h-4 w-4 border-2 border-blue-500 border-t-transparent rounded-full" />
        )}
      </div>

      <div className="flex items-center justify-between text-xs text-gray-500">
        <span>{new Date(message.createdAt).toLocaleString('zh-CN')}</span>

        {message.status === 'queued' && (
          <button
            onClick={handleCancel}
            disabled={cancelling}
            className="px-2 py-1 text-red-600 hover:bg-red-50 rounded disabled:opacity-50"
          >
            {cancelling ? '取消中...' : '取消'}
          </button>
        )}
      </div>

      {message.errorMessage && (
        <p className="text-xs text-red-600 bg-red-50 p-2 rounded">
          {message.errorMessage}
        </p>
      )}
    </div>
  );
}
