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
  validate: (specText: string) => call<{ valid: boolean; problems: string[] }>("POST", "/v1/validate", specText),
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
};
