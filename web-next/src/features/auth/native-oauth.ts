export type NativeOAuthKind = 'mobile' | 'desktop'
export interface NativeOAuthParams {
  response_type: string
  client_id: string
  redirect_uri: string
  code_challenge: string
  code_challenge_method: string
  scope: string
  state?: string
}

const clients = {
  mobile: { id: 'agents-anywhere-mobile', redirect: 'agents-anywhere://oauth/callback' },
  desktop: { id: 'agents-anywhere-desktop', redirect: 'agents-anywhere-desktop://oauth/callback' },
}

export function readNativeOAuthParams(params: { get(name: string): string | null }, kind: NativeOAuthKind): NativeOAuthParams | null {
  const client = clients[kind]
  const redirect = params.get('redirect_uri') ?? ''
  const challenge = params.get('code_challenge') ?? ''
  const state = params.get('state') ?? ''
  if (params.get('response_type') !== 'code' || params.get('client_id') !== client.id) return null
  if (redirect !== client.redirect) return null
  const method = params.get('code_challenge_method') || 'S256'
  if (!challenge || method !== 'S256') return null
  return {
    response_type: 'code', client_id: client.id, redirect_uri: redirect,
    code_challenge: challenge, code_challenge_method: method, scope: params.get('scope') || '',
    ...(state ? { state } : {}),
  }
}
