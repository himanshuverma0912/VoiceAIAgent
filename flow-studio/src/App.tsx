import { useEffect, useState, type ReactNode } from "react";
import { ApiError, api, type Me } from "./api";
import { completeSignIn, justSignedOut, markSilentTried, shouldTrySilent, signOut, startSignIn } from "./auth";
import {
  IconChannel,
  IconChevron,
  IconIntegrations,
  IconModels,
  IconMoon,
  IconReviews,
  IconSignOut,
  IconStudio,
  IconSun,
  IconWorkspaces,
} from "./components/Icons";
import { Toasts } from "./components/ui";
import { useTheme } from "./theme";
import { CallHistory } from "./pages/CallHistory";
import { FlowEditor } from "./pages/FlowEditor";
import { FlowList } from "./pages/FlowList";
import { Integrations } from "./pages/Integrations";
import { ModelCatalog } from "./pages/ModelCatalog";
import { Reviews, useReviewCount } from "./pages/Reviews";
import { WorkspaceMembers, Workspaces } from "./pages/Workspaces";
import { Link, usePath } from "./router";

type State = { phase: "loading" } | { phase: "signed-out"; error?: string } | { phase: "ready"; me: Me };

export function App() {
  const [state, setState] = useState<State>({ phase: "loading" });

  useEffect(() => {
    (async () => {
      try {
        const result = await completeSignIn();
        if (!result.signedIn) {
          // Not signed in yet: try once without showing anything (works while the
          // Keycloak session is still open, e.g. after a reload).
          if (!result.error && !justSignedOut() && shouldTrySilent()) {
            markSilentTried();
            await startSignIn(true);
          }
          setState({ phase: "signed-out", error: result.error });
          return;
        }
        sessionStorage.removeItem("flow-studio.silent-tried");
        setState({ phase: "ready", me: await api.me() });
      } catch (e) {
        const msg = e instanceof ApiError && e.status === 403 ? e.message : (e as Error).message;
        setState({ phase: "signed-out", error: msg });
      }
    })();
  }, []);

  if (state.phase === "loading") return <div className="center muted">Signing in…</div>;
  if (state.phase === "signed-out") return <SignedOut error={state.error} />;
  return <Shell me={state.me} />;
}

const EDITOR_RE = /^\/flows\/[a-z][a-z0-9_]*\/?$/;

function Shell({ me }: { me: Me }) {
  const path = usePath();
  const inEditor = EDITOR_RE.test(path);
  // The sidebar shrinks to icons in the editor, so the canvas keeps its room;
  // the toggle overrides that until the person moves between editor and pages.
  const [override, setOverride] = useState<boolean | null>(null);
  useEffect(() => setOverride(null), [inEditor]);
  const collapsed = override ?? inEditor;
  return (
    <div className={`shell${collapsed ? " collapsed" : ""}`}>
      <SideBar me={me} path={path} collapsed={collapsed} onToggle={() => setOverride(!collapsed)} />
      <div className="main-col">
        <AppBar me={me} />
        <Routes me={me} />
      </div>
      <Toasts />
    </div>
  );
}

function Routes({ me }: { me: Me }) {
  const path = usePath();
  const calls = /^\/flows\/([a-z][a-z0-9_]*)\/calls\/?$/.exec(path);
  if (calls) return <CallHistory key={calls[1]} flowId={calls[1]} />;
  if (/^\/admin\/models\/?$/.test(path) && me.roles.includes("platform-admin")) return <ModelCatalog />;
  if (/^\/(admin\/)?integrations\/?$/.test(path)) return <Integrations me={me} />;
  if (/^\/reviews\/?$/.test(path)) return <Reviews me={me} />;
  if (/^\/workspaces\/?$/.test(path)) return <Workspaces me={me} />;
  const ws = /^\/workspaces\/([a-z][a-z0-9_-]*)\/?$/.exec(path);
  if (ws) return <WorkspaceMembers key={ws[1]} me={me} workspace={ws[1]} />;
  const m = /^\/flows\/([a-z][a-z0-9_]*)\/?$/.exec(path);
  if (m) return <FlowEditor key={m[1]} me={me} flowId={m[1]} />;
  return <FlowList me={me} />;
}

function SideBar({ me, path, collapsed, onToggle }: { me: Me; path: string; collapsed: boolean; onToggle: () => void }) {
  const waiting = useReviewCount(me);
  const admin = me.roles.includes("platform-admin");
  const items: { to: string; label: string; icon: ReactNode; current: boolean; count?: number }[] = [
    { to: "/", label: "Flows", icon: <IconStudio />, current: path === "/" || path.startsWith("/flows/") },
    { to: "/reviews", label: "Reviews", icon: <IconReviews />, current: path.startsWith("/reviews"), count: waiting },
    { to: "/workspaces", label: "Workspaces", icon: <IconWorkspaces />, current: path.startsWith("/workspaces") },
    { to: "/integrations", label: "Integrations", icon: <IconIntegrations />, current: /^\/(admin\/)?integrations/.test(path) },
  ];
  if (admin) {
    items.push({ to: "/admin/models", label: "Model catalog", icon: <IconModels />, current: path.startsWith("/admin/models") });
  }
  return (
    <nav className="sidebar" aria-label="Main">
      <Link to="/" className="brand" title="Flow Studio">
        <img src="/favicon.svg" alt="" />
        <span className="brand-text">
          <strong>Flow Studio</strong>
          <small>Voice agent builder</small>
        </span>
      </Link>
      <div className="side-nav">
        {items.map((i) => (
          <Link key={i.to} to={i.to} className="nav-item" title={collapsed ? i.label : undefined} current={i.current}>
            {i.icon}
            <span className="nav-label">{i.label}</span>
            {i.count ? <span className="count">{i.count}</span> : null}
          </Link>
        ))}
      </div>
      <button className="side-toggle" onClick={onToggle} title={collapsed ? "Show the menu" : "Shrink the menu"} aria-label={collapsed ? "Show the menu" : "Shrink the menu"}>
        <IconChevron size={18} />
      </button>
    </nav>
  );
}

function AppBar({ me }: { me: Me }) {
  const admin = me.roles.includes("platform-admin");
  const names = me.workspaces.map((m) => m.name || m.workspace_id);
  const where = admin ? "All workspaces" : names.length === 0 ? "No workspace yet" : names.length > 2 ? `${names.length} workspaces` : names.join(", ");
  const roles = new Set<string>();
  for (const m of me.workspaces) {
    if (m.maker) roles.add("Maker");
    if (m.checker) roles.add("Checker");
    if (m.owner) roles.add("Owner");
  }
  const role = admin ? "Platform admin" : roles.size ? [...roles].join(" · ") : "Viewer";
  const who = me.name || me.email;
  const initials =
    who
      .split(/[\s@._-]+/)
      .filter(Boolean)
      .slice(0, 2)
      .map((w) => w[0]!.toUpperCase())
      .join("") || "?";
  return (
    <header className="appbar">
      <div className="ws-chip" title={names.join(", ")}>
        <IconChannel size={22} />
        <span>
          <small>Workspace</small>
          <strong>{where}</strong>
        </span>
      </div>
      <span className="spacer" />
      <div className="user-pill" title={me.email}>
        <span className="avatar" aria-hidden="true">
          {initials}
        </span>
        <span className="user-text">
          <strong>{who}</strong>
          <small>{role}</small>
        </span>
      </div>
      <ThemeToggle />
      <button className="round" onClick={signOut} aria-label="Sign out" title="Sign out">
        <IconSignOut />
      </button>
    </header>
  );
}

/** Switches light/dark; shows what you'd switch to. */
function ThemeToggle({ className = "round" }: { className?: string }) {
  const [theme, toggle] = useTheme();
  const label = theme === "dark" ? "Switch to light mode" : "Switch to dark mode";
  return (
    <button className={className} onClick={toggle} aria-label={label} title={label}>
      {theme === "dark" ? <IconSun /> : <IconMoon />}
    </button>
  );
}

function SignedOut({ error }: { error?: string }) {
  return (
    <div className="center signin">
      <ThemeToggle className="round corner" />
      <div className="card signin-card">
        <div className="brand">
          <img src="/favicon.svg" alt="" />
          <span className="brand-text">
            <strong>Flow Studio</strong>
            <small>Voice agent builder</small>
          </span>
        </div>
        <h1>Welcome</h1>
        <p className="muted">Build and publish the conversations your voice agent runs. Sign in with your company account.</p>
        {error && <div className="notice bad">{error}</div>}
        <button className="primary wide" onClick={() => startSignIn(false)}>
          Sign in
        </button>
      </div>
    </div>
  );
}
