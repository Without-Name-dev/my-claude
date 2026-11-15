/** Keep the submit placeholder only until the actual message is rendered. */
export function shouldShowPromptPlaceholder(
  prompt: string | undefined,
  viewingAgent: boolean,
  submittedMessageId: string | undefined,
  displayedMessages: readonly { uuid: string }[],
): boolean {
  if (!prompt || viewingAgent) return false
  return (
    !submittedMessageId ||
    !displayedMessages.some(message => message.uuid === submittedMessageId)
  )
}
