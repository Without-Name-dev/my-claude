"use client"

import * as React from "react"
import { Activity, Boxes, Eye, EyeOff, KeyRound, Pencil, Plus, Trash2 } from "lucide-react"
import { useLocale, useTranslations } from "next-intl"
import { toast } from "sonner"

import { LoadingState } from "@/components/loading-state"
import { SettingsSection } from "@/components/settings/settings-section"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group"
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { modelProvidersApi, providerReferencesFromError } from "@/features/model-providers/api"
import {
  isModelProviderAuthType,
  isModelProviderProtocol,
  type ModelProviderAuthType,
  type ModelProviderCheckMode,
  type ModelProviderProtocol,
  type ModelProviderView,
  type ProviderReferences,
} from "@/features/model-providers/types"
import { isApiError } from "@/lib/api/errors"

type ProviderEditorState =
  | { mode: "create" }
  | { mode: "edit"; provider: ModelProviderView }
  | null

function protocolLabelKey(protocol: string): string {
  return protocol === "openai_chat_completions"
    ? "modelProviderProtocolOpenAI"
    : "modelProviderProtocolAnthropic"
}

// ── Create / edit dialog ───────────────────────────────────────

function ProviderEditorDialog({
  editor,
  token,
  onOpenChange,
  onSaved,
}: {
  editor: ProviderEditorState
  token: string
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}) {
  const t = useTranslations("pages.settings")
  const tCommon = useTranslations("common")
  const editing = editor?.mode === "edit" ? editor.provider : null
  const [name, setName] = React.useState("")
  const [protocol, setProtocol] = React.useState<ModelProviderProtocol>("anthropic_messages")
  const [baseUrl, setBaseUrl] = React.useState("")
  const [authType, setAuthType] = React.useState<ModelProviderAuthType>("api_key")
  const [modelId, setModelId] = React.useState("")
  const [modelDisplayName, setModelDisplayName] = React.useState("")
  const [credential, setCredential] = React.useState("")
  const [clearCredential, setClearCredential] = React.useState(false)
  const [showCredential, setShowCredential] = React.useState(false)
  const [saving, setSaving] = React.useState(false)
  const savingRef = React.useRef(false)

  React.useEffect(() => {
    if (editing) {
      setName(editing.name)
      setProtocol(isModelProviderProtocol(editing.protocol) ? editing.protocol : "anthropic_messages")
      setBaseUrl(editing.baseUrl)
      setAuthType(isModelProviderAuthType(editing.authType) ? editing.authType : "api_key")
      setModelId(editing.modelId)
      setModelDisplayName(editing.modelDisplayName)
    } else {
      setName("")
      setProtocol("anthropic_messages")
      setBaseUrl("")
      setAuthType("api_key")
      setModelId("")
      setModelDisplayName("")
    }
    // The credential never survives in form state past the dialog's lifetime.
    setCredential("")
    setClearCredential(false)
    setShowCredential(false)
    setSaving(false)
    savingRef.current = false
  }, [editor])

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (!editor || savingRef.current) return
    if (!name.trim() || !baseUrl.trim() || !modelId.trim()) return
    savingRef.current = true
    setSaving(true)
    try {
      if (editor.mode === "edit") {
        await modelProvidersApi.update(token, editor.provider.id, {
          name: name.trim(),
          protocol,
          baseUrl: baseUrl.trim(),
          authType,
          modelId: modelId.trim(),
          modelDisplayName: modelDisplayName.trim(),
          expectedRevision: editor.provider.revision,
          ...(clearCredential ? { clearCredential: true } : {}),
          ...(credential && !clearCredential ? { credential } : {}),
        })
        toast.success(t("modelProviderUpdateSuccess"))
      } else {
        await modelProvidersApi.create(token, {
          name: name.trim(),
          protocol,
          baseUrl: baseUrl.trim(),
          authType,
          modelId: modelId.trim(),
          ...(modelDisplayName.trim() ? { modelDisplayName: modelDisplayName.trim() } : {}),
          ...(credential ? { credential } : {}),
        })
        toast.success(t("modelProviderCreateSuccess"))
      }
      setCredential("")
      setClearCredential(false)
      onOpenChange(false)
      onSaved()
    } catch (error) {
      if (isApiError(error) && error.status === 409) {
        toast.error(t("modelProviderConflict"))
        onOpenChange(false)
        onSaved()
        return
      }
      toast.error(
        error instanceof Error
          ? error.message
          : t(editing ? "modelProviderUpdateFailed" : "modelProviderCreateFailed"),
      )
    } finally {
      savingRef.current = false
      setSaving(false)
    }
  }

  const credentialPlaceholder = editing?.credentialConfigured && !clearCredential
    ? t("modelProviderCredentialConfiguredPlaceholder")
    : t("modelProviderCredentialPlaceholder")

  return (
    <Dialog open={editor !== null} onOpenChange={(open) => { if (!savingRef.current) onOpenChange(open) }}>
      <DialogContent className="sm:max-w-lg">
        <form className="flex flex-col gap-6" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t(editing ? "modelProviderEditTitle" : "modelProviderCreateTitle")}</DialogTitle>
            <DialogDescription>
              {t(editing ? "modelProviderEditDescription" : "modelProviderCreateDescription")}
            </DialogDescription>
          </DialogHeader>

          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="model-provider-name">{t("modelProviderName")}</FieldLabel>
              <Input
                id="model-provider-name"
                value={name}
                maxLength={255}
                disabled={saving}
                onChange={(event) => setName(event.currentTarget.value)}
                required
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="model-provider-protocol">{t("modelProviderProtocolLabel")}</FieldLabel>
              <Select
                value={protocol}
                disabled={saving}
                onValueChange={(value) => setProtocol(value as ModelProviderProtocol)}
              >
                <SelectTrigger id="model-provider-protocol" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    <SelectItem value="anthropic_messages">{t("modelProviderProtocolAnthropic")}</SelectItem>
                    <SelectItem value="openai_chat_completions">{t("modelProviderProtocolOpenAI")}</SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
            </Field>
            <Field>
              <FieldLabel htmlFor="model-provider-base-url">{t("modelProviderBaseUrl")}</FieldLabel>
              <Input
                id="model-provider-base-url"
                value={baseUrl}
                maxLength={2048}
                disabled={saving}
                inputMode="url"
                spellCheck={false}
                autoComplete="off"
                onChange={(event) => setBaseUrl(event.currentTarget.value)}
                placeholder="https://api.example.com"
                required
              />
              <FieldDescription>{t("modelProviderBaseUrlDescription")}</FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="model-provider-auth-type">{t("modelProviderAuthType")}</FieldLabel>
              <Select
                value={authType}
                disabled={saving}
                onValueChange={(value) => setAuthType(value as ModelProviderAuthType)}
              >
                <SelectTrigger id="model-provider-auth-type" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    <SelectItem value="api_key">{t("modelProviderAuthApiKey")}</SelectItem>
                    <SelectItem value="bearer">{t("modelProviderAuthBearer")}</SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
            </Field>
            <Field>
              <FieldLabel htmlFor="model-provider-model-id">{t("modelProviderModelId")}</FieldLabel>
              <Input
                id="model-provider-model-id"
                value={modelId}
                maxLength={255}
                disabled={saving}
                spellCheck={false}
                onChange={(event) => setModelId(event.currentTarget.value)}
                className="code-mono text-xs"
                required
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="model-provider-model-display-name">{t("modelProviderModelDisplayName")}</FieldLabel>
              <Input
                id="model-provider-model-display-name"
                value={modelDisplayName}
                maxLength={255}
                disabled={saving}
                onChange={(event) => setModelDisplayName(event.currentTarget.value)}
              />
            </Field>
            <Field data-disabled={clearCredential || undefined}>
              <FieldLabel htmlFor="model-provider-credential">{t("modelProviderCredential")}</FieldLabel>
              <InputGroup>
                <InputGroupInput
                  id="model-provider-credential"
                  type={showCredential ? "text" : "password"}
                  value={credential}
                  disabled={saving || clearCredential}
                  autoComplete="new-password"
                  spellCheck={false}
                  onChange={(event) => setCredential(event.currentTarget.value)}
                  placeholder={credentialPlaceholder}
                  data-1p-ignore
                  data-bwignore="true"
                  data-form-type="other"
                  data-lpignore="true"
                />
                <InputGroupAddon align="inline-end">
                  <InputGroupButton
                    onClick={() => setShowCredential((current) => !current)}
                    aria-label={showCredential ? t("modelProviderHideCredential") : t("modelProviderShowCredential")}
                  >
                    {showCredential ? <EyeOff /> : <Eye />}
                  </InputGroupButton>
                </InputGroupAddon>
              </InputGroup>
              <FieldDescription>{t("modelProviderCredentialDescription")}</FieldDescription>
            </Field>
            {editing?.credentialConfigured ? (
              <Field orientation="horizontal">
                <Checkbox
                  id="model-provider-clear-credential"
                  checked={clearCredential}
                  onCheckedChange={(checked: boolean | "indeterminate") => setClearCredential(checked === true)}
                  disabled={saving}
                />
                <FieldLabel htmlFor="model-provider-clear-credential" className="font-normal">
                  {t("modelProviderClearCredential")}
                </FieldLabel>
              </Field>
            ) : null}
          </FieldGroup>

          <DialogFooter>
            <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
              {tCommon("cancel")}
            </Button>
            <Button
              type="submit"
              disabled={saving || !name.trim() || !baseUrl.trim() || !modelId.trim()}
            >
              {saving ? <Spinner data-icon="inline-start" /> : null}
              {editing ? tCommon("save") : tCommon("create")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// ── Tab ────────────────────────────────────────────────────────

export function ModelProvidersTab({ token }: { token: string }) {
  const t = useTranslations("pages.settings")
  const tCommon = useTranslations("common")
  const locale = useLocale()
  const [providers, setProviders] = React.useState<ModelProviderView[] | null>(null)
  const [loadError, setLoadError] = React.useState<string | null>(null)
  const [editor, setEditor] = React.useState<ProviderEditorState>(null)
  const [removeTarget, setRemoveTarget] = React.useState<ModelProviderView | null>(null)
  const [removeRefs, setRemoveRefs] = React.useState<ProviderReferences | null>(null)
  const [deleting, setDeleting] = React.useState(false)
  const [busyActionId, setBusyActionId] = React.useState<string | null>(null)

  const dateFormatter = React.useMemo(
    () => new Intl.DateTimeFormat(locale, {
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    }),
    [locale],
  )

  const load = React.useCallback(async () => {
    if (!token) return
    try {
      const response = await modelProvidersApi.list(token)
      setProviders(response.providers)
      setLoadError(null)
    } catch (error) {
      setLoadError(error instanceof Error ? error.message : t("modelProviderLoadFailed"))
    }
  }, [token, t])

  React.useEffect(() => {
    if (!token) return
    setProviders(null)
    setLoadError(null)
    void load()
  }, [token, load])

  const toggleEnabled = async (provider: ModelProviderView, enabled: boolean) => {
    if (!token || busyActionId) return
    setBusyActionId(provider.id)
    const previous = providers
    setProviders((current) =>
      current?.map((item) => (item.id === provider.id ? { ...item, enabled } : item)) ?? current)
    try {
      const response = await modelProvidersApi.update(token, provider.id, {
        enabled,
        expectedRevision: provider.revision,
      })
      setProviders((current) =>
        current?.map((item) => (item.id === provider.id ? response.provider : item)) ?? current)
    } catch (error) {
      setProviders(previous)
      if (isApiError(error) && error.status === 409) {
        toast.error(t("modelProviderConflict"))
        void load()
      } else {
        toast.error(error instanceof Error ? error.message : t("modelProviderUpdateFailed"))
      }
    } finally {
      setBusyActionId(null)
    }
  }

  const runCheck = async (provider: ModelProviderView, mode: ModelProviderCheckMode) => {
    if (!token || busyActionId) return
    setBusyActionId(provider.id)
    try {
      const response = await modelProvidersApi.check(token, provider.id, mode)
      const check = response.check
      const description = [check.status, check.detail].filter(Boolean).join(" · ") || undefined
      // A connect probe never proves authentication; only the real request
      // mode can. Requiring authOk for both made every connect test "fail".
      const passed = check.mode === "request" ? check.reachable && check.authOk : check.reachable
      if (passed) {
        toast.success(t("modelProviderCheckOk", { name: provider.name }), { description })
      } else {
        toast.error(t("modelProviderCheckFailed", { name: provider.name }), { description })
      }
    } catch (error) {
      toast.error(error instanceof Error
        ? error.message
        : t("modelProviderCheckFailed", { name: provider.name }))
    } finally {
      setBusyActionId(null)
    }
  }

  const confirmDelete = async () => {
    if (!token || !removeTarget || deleting) return
    setDeleting(true)
    try {
      await modelProvidersApi.delete(token, removeTarget.id, { force: removeRefs !== null })
      toast.success(t("modelProviderDeleteSuccess"))
      setRemoveTarget(null)
      setRemoveRefs(null)
      await load()
    } catch (error) {
      // Without force the server answers 409 with the reference counts; show
      // them and require an explicit second confirm before force-deleting.
      const refs = providerReferencesFromError(error)
      if (refs) {
        setRemoveRefs(refs)
        return
      }
      toast.error(error instanceof Error ? error.message : t("modelProviderDeleteFailed"))
    } finally {
      setDeleting(false)
    }
  }

  if (!token) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        {t("unavailable")}
      </div>
    )
  }

  if (providers === null && loadError === null) {
    return <LoadingState className="min-h-64" />
  }

  if (providers === null) {
    return (
      <div className="flex flex-col items-center justify-center gap-3 py-16 text-center">
        <p className="text-sm text-destructive">{loadError}</p>
        <Button type="button" variant="outline" size="sm" onClick={() => void load()}>
          {tCommon("retry")}
        </Button>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <SettingsSection
        title={t("modelProviders")}
        description={t("modelProvidersDescription")}
        action={
          <Button type="button" size="sm" onClick={() => setEditor({ mode: "create" })}>
            <Plus data-icon="inline-start" />
            {t("modelProviderAdd")}
          </Button>
        }
        contentClassName="p-0"
      >
        {providers.length === 0 ? (
          <Empty className="m-4 min-h-48 border">
            <EmptyHeader>
              <EmptyMedia variant="icon"><Boxes /></EmptyMedia>
              <EmptyTitle>{t("modelProviderEmptyTitle")}</EmptyTitle>
              <EmptyDescription>{t("modelProviderEmptyDescription")}</EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          providers.map((provider, index) => {
            const busy = busyActionId === provider.id
            return (
              <React.Fragment key={provider.id}>
                {index > 0 ? <Separator /> : null}
                <div className="flex items-center gap-3 px-4 py-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                      <p className="truncate text-sm font-medium">{provider.name}</p>
                      <Badge variant="outline" className="shrink-0 font-normal">
                        {t(protocolLabelKey(provider.protocol))}
                      </Badge>
                      {provider.credentialConfigured ? (
                        <Badge variant="secondary" className="shrink-0 gap-1 font-normal">
                          <KeyRound className="size-3" />
                          {t("modelProviderCredentialConfigured")}
                        </Badge>
                      ) : (
                        <Badge variant="outline" className="shrink-0 font-normal text-muted-foreground">
                          {t("modelProviderCredentialMissing")}
                        </Badge>
                      )}
                    </div>
                    <p className="mt-0.5 truncate text-xs text-muted-foreground">
                      {provider.modelDisplayName
                        ? `${provider.modelDisplayName} · ${provider.modelId}`
                        : provider.modelId}
                      {provider.updatedAt ? ` · ${dateFormatter.format(new Date(provider.updatedAt))}` : ""}
                    </p>
                  </div>
                  <div className="flex shrink-0 items-center gap-2">
                    <Switch
                      checked={provider.enabled}
                      disabled={busy}
                      onCheckedChange={(enabled: boolean) => void toggleEnabled(provider, enabled)}
                      aria-label={provider.enabled
                        ? t("modelProviderDisable", { name: provider.name })
                        : t("modelProviderEnable", { name: provider.name })}
                    />
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button type="button" variant="outline" size="sm" disabled={busy}>
                          {busy ? <Spinner data-icon="inline-start" /> : <Activity data-icon="inline-start" />}
                          {t("modelProviderCheck")}
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onSelect={() => void runCheck(provider, "connect")}>
                          {t("modelProviderCheckConnect")}
                        </DropdownMenuItem>
                        <DropdownMenuItem onSelect={() => void runCheck(provider, "request")}>
                          {t("modelProviderCheckRequest")}
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      onClick={() => setEditor({ mode: "edit", provider })}
                      aria-label={t("modelProviderEditTitle", { name: provider.name })}
                    >
                      <Pencil />
                    </Button>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      className="text-muted-foreground hover:text-destructive"
                      onClick={() => {
                        setRemoveRefs(null)
                        setRemoveTarget(provider)
                      }}
                      aria-label={t("modelProviderDeleteTitle", { name: provider.name })}
                    >
                      <Trash2 />
                    </Button>
                  </div>
                </div>
              </React.Fragment>
            )
          })
        )}
      </SettingsSection>

      <ProviderEditorDialog
        editor={editor}
        token={token}
        onOpenChange={(open) => { if (!open) setEditor(null) }}
        onSaved={() => void load()}
      />

      <AlertDialog
        open={removeTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setRemoveTarget(null)
            setRemoveRefs(null)
          }
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {removeRefs ? t("modelProviderDeleteReferencedTitle") : t("modelProviderDeleteTitle")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {removeRefs
                ? t("modelProviderDeleteReferencedDescription", {
                  name: removeTarget?.name ?? "",
                  connectorDefaults: removeRefs.connectorDefaults,
                  explicitRoutes: removeRefs.explicitRoutes,
                })
                : t("modelProviderDeleteDescription", { name: removeTarget?.name ?? "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>{tCommon("cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
              disabled={deleting}
              onClick={(event) => {
                event.preventDefault()
                void confirmDelete()
              }}
            >
              {deleting ? <Spinner data-icon="inline-start" /> : null}
              {removeRefs ? t("modelProviderDeleteForce") : tCommon("delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
