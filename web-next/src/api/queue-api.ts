// Message Queue API Client

export interface QueuedMessage {
  id: string;
  sessionId: string;
  content: string;
  attachments?: string[];
  status: 'queued' | 'running' | 'completed' | 'error' | 'cancelled';
  source: string;
  errorMessage?: string;
  createdAt: string;
  startedAt?: string;
  completedAt?: string;
  seq: number;
}

export interface EnqueueMessageRequest {
  content: string;
  attachments?: string[];
  source?: string;
}

export interface QueueMessagesResponse {
  messages: QueuedMessage[];
}

/**
 * Enqueue a new message to the queue
 */
export async function enqueueMessage(
  sessionId: string,
  data: EnqueueMessageRequest
): Promise<QueuedMessage> {
  const response = await fetch(`/api/v2/sessions/${sessionId}/queue`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      content: data.content,
      attachments: data.attachments,
      source: data.source || 'web',
    }),
  });

  if (!response.ok) {
    throw new Error(`Failed to enqueue message: ${response.statusText}`);
  }

  return response.json();
}

/**
 * Get queue messages (supports offline recovery with ?since=seq)
 */
export async function getQueueMessages(
  sessionId: string,
  sinceSeq?: number
): Promise<QueuedMessage[]> {
  const url = sinceSeq !== undefined
    ? `/api/v2/sessions/${sessionId}/queue?since=${sinceSeq}`
    : `/api/v2/sessions/${sessionId}/queue`;

  const response = await fetch(url, {
    method: 'GET',
  });

  if (!response.ok) {
    throw new Error(`Failed to get queue messages: ${response.statusText}`);
  }

  const data: QueueMessagesResponse = await response.json();
  return data.messages;
}

/**
 * Cancel a queued message
 */
export async function cancelQueueMessage(
  sessionId: string,
  messageId: string
): Promise<void> {
  const response = await fetch(`/api/v2/sessions/${sessionId}/queue/${messageId}`, {
    method: 'DELETE',
  });

  if (!response.ok) {
    throw new Error(`Failed to cancel message: ${response.statusText}`);
  }
}

/**
 * Update message status (typically called by connector)
 */
export async function updateMessageStatus(
  sessionId: string,
  messageId: string,
  status: string,
  errorMessage?: string
): Promise<void> {
  const response = await fetch(`/api/v2/sessions/${sessionId}/queue/${messageId}`, {
    method: 'PATCH',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      status,
      errorMessage,
    }),
  });

  if (!response.ok) {
    throw new Error(`Failed to update message status: ${response.statusText}`);
  }
}
