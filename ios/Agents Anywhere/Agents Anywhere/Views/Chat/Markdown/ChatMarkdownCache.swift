import Foundation

/// Parsed markdown kept across view lifetimes.
///
/// A row inside the transcript's lazy stack is thrown away when it leaves the
/// realized window and rebuilt when it comes back. Each rebuild used to parse
/// the whole message again: the row drew at zero height until the parse landed,
/// then inflated — which shifted everything around the reader while they were
/// scrolling, and cost a full Textual layout per row on the main actor. A
/// settled message always parses to the same blocks, so they are kept here by
/// their text.
///
/// The lock is deliberate: the view's initialiser seeds itself from here, and
/// an initialiser is not main-actor isolated.
nonisolated final class ChatMarkdownCache: @unchecked Sendable {
    static let shared = ChatMarkdownCache()

    struct Value {
        let blocks: [MarkdownBlockSnapshot]
        let state: MarkdownParseState
    }

    private struct Key: Hashable {
        let text: String
        let resolvesFileReferences: Bool
    }

    private let lock = NSLock()
    /// One entry is a message. The cap keeps a long conversation bounded while
    /// still holding more history than the reader normally scrolls back
    /// through: an evicted message would be laid out at zero height again.
    private let limit = 600
    /// A single very long message is cheaper to re-parse than to keep.
    private let maximumTextLength = 24_000
    private var entries: [Key: Value] = [:]
    private var order: [Key] = []

    func value(for text: String, resolvesFileReferences: Bool) -> Value? {
        let key = Key(text: text, resolvesFileReferences: resolvesFileReferences)
        lock.lock(); defer { lock.unlock() }
        guard let value = entries[key] else { return nil }
        touch(key)
        return value
    }

    func store(_ value: Value, for text: String, resolvesFileReferences: Bool) {
        guard text.count <= maximumTextLength else { return }
        let key = Key(text: text, resolvesFileReferences: resolvesFileReferences)
        lock.lock(); defer { lock.unlock() }
        entries[key] = value
        touch(key)
        while order.count > limit {
            entries.removeValue(forKey: order.removeFirst())
        }
    }

    /// Called with the lock held.
    private func touch(_ key: Key) {
        order.removeAll { $0 == key }
        order.append(key)
    }
}
