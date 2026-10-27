#!/usr/bin/env bash
# Add or update an integration (a bank API or MCP server flows can call) from a
# JSON file. Run on EC2 (the config service listens on 127.0.0.1:8080 there).
#
#   export CONFIG_ADMIN_TOKEN=...                      # from ~/infra/.env
#   scripts/add-integration.sh infra/integrations/demo_bank.json
#   scripts/add-integration.sh infra/integrations/demo_bank_rest.json
#
# The file name (without .json) is the integration id. Admins can also manage
# integrations in Flow Studio (Integrations). No secrets go in the file: its
# auth_env names a variable in the voice agent's .env (INTEGRATION_...).
set -euo pipefail

FILE="${1:?usage: add-integration.sh <integration.json>}"
URL="${CONFIG_SERVICE_URL:-http://127.0.0.1:8080}"
: "${CONFIG_ADMIN_TOKEN:?export CONFIG_ADMIN_TOKEN first (it is in ~/infra/.env)}"
ID=$(basename "$FILE" .json)

curl -sS --fail-with-body -X PUT "$URL/v1/integrations/$ID" \
  -H "Authorization: Bearer $CONFIG_ADMIN_TOKEN" -H "Content-Type: application/json" \
  --data-binary @"$FILE" | python3 -c '
import json, sys
r = json.load(sys.stdin)
i = r["integration"]
print(("Added" if r["created"] else "Updated"), i["integration_id"], "-", i["label"])
print("  tools:", ", ".join(t["name"] + (" (needs a verified caller)" if t["sensitive"] else "") for t in i["tools"]))
if r["used_by"]:
    print("  used by:", ", ".join(r["used_by"]))
'
