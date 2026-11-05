// Standalone cache checks with minimal wire stubs. Compile with Swift 6.2:
// swiftc -o /tmp/attachment-check 'Agents Anywhere/Agents Anywhere/Models/Chat/ChatAttachmentStore.swift' Tests/ChatAttachmentStoreChecks.swift
// /tmp/attachment-check
import Foundation

enum JSONValue: Codable, Hashable {
    case object([String: JSONValue]), string(String), number(Double), bool(Bool), null
    subscript(_ key: String) -> JSONValue? {
        if case let .object(fields) = self { return fields[key] }
        return nil
    }
    var stringValue: String? { if case let .string(value) = self { return value }; return nil }
}
struct V2AttachmentContent {
    let raw: JSONValue
    init(rawContent: JSONValue) { raw = rawContent }
    var fileId: String? { raw["fileId"]?.stringValue }
    var name: String? { raw["name"]?.stringValue }
    var mediaType: String? { raw["mediaType"]?.stringValue }
    var size: Int? { if case let .number(value) = raw["size"] { return Int(value) }; return nil }
    var openUrl: String? { raw["openUrl"]?.stringValue }
    var downloadUrl: String? { raw["downloadUrl"]?.stringValue }
}
extension V2AttachmentContent: Equatable {}
struct V2AttachmentReference {
    let fileId: String
    let name: String
    let mediaType: String
    let size: Int
    let openUrl: String = ""
    let downloadUrl: String = ""
}
struct ChatAttachment {
    let id: String
    let name: String
    let data: Data
    let mediaType: String
    let previewData: Data?
    var uploaded: V2AttachmentReference?
    var isImage: Bool { mediaType.hasPrefix("image/") }
}

@main
struct ChatAttachmentStoreChecks {
    @MainActor static func main() throws {
        let store = ChatAttachmentStore()
        var photo = ChatAttachment(id: "local-1", name: "photo.png", data: Data(repeating: 1, count: 25 * 1024 * 1024),
                                   mediaType: "image/png", previewData: Data([1, 2]), uploaded: nil)
        store.remember([photo], clientID: "message")
        let local = store.resolve([photo.content], clientID: "message")[0]
        precondition(store.original(for: local.content) == nil)
        photo.uploaded = V2AttachmentReference(fileId: "file_1", name: "renamed.png", mediaType: "image/png", size: photo.data.count)
        store.remember([photo], clientID: "message")
        let uploaded = store.resolve([photo.content], clientID: "message")[0]
        precondition(local.id == uploaded.id, "upload must keep the image view identity")
        precondition(store.original(for: uploaded.content)?.count == photo.data.count)
        let archive = try JSONEncoder().encode(store.archived())
        precondition(archive.count < 10_000, "original image must not be written to the archive")

        let next = V2AttachmentContent(rawContent: .object(["fileId": .string("file_2"), "mediaType": .string("image/png")]))
        store.cacheOriginal(Data(repeating: 2, count: 26 * 1024 * 1024), for: next)
        precondition(store.original(for: uploaded.content) == nil, "oldest original must be evicted at the limit")
        precondition(store.original(for: next)?.count == 26 * 1024 * 1024)
        store.clear()
        precondition(store.original(for: next) == nil)
        print("Attachment cache checks passed")
    }
}
