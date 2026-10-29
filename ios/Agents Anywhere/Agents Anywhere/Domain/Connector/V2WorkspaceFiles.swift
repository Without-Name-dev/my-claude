import Foundation

nonisolated struct V2WorkspaceFilesListRequest: Encodable, Hashable {
    let root: String
    let path: String
}

nonisolated struct V2WorkspaceFileReadRequest: Encodable, Hashable {
    let path: String
}

nonisolated struct V2WorkspaceDirectoryResponse: Decodable, Hashable {
    let ok: Bool
    let result: V2WorkspaceDirectory?
    let error: V2RuntimeError?
}

nonisolated struct V2WorkspaceDirectory: Decodable, Hashable {
    let path: String
    let entries: [V2WorkspaceEntry]
    let truncated: Bool?
    let targetPath: String?
    let targetType: String?
}

nonisolated struct V2WorkspaceEntry: Decodable, Identifiable, Hashable {
    let name: String
    let path: String
    let type: String
    let size: Int?
    /// The connector reports epoch milliseconds.
    let modifiedAt: Int64?

    var id: String { path }
    var isDirectory: Bool { type == "directory" }
    var isFile: Bool { type == "file" }
}

nonisolated struct V2WorkspaceFilePreviewToken: Decodable, Hashable {
    let previewToken: String
    let expiresAt: String
    let serverTime: String
}

nonisolated struct V2WorkspaceFileReadResponse: Decodable {
    let ok: Bool
    let result: V2WorkspaceFileTransfer?
    let error: V2RuntimeError?
}

nonisolated struct V2WorkspaceFileTransfer: Decodable {
    let name: String
    let size: Int64
    let downloadUrl: String
}

nonisolated struct V2WorkspaceTextRequest: Encodable, Hashable {
    let path: String
    var maxBytes: Int = 1_048_576
}

nonisolated struct V2WorkspaceTextResponse: Decodable, Hashable {
    let path: String
    let name: String
    let size: Int
    let sha256: String
    let encoding: String
    let content: String
    let truncated: Bool
    let binary: Bool
    let serverTime: String
}

nonisolated struct V2WorkspaceFileWriteRequest: Encodable, Hashable {
    let path: String
    let content: String
    let encoding: String
    let ifMatch: String?

    init(path: String, content: String, encoding: String = "utf-8", ifMatch: String? = nil) {
        self.path = path
        self.content = content
        self.encoding = encoding
        self.ifMatch = ifMatch
    }
}

nonisolated struct V2WorkspaceFileWriteResponse: Decodable, Hashable {
    let ok: Bool
    let result: V2WorkspaceWriteResult?
    let error: V2RuntimeError?
    let serverTime: String
}

nonisolated struct V2WorkspaceWriteResult: Decodable, Hashable {
    let path: String
    let size: Int
    let sha256: String
}
