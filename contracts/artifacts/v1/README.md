# Artifacts Contract v1

## Overview

Artifacts are AI-generated content blocks that can be displayed, edited, and exported independently from the chat timeline.

## Supported Types

- `code`: Source code in any language
- `html`: HTML documents/snippets
- `svg`: SVG graphics
- `markdown`: Markdown documents
- `json`: JSON data
- `mermaid`: Mermaid diagrams
- `react`: React components

## Detection

Artifacts are identified in assistant messages through:

1. **Fenced code blocks with metadata**:
   ````markdown
   ```language artifact
   title: Example Artifact
   content here
   ```
   ````

2. **Timeline items with type="artifact"** (from V2TimelineItem):
   ```json
   {
     "type": "artifact",
     "content": {
       "title": "Chart",
       "mediaType": "text/html",
       "url": "..."
     }
   }
   ```

## Client Implementation

### Extraction
Parse message content for:
- Code fences with `artifact` keyword
- Timeline items with `type="artifact"`

### Display
- Show in dedicated panel/view
- Support preview mode
- Enable editing for code/text types
- Provide export functionality

### Storage
- Store artifact references with session
- Track versions/updates
- Support re-opening from history
