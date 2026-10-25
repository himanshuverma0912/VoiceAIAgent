// Sign-in: OpenID Connect Authorization Code + PKCE with Keycloak's
// voice-platform realm, which signs people in at the company directory over
// SAML (kc_idp_hint skips Keycloak's own form).
//
// Tokens live in memory only. After a reload the app asks Keycloak again with
// prompt=none: if the Keycloak session is still open (it is, for 30 minutes of
// inactivity) the person is signed straight back in without typing anything.

const ORIGIN = location.origin;
const OIDC = `${ORIGIN}/auth/realms/voice-platform/protocol/openid-connect`;
const CLIENT_ID = "flow-studio";
const IDP_HINT = "corp-idp";
const PENDING = "flow-studio.signin";

interface Tokens {
  access: string;
  refresh?: string;
  id?: string;
  expiresAt: number; // ms
}

let tokens: Tokens | null = null;
let refreshing: Promise<void> | null = null;

const b64url = (bytes: ArrayBuffer | Uint8Array) =>
  btoa(String.fromCharCode(...new Uint8Array(bytes)))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
const random = (n: number) => b64url(crypto.getRandomValues(new Uint8Array(n)));

/** Go to Keycloak. silent: don't show a sign-in page; come back with an error if not signed in. */
export async function startSignIn(silent = false): Promise<never> {
  const verifier = random(48);
  const state = random(16);
  sessionStorage.setItem(PENDING, JSON.stringify({ verifier, state, returnTo: location.pathname + location.hash, silent }));
  const challenge = b64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)));
  const params = new URLSearchParams({
    client_id: CLIENT_ID,
    response_type: "code",
    scope: "openid",
    redirect_uri: `${ORIGIN}/`,
    state,
    code_challenge: challenge,
    code_challenge_method: "S256",
    kc_idp_hint: IDP_HINT,
  });
  if (silent) params.set("prompt", "none");
  location.assign(`${OIDC}/auth?${params}`);
  return new Promise<never>(() => {}); // the page is leaving
}

export type SignInResult = { signedIn: true } | { signedIn: false; error?: string };

/** Call once at start-up. Finishes a sign-in that Keycloak just redirected back from. */
export async function completeSignIn(): Promise<SignInResult> {
  const params = new URLSearchParams(location.search);
  const pending = JSON.parse(sessionStorage.getItem(PENDING) || "null") as
    | { verifier: string; state: string; returnTo: string; silent: boolean }
    | null;
  const code = params.get("code");
  const error = params.get("error");
  if (!code && !error) return { signedIn: false };

  sessionStorage.removeItem(PENDING);
  history.replaceState(null, "", pending?.returnTo || "/"); // drop ?code=... from the address bar
  if (error) {
    // login_required after a silent attempt just means "show the Sign in button".
    if (pending?.silent && ["login_required", "interaction_required"].includes(error)) return { signedIn: false };
    return { signedIn: false, error: params.get("error_description") || error };
  }
  if (!pending || pending.state !== params.get("state")) {
    return { signedIn: false, error: "Sign-in didn't match this browser tab. Please sign in again." };
  }
  await tokenRequest({
    grant_type: "authorization_code",
    code: code!,
    redirect_uri: `${ORIGIN}/`,
    code_verifier: pending.verifier,
  });
  return { signedIn: true };
}

/** True if a silent sign-in should be tried (not just tried and failed). */
export function shouldTrySilent(): boolean {
  return !sessionStorage.getItem("flow-studio.silent-tried");
}

export function markSilentTried(): void {
  sessionStorage.setItem("flow-studio.silent-tried", "1");
}

async function tokenRequest(form: Record<string, string>): Promise<void> {
  const resp = await fetch(`${OIDC}/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ client_id: CLIENT_ID, ...form }),
  });
  if (!resp.ok) throw new Error(`Keycloak refused the sign-in (${resp.status})`);
  const t = await resp.json();
  tokens = {
    access: t.access_token,
    refresh: t.refresh_token ?? tokens?.refresh,
    id: t.id_token ?? tokens?.id,
    expiresAt: Date.now() + (t.expires_in ?? 300) * 1000,
  };
}

/** A valid access token, refreshed when it has less than 30 s left. */
export async function accessToken(): Promise<string> {
  if (!tokens) return startSignIn();
  if (Date.now() > tokens.expiresAt - 30_000) {
    refreshing ??= tokenRequest({ grant_type: "refresh_token", refresh_token: tokens.refresh ?? "" }).finally(() => {
      refreshing = null;
    });
    try {
      await refreshing;
    } catch {
      tokens = null;
      return startSignIn(true); // session over: back through Keycloak (unsaved edits warn first)
    }
  }
  return tokens.access;
}

export function signOut(): void {
  const params = new URLSearchParams({ client_id: CLIENT_ID, post_logout_redirect_uri: `${ORIGIN}/` });
  if (tokens?.id) params.set("id_token_hint", tokens.id);
  tokens = null;
  sessionStorage.removeItem("flow-studio.silent-tried");
  sessionStorage.setItem("flow-studio.signed-out", "1");
  location.assign(`${OIDC}/logout?${params}`);
}

/** After an explicit sign-out, don't sign straight back in silently. */
export function justSignedOut(): boolean {
  const v = sessionStorage.getItem("flow-studio.signed-out");
  sessionStorage.removeItem("flow-studio.signed-out");
  return v === "1";
}
