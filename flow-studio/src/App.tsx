import { useEffect, useState } from "react";
import { ApiError, api, type Me } from "./api";
import { completeSignIn, justSignedOut, markSilentTried, shouldTrySilent, signOut, startSignIn } from "./auth";
import { Toasts } from "./components/ui";
import { FlowEditor } from "./pages/FlowEditor";
import { FlowList } from "./pages/FlowList";
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
  return (
    <>
      <TopBar me={state.me} />
      <Routes me={state.me} />
      <Toasts />
    </>
  );
}

function Routes({ me }: { me: Me }) {
  const path = usePath();
  const m = /^\/flows\/([a-z][a-z0-9_]*)\/?$/.exec(path);
  if (m) return <FlowEditor key={m[1]} me={me} flowId={m[1]} />;
  return <FlowList me={me} />;
}

function TopBar({ me }: { me: Me }) {
  return (
    <header className="topbar">
      <Link to="/" className="brand">
        <img src="/favicon.svg" alt="" />
        Flow Studio
      </Link>
      <span className="spacer" />
      <div className="user">
        <div>{me.name || me.email}</div>
        <div className="muted">
          {me.teams.length ? `Teams: ${me.teams.join(", ")}` : me.roles.includes("platform-admin") ? "Platform admin" : "No team"}
        </div>
      </div>
      <button onClick={signOut}>Sign out</button>
    </header>
  );
}

function SignedOut({ error }: { error?: string }) {
  return (
    <div className="center">
      <div className="card" style={{ width: "min(420px, 100%)" }}>
        <img src="/favicon.svg" alt="" width={40} height={40} />
        <h1 style={{ margin: "12px 0 6px" }}>Flow Studio</h1>
        <p className="muted" style={{ marginTop: 0 }}>
          Build and publish the conversations your voice agent runs. Sign in with your company account.
        </p>
        {error && <div className="notice bad">{error}</div>}
        <button className="primary" onClick={() => startSignIn(false)}>
          Sign in
        </button>
      </div>
    </div>
  );
}
