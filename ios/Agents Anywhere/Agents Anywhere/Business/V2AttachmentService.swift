import Foundation

struct V2AttachmentService {
    let attachmentAPI: any V2AttachmentAPIProtocol

    /// Uploads user-selected bytes to the server's session-scoped attachment store.
    func upload(
        sessionId: V2SessionID,
        attachments: [V2LocalAttachment]
    ) async throws -> [V2AttachmentReference] {
        if attachments.isEmpty { throw V2BusinessError.emptyAttachmentSelection }
        if attachments.count > 5 {
            throw V2BusinessError.tooManyAttachments(maximum: 5)
        }
        let files = try attachments.map { attachment in
            if attachment.data.isEmpty {
                throw V2BusinessError.emptyAttachment(name: attachment.name)
            }
            if attachment.data.count > 25 * 1024 * 1024 { throw V2BusinessError.attachmentTooLarge(name: attachment.name) }
            return HTTPUploadFile(
                fieldName: "files",
                fileName: attachment.name,
                mediaType: attachment.mediaType,
                data: attachment.data
            )
        }
        return try await attachmentAPI.upload(sessionId: sessionId, files: files).attachments
    }

    func download(sessionId: V2SessionID, fileId: V2AttachmentID) async throws -> Data {
        let response = try await attachmentAPI.download(sessionId: sessionId, fileId: fileId)
        // Large base64 payloads can take noticeable time to decode. Keep the
        // chat and the keyboard responsive while the image bytes are prepared.
        let encoded = response.contentBase64
        let worker = Task.detached(priority: .userInitiated) { Data(base64Encoded: encoded) }
        let decoded = await withTaskCancellationHandler {
            await worker.value
        } onCancel: {
            worker.cancel()
        }
        try Task.checkCancellation()
        guard let data = decoded else {
            throw HTTPError.decoding(message: String(localized: "The attachment content is not valid Base64 data."))
        }
        return data
    }
}

/// Shares one in-flight download between callers (the visible thumbnail task
/// and a tap that opens the same attachment). Two concurrent 3 MB reads of the
/// same file doubled the transfer time on cellular and made the tap look
/// stuck; the second caller now awaits the first task's result.
@MainActor
final class V2AttachmentInflight {
    private struct Download {
        let id: UUID
        let task: Task<Data, Error>
    }
    private var tasks: [String: Download] = [:]

    func download(using service: V2AttachmentService, sessionId: V2SessionID, fileId: V2AttachmentID) async throws -> Data {
        let key = "\(sessionId)/\(fileId)"
        if let existing = tasks[key] {
            return try await existing.task.value
        }
        let id = UUID()
        let task = Task { try await service.download(sessionId: sessionId, fileId: fileId) }
        tasks[key] = Download(id: id, task: task)
        defer { if tasks[key]?.id == id { tasks[key] = nil } }
        do {
            return try await task.value
        } catch {
            // A failed read must not poison the next attempt.
            throw error
        }
    }

    func cancel(sessionId: V2SessionID, fileId: V2AttachmentID) {
        let key = "\(sessionId)/\(fileId)"
        let current = tasks.removeValue(forKey: key)
        current?.task.cancel()
    }
}
