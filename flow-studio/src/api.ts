// The config service's API, as Flow Studio uses it (through Caddy at /api).
import { accessToken } from "./auth";

/** A person's place in a workspace. Everyone in it can look; makers edit and
 * submit; checkers approve or reject other people's submissions; owners manage members. */
export interface Membership {
  workspace_id: string;
  name: string;
  maker: boolean;
  checker: boolean;
  owner: boolean;
}

export interface Me {
  subject: string;
  email: string;
  name: string;
  roles: string[];
  workspaces: Membership[];
}

export interface ReviewRef {
  review_id: number;
  submitted_by: string;
  submitted_at: string;
  checksum: string;
}

export interface FlowSummary {
  flow_id: string;
  name: string;
  workspace: string;
  latest_version: number | null;
  draft_updated_at: string | null;
  draft_valid: boolean | null;
  numbers: string[];
  pending_review: ReviewRef | null;
}

export interface Draft {
  spec: unknown;
  checksum: string;
  layout: { positions: Record<string, { x: number; y: number }> } | null;
  problems: string[];
  updated_at: string;
  updated_by: string;
}

export interface Version {
  flow_id: string;
  version: number;
  checksum: string;
  published_at: string;
  published_by: string; // the checker who approved it
  submitted_by: string; // the maker who submitted it ("" if published by a script)
  spec?: unknown;
}

export interface Review {
  review_id: number;
  flow_id: string;
  flow_name: string;
  workspace: string;
  checksum: string;
  base_version: number | null;
  note: string;
  submitted_by: string;
  submitted_at: string;
  status: "pending" | "approved" | "rejected" | "withdrawn" | "superseded";
  decided_by: string;
  decided_at: string | null;
  decision_note: string;
  version: number | null;
  spec?: unknown;
  for_me?: boolean;
}

export interface Workspace {
  workspace_id: string;
  name: string;
  created_at: string;
  created_by: string;
  flows: number;
  members: number;
  my_roles: { maker: boolean; checker: boolean; owner: boolean } | null;
}

export interface Member {
  email: string;
  maker: boolean;
  checker: boolean;
  owner: boolean;
  added_at: string;
  added_by: string;
}

export interface BuildResult {
  flow: unknown;
  notes: string[];
  problems: string[];
  attempts: number;
  steps: number;
  input_tokens: number;
  output_tokens: number;
}

export interface Build {
  build_id: string;
  flow_id: string;
  name: string;
  workspace: string;
  status: "running" | "done" | "failed";
  progress: string;
  error?: string;
  result?: BuildResult;
  started_at: string;
}

// ── Model catalog: the models flows may use ──

export type CatalogKind = "llm" | "stt" | "tts_voice";

export interface CatalogEntry {
  kind: CatalogKind;
  model_id: string;
  label: string;
  notes: string;
  languages: string[]; // empty: any
  workspaces: string[]; // empty: every workspace
  enabled: boolean;
  updated_at: string;
  updated_by: string;
}

export type CatalogInput = Pick<CatalogEntry, "label" | "notes" | "languages" | "workspaces" | "enabled">;

/** May a flow of this workspace use the entry? (The server checks the same.) */
export function offeredTo(e: CatalogEntry, workspace: string): boolean {
  return e.enabled && (e.workspaces.length === 0 || e.workspaces.includes(workspace));
}

// ── Call history (counts and timings only; no call content) ──

// ── Integrations: bank APIs and MCP servers flows may call ──

export interface ToolInputDef {
  name: string;
  type: "string" | "number" | "integer" | "boolean";
  description: string;
  required: boolean;
  enum?: string[];
}

export interface IntegrationTool {
  name: string;
  description: string;
  inputs: ToolInputDef[];
  outputs: string[];
  sensitive: boolean; // needs a verified caller
  locked: string[]; // inputs the flow must set (never the AI)
  max_calls: number;
  method?: string; // REST only (admins)
  path?: string;
}

export interface Integration {
  integration_id: string;
  kind: "mcp" | "http";
  label: string;
  notes: string;
  url?: string; // admins only
  test_url?: string;
  auth_header?: string;
  auth_env?: string;
  timeout_ms?: number;
  workspaces: string[];
  enabled: boolean;
  tools: IntegrationTool[];
  updated_at: string;
  updated_by: string;
}

export type IntegrationInput = Omit<Integration, "integration_id" | "updated_at" | "updated_by">;

/** A tool a flow of this workspace may use, as "<integration>.<tool>". */
export interface OfferedTool {
  ref: string;
  integration: Integration;
  tool: IntegrationTool;
}

export function offeredTools(list: Integration[] | null, workspace: string): OfferedTool[] {
  return (list ?? [])
    .filter((i) => i.enabled && (i.workspaces.length === 0 || i.workspaces.includes(workspace)))
    .flatMap((i) => i.tools.map((t) => ({ ref: `${i.integration_id}.${t.name}`, integration: i, tool: t })));
}

export type Outcome = "completed" | "ended_early" | "caller_hung_up" | "time_limit" | "error" | "other";
export type Channel = "phone" | "studio_test" | "all";

export interface CallUsage {
  llm_input_tokens: number;
  llm_output_tokens: number;
  stt_audio_seconds: number;
  tts_characters: number;
  tts_audio_seconds: number;
}

export interface Call {
  call_id: string;
  flow_version: number | null;
  flow_source: string;
  channel: "phone" | "studio_test";
  runtime: string;
  started_at: string;
  duration_ms: number;
  outcome: Outcome;
  steps: { node: string; ms: number }[];
  tools?: { tool: string; ok: boolean; ms: number }[];
  turns: { caller: number; agent: number };
  latency_ms: { p50: number; p95: number; count: number } | null;
  usage: CallUsage;
}

export interface Stats {
  avg: number;
  p50: number;
  p95: number;
}

export interface CallSummary {
  days: number;
  channel: string;
  calls: number;
  truncated: boolean;
  outcomes: Record<Outcome, number>;
  duration_ms: Stats | null;
  latency_ms: Stats | null;
  steps: { node: string; reached: number; avg_ms: number; ended_here: number; hung_up_here: number }[];
  tools?: { tool: string; uses: number; failed: number; avg_ms: number; p95_ms: number }[];
  versions: {
    version: number | null;
    source: string;
    calls: number;
    completed: number;
    caller_hung_up: number;
    avg_duration_ms: number;
    last_call_at: string;
  }[];
  per_day: { date: string; calls: number; completed: number }[];
  usage: CallUsage;
  avg_turns: { caller: number; agent: number } | null;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public problems: string[] = [],
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: string, headers: Record<string, string> = {}): Promise<T> {
  const resp = await fetch(`/api${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${await accessToken()}`,
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      ...headers,
    },
    body,
  });
  if (resp.status === 204) return undefined as T;
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new ApiError(resp.status, data.error || `${method} ${path} failed (${resp.status})`, data.problems);
  return data as T;
}

const isAdmin = (me: Me) => me.roles.includes("platform-admin");
const membership = (me: Me, ws: string) => me.workspaces.find((m) => m.workspace_id === ws);

/** What the signed-in person may do (the server checks the same rules). */
export const can = {
  admin: isAdmin,
  see: (me: Me, ws: string) => isAdmin(me) || !!membership(me, ws),
  edit: (me: Me, ws: string) => isAdmin(me) || !!membership(me, ws)?.maker,
  approve: (me: Me, ws: string) => isAdmin(me) || !!membership(me, ws)?.checker,
  manage: (me: Me, ws: string) => isAdmin(me) || !!membership(me, ws)?.owner,
  /** Workspaces this person can create flows in ("*" = any, for admins). */
  createIn: (me: Me): Membership[] | "*" => (isAdmin(me) ? "*" : me.workspaces.filter((m) => m.maker)),
};

export const api = {
  me: () => call<Me>("GET", "/v1/me"),
  flows: () => call<{ flows: FlowSummary[] }>("GET", "/v1/flows").then((r) => r.flows),
  flow: (id: string) => call<FlowSummary>("GET", `/v1/flows/${id}`),
  draft: (id: string) => call<Draft>("GET", `/v1/flows/${id}/draft`),
  /** workspace: also check the model catalog as it applies there. */
  validate: (specText: string, workspace?: string) =>
    call<{ valid: boolean; problems: string[] }>(
      "POST",
      `/v1/validate${workspace ? `?workspace=${encodeURIComponent(workspace)}` : ""}`,
      specText,
    ),
  catalog: () => call<{ entries: CatalogEntry[] }>("GET", "/v1/catalog").then((r) => r.entries),
  integrations: () => call<{ integrations: Integration[] }>("GET", "/v1/integrations").then((r) => r.integrations),
  putIntegration: (id: string, input: IntegrationInput) =>
    call<{ integration: Integration; created: boolean; used_by: string[] }>("PUT", `/v1/integrations/${id}`, JSON.stringify(input)),
  deleteIntegration: (id: string) => call<void>("DELETE", `/v1/integrations/${id}`),
  putCatalog: (kind: CatalogKind, id: string, input: CatalogInput) =>
    call<{ created: boolean; used_by: string[] }>("PUT", `/v1/catalog/${kind}/${encodeURIComponent(id)}`, JSON.stringify(input)),
  deleteCatalog: (kind: CatalogKind, id: string) => call<void>("DELETE", `/v1/catalog/${kind}/${encodeURIComponent(id)}`),
  /** ifMatch: only save if nobody else saved since (409 otherwise). createOnly: fail if the flow exists. */
  saveDraft: (id: string, specText: string, opts: { workspace?: string; ifMatch?: string; createOnly?: boolean } = {}) =>
    call<{ valid: boolean; problems: string[]; checksum: string }>(
      "PUT",
      `/v1/flows/${id}/draft${opts.workspace ? `?workspace=${encodeURIComponent(opts.workspace)}` : ""}`,
      specText,
      {
        ...(opts.ifMatch ? { "If-Match": `"${opts.ifMatch}"` } : {}),
        ...(opts.createOnly ? { "If-None-Match": "*" } : {}),
      },
    ),
  saveLayout: (id: string, positions: Record<string, { x: number; y: number }>) =>
    call<void>("PUT", `/v1/flows/${id}/layout`, JSON.stringify({ positions })),
  // ── maker-checker ──
  submitReview: (id: string, note: string) => call<Review>("POST", `/v1/flows/${id}/reviews`, JSON.stringify({ note })),
  reviews: (id: string) => call<{ reviews: Review[] }>("GET", `/v1/flows/${id}/reviews`).then((r) => r.reviews),
  review: (id: string, reviewId: number) => call<Review>("GET", `/v1/flows/${id}/reviews/${reviewId}`),
  approve: (id: string, reviewId: number, note: string) =>
    call<{ version: number; published_by: string; submitted_by: string }>(
      "POST",
      `/v1/flows/${id}/reviews/${reviewId}/approve`,
      JSON.stringify({ note }),
    ),
  reject: (id: string, reviewId: number, note: string) =>
    call<Review>("POST", `/v1/flows/${id}/reviews/${reviewId}/reject`, JSON.stringify({ note })),
  withdraw: (id: string, reviewId: number) => call<Review>("POST", `/v1/flows/${id}/reviews/${reviewId}/withdraw`),
  pendingReviews: () => call<{ reviews: Review[] }>("GET", "/v1/reviews").then((r) => r.reviews),
  // ── workspaces ──
  workspaces: () => call<{ workspaces: Workspace[] }>("GET", "/v1/workspaces").then((r) => r.workspaces),
  putWorkspace: (ws: string, name: string) => call<Workspace>("PUT", `/v1/workspaces/${ws}`, JSON.stringify({ name })),
  deleteWorkspace: (ws: string) => call<void>("DELETE", `/v1/workspaces/${ws}`),
  members: (ws: string) => call<{ workspace: Workspace; members: Member[] }>("GET", `/v1/workspaces/${ws}/members`),
  putMember: (ws: string, email: string, roles: { maker: boolean; checker: boolean; owner: boolean }) =>
    call<Member>("PUT", `/v1/workspaces/${ws}/members/${encodeURIComponent(email)}`, JSON.stringify(roles)),
  removeMember: (ws: string, email: string) => call<void>("DELETE", `/v1/workspaces/${ws}/members/${encodeURIComponent(email)}`),
  // ── AI flow builder ──
  startBuild: (req: { workspace: string; flow_id: string; name: string; description: string }) =>
    call<Build>("POST", "/v1/builds", JSON.stringify(req)),
  build: (id: string) => call<Build>("GET", `/v1/builds/${id}`),
  versions: (id: string) => call<{ versions: Version[] }>("GET", `/v1/flows/${id}/versions`).then((r) => r.versions),
  version: (id: string, n: number) => call<Version>("GET", `/v1/flows/${id}/versions/${n}`),
  /** A browser test call of the saved draft: a LiveKit room with the agent dispatched. */
  startTest: (id: string) =>
    call<{ url: string; token: string; room: string; max_seconds: number }>("POST", `/v1/flows/${id}/test-sessions`),
  calls: (id: string, days: number, channel: Channel, limit = 50) =>
    call<{ calls: Call[] }>("GET", `/v1/flows/${id}/calls?days=${days}&channel=${channel}&limit=${limit}`).then((r) => r.calls),
  callSummary: (id: string, days: number, channel: Channel) =>
    call<CallSummary>("GET", `/v1/flows/${id}/calls/summary?days=${days}&channel=${channel}`),
};
