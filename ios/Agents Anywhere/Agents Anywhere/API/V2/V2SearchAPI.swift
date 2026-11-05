import Foundation

extension V2APIClient {
    // MARK: - Search

    struct SearchResult: Codable {
        let sessionId: String
        let sessionTitle: String
        let itemId: String
        let itemType: String
        let role: String?
        let content: String
        let snippet: String
        let itemTime: Int64?
        let rank: Double

        enum CodingKeys: String, CodingKey {
            case sessionId = "session_id"
            case sessionTitle = "session_title"
            case itemId = "item_id"
            case itemType = "item_type"
            case role
            case content
            case snippet
            case itemTime = "item_time"
            case rank
        }
    }

    struct SearchResponse: Codable {
        let results: [SearchResult]
        let total: Int
        let limit: Int
        let offset: Int
    }

    struct SearchSuggestionsResponse: Codable {
        let suggestions: [String]
    }

    func search(query: String, limit: Int = 20, offset: Int = 0, sessionIds: [String]? = nil) async throws -> SearchResponse {
        var queryItems = [
            URLQueryItem(name: "q", value: query),
            URLQueryItem(name: "limit", value: "\(limit)"),
            URLQueryItem(name: "offset", value: "\(offset)")
        ]

        if let sessionIds = sessionIds, !sessionIds.isEmpty {
            queryItems.append(URLQueryItem(name: "session_ids", value: sessionIds.joined(separator: ",")))
        }

        var components = URLComponents(url: baseURL.appendingPathComponent("/api/v2/search"), resolvingAgainstBaseURL: false)!
        components.queryItems = queryItems

        let request = try makeRequest(url: components.url!, method: "GET")
        let (data, _) = try await URLSession.shared.data(for: request)
        return try JSONDecoder().decode(SearchResponse.self, from: data)
    }

    func searchSuggestions(prefix: String, limit: Int = 5) async throws -> SearchSuggestionsResponse {
        var components = URLComponents(url: baseURL.appendingPathComponent("/api/v2/search/suggestions"), resolvingAgainstBaseURL: false)!
        components.queryItems = [
            URLQueryItem(name: "prefix", value: prefix),
            URLQueryItem(name: "limit", value: "\(limit)")
        ]

        let request = try makeRequest(url: components.url!, method: "GET")
        let (data, _) = try await URLSession.shared.data(for: request)
        return try JSONDecoder().decode(SearchSuggestionsResponse.self, from: data)
    }
}
