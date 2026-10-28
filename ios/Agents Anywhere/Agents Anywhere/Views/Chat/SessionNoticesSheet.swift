import SwiftUI

struct SessionNoticesSheet: View {
    let model: SessionChatModel
    var initialNoticeID: String? = nil
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        NavigationStack {
            ScrollViewReader { proxy in
                ScrollView {
                    let visible = model.session.notices.notices.filter(\.isVisible)
                    if visible.isEmpty {
                        ContentUnavailableView(String(localized: "没有待处理交互"), systemImage: "checkmark.circle",
                            description: Text(String(localized: "审批和问题会显示在这里。")))
                            .padding(.top, 60)
                    } else {
                        VStack(spacing: 16) {
                            ForEach(visible) { item in
                                SessionInteractionContent(item: item, chat: model).id(item.id)
                            }
                        }.padding(16)
                    }
                }
                .onAppear { if let initialNoticeID { proxy.scrollTo(initialNoticeID, anchor: .top) } }
            }
            .navigationTitle(String(localized: "交互与通知"))
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { SheetCloseToolbar { dismiss() } }
        }
        .appSheetPresentation(.compact)
    }
}
