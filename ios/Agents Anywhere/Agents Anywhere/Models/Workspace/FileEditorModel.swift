import Foundation
import Combine

@MainActor
final class FileEditorModel: ObservableObject {
    @Published var content: String = ""
    @Published var originalContent: String = ""
    @Published var originalSHA256: String = ""
    @Published var isLoading = false
    @Published var isSaving = false
    @Published var error: String?
    @Published var saveSuccess = false
    @Published var isDirty = false
    @Published private(set) var isEditable = false

    let connectorId: String
    let root: String
    let path: String
    let name: String
    let service: V2WorkspaceFilesService

    private var cancellables = Set<AnyCancellable>()

    init(connectorId: String, root: String, path: String, name: String, service: V2WorkspaceFilesService) {
        self.connectorId = connectorId
        self.root = root
        self.path = path
        self.name = name
        self.service = service

        // Track content changes
        $content
            .dropFirst()
            .sink { [weak self] newContent in
                guard let self = self else { return }
                self.isDirty = newContent != self.originalContent
                self.saveSuccess = false
            }
            .store(in: &cancellables)
    }

    func load() async {
        guard !isLoading else { return }
        isLoading = true
        error = nil
        isEditable = false

        do {
            let response = try await service.readText(
                connectorId: connectorId,
                root: root,
                path: path,
                maxBytes: 4_194_304
            )

            originalContent = response.content
            originalSHA256 = response.sha256
            content = response.content
            isDirty = false
            isEditable = !response.truncated && !response.binary && !response.sha256.isEmpty

            if response.truncated {
                error = String(localized: "文件内容已截断，仅显示前 4MB，无法编辑或保存。")
            } else if response.binary {
                error = String(localized: "二进制文件不能在文本编辑器中保存。")
            } else if response.sha256.isEmpty {
                error = String(localized: "无法确认文件版本，暂时不能编辑或保存。")
            }
        } catch {
            self.error = error.localizedDescription
        }

        isLoading = false
    }

    func save() async {
        guard !isSaving, isDirty, isEditable else { return }
        isSaving = true
        error = nil
        saveSuccess = false
        let savedContent = content

        do {
            let result = try await service.writeFile(
                connectorId: connectorId,
                root: root,
                path: path,
                content: savedContent,
                ifMatch: originalSHA256
            )

            originalContent = savedContent
            originalSHA256 = result.sha256
            isDirty = content != savedContent
            saveSuccess = !isDirty
            if result.sha256.isEmpty {
                isEditable = false
                error = String(localized: "文件已保存，但无法确认新版本；请重新打开文件后继续编辑。")
            }
        } catch {
            self.error = error.localizedDescription
        }

        isSaving = false
    }

    func revert() {
        content = originalContent
        isDirty = false
        saveSuccess = false
        error = nil
    }
}
