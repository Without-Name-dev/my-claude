"use client"

import * as React from "react"
import { Ban, RefreshCw, X } from "lucide-react"
import { useLocale, useTranslations } from "next-intl"
import { toast } from "sonner"

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
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { Spinner } from "@/components/ui/spinner"
import { modelProvidersApi } from "@/features/model-providers/api"
import type {
  ConnectorGatewayView,
  ConnectorModelRouteView,
  ModelProviderView,
} from "@/features/model-providers/types"

function gatewayStatusKey(gateway: ConnectorGatewayView): string {
  if (gateway.credentialStatus === "active") return "statusActive"
  if (gateway.credentialStatus === "revoked") return "statusRevoked"
  return "statusNone"
}

function gatewayStatusBadgeVariant(gateway: ConnectorGatewayView) {
  if (gateway.credentialStatus === "active") return "secondary" as const
  if (gateway.credentialStatus === "revoked") return "destructive" as const
  return "outline" as const
}

/** Model gateway section on the device page: the default provider for this
 * desktop and its gateway credential state. */
export function DeviceModelGatewaySection({
  connectorId,
  token,
}: {
  connectorId: string
  token: string
}) {
  const t = useTranslations("dashboard.device.gateway")
  const tCommon = useTranslations("common")
  const locale = useLocale()
  const [route, setRoute] = React.useState<ConnectorModelRouteView | null>(null)
  const [providers, setProviders] = React.useState<ModelProviderView[] | null>(null)
  const [loadFailed, setLoadFailed] = React.useState(false)
  const [applyToAll, setApplyToAll] = React.useState(false)
  const [savingRoute, setSavingRoute] = React.useState(false)
  const [gatewayBusy, setGatewayBusy] = React.useState(false)
  const [disableOpen, setDisableOpen] = React.useState(false)

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
      const [routeResponse, providersResponse] = await Promise.all([
        modelProvidersApi.getConnectorRoute(token, connectorId),
        modelProvidersApi.list(token),
      ])
      setRoute(routeResponse.route)
      setProviders(providersResponse.providers)
      setLoadFailed(false)
    } catch {
      setLoadFailed(true)
    }
  }, [token, connectorId])

  React.useEffect(() => {
    if (!token) return
    setRoute(null)
    setProviders(null)
    setLoadFailed(false)
    void load()
  }, [token, load])

  const applyDefault = async (providerId: string | null) => {
    if (!token || savingRoute) return
    setSavingRoute(true)
    try {
      const response = await modelProvidersApi.setConnectorRoute(token, connectorId, {
        providerId,
        applyToSessions: providerId && applyToAll ? "all" : "none",
      })
      setRoute(response.route)
      if (response.applied) {
        toast.success(t("appliedResult", {
          pinned: response.applied.pinned,
          failed: response.applied.failed,
        }))
      } else if (providerId) {
        toast.success(t("defaultSaved"))
      } else {
        toast.success(t("defaultCleared"))
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t("saveFailed"))
    } finally {
      setSavingRoute(false)
    }
  }

  const enableGateway = async () => {
    if (!token || gatewayBusy) return
    setGatewayBusy(true)
    try {
      const response = await modelProvidersApi.enableConnectorGateway(token, connectorId)
      setRoute((current) => (current ? { ...current, gateway: response.gateway } : current))
      if (response.pushed) {
        toast.success(t("enabled"))
      } else {
        // The credential was issued but the desktop was offline; it retries
        // the delivery on reconnect, so ask the user to try again later.
        toast.warning(t("pushPending"))
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t("enableFailed"))
    } finally {
      setGatewayBusy(false)
    }
  }

  const disableGateway = async () => {
    if (!token || gatewayBusy) return
    setGatewayBusy(true)
    try {
      const response = await modelProvidersApi.disableConnectorGateway(token, connectorId)
      setRoute((current) => (current ? { ...current, gateway: response.gateway } : current))
      setDisableOpen(false)
      toast.success(t("disabled"))
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t("disableFailed"))
    } finally {
      setGatewayBusy(false)
    }
  }

  const renderGatewayMeta = (gateway: ConnectorGatewayView) => {
    const parts: string[] = []
    if (gateway.credentialPrefix) parts.push(`${t("prefix")}: ${gateway.credentialPrefix}`)
    if (gateway.createdAt) parts.push(`${t("issuedAt")}: ${dateFormatter.format(new Date(gateway.createdAt))}`)
    if (gateway.lastUsedAt) parts.push(`${t("lastUsedAt")}: ${dateFormatter.format(new Date(gateway.lastUsedAt))}`)
    return parts.join(" · ") || t("statusNone")
  }

  return (
    <section className="mb-8">
      <div className="mb-3 flex items-center">
        <h2 className="text-xs font-semibold uppercase tracking-widest text-muted-foreground">
          {t("title")}
        </h2>
      </div>

      {loadFailed ? (
        <div className="flex items-center gap-3 text-sm text-muted-foreground">
          <span className="text-destructive">{t("loadFailed")}</span>
          <Button type="button" variant="outline" size="sm" onClick={() => void load()}>
            <RefreshCw />
            {tCommon("retry")}
          </Button>
        </div>
      ) : route === null || providers === null ? (
        <p className="py-3 text-sm text-muted-foreground">{tCommon("loading")}</p>
      ) : (
        <div className="flex flex-col gap-4 rounded-lg border border-border bg-card px-4 py-4">
          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <div className="min-w-0">
                <p className="text-sm font-medium">{t("defaultProvider")}</p>
                <p className="truncate text-xs text-muted-foreground">
                  {route.default
                    ? `${route.default.providerName} · ${route.default.modelDisplayName || route.default.modelId}`
                    : t("defaultUnset")}
                </p>
              </div>
              {route.default ? (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={savingRoute}
                  onClick={() => void applyDefault(null)}
                >
                  {savingRoute ? <Spinner data-icon="inline-start" /> : <X data-icon="inline-start" />}
                  {t("clearDefault")}
                </Button>
              ) : null}
            </div>
            <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
              <Select
                value={route.default?.providerId ?? ""}
                disabled={providers.length === 0 || savingRoute}
                onValueChange={(value) => { void applyDefault(value) }}
              >
                <SelectTrigger className="w-full sm:w-80" aria-label={t("chooseProvider")}>
                  <SelectValue placeholder={providers.length === 0 ? t("noProviders") : t("chooseProvider")} />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    {providers.map((provider) => (
                      <SelectItem key={provider.id} value={provider.id} disabled={!provider.enabled}>
                        {provider.name} · {provider.modelDisplayName || provider.modelId}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              <label
                htmlFor="model-gateway-apply-all"
                className="flex items-center gap-2 text-sm text-muted-foreground"
              >
                <Checkbox
                  id="model-gateway-apply-all"
                  checked={applyToAll}
                  onCheckedChange={(checked: boolean | "indeterminate") => setApplyToAll(checked === true)}
                  disabled={savingRoute}
                />
                {t("applyToAll")}
              </label>
            </div>
          </div>

          <Separator />

          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <p className="text-sm font-medium">{t("gatewayCredential")}</p>
                <Badge variant={gatewayStatusBadgeVariant(route.gateway)} className="font-normal">
                  {t(gatewayStatusKey(route.gateway))}
                </Badge>
                {route.gateway.connected ? (
                  <Badge variant="secondary" className="font-normal">{t("connected")}</Badge>
                ) : null}
              </div>
              <p className="mt-0.5 truncate text-xs text-muted-foreground">
                {renderGatewayMeta(route.gateway)}
              </p>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              {route.gateway.credentialStatus === "active" ? (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="text-muted-foreground hover:text-destructive"
                  disabled={gatewayBusy}
                  onClick={() => setDisableOpen(true)}
                >
                  {gatewayBusy ? <Spinner data-icon="inline-start" /> : <Ban data-icon="inline-start" />}
                  {t("disable")}
                </Button>
              ) : (
                <Button
                  type="button"
                  size="sm"
                  disabled={gatewayBusy}
                  onClick={() => void enableGateway()}
                >
                  {gatewayBusy ? <Spinner data-icon="inline-start" /> : null}
                  {t("enable")}
                </Button>
              )}
            </div>
          </div>
        </div>
      )}

      <AlertDialog open={disableOpen} onOpenChange={setDisableOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("disableTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t("disableDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={gatewayBusy}>{tCommon("cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
              disabled={gatewayBusy}
              onClick={disableGateway}
            >
              {gatewayBusy ? tCommon("loading") : t("disable")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  )
}
