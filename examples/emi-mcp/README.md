# EMI MCP servers (demo)

Two small MCP servers with made-up loan data, to test developers adding their
own MCP servers to a flow from two places:

| Integration id | Role | Where it runs | Tools |
|---|---|---|---|
| `emi_core` | the loan system | **inside your AWS**: a second EC2 in the agent's VPC, on its private IP | `verify_borrower`, `get_emi_status`, `check_recent_debits`, `raise_debit_dispute` |
| `emi_helpdesk` | customer services | **anywhere on the internet**, over HTTPS (here: your Mac + a Cloudflare tunnel) | `emi_faq`, `send_payment_link`, `request_callback` |

Both come from one program, `emi_mcp.py` (`EMI_MCP_ROLE=core` or `helpdesk`).
`emi-payment-issue.json` is the EMI flow using both. The `*.tools.json` files hold
each server's tools with the rules already set (which tools need a verified
caller, which inputs the flow sets), to paste into Flow Studio.

Nothing is stored and nothing is sent; every request needs
`Authorization: Bearer <EMI_MCP_TOKEN>`.

| Loan (last 4) | Date of birth | Borrower | Try |
|---|---|---|---|
| 4321 | 14 May 1990 | Asha, home loan | "My EMI was debited twice" → finds the duplicate, raises a dispute |
| 8765 | 2 Nov 1985 | Ravi, personal loan | "I missed my EMI" → bounced, ₹12,990 to pay, sends a payment link |
| 1111 | 30 Jan 1978 | Meera, car loan | "When is my next EMI?" → all paid |

Make two different tokens first (one per server):

```
openssl rand -hex 24     # for emi_core
openssl rand -hex 24     # for emi_helpdesk
```

## 1. `emi_core` inside your AWS (private)

1. **EC2 → Launch instance**: name `emi-core-mcp`, Ubuntu 24.04, `t3.micro`, key
   pair `voiceai`. Under **Network settings → Edit**: the **same VPC** as the agent's
   EC2 (the default one), auto-assign public IP on (only for SSH and installing Docker).
   Create a security group `emi-core-mcp` with:
   - SSH (22) from **My IP**;
   - Custom TCP **8000** from the **agent EC2's security group** (its `sg-…` id, on
     the agent instance's *Security* tab), so nothing else can reach the server.
2. Copy this folder over and start it (from the repo folder on your Mac):
   ```
   scp -i ~/Downloads/voiceai.pem -r examples/emi-mcp ubuntu@<NEW_PUBLIC_IP>:~/
   ssh -i ~/Downloads/voiceai.pem ubuntu@<NEW_PUBLIC_IP>
   sudo apt-get update && sudo apt-get install -y docker.io
   cd ~/emi-mcp && sudo docker build -t emi-mcp .
   sudo docker run -d --name emi-core --restart unless-stopped -p 8000:8000 \
     -e EMI_MCP_ROLE=core -e EMI_MCP_TOKEN=<core token> emi-mcp
   ```
3. Note its **private** IP (EC2 console, e.g. `172.31.40.12`) and check it from the
   **agent's** EC2:
   ```
   curl http://<PRIVATE_IP>:8000/healthz      # {"status":"ok","role":"core"}
   ```
   Address for Flow Studio: `http://<PRIVATE_IP>:8000/mcp`. Calls never leave the VPC.

Stop the instance after testing; it costs about $0.01 an hour while running.

## 2. `emi_helpdesk` anywhere (public HTTPS)

On your Mac (it stands in for "another cloud or a vendor"):

```
cd examples/emi-mcp
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
EMI_MCP_ROLE=helpdesk EMI_MCP_TOKEN=<helpdesk token> python emi_mcp.py
```

In a second terminal, give it a public HTTPS address:

```
brew install cloudflared
cloudflared tunnel --url http://localhost:8000
```

It prints `https://<random-words>.trycloudflare.com`. Check it:
`curl https://<random-words>.trycloudflare.com/healthz`. Address for Flow
Studio: `https://<random-words>.trycloudflare.com/mcp`.

Keep both terminals open and the Mac awake while testing. The address changes
each time `cloudflared` restarts; then edit the integration in Flow Studio (new
address, token typed again, approved again). For something that stays up, run the
same `Dockerfile` on any host with HTTPS (another cloud VM, Render, Fly.io…).

## 3. In Flow Studio

If `INTEGRATION_ALLOWED_HOSTS` is set in `~/infra/.env`, it must include both:
`172.31.0.0/16,*.trycloudflare.com`.

1. **A maker in the EMI workspace**: Integrations → **Add an MCP server**, twice:

   | | `emi_core` | `emi_helpdesk` |
   |---|---|---|
   | Workspace | EMI | EMI |
   | Integration id | `emi_core` | `emi_helpdesk` (the flow uses these exact ids) |
   | URL | `http://<PRIVATE_IP>:8000/mcp` | `https://<random-words>.trycloudflare.com/mcp` |
   | Sign-in | Token in a header, `Authorization`, `Bearer <core token>` | `Authorization`, `Bearer <helpdesk token>` |

   Press **Connect & load tools** (proves the platform reaches it), then **Edit as
   JSON** and paste `emi_core.tools.json` / `emi_helpdesk.tools.json` (the rules),
   give it a name, and **Send for review**.
2. **A checker in the EMI workspace**: Reviews → approve both.
3. **The maker**: open the EMI flow → JSON → paste `emi-payment-issue.json` → Save.
   **Test call** it with the borrowers above, then **Submit for review**; the checker
   approves, and phone calls use it.

Call history shows each call's tools (`emi_core.verify_borrower` …) and whether
they worked.
