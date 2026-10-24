#!/usr/bin/env bash
# Save a flow file as the draft in the config service, publish it, and
# optionally point a phone number at it. Run on EC2 (the service listens on
# 127.0.0.1:8080 there), or anywhere with an SSH tunnel to it.
#
#   export CONFIG_ADMIN_TOKEN=...            # from ~/infra/.env
#   scripts/publish-flow.sh flows/emi-payment-issue.json               # publish only
#   scripts/publish-flow.sh flows/emi-payment-issue.json +918045678901 # and map the number
#
# Until the UI exists (Phase 4), this is how flow teams publish.
set -euo pipefail

FILE="${1:?usage: publish-flow.sh <flow.json> [phone number]}"
NUMBER="${2:-}"
URL="${CONFIG_SERVICE_URL:-http://127.0.0.1:8080}"
: "${CONFIG_ADMIN_TOKEN:?export CONFIG_ADMIN_TOKEN first (it is in ~/infra/.env)}"
AUTH="Authorization: Bearer $CONFIG_ADMIN_TOKEN"

# Prints one field of a JSON answer (python3 is on Ubuntu by default; jq may not be).
field() { python3 -c 'import json,sys; v=json.load(sys.stdin).get(sys.argv[1]); print(v if not isinstance(v,(list,dict)) else json.dumps(v))' "$1"; }

FLOW_ID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["flow_id"])' "$FILE")
echo "Flow: $FLOW_ID ($FILE)"

# 1. Save as draft. Problems (if any) are listed and nothing is published.
ANSWER=$(curl -sS --fail-with-body -X PUT "$URL/v1/flows/$FLOW_ID/draft" -H "$AUTH" \
  -H "Content-Type: application/json" --data-binary "@$FILE")
if [ "$(echo "$ANSWER" | field valid)" != "True" ]; then
  echo "The flow has problems, not published:"
  echo "$ANSWER" | python3 -c 'import json,sys; [print("  -", p) for p in json.load(sys.stdin)["problems"]]'
  exit 1
fi
echo "Draft saved (valid)"

# 2. Publish: the draft becomes the next version.
ANSWER=$(curl -sS --fail-with-body -X POST "$URL/v1/flows/$FLOW_ID/publish" -H "$AUTH")
if [ "$(echo "$ANSWER" | field unchanged)" = "True" ]; then
  echo "Nothing changed since version $(echo "$ANSWER" | field version); still live"
else
  echo "Published version $(echo "$ANSWER" | field version)"
fi

# 3. Optionally route a number to this flow (latest version from now on).
if [ -n "$NUMBER" ]; then
  ENCODED=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$NUMBER")
  ANSWER=$(curl -sS --fail-with-body -X PUT "$URL/v1/numbers/$ENCODED" -H "$AUTH" \
    -H "Content-Type: application/json" --data "{\"flow_id\": \"$FLOW_ID\", \"version_pin\": null}")
  echo "Calls to $(echo "$ANSWER" | field number) now run $FLOW_ID (latest version)"
fi
