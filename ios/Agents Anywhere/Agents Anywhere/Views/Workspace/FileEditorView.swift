import SwiftUI

struct FileEditorView: View {
    @StateObject private var model: FileEditorModel
    @Environment(\.dismiss) private var dismiss
    @FocusState private var isEditorFocused: Bool
    @State private var confirmsClose = false
    @State private var confirmsRevert = false

    init(connectorId: String, root: String, path: String, name: String, service: V2WorkspaceFilesService) {
        _model = StateObject(wrappedValue: FileEditorModel(
            connectorId: connectorId,
            root: root,
            path: path,
            name: name,
            service: service
        ))
    }

    var body: some View {
        NavigationStack {
            ZStack {
                if model.isLoading {
                    ProgressView(String(localized: "加载文件…"))
                        .padding(20)
                        .background(.regularMaterial, in: .rect(cornerRadius: 18))
                } else {
                    editorContent
                }
            }
            .navigationTitle(model.name)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    if model.isDirty {
                        Button(String(localized: "恢复"), appSymbol: "arrow.uturn.backward") {
                            confirmsRevert = true
                        }
                        .disabled(model.isSaving)
                    }
                }

                ToolbarItem(placement: .topBarTrailing) {
                    if model.isSaving {
                        ProgressView()
                            .accessibilityLabel(String(localized: "正在保存"))
                    } else {
                        Button(String(localized: "保存"), appSymbol: "square.and.arrow.down") {
                            Task { await model.save() }
                        }
                        .disabled(!model.isDirty || !model.isEditable)
                    }
                }

                SheetCloseToolbar(disabled: model.isSaving) {
                    if model.isDirty { confirmsClose = true } else { dismiss() }
                }
            }
            .task {
                await model.load()
            }
        }
        .appSheetPresentation(.expanded)
        .interactiveDismissDisabled(model.isDirty || model.isSaving)
        .confirmDiscardChanges($confirmsClose) { dismiss() }
        .confirmationDialog(String(localized: "恢复到上次保存的内容？"), isPresented: $confirmsRevert, titleVisibility: .visible) {
            Button(String(localized: "恢复内容"), role: .destructive) { model.revert() }
            Button(String(localized: "继续编辑"), role: .cancel) {}
        } message: {
            Text(String(localized: "当前未保存的修改将丢失。"))
        }
    }

    @ViewBuilder
    private var editorContent: some View {
        VStack(spacing: 0) {
            if model.isDirty {
                HStack(spacing: 6) {
                    Image(systemName: "circle.fill").font(.system(size: 6))
                    Text(String(localized: "有未保存的修改"))
                    Spacer()
                }
                .font(.caption).foregroundStyle(.secondary)
                .padding(.horizontal, 16).padding(.vertical, 6)
                .accessibilityElement(children: .combine)
            }
            if let error = model.error {
                errorBanner(error)
            }

            if model.saveSuccess {
                successBanner
            }

            TextEditor(text: $model.content)
                .disabled(!model.isEditable)
                .font(.system(.body, design: .monospaced))
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
                .focused($isEditorFocused)
                .scrollContentBackground(.hidden)
                .background(Color(uiColor: .systemBackground))
                .overlay(alignment: .topLeading) {
                    if model.isEditable && model.content.isEmpty && !isEditorFocused {
                        Text(String(localized: "开始编辑文件…"))
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, 8)
                            .padding(.vertical, 12)
                            .allowsHitTesting(false)
                    }
                }
        }
    }

    private func errorBanner(_ message: String) -> some View {
        HStack {
            Label(message, appSymbol: "exclamationmark.triangle.fill")
                .font(.callout)
                .foregroundStyle(.white)
            Spacer()
            Button(action: { model.error = nil }) {
                AppSymbol("xmark.circle.fill")
                    .foregroundStyle(.white.opacity(0.8))
            }
            .accessibilityLabel(String(localized: "关闭提示"))
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 12)
        .background(Color.red)
        .transition(.move(edge: .top).combined(with: .opacity))
    }

    private var successBanner: some View {
        HStack {
            Label(String(localized: "已保存"), appSymbol: "checkmark.circle.fill")
                .font(.callout)
                .foregroundStyle(.white)
            Spacer()
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 12)
        .background(Color.green)
        .transition(.move(edge: .top).combined(with: .opacity))
        .onAppear {
            DispatchQueue.main.asyncAfter(deadline: .now() + 2) {
                withAnimation {
                    model.saveSuccess = false
                }
            }
        }
    }
}
