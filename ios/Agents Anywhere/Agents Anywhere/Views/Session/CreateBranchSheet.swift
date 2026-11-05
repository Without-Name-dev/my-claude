//
//  CreateBranchSheet.swift
//  Agents Anywhere
//
//  Sheet for creating a new branch from a message
//

import SwiftUI

struct CreateBranchSheet: View {
    let sessionId: String
    let itemId: String
    let onCreated: () -> Void
    @Environment(\.dismiss) private var dismiss

    @State private var branchName = ""
    @State private var isCreating = false
    @State private var errorMessage: String?

    var body: some View {
        NavigationView {
            Form {
                Section(header: Text("Branch Name")) {
                    TextField("Optional: e.g., Alternative approach", text: $branchName)
                        .disabled(isCreating)
                }

                if let error = errorMessage {
                    Section {
                        Text(error)
                            .foregroundColor(.red)
                            .font(.subheadline)
                    }
                }
            }
            .navigationTitle("Create Branch")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        dismiss()
                    }
                    .disabled(isCreating)
                }

                ToolbarItem(placement: .confirmationAction) {
                    Button("Create") {
                        Task { await createBranch() }
                    }
                    .disabled(isCreating)
                }
            }
        }
    }

    private func createBranch() async {
        isCreating = true
        errorMessage = nil

        do {
            let name = branchName.isEmpty ? nil : branchName
            _ = try await V2API.shared.createBranch(
                sessionId: sessionId,
                itemId: itemId,
                name: name
            )
            onCreated()
            dismiss()
        } catch {
            errorMessage = "Failed to create branch: \(error.localizedDescription)"
            isCreating = false
        }
    }
}

// Extension for long-press gesture on timeline messages
extension View {
    func branchGesture(sessionId: String, itemId: String, onCreated: @escaping () -> Void) -> some View {
        self.modifier(BranchGestureModifier(sessionId: sessionId, itemId: itemId, onCreated: onCreated))
    }
}

struct BranchGestureModifier: ViewModifier {
    let sessionId: String
    let itemId: String
    let onCreated: () -> Void

    @State private var showingBranchSheet = false

    func body(content: Content) -> some View {
        content
            .contextMenu {
                Button(action: {
                    showingBranchSheet = true
                }) {
                    Label("Create Branch", systemImage: "arrow.branch")
                }
            }
            .sheet(isPresented: $showingBranchSheet) {
                CreateBranchSheet(
                    sessionId: sessionId,
                    itemId: itemId,
                    onCreated: onCreated
                )
            }
    }
}
