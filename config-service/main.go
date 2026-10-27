// Config service: stores flows (prompts, nodes, guardrails), their published
// versions and which phone number runs which flow, in Postgres. The voice agent
// asks it which flow to run at the start of each call.
//
// Settings (environment):
//
//	DATABASE_URL   postgres://user:pass@host:5432/db?sslmode=disable
//	ADMIN_TOKEN    token for /v1/* (flow teams, the UI)
//	RUNTIME_TOKEN  token for /internal/v1/* (the voice agent)
//	LISTEN_ADDR    default :8080
//
// Sign-in for people (Flow Studio, through Keycloak). Without OIDC_ISSUER only
// ADMIN_TOKEN works on /v1:
//
//	OIDC_ISSUER    issuer in the tokens, e.g. https://voiceflow.duckdns.org/auth/realms/voice-platform
//	OIDC_JWKS_URL  where to read Keycloak's keys; default <OIDC_ISSUER>/protocol/openid-connect/certs
//	               (on EC2 the internal http://keycloak:8080/auth/realms/voice-platform/protocol/openid-connect/certs)
//	OIDC_AUDIENCE  default config-api
//
// Browser test calls from Flow Studio ("Talk to this draft"); off when unset:
//
//	LIVEKIT_URL         wss://<project>.livekit.cloud (same as the agent's)
//	LIVEKIT_API_KEY     the agent's LiveKit key and secret
//	LIVEKIT_API_SECRET
//	LIVEKIT_AGENT_NAME  default VoiceAIAgentInbound
//
// The AI flow builder (turns a written description into a flow); off when unset:
//
//	LITELLM_URL         e.g. http://litellm:4000
//	LITELLM_API_KEY     a LiteLLM key
//	FLOW_BUILDER_MODEL  LiteLLM alias, default flow-builder (see infra/litellm-config.yaml)
//
// Call history is deleted after CALL_RETENTION_DAYS (default 90).
//
// `config-service -healthcheck` exits 0 if the service on this host is up
// (used by Docker; the image has no curl).
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/api"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/builder"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/livekit"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "check the local service is up, then exit")
	flag.Parse()
	addr := env("LISTEN_ADDR", ":8080")
	if *healthcheck {
		os.Exit(runHealthcheck(addr))
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(addr); err != nil {
		slog.Error("config service stopped", "error", err)
		os.Exit(1)
	}
}

func run(addr string) error {
	dbURL, admin, runtime := env("DATABASE_URL", ""), env("ADMIN_TOKEN", ""), env("RUNTIME_TOKEN", "")
	if dbURL == "" || admin == "" || runtime == "" {
		return errors.New("DATABASE_URL, ADMIN_TOKEN and RUNTIME_TOKEN must all be set")
	}
	if len(admin) < 24 || len(runtime) < 24 || admin == runtime {
		return errors.New("ADMIN_TOKEN and RUNTIME_TOKEN must be different and at least 24 characters (openssl rand -hex 24)")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The database container may still be starting: retry for up to a minute.
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil {
			break
		}
		if attempt == 20 || ctx.Err() != nil {
			return errors.New("database not reachable: " + err.Error())
		}
		slog.Info("waiting for the database", "attempt", attempt)
		time.Sleep(3 * time.Second)
	}
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}

	var users *auth.Verifier
	if issuer := strings.TrimRight(env("OIDC_ISSUER", ""), "/"); issuer != "" {
		users = &auth.Verifier{
			Issuer:   issuer,
			Audience: env("OIDC_AUDIENCE", "config-api"),
			JWKSURL:  env("OIDC_JWKS_URL", issuer+"/protocol/openid-connect/certs"),
		}
		slog.Info("sign-in with Keycloak enabled", "issuer", issuer, "audience", users.Audience)
	} else {
		slog.Warn("OIDC_ISSUER not set: only ADMIN_TOKEN can use /v1")
	}

	lk := livekit.Config{
		URL:       env("LIVEKIT_URL", ""),
		APIKey:    env("LIVEKIT_API_KEY", ""),
		APISecret: env("LIVEKIT_API_SECRET", ""),
		AgentName: env("LIVEKIT_AGENT_NAME", "VoiceAIAgentInbound"),
	}
	if lk.Enabled() {
		slog.Info("browser test calls enabled", "livekit", lk.URL, "agent", lk.AgentName)
	} else {
		slog.Warn("LIVEKIT_URL / LIVEKIT_API_KEY / LIVEKIT_API_SECRET not set: browser test calls are off")
	}

	st := store.New(db)

	// The AI flow builder (Flow Studio's "Describe it"): LiteLLM and a model alias.
	var flowBuilder *builder.Builder
	if url, key := env("LITELLM_URL", ""), env("LITELLM_API_KEY", ""); url != "" && key != "" {
		flowBuilder = builder.New(builder.Config{URL: url, APIKey: key, Model: env("FLOW_BUILDER_MODEL", "flow-builder")})
		slog.Info("AI flow builder enabled", "model", flowBuilder.Model())
	} else {
		slog.Warn("LITELLM_URL / LITELLM_API_KEY not set: the AI flow builder is off")
	}

	// Call history is kept for CALL_RETENTION_DAYS (default 90), then deleted.
	retention, err := strconv.Atoi(env("CALL_RETENTION_DAYS", "90"))
	if err != nil || retention < 1 || retention > 730 {
		return errors.New("CALL_RETENTION_DAYS must be a number of days between 1 and 730")
	}
	go purgeOldCalls(ctx, st, retention)

	srv := &http.Server{
		Addr:              addr,
		Handler:           (&api.Server{Store: st, AdminToken: admin, RuntimeToken: runtime, Users: users, LiveKit: lk, Builder: flowBuilder}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	slog.Info("config service listening", "addr", addr)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// purgeOldCalls deletes call history older than the retention period, at
// start-up and then every 6 hours.
func purgeOldCalls(ctx context.Context, st *store.Store, days int) {
	for {
		n, err := st.PurgeCalls(ctx, time.Now().AddDate(0, 0, -days))
		if err != nil && ctx.Err() == nil {
			slog.Error("couldn't delete old call history", "error", err)
		} else if n > 0 {
			slog.Info("deleted old call history", "calls", n, "older_than_days", days)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(6 * time.Hour):
		}
	}
}

func runHealthcheck(addr string) int {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
