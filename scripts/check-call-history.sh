#!/usr/bin/env bash
# Run on EC2 to check that call history works end to end:
#   bash check-call-history.sh
# It only prints counts and timings (the same content-free facts the Calls page
# shows), never anything about who called.
set -u
FAIL=0
ok()  { echo "✓ $*"; }
bad() { echo "✗ $*"; FAIL=1; }

PUBLIC_HOST=$(grep -E '^PUBLIC_HOST=' ~/infra/.env 2>/dev/null | cut -d= -f2- | tr -d '"'"'"' \r')

echo "── Platform containers"
for c in config-service keycloak studio platform-db; do
  s=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$c" 2>/dev/null)
  if [ "$s" = healthy ] || [ "$s" = running ]; then ok "$c: $s"; else bad "$c: ${s:-not found}"; fi
done

echo "── Database"
if [ "$(docker exec platform-db psql -U platform -d platform -tAc 'select count(*) from schema_migrations where version = 4' 2>/dev/null)" = 1 ]; then
  ok "calls table exists (migration 004 applied)"
else
  bad "migration 004 not applied: docker logs config-service | tail -30"
fi

echo "── Flow Studio from the internet"
if [ -n "$PUBLIC_HOST" ]; then
  code=$(curl -s -o /dev/null -w '%{http_code}' "https://$PUBLIC_HOST/api/v1/flows/x/calls/summary")
  # 401 = the route exists and wants a sign-in, which is right.
  if [ "$code" = 401 ]; then ok "https://$PUBLIC_HOST is up and the Calls API answers"; else bad "https://$PUBLIC_HOST/api answered $code (expected 401)"; fi
else
  bad "PUBLIC_HOST not found in ~/infra/.env"
fi

echo "── Voice agent"
AGENT=$(docker ps --filter "name=^inboundagent" --format '{{.Names}}' | head -1)
if [ -z "$AGENT" ]; then
  bad "no running agent container"
else
  ok "agent container: $AGENT"
  if docker exec "$AGENT" python -c "import flow_engine.calllog" 2>/dev/null; then
    ok "agent has the call-history code"
  else
    bad "agent is an older image (no flow_engine/calllog.py): check the 'Deploy Voice Agent to EC2' run"
  fi
  # Can the agent reach the config service with its token? 401 means the
  # agent's CONFIG_SERVICE_TOKEN doesn't match CONFIG_RUNTIME_TOKEN in ~/infra/.env.
  code=$(docker exec -i "$AGENT" python - <<'EOF' 2>/dev/null
import os, urllib.request, urllib.error
url, token = os.getenv("CONFIG_SERVICE_URL", "").strip(), os.getenv("CONFIG_SERVICE_TOKEN", "").strip()
if not url or not token:
    print("unset")
else:
    req = urllib.request.Request(url.rstrip("/") + "/internal/v1/calls", data=b"{}", method="POST",
                                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    try:
        print(urllib.request.urlopen(req, timeout=3).status)
    except urllib.error.HTTPError as e:
        print(e.code)
    except Exception as e:
        print(type(e).__name__)
EOF
)
  # An empty record is refused (422), which proves the URL and token are right without storing anything.
  case "$code" in
    422) ok "agent reaches the config service and its token is accepted" ;;
    401) bad "agent's CONFIG_SERVICE_TOKEN is wrong (must equal CONFIG_RUNTIME_TOKEN in ~/infra/.env)" ;;
    unset) bad "CONFIG_SERVICE_URL or CONFIG_SERVICE_TOKEN missing from ~/VoiceAIAgent/.env" ;;
    *) bad "agent couldn't send to the config service ($code)" ;;
  esac
  n=$(docker logs --since 24h "$AGENT" 2>&1 | grep -ciE "couldn't send the call record|didn't take the call record")
  if [ "$n" = 0 ]; then ok "no failed call records in the agent's log (last 24 h)"; else bad "$n failed call records in the last 24 h: docker logs $AGENT 2>&1 | grep -i 'call record'"; fi
fi

echo "── Call history (last 7 days)"
docker exec platform-db psql -U platform -d platform -P pager=off -c "
  SELECT channel, count(*) AS calls,
         count(*) FILTER (WHERE outcome = 'completed') AS completed,
         count(*) FILTER (WHERE outcome = 'caller_hung_up') AS hung_up
  FROM calls WHERE started_at > now() - interval '7 days' GROUP BY channel ORDER BY channel" 2>/dev/null
echo "Latest 5 calls:"
docker exec platform-db psql -U platform -d platform -P pager=off -c "
  SELECT to_char(started_at AT TIME ZONE 'Asia/Kolkata', 'DD Mon HH24:MI') AS started_ist,
         flow_id, coalesce('v' || flow_version, flow_source) AS version, channel, outcome,
         round(duration_ms / 1000.0) || ' s' AS length,
         (SELECT string_agg(s->>'node', ' → ') FROM jsonb_array_elements(steps) s) AS steps
  FROM calls ORDER BY started_at DESC LIMIT 5" 2>/dev/null

echo
if [ "$FAIL" = 0 ]; then echo "All checks passed."; else echo "Some checks failed (✗ above)."; fi
exit "$FAIL"
