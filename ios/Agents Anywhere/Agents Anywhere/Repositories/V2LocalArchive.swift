import Foundation

nonisolated struct V2DashboardArchive: Codable {
    struct Page: Codable { let scope: V2SessionListScope; let info: V2SessionPageInfo }
    let connectors: [V2Connector]
    let projects: [V2Project]
    let sessions: [V2SessionMeta]
    let pages: [Page]
    var firstPageIDs: Set<String>? = nil
    var extendedScopes: Set<V2SessionListScope>? = nil
    struct Membership: Codable { let scope: V2SessionListScope; let ids: Set<String> }
    var projectMemberships: [Membership]? = nil
}

nonisolated struct V2SessionArchive: Codable {
    struct Pending: Codable {
        let id: String
        let content: String
        let attachmentIDs: [String]
        let attachments: [ChatAttachment]
        let error: String?
        let rejected: Bool
    }
    let session: V2SessionMeta
    let items: [JSONValue]
    let cursor: String
    let hasOlderItems: Bool
    let hasNewerItems: Bool
    let draft: String
    let draftAttachments: [ChatAttachment]
    /// Draft files whose bytes did not fit in the archive. They are reported
    /// on restore instead of coming back as empty attachments.
    var droppedDraftAttachments: [String]? = nil
    let pending: [Pending]
    var previews: ChatAttachmentStore.Archive? = nil

    @MainActor init(data: V2SessionData, model: V2SessionModel) {
        session = data.session; items = data.items.map(\.raw); cursor = data.cursor
        hasOlderItems = data.hasOlderItems; hasNewerItems = data.hasNewerItems
        previews = model.attachmentPreviews.archived()
        // Marked input-method text is provisional; persist the last committed
        // text so a restore does not greet the person with pinyin.
        draft = model.composer.isComposing ? model.committedDraft : model.draft
        // One archive is one JSON file with a 64 MiB ceiling, and base64 makes
        // bytes larger again. Keeping every attachment would exceed it and the
        // whole draft — text included — would silently stop being saved, so
        // bytes are kept up to a budget and the rest are reported on restore.
        var budget = 24 * 1024 * 1024
        var kept: [ChatAttachment] = []
        var dropped: [String] = []
        for attachment in model.composer.attachments {
            let archived = attachment.archived()
            if archived.uploaded == nil {
                guard archived.data.count <= budget else {
                    dropped.append(archived.name)
                    continue
                }
                budget -= archived.data.count
            }
            kept.append(archived)
        }
        draftAttachments = kept
        droppedDraftAttachments = dropped.isEmpty ? nil : dropped
        pending = model.pendingMessages.filter { $0.delivery != .confirmed }.map { value in
            let error: String?; let rejected: Bool
            switch value.delivery {
            case .rejected(let failure): error = failure.message; rejected = true
            case .uncertain(let failure): error = failure.message; rejected = false
            default: error = nil; rejected = false
            }
            // Pending uploads used to bypass the draft's byte budget. Two
            // failed 25 MB uploads were enough to push the archive past the
            // 64 MiB ceiling, at which point saveSession silently refused and
            // the draft, pending sends and timeline all stopped persisting.
            let attachments: [ChatAttachment] = value.attachments.map { attachment in
                let archived = attachment.archived()
                guard archived.uploaded == nil, !archived.data.isEmpty else { return archived }
                guard archived.data.count <= budget else {
                    dropped.append(archived.name)
                    return ChatAttachment(id: archived.id, name: archived.name, data: Data(),
                        mediaType: archived.mediaType, previewData: archived.previewData)
                }
                budget -= archived.data.count
                return archived
            }
            return Pending(id: value.id, content: value.content, attachmentIDs: value.attachmentIDs,
                attachments: attachments, error: error, rejected: rejected)
        }
    }

    @MainActor func projection(maximumItems: Int) throws -> V2SessionProjection {
        projection(items: try items.map { try V2TimelineItem(json: $0) }, maximumItems: maximumItems)
    }

    @MainActor func projection(items decoded: [V2TimelineItem], maximumItems: Int) -> V2SessionProjection {
        let empty = V2RuntimeCapabilitySnapshot(revision: 0, capabilities: [])
        let snapshot = V2SessionSnapshot(session: session, state: nil,
            timeline: .init(items: decoded, nextSeq: V2SessionProjection.sequence(cursor), hasMore: hasOlderItems),
            approvals: [], notices: [], effectiveCapabilities: empty, runtimeCapabilities: empty,
            catalogs: [:], eventCursor: cursor, serverTime: "")
        return V2SessionProjection(archive: snapshot, hasNewerItems: hasNewerItems, maximumItems: maximumItems)
    }
}

/// An archive plus its timeline already decoded by the store actor.
nonisolated struct V2SessionRestore {
    let archive: V2SessionArchive
    let items: [V2TimelineItem]
}

extension ChatAttachment {
    func archived() -> Self {
        // Uploaded bytes are retrievable by fileId. Keep the small thumbnail and
        // original local bytes only while an upload has not completed.
        var copy = Self(id: id, name: name, data: uploaded == nil ? data : Data(), mediaType: mediaType, previewData: previewData)
        copy.uploaded = uploaded
        return copy
    }
}
