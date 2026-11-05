//
//  BranchTreeView.swift
//  Agents Anywhere
//
//  Branch tree visualization and management
//

import SwiftUI

struct BranchTreeView: View {
    let sessionId: String
    @Binding var activeBranchId: String?
    @StateObject private var viewModel = BranchTreeViewModel()

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Conversation Branches")
                .font(.headline)
                .padding(.horizontal)

            if viewModel.isLoading {
                ProgressView()
                    .frame(maxWidth: .infinity)
                    .padding()
            } else if viewModel.branches.isEmpty {
                Text("No branches yet")
                    .foregroundColor(.secondary)
                    .font(.subheadline)
                    .padding()
            } else {
                ScrollView {
                    VStack(spacing: 8) {
                        // Main branch
                        BranchRow(
                            name: "Main Branch",
                            isActive: activeBranchId == nil || activeBranchId == "main",
                            depth: 0
                        ) {
                            await switchBranch(to: "main")
                        } onDelete: {
                            // Cannot delete main branch
                        }

                        // Branch tree
                        ForEach(viewModel.rootBranches) { branch in
                            BranchTreeNode(
                                branch: branch,
                                allBranches: viewModel.branches,
                                activeBranchId: activeBranchId,
                                depth: 0,
                                onSwitch: { branchId in
                                    await switchBranch(to: branchId)
                                },
                                onDelete: { branchId in
                                    await deleteBranch(branchId)
                                }
                            )
                        }
                    }
                    .padding(.horizontal)
                }
            }
        }
        .task {
            await viewModel.loadBranches(sessionId: sessionId)
        }
    }

    private func switchBranch(to branchId: String) async {
        do {
            try await V2API.shared.switchBranch(sessionId: sessionId, branchId: branchId)
            activeBranchId = branchId == "main" ? nil : branchId
            await viewModel.loadBranches(sessionId: sessionId)
        } catch {
            print("Failed to switch branch: \(error)")
        }
    }

    private func deleteBranch(_ branchId: String) async {
        do {
            try await V2API.shared.deleteBranch(sessionId: sessionId, branchId: branchId)
            await viewModel.loadBranches(sessionId: sessionId)
        } catch {
            print("Failed to delete branch: \(error)")
        }
    }
}

struct BranchTreeNode: View {
    let branch: ConversationBranch
    let allBranches: [ConversationBranch]
    let activeBranchId: String?
    let depth: Int
    let onSwitch: (String) async -> Void
    let onDelete: (String) async -> Void

    var children: [ConversationBranch] {
        allBranches.filter { $0.parentBranchId == branch.branchId }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            BranchRow(
                name: branch.displayName,
                isActive: activeBranchId == branch.branchId,
                depth: depth
            ) {
                await onSwitch(branch.branchId)
            } onDelete: {
                await onDelete(branch.branchId)
            }

            ForEach(children) { child in
                BranchTreeNode(
                    branch: child,
                    allBranches: allBranches,
                    activeBranchId: activeBranchId,
                    depth: depth + 1,
                    onSwitch: onSwitch,
                    onDelete: onDelete
                )
            }
        }
    }
}

struct BranchRow: View {
    let name: String
    let isActive: Bool
    let depth: Int
    let onTap: () async -> Void
    let onDelete: () async -> Void

    var body: some View {
        HStack {
            HStack(spacing: 8) {
                Image(systemName: isActive ? "circle.fill" : "circle")
                    .foregroundColor(isActive ? .blue : .gray)
                    .font(.system(size: 10))

                Text(name)
                    .font(.subheadline)
                    .fontWeight(isActive ? .semibold : .regular)
            }
            .padding(.leading, CGFloat(depth * 20))

            Spacer()

            if depth > 0 {
                Button(action: {
                    Task { await onDelete() }
                }) {
                    Image(systemName: "trash")
                        .foregroundColor(.red)
                        .font(.system(size: 14))
                }
            }
        }
        .padding(.vertical, 8)
        .padding(.horizontal, 12)
        .background(isActive ? Color.blue.opacity(0.1) : Color.clear)
        .cornerRadius(8)
        .contentShape(Rectangle())
        .onTapGesture {
            Task { await onTap() }
        }
    }
}

@MainActor
class BranchTreeViewModel: ObservableObject {
    @Published var branches: [ConversationBranch] = []
    @Published var isLoading = false

    var rootBranches: [ConversationBranch] {
        branches.filter { $0.parentBranchId == nil || $0.parentBranchId?.isEmpty == true }
    }

    func loadBranches(sessionId: String) async {
        isLoading = true
        defer { isLoading = false }

        do {
            branches = try await V2API.shared.listBranches(sessionId: sessionId)
        } catch {
            print("Failed to load branches: \(error)")
        }
    }
}
