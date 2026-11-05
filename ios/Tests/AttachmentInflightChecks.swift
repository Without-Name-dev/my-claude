// Compile with Swift 6.2 and Business/V2AttachmentService.swift.
import Foundation
#if os(Linux)
extension String { init(localized value: String) { self = value } }
#endif
typealias V2SessionID = String
typealias V2AttachmentID = String
struct V2LocalAttachment { let data: Data; let name: String; let mediaType: String }
struct V2AttachmentReference {}
struct HTTPUploadFile { let fieldName: String; let fileName: String; let mediaType: String; let data: Data }
struct V2AttachmentUploadResponse { let attachments: [V2AttachmentReference] }
struct V2AttachmentDownload { let contentBase64: String }
enum HTTPError: Error { case decoding(message: String) }
enum V2BusinessError: Error {
    case emptyAttachmentSelection, tooManyAttachments(maximum: Int), emptyAttachment(name: String), attachmentTooLarge(name: String)
}
@MainActor protocol V2AttachmentAPIProtocol {
    func upload(sessionId: String, files: [HTTPUploadFile]) async throws -> V2AttachmentUploadResponse
    func download(sessionId: String, fileId: String) async throws -> V2AttachmentDownload
}
@MainActor final class FakeAttachmentAPI: V2AttachmentAPIProtocol {
    var calls = 0
    func upload(sessionId: String, files: [HTTPUploadFile]) async throws -> V2AttachmentUploadResponse {
        V2AttachmentUploadResponse(attachments: [])
    }
    func download(sessionId: String, fileId: String) async throws -> V2AttachmentDownload {
        calls += 1
        let invocation = calls
        // Simulate an old request that returns after cancellation. The next
        // request remains in flight while the old caller runs its cleanup.
        if invocation == 1 { try? await Task.sleep(for: .seconds(10)) }
        else { try await Task.sleep(for: .milliseconds(300)) }
        return V2AttachmentDownload(contentBase64: Data([UInt8(invocation)]).base64EncodedString())
    }
}
@main struct AttachmentInflightChecks {
    @MainActor static func main() async throws {
        let api = FakeAttachmentAPI()
        let service = V2AttachmentService(attachmentAPI: api)
        let inflight = V2AttachmentInflight()
        let first = Task { try await inflight.download(using: service, sessionId: "s", fileId: "f") }
        try await Task.sleep(for: .milliseconds(30))
        inflight.cancel(sessionId: "s", fileId: "f")
        let second = Task { try await inflight.download(using: service, sessionId: "s", fileId: "f") }
        _ = try? await first.value
        let third = Task { try await inflight.download(using: service, sessionId: "s", fileId: "f") }
        let secondData = try await second.value
        let thirdData = try await third.value
        precondition(api.calls == 2, "old cleanup must not remove the replacement download")
        precondition(secondData == Data([2]) && thirdData == secondData)
        print("Attachment inflight checks passed")
    }
}
