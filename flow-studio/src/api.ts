// The config service's API, as Flow Studio uses it (through Caddy at /api).
import { accessToken } from "./auth";

export interface Me {
  subject: string;
  email: string;
  name: string;
  roles: string[];
  teams: string[];
}

export interface FlowSummary {
  flow_id: string;
  name: string;
  team: string;
  latest_version: number | null;
  draft_updated_at: string | null;
  draft_valid: boolean | null;
  numbers: string[];
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
  published_by: string;
  spec?: unknown;
}

// ── Model catalog: the models flows may use ──

export type CatalogKind = "llm" | "stt" | "tts_voice";

export interface CatalogEntry {
  kind: CatalogKind;
  model_id: string;
  label: string;
  notes: string;
  languages: string[]; // empty: any
  teams: string[]; // empty: every team
  enabled: boolean;
  updated_at: string;
  updated_by: string;
}

export type CatalogInput = Pick<CatalogEntry, "label" | "notes" | "languages" | "teams" | "enabled">;

/** May a flow of this team use the entry? (The server checks the same.) */
export function offeredTo(e: CatalogEntry, team: string): boolean {
  return e.enabled && (e.teams.length === 0 || e.teams.includes(team));
}

// ── Call history (counts and timings only; no call content) ──

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

export const can = {
  edit: (me: Me, team: string) => me.roles.includes("platform-admin") || (me.roles.includes("flow-editor") && me.teams.includes(team)),
  publish: (me: Me, team: string) =>
    me.roles.includes("platform-admin") || (me.roles.includes("flow-publisher") && me.teams.includes(team)),
  /** Teams this person can create flows in ("*" = any, for admins). */
  createIn: (me: Me): string[] | "*" =>
    me.roles.includes("platform-admin") ? "*" : me.roles.includes("flow-editor") ? me.teams : [],
};

export const api = {
  me: () => call<Me>("GET", "/v1/me"),
  flows: () => call<{ flows: FlowSummary[] }>("GET", "/v1/flows").then((r) => r.flows),
  flow: (id: string) => call<FlowSummary>("GET", `/v1/flows/${id}`),
  draft: (id: string) => call<Draft>("GET", `/v1/flows/${id}/draft`),
  /** team: also check the model catalog as it applies to that team. */
  validate: (specText: string, team?: string) =>
    call<{ valid: boolean; problems: string[] }>("POST", `/v1/validate${team ? `?team=${encodeURIComponent(team)}` : ""}`, specText),
  catalog: () => call<{ entries: CatalogEntry[] }>("GET", "/v1/catalog").then((r) => r.entries),
  putCatalog: (kind: CatalogKind, id: string, input: CatalogInput) =>
    call<{ created: boolean; used_by: string[] }>("PUT", `/v1/catalog/${kind}/${encodeURIComponent(id)}`, JSON.stringify(input)),
  deleteCatalog: (kind: CatalogKind, id: string) => call<void>("DELETE", `/v1/catalog/${kind}/${encodeURIComponent(id)}`),
  /** ifMatch: only save if nobody else saved since (409 otherwise). createOnly: fail if the flow exists. */
  saveDraft: (id: string, specText: string, opts: { team?: string; ifMatch?: string; createOnly?: boolean } = {}) =>
    call<{ valid: boolean; problems: string[]; checksum: string }>(
      "PUT",
      `/v1/flows/${id}/draft${opts.team ? `?team=${encodeURIComponent(opts.team)}` : ""}`,
      specText,
      {
        ...(opts.ifMatch ? { "If-Match": `"${opts.ifMatch}"` } : {}),
        ...(opts.createOnly ? { "If-None-Match": "*" } : {}),
      },
    ),
  saveLayout: (id: string, positions: Record<string, { x: number; y: number }>) =>
    call<void>("PUT", `/v1/flows/${id}/layout`, JSON.stringify({ positions })),
  publish: (id: string) =>
    call<{ version: number; unchanged: boolean; published_by: string }>("POST", `/v1/flows/${id}/publish`),
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
