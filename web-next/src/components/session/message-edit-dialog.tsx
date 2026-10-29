"use client"

import * as React from "react"
import { useTranslations } from "next-intl"
import { toast } from "sonner"

import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { editTimelineItem } from "@/features/dashboard/api"

interface MessageEditDialogProps {
  token: string
  sessionId: string
  itemId: string
  initialContent: string
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function MessageEditDialog({
  token,
  sessionId,
  itemId,
  initialContent,
  open,
  onOpenChange,
}: MessageEditDialogProps) {
  const t = useTranslations("dashboard.session")
  const tCommon = useTranslations("common")
  const [content, setContent] = React.useState(initialContent)
  const [isSubmitting, setIsSubmitting] = React.useState(false)

  React.useEffect(() => {
    if (open) {
      setContent(initialContent)
    }
  }, [open, initialContent])

  const handleSubmit = async () => {
    if (!content.trim()) {
      toast.error(t("editMessageEmpty"))
      return
    }

    setIsSubmitting(true)
    try {
      await editTimelineItem(token, sessionId, itemId, content.trim())
      toast.success(t("editMessageSuccess"))
      onOpenChange(false)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t("editMessageFailed"))
    } finally {
      setIsSubmitting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-[600px]">
        <DialogHeader>
          <DialogTitle>{t("editMessage")}</DialogTitle>
          <DialogDescription>{t("editMessageDescription")}</DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <Textarea
            value={content}
            onChange={(e) => setContent(e.target.value)}
            placeholder={t("editMessagePlaceholder")}
            className="min-h-[200px]"
            disabled={isSubmitting}
          />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={isSubmitting}>
            {tCommon("cancel")}
          </Button>
          <Button onClick={handleSubmit} disabled={isSubmitting || !content.trim()}>
            {isSubmitting ? t("editMessageSaving") : t("editMessageSave")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
