import SwiftUI
import WebKit

/// Represents a parsed artifact from AI responses
struct Artifact: Identifiable, Hashable {
    let id: String
    let type: ArtifactType
    let title: String?
    let language: String?
    let content: String
    let createdAt: Date

    enum ArtifactType: String, CaseIterable {
        case code
        case html
        case svg
        case markdown
        case json
        case mermaid
        case react

        var displayName: String {
            switch self {
            case .code: return "Code"
            case .html: return "HTML"
            case .svg: return "SVG"
            case .markdown: return "Markdown"
            case .json: return "JSON"
            case .mermaid: return "Mermaid"
            case .react: return "React"
            }
        }

        var icon: String {
            switch self {
            case .code: return "chevron.left.forwardslash.chevron.right"
            case .html, .react: return "doc.richtext"
            case .svg: return "paintbrush"
            case .markdown: return "doc.text"
            case .json: return "curlybraces"
            case .mermaid: return "chart.bar"
            }
        }
    }
}

/// Extracts artifacts from V2TimelineItem
extension Artifact {
    static func extract(from item: V2TimelineItem) -> Artifact? {
        guard item.type == .artifact,
              case let .artifact(content) = item.content else {
            return nil
        }

        let artifactType = detectType(from: content)
        let artifactContent = extractContent(from: content)

        return Artifact(
            id: item.id,
            type: artifactType,
            title: content.title,
            language: detectLanguage(from: content),
            content: artifactContent,
            createdAt: ISO8601DateFormatter().date(from: item.createdAt) ?? Date()
        )
    }

    static func extractFromMessage(_ text: String, messageId: String) -> [Artifact] {
        var artifacts: [Artifact] = []
        let pattern = "```(\\w+)\\s+artifact\\s*\\n(?:title:\\s*(.+?)\\n)?([\\s\\S]*?)```"

        guard let regex = try? NSRegularExpression(pattern: pattern, options: []) else {
            return []
        }

        let nsText = text as NSString
        let matches = regex.matches(in: text, options: [], range: NSRange(location: 0, length: nsText.length))

        for (index, match) in matches.enumerated() {
            let language = match.range(at: 1).location != NSNotFound
                ? nsText.substring(with: match.range(at: 1)) : ""
            let title = match.range(at: 2).location != NSNotFound
                ? nsText.substring(with: match.range(at: 2)) : nil
            let content = match.range(at: 3).location != NSNotFound
                ? nsText.substring(with: match.range(at: 3)) : ""

            let type = ArtifactType(rawValue: language.lowercased()) ?? .code

            artifacts.append(Artifact(
                id: "\(messageId)-artifact-\(index)",
                type: type,
                title: title,
                language: language,
                content: content.trimmingCharacters(in: .whitespacesAndNewlines),
                createdAt: Date()
            ))
        }

        return artifacts
    }

    private static func detectType(from content: V2ArtifactContent) -> ArtifactType {
        if let mediaType = content.mediaType {
            if mediaType.contains("html") { return .html }
            if mediaType.contains("svg") { return .svg }
            if mediaType.contains("json") { return .json }
            if mediaType.contains("markdown") { return .markdown }
        }
        return .code
    }

    private static func detectLanguage(from content: V2ArtifactContent) -> String? {
        content.raw["language"]?.stringValue
    }

    private static func extractContent(from content: V2ArtifactContent) -> String {
        if let text = content.raw["content"]?.stringValue {
            return text
        }
        if let text = content.raw["text"]?.stringValue {
            return text
        }
        return ""
    }
}

/// Main Artifacts Panel View
struct ArtifactsPanel: View {
    let sessionId: String
    @Binding var isPresented: Bool
    @State private var artifacts: [Artifact] = []
    @State private var selectedArtifact: Artifact?
    @State private var editMode: Bool = false

    var body: some View {
        NavigationView {
            if artifacts.isEmpty {
                emptyState
            } else {
                artifactsList
            }
        }
        .onAppear(perform: loadArtifacts)
    }

    private var emptyState: some View {
        VStack(spacing: 16) {
            Image(systemName: "tray")
                .font(.system(size: 64))
                .foregroundColor(.secondary)
            Text("No Artifacts")
                .font(.title2)
                .fontWeight(.semibold)
            Text("AI-generated code, charts, and documents will appear here")
                .font(.subheadline)
                .foregroundColor(.secondary)
                .multilineTextAlignment(.center)
                .padding(.horizontal, 32)
        }
        .navigationTitle("Artifacts")
        .toolbar {
            ToolbarItem(placement: .navigationBarTrailing) {
                Button("Done") { isPresented = false }
            }
        }
    }

    private var artifactsList: some View {
        List {
            ForEach(artifacts) { artifact in
                ArtifactRow(artifact: artifact)
                    .contentShape(Rectangle())
                    .onTapGesture {
                        selectedArtifact = artifact
                    }
            }
        }
        .navigationTitle("Artifacts")
        .toolbar {
            ToolbarItem(placement: .navigationBarTrailing) {
                Button("Done") { isPresented = false }
            }
        }
        .sheet(item: $selectedArtifact) { artifact in
            ArtifactDetailView(artifact: artifact)
        }
    }

    private func loadArtifacts() {
        // TODO: Load from session timeline
        // This would query V2TimelineItem for the current session
        // and extract artifacts using Artifact.extract(from:)
    }
}

struct ArtifactRow: View {
    let artifact: Artifact

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: artifact.type.icon)
                .font(.title2)
                .foregroundColor(.accentColor)
                .frame(width: 40)

            VStack(alignment: .leading, spacing: 4) {
                Text(artifact.title ?? "Untitled \(artifact.type.displayName)")
                    .font(.headline)

                HStack {
                    Text(artifact.type.displayName)
                        .font(.caption)
                        .foregroundColor(.secondary)

                    if let language = artifact.language {
                        Text("·")
                            .foregroundColor(.secondary)
                        Text(language)
                            .font(.caption)
                            .foregroundColor(.secondary)
                    }
                }
            }

            Spacer()

            Image(systemName: "chevron.right")
                .font(.caption)
                .foregroundColor(.secondary)
        }
        .padding(.vertical, 4)
    }
}

struct ArtifactDetailView: View {
    let artifact: Artifact
    @Environment(\.dismiss) private var dismiss
    @State private var isEditing = false
    @State private var editedContent: String
    @State private var showShareSheet = false

    init(artifact: Artifact) {
        self.artifact = artifact
        _editedContent = State(initialValue: artifact.content)
    }

    var body: some View {
        NavigationView {
            VStack(spacing: 0) {
                if artifact.type == .html || artifact.type == .svg {
                    WebPreviewView(content: previewContent)
                } else {
                    contentEditor
                }
            }
            .navigationTitle(artifact.title ?? "Artifact")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button("Close") { dismiss() }
                }

                ToolbarItem(placement: .navigationBarTrailing) {
                    Menu {
                        Button(action: { isEditing.toggle() }) {
                            Label(isEditing ? "Preview" : "Edit", systemImage: isEditing ? "eye" : "pencil")
                        }

                        Button(action: { showShareSheet = true }) {
                            Label("Export", systemImage: "square.and.arrow.up")
                        }

                        Button(action: copyContent) {
                            Label("Copy", systemImage: "doc.on.doc")
                        }
                    } label: {
                        Image(systemName: "ellipsis.circle")
                    }
                }
            }
        }
        .sheet(isPresented: $showShareSheet) {
            ShareSheet(items: [editedContent])
        }
    }

    private var contentEditor: some View {
        ScrollView {
            if isEditing {
                TextEditor(text: $editedContent)
                    .font(.system(.body, design: .monospaced))
                    .padding()
            } else {
                Text(editedContent)
                    .font(.system(.body, design: .monospaced))
                    .textSelection(.enabled)
                    .padding()
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    private var previewContent: String {
        if artifact.type == .svg {
            return """
            <!DOCTYPE html>
            <html>
            <head><meta name="viewport" content="width=device-width, initial-scale=1.0"></head>
            <body style="margin:0;padding:0;display:flex;justify-content:center;align-items:center;min-height:100vh;">
            \(editedContent)
            </body>
            </html>
            """
        }
        return editedContent
    }

    private func copyContent() {
        UIPasteboard.general.string = editedContent
    }
}

struct WebPreviewView: View {
    let content: String

    var body: some View {
        WebView(htmlContent: content)
    }
}

struct WebView: UIViewRepresentable {
    let htmlContent: String

    func makeUIView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        let webView = WKWebView(frame: .zero, configuration: config)
        return webView
    }

    func updateUIView(_ webView: WKWebView, context: Context) {
        webView.loadHTMLString(htmlContent, baseURL: nil)
    }
}

struct ShareSheet: UIViewControllerRepresentable {
    let items: [Any]

    func makeUIViewController(context: Context) -> UIActivityViewController {
        UIActivityViewController(activityItems: items, applicationActivities: nil)
    }

    func updateUIViewController(_ uiViewController: UIActivityViewController, context: Context) {}
}

