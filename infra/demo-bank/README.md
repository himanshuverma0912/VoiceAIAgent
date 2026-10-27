# Demo bank

Made-up credit card customers, so flows can use tools (verify the caller, card
balance, recent payments, raise a dispute, help-centre answers, branches) and be
test-called before a bank's real systems are connected. Nothing is stored.

- MCP (streamable HTTP): `http://demo-bank:8000/mcp`
- REST: `POST http://demo-bank:8000/api/<tool>` with a JSON body
  (`GET` for `search_faq` and `find_branch`)

Started by `infra/docker-compose.yml`, reachable only inside the Docker network.
Set `DEMO_BANK_TOKEN` in `infra/.env` to require `Authorization: Bearer <token>`,
and give the voice agent the same value as `INTEGRATION_DEMO_BANK_TOKEN=Bearer <token>`.

| Card (last 4) | Date of birth | Customer |
|---|---|---|
| 4321 | 14 May 1990 | Asha: balance due soon, a payment to "XYZ GLOBAL SERVICES" to dispute |
| 8765 | 2 Nov 1985 | Ravi: payment overdue, late fee |
| 1111 | 30 Jan 1978 | Meera: nothing due |

Register it with `scripts/add-integration.sh infra/integrations/demo_bank.json`
(MCP) or `infra/integrations/demo_bank_rest.json` (REST).
