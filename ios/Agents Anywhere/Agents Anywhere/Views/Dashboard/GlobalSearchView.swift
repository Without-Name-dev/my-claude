import SwiftUI

struct GlobalSearchView: View {
    @Environment(\.dismiss) private var dismiss
    @StateObject private var viewModel = GlobalSearchViewModel()
    @State private var searchText = ""
    @FocusState private var isSearchFocused: Bool

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                searchBar

                if viewModel.isLoading && viewModel.results.isEmpty {
                    Spacer()
                    ProgressView()
                    Spacer()
                } else if searchText.isEmpty {
                    emptyState
                } else if viewModel.results.isEmpty && !viewModel.isLoading {
                    noResults
                } else {
                    resultsList
                }
            }
            .navigationTitle("Search")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        dismiss()
                    }
                }
            }
        }
        .onAppear {
            isSearchFocused = true
        }
    }

    private var searchBar: some View {
        HStack {
            Image(systemName: "magnifyingglass")
                .foregroundColor(.secondary)

            TextField("Search messages", text: $searchText)
                .textFieldStyle(.plain)
                .focused($isSearchFocused)
                .onChange(of: searchText) { _, newValue in
                    viewModel.search(query: newValue)
                }
                .submitLabel(.search)

            if !searchText.isEmpty {
                Button(action: {
                    searchText = ""
                    viewModel.clearResults()
                }) {
                    Image(systemName: "xmark.circle.fill")
                        .foregroundColor(.secondary)
                }
            }
        }
        .padding(12)
        .background(Color(.systemGray6))
        .cornerRadius(10)
        .padding()
    }

    private var emptyState: some View {
        VStack(spacing: 16) {
            Spacer()
            Image(systemName: "magnifyingglass")
                .font(.system(size: 48))
                .foregroundColor(.secondary)
            Text("Search across all conversations")
                .font(.headline)
            Text("Find messages, code, and insights")
                .font(.subheadline)
                .foregroundColor(.secondary)
            Spacer()
        }
        .padding()
    }

    private var noResults: some View {
        VStack(spacing: 16) {
            Spacer()
            Image(systemName: "doc.text.magnifyingglass")
                .font(.system(size: 48))
                .foregroundColor(.secondary)
            Text("No results found")
                .font(.headline)
            Text("Try different keywords")
                .font(.subheadline)
                .foregroundColor(.secondary)
            Spacer()
        }
        .padding()
    }

    private var resultsList: some View {
        ScrollView {
            LazyVStack(spacing: 12) {
                ForEach(viewModel.results, id: \.itemId) { result in
                    SearchResultRow(result: result)
                        .onTapGesture {
                            viewModel.selectResult(result)
                            dismiss()
                        }
                }

                if viewModel.hasMore {
                    ProgressView()
                        .padding()
                        .onAppear {
                            viewModel.loadMore()
                        }
                }
            }
            .padding()
        }
    }
}

struct SearchResultRow: View {
    let result: V2APIClient.SearchResult

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text(result.sessionTitle.isEmpty ? "Untitled" : result.sessionTitle)
                    .font(.subheadline)
                    .foregroundColor(.secondary)

                Spacer()

                if let role = result.role {
                    Text(role.capitalized)
                        .font(.caption)
                        .padding(.horizontal, 8)
                        .padding(.vertical, 4)
                        .background(roleColor(role))
                        .foregroundColor(.white)
                        .cornerRadius(4)
                }
            }

            Text(attributedSnippet)
                .font(.body)
                .lineLimit(3)

            if let itemTime = result.itemTime {
                Text(formatDate(itemTime))
                    .font(.caption)
                    .foregroundColor(.secondary)
            }
        }
        .padding()
        .background(Color(.systemBackground))
        .cornerRadius(12)
        .shadow(color: .black.opacity(0.05), radius: 2, x: 0, y: 1)
    }

    private var attributedSnippet: AttributedString {
        var text = result.snippet
        text = text.replacingOccurrences(of: "<mark>", with: "**")
        text = text.replacingOccurrences(of: "</mark>", with: "**")

        do {
            return try AttributedString(markdown: text)
        } catch {
            return AttributedString(result.snippet)
        }
    }

    private func roleColor(_ role: String) -> Color {
        switch role.lowercased() {
        case "user": return .blue
        case "assistant": return .purple
        default: return .gray
        }
    }

    private func formatDate(_ timestamp: Int64) -> String {
        let date = Date(timeIntervalSince1970: TimeInterval(timestamp) / 1000)
        let formatter = RelativeDateTimeFormatter()
        formatter.unitsStyle = .short
        return formatter.localizedString(for: date, relativeTo: Date())
    }
}

@MainActor
class GlobalSearchViewModel: ObservableObject {
    @Published var results: [V2APIClient.SearchResult] = []
    @Published var isLoading = false
    @Published var hasMore = false

    private var currentQuery = ""
    private var currentOffset = 0
    private let limit = 20
    private var searchTask: Task<Void, Never>?

    func search(query: String) {
        guard !query.isEmpty else {
            clearResults()
            return
        }

        searchTask?.cancel()
        currentQuery = query
        currentOffset = 0

        searchTask = Task {
            do {
                try await Task.sleep(nanoseconds: 300_000_000)
                guard !Task.isCancelled else { return }

                isLoading = true

                let response = try await APIClient.shared.v2.search(
                    query: query,
                    limit: limit,
                    offset: currentOffset
                )

                guard !Task.isCancelled else { return }

                results = response.results
                hasMore = response.total > results.count
                isLoading = false
            } catch {
                if !Task.isCancelled {
                    print("Search error: \(error)")
                    isLoading = false
                }
            }
        }
    }

    func loadMore() {
        guard !isLoading && hasMore else { return }

        currentOffset += limit
        isLoading = true

        Task {
            do {
                let response = try await APIClient.shared.v2.search(
                    query: currentQuery,
                    limit: limit,
                    offset: currentOffset
                )

                results.append(contentsOf: response.results)
                hasMore = response.total > results.count
                isLoading = false
            } catch {
                print("Load more error: \(error)")
                isLoading = false
            }
        }
    }

    func clearResults() {
        searchTask?.cancel()
        results = []
        currentQuery = ""
        currentOffset = 0
        hasMore = false
    }

    func selectResult(_ result: V2APIClient.SearchResult) {
        NotificationCenter.default.post(
            name: .navigateToSession,
            object: nil,
            userInfo: ["sessionId": result.sessionId, "itemId": result.itemId]
        )
    }
}

extension Notification.Name {
    static let navigateToSession = Notification.Name("navigateToSession")
}
