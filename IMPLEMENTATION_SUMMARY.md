# Message Editing and Retry Implementation

## Overview
Implemented complete message editing and retry functionality across Go Server, iOS, and Web clients.

## Go Server

### Database Migration
- **File**: `/c/Users/25640/my_claude/go/internal/store/migrations/0009_message_edit_history.sql`
- Added `message_edit_history` table to track edit versions
- Added `edited_at` and `edit_count` columns to `timeline_items`

### Store Layer
- **File**: `/c/Users/25640/my_claude/go/internal/store/message_edits.go`
- `SaveMessageEdit()`: Records edit history
- `GetMessageEditHistory()`: Retrieves edit versions
- `DeleteTimelineItemsAfter()`: Truncates timeline after edited message

### API Handlers
- **File**: `/c/Users/25640/my_claude/go/internal/server/timeline_handlers.go`
- `PATCH /api/v2/sessions/{session_id}/timeline/{item_id}`: Edit message content
- `POST /api/v2/sessions/{session_id}/timeline/{item_id}/retry`: Retry failed/edited message
- `publishTimelineReset()`: Broadcasts timeline snapshot to connected clients

### Routes
- **File**: `/c/Users/25640/my_claude/go/internal/server/app.go` (lines 160-162)
- Registered edit and retry endpoints

## iOS Client

### API Layer
- **File**: `/c/Users/25640/my_claude/ios/Agents Anywhere/Agents Anywhere/API/V2/V2TimelineAPI.swift`
- `V2TimelineAPI` protocol and implementation
- `editTimelineItem()`: Edit message via PATCH endpoint
- `retryMessage()`: Retry message via POST endpoint
- Request/response types: `V2TimelineEditRequest`, `V2TimelineEditResponse`, `V2TimelineRetryResponse`

## Web Client

### API Client
- **File**: `/c/Users/25640/my_claude/web-next/src/features/dashboard/api.ts` (appended)
- `editTimelineItem()`: Async function to edit messages
- `retryMessage()`: Async function to retry messages

### UI Components
- **File**: `/c/Users/25640/my_claude/web-next/src/components/session/message-edit-dialog.tsx`
- Modal dialog for editing message content
- Validation and error handling
- Integration with timeline API

### Timeline Entry
- **File**: `/c/Users/25640/my_claude/web-next/src/components/session/session-timeline-entry.tsx`
- Added Edit and Retry options to context menu for user messages
- Integrated `MessageEditDialog` component
- Enhanced `TimelineEntryContextMenu` with edit/retry callbacks

## Features

### Message Editing
1. User right-clicks/long-presses on their message
2. Selects "Edit" from context menu
3. Dialog opens with current message text
4. User modifies and saves
5. Server:
   - Saves original content to edit history
   - Updates message content
   - Deletes all timeline items after the edited message
   - Broadcasts timeline reset event
   - Automatically regenerates response

### Message Retry
1. User right-clicks/long-presses on their message (especially failed messages)
2. Selects "Retry" from context menu
3. Server resends the message with a new message ID
4. Response is regenerated

## API Endpoints

### Edit Message
```
PATCH /api/v2/sessions/{session_id}/timeline/{item_id}
Body: { "content": "edited text" }
Response: { "ok": true }
```

### Retry Message
```
POST /api/v2/sessions/{session_id}/timeline/{item_id}/retry
Response: { "ok": true, "result": { "messageId": "msg_..." } }
```

## Verification Status

### Completed
- ✅ Go server builds successfully
- ✅ Database migration created
- ✅ Store layer methods implemented
- ✅ API handlers with edit history tracking
- ✅ Timeline truncation on edit
- ✅ Automatic regeneration after edit
- ✅ iOS API client created
- ✅ Web API client added
- ✅ Web UI dialog component created
- ✅ Web timeline integration with context menu

### Not Verified (requires running environment)
- ⚠️ Web typecheck (yarn not available)
- ⚠️ Database migration application
- ⚠️ End-to-end testing with running server
- ⚠️ iOS UI integration (requires Xcode project update)
- ⚠️ Real-time WebSocket event delivery
- ⚠️ OAuth callbacks and remote sessions

## Next Steps

1. **iOS UI Integration**: Add context menu/long-press handlers to message views
2. **Localization**: Add translation keys for edit/retry UI text
3. **Testing**: Run local environment and verify edit/regenerate flow
4. **Migration**: Apply database migration to production
5. **Documentation**: Update API documentation with new endpoints
