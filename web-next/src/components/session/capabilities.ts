import type { ProtocolCapability, ProtocolCapabilitySet } from "@/features/dashboard/types"

/**
 * The Go server rejects attachments on message send and create-and-start until
 * it can persist uploads and forward AttachmentRefs to the connector. The
 * new-session composer hides upload controls while this is false; existing
 * sessions are already gated by the session-scoped runtime.attachment
 * capability, which the server does not advertise yet.
 */
export const SERVER_ATTACHMENT_SUPPORTED = false

export const CAPABILITY = {
  sendMessage: "session.send_message",
  commands: "session.commands",
  interrupt: "session.interrupt",
  steer: "session.steer",
  approveInteraction: "session.interaction.approval",
  attachment: "runtime.attachment",
  runtimeConfig: "runtime.config",
  modelCatalog: "catalog.model",
  permissionCatalog: "catalog.permission",
  effortCatalog: "catalog.effort",
} as const

export type KnownCapabilityId = (typeof CAPABILITY)[keyof typeof CAPABILITY]

export type RuntimeCapabilityScope = string | {
  runtimeId?: string | null
  runtimeType?: string | null
}

export function findCapability(
  capabilitySet: ProtocolCapabilitySet | null | undefined,
  capabilityId: string,
  runtime?: RuntimeCapabilityScope,
): ProtocolCapability | null {
  const matches = capabilitySet?.capabilities.filter(
    (capability) => capability.capabilityId === capabilityId,
  ) ?? []
  if (!runtime) return matches[0] ?? null
  const runtimeId = typeof runtime === "string" ? runtime : runtime.runtimeId ?? null
  const runtimeType = typeof runtime === "string" ? runtime : runtime.runtimeType ?? null
  return matches.find(
    (capability) => runtimeId && capability.runtimeId === runtimeId,
  )
    ?? matches.find((capability) => runtimeType && capability.runtime === runtimeType)
    ?? matches.find((capability) => !capability.runtime && !capability.runtimeId)
    ?? null
}

export function capabilityIsUsable(
  capabilitySet: ProtocolCapabilitySet | null | undefined,
  capabilityId: KnownCapabilityId,
  runtime?: RuntimeCapabilityScope,
): boolean {
  const capability = findCapability(capabilitySet, capabilityId, runtime)
  return Boolean(capability?.supported && capability.available && capability.allowed)
}

/** Omitted MIME restrictions preserve existing runtimes; an empty list allows no files. */
export function attachmentMimeTypes(
  capabilitySet: ProtocolCapabilitySet | null | undefined,
  runtime?: RuntimeCapabilityScope,
): string[] | undefined {
  if (!capabilityIsUsable(capabilitySet, CAPABILITY.attachment, runtime)) return []
  const metadata = findCapability(capabilitySet, CAPABILITY.attachment, runtime)?.metadata
  const value = metadata && typeof metadata === "object" && "allowedMimeTypes" in metadata ? metadata.allowedMimeTypes : undefined
  if (value === undefined) return undefined
  if (!Array.isArray(value)) return []
  return [...new Set(value.filter((type): type is string => typeof type === "string")
    .map((type) => type.trim().toLowerCase())
    .filter((type) => /^[a-z0-9!#$&^_.+-]+\/[a-z0-9!#$&^_.+-]+$/.test(type)))]
}

export function attachmentMimeAllowed(mediaType: string, allowedMimeTypes: readonly string[] | undefined): boolean {
  return allowedMimeTypes === undefined || allowedMimeTypes.includes(mediaType.split(";", 1)[0]!.trim().toLowerCase())
}
