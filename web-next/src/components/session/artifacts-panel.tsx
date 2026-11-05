'use client'

import { useState, useMemo } from 'react'
import { X, Code, FileText, Image as ImageIcon, Download, Copy, Edit, Eye, ChevronRight } from 'lucide-react'

export interface Artifact {
  id: string
  type: 'code' | 'html' | 'svg' | 'markdown' | 'json' | 'mermaid' | 'react'
  title?: string
  language?: string
  content: string
  createdAt: string
}

interface ArtifactsPanelProps {
  sessionId: string
  artifacts: Artifact[]
  onClose: () => void
}

const ARTIFACT_TYPE_CONFIG = {
  code: { label: 'Code', icon: Code },
  html: { label: 'HTML', icon: FileText },
  svg: { label: 'SVG', icon: ImageIcon },
  markdown: { label: 'Markdown', icon: FileText },
  json: { label: 'JSON', icon: Code },
  mermaid: { label: 'Mermaid', icon: ImageIcon },
  react: { label: 'React', icon: Code }
}

export function ArtifactsPanel({ artifacts, onClose }: ArtifactsPanelProps) {
  const [selectedArtifact, setSelectedArtifact] = useState<Artifact | null>(null)

  if (selectedArtifact) {
    return (
      <ArtifactDetailView
        artifact={selectedArtifact}
        onBack={() => setSelectedArtifact(null)}
        onClose={onClose}
      />
    )
  }

  return (
    <div className="flex flex-col h-full bg-white dark:bg-gray-900">
      <div className="flex items-center justify-between px-4 py-3 border-b dark:border-gray-700">
        <h2 className="text-lg font-semibold">Artifacts</h2>
        <button
          onClick={onClose}
          className="p-1 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800"
        >
          <X className="w-5 h-5" />
        </button>
      </div>

      {artifacts.length === 0 ? (
        <div className="flex-1 flex items-center justify-center p-8">
          <div className="text-center max-w-sm">
            <div className="mx-auto w-16 h-16 mb-4 rounded-full bg-gray-100 dark:bg-gray-800 flex items-center justify-center">
              <FileText className="w-8 h-8 text-gray-400" />
            </div>
            <h3 className="text-lg font-semibold mb-2">No Artifacts</h3>
            <p className="text-sm text-gray-600 dark:text-gray-400">
              AI-generated code, charts, and documents will appear here
            </p>
          </div>
        </div>
      ) : (
        <div className="flex-1 overflow-y-auto">
          <div className="divide-y dark:divide-gray-700">
            {artifacts.map((artifact) => (
              <ArtifactRow
                key={artifact.id}
                artifact={artifact}
                onClick={() => setSelectedArtifact(artifact)}
              />
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

interface ArtifactRowProps {
  artifact: Artifact
  onClick: () => void
}

function ArtifactRow({ artifact, onClick }: ArtifactRowProps) {
  const config = ARTIFACT_TYPE_CONFIG[artifact.type]
  const Icon = config.icon

  return (
    <button
      onClick={onClick}
      className="w-full px-4 py-3 flex items-center gap-3 hover:bg-gray-50 dark:hover:bg-gray-800 transition-colors text-left"
    >
      <div className="flex-shrink-0 w-10 h-10 rounded-lg bg-blue-50 dark:bg-blue-900/20 flex items-center justify-center">
        <Icon className="w-5 h-5 text-blue-600 dark:text-blue-400" />
      </div>

      <div className="flex-1 min-w-0">
        <div className="font-medium truncate">
          {artifact.title || `Untitled ${config.label}`}
        </div>
        <div className="text-sm text-gray-600 dark:text-gray-400 flex items-center gap-2">
          <span>{config.label}</span>
          {artifact.language && (
            <>
              <span>·</span>
              <span>{artifact.language}</span>
            </>
          )}
        </div>
      </div>

      <ChevronRight className="w-5 h-5 text-gray-400 flex-shrink-0" />
    </button>
  )
}

interface ArtifactDetailViewProps {
  artifact: Artifact
  onBack: () => void
  onClose: () => void
}

function ArtifactDetailView({ artifact, onBack, onClose }: ArtifactDetailViewProps) {
  const [isEditing, setIsEditing] = useState(false)
  const [editedContent, setEditedContent] = useState(artifact.content)
  const [copied, setCopied] = useState(false)

  const handleCopy = async () => {
    await navigator.clipboard.writeText(editedContent)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const handleDownload = () => {
    const extension = artifact.type === 'code' ? artifact.language || 'txt' : artifact.type
    const filename = `${artifact.title || 'artifact'}.${extension}`
    const blob = new Blob([editedContent], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    a.click()
    URL.revokeObjectURL(url)
  }

  const canPreview = artifact.type === 'html' || artifact.type === 'svg'

  return (
    <div className="flex flex-col h-full bg-white dark:bg-gray-900">
      <div className="flex items-center justify-between px-4 py-3 border-b dark:border-gray-700">
        <div className="flex items-center gap-2">
          <button
            onClick={onBack}
            className="p-1 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800"
          >
            <ChevronRight className="w-5 h-5 rotate-180" />
          </button>
          <h2 className="text-lg font-semibold truncate">
            {artifact.title || 'Artifact'}
          </h2>
        </div>

        <div className="flex items-center gap-2">
          {canPreview && (
            <button
              onClick={() => setIsEditing(!isEditing)}
              className="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800"
              title={isEditing ? 'Preview' : 'Edit'}
            >
              {isEditing ? <Eye className="w-5 h-5" /> : <Edit className="w-5 h-5" />}
            </button>
          )}

          <button
            onClick={handleCopy}
            className="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800"
            title="Copy"
          >
            <Copy className="w-5 h-5" />
          </button>

          <button
            onClick={handleDownload}
            className="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800"
            title="Download"
          >
            <Download className="w-5 h-5" />
          </button>

          <button
            onClick={onClose}
            className="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800"
          >
            <X className="w-5 h-5" />
          </button>
        </div>
      </div>

      {copied && (
        <div className="px-4 py-2 bg-green-50 dark:bg-green-900/20 text-green-800 dark:text-green-200 text-sm">
          Copied to clipboard
        </div>
      )}

      <div className="flex-1 overflow-hidden">
        {canPreview && !isEditing ? (
          <ArtifactPreview artifact={artifact} content={editedContent} />
        ) : (
          <ArtifactEditor
            content={editedContent}
            onChange={setEditedContent}
            readOnly={!isEditing && canPreview}
          />
        )}
      </div>
    </div>
  )
}

interface ArtifactPreviewProps {
  artifact: Artifact
  content: string
}

function ArtifactPreview({ artifact, content }: ArtifactPreviewProps) {
  const previewContent = useMemo(() => {
    if (artifact.type === 'svg') {
      return `<!DOCTYPE html>
<html>
<head>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>body { margin: 0; padding: 20px; display: flex; justify-content: center; align-items: center; min-height: 100vh; }</style>
</head>
<body>${content}</body>
</html>`
    }
    return content
  }, [artifact.type, content])

  return (
    <div className="w-full h-full">
      <iframe
        srcDoc={previewContent}
        className="w-full h-full border-0"
        sandbox="allow-scripts"
        title="Artifact Preview"
      />
    </div>
  )
}

interface ArtifactEditorProps {
  content: string
  onChange: (content: string) => void
  readOnly: boolean
}

function ArtifactEditor({ content, onChange, readOnly }: ArtifactEditorProps) {
  return (
    <div className="h-full overflow-auto">
      <textarea
        value={content}
        onChange={(e) => onChange(e.target.value)}
        readOnly={readOnly}
        className="w-full h-full p-4 font-mono text-sm bg-white dark:bg-gray-900 border-0 focus:outline-none resize-none"
        spellCheck={false}
      />
    </div>
  )
}

/**
 * Extract artifacts from timeline items
 */
export function extractArtifactsFromTimeline(items: any[]): Artifact[] {
  const artifacts: Artifact[] = []

  for (const item of items) {
    if (item.type === 'artifact' && item.content) {
      const content = item.content
      const artifact: Artifact = {
        id: item.id,
        type: detectArtifactType(content.mediaType),
        title: content.title || content.name,
        language: content.language,
        content: content.content || content.text || '',
        createdAt: item.createdAt
      }
      artifacts.push(artifact)
    }

    // Extract from message content (markdown code blocks with artifact keyword)
    if (item.type === 'message' && item.content?.text) {
      const messageArtifacts = extractArtifactsFromMarkdown(item.content.text, item.id)
      artifacts.push(...messageArtifacts)
    }
  }

  return artifacts
}

function detectArtifactType(mediaType?: string): Artifact['type'] {
  if (!mediaType) return 'code'
  if (mediaType.includes('html')) return 'html'
  if (mediaType.includes('svg')) return 'svg'
  if (mediaType.includes('json')) return 'json'
  if (mediaType.includes('markdown')) return 'markdown'
  return 'code'
}

function extractArtifactsFromMarkdown(text: string, messageId: string): Artifact[] {
  const artifacts: Artifact[] = []
  const pattern = /```(\w+)\s+artifact\s*\n(?:title:\s*(.+?)\n)?([\s\S]*?)```/g

  let match
  let index = 0
  while ((match = pattern.exec(text)) !== null) {
    const [, language, title, content] = match

    artifacts.push({
      id: `${messageId}-artifact-${index}`,
      type: (language?.toLowerCase() as Artifact['type']) || 'code',
      title: title?.trim(),
      language: language || 'text',
      content: content?.trim() || '',
      createdAt: new Date().toISOString()
    })
    index++
  }

  return artifacts
}
