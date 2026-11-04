"""Demo EMI loan MCP servers, for testing workspace integrations end to end.

One program, two roles, to test the two places an enterprise MCP server can be:

  EMI_MCP_ROLE=core      the loan system: borrowers' EMIs and debits. Meant to
                         run privately inside your AWS (a second EC2 in the
                         agent's VPC), reached on its private address.
      verify_borrower(loan_last4, date_of_birth)              -> first name, loan type
      get_emi_status(loan_last4, date_of_birth)               -> EMI, due date, overdue, charges   (sensitive)
      check_recent_debits(loan_last4, date_of_birth)          -> debits, any duplicate             (sensitive)
      raise_debit_dispute(loan_last4, date_of_birth, debit_date, amount) -> reference              (sensitive)

  EMI_MCP_ROLE=helpdesk  customer services that could be anywhere on the
                         internet (another cloud, a SaaS vendor). Reached over
                         HTTPS, e.g. through a Cloudflare tunnel.
      emi_faq(question)                                       -> an answer about EMI rules
      send_payment_link(mobile, loan_last4, amount)           -> reference (demo: nothing is sent)  (sensitive)
      request_callback(mobile, reason)                        -> reference and when

Everything is made up and nothing is stored. Requests need
"Authorization: Bearer $EMI_MCP_TOKEN". Bodies are never logged.

Demo borrowers (loan account last 4 / date of birth):
  4321 / 1990-05-14   Asha    home loan; this month's EMI was debited twice
  8765 / 1985-11-02   Ravi    personal loan; last EMI bounced, overdue with charges
  1111 / 1978-01-30   Meera   car loan; all paid

Run:  EMI_MCP_ROLE=core EMI_MCP_TOKEN=... python emi_mcp.py     (port 8000, or PORT)
MCP endpoint: http://<host>:8000/mcp     Health: GET /healthz (no token)
"""

from __future__ import annotations

import hmac
import os
import random
import re
import sys
from datetime import date, timedelta
from typing import Any

import uvicorn
from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError
from mcp.server.transport_security import TransportSecuritySettings
from starlette.requests import Request
from starlette.responses import JSONResponse

TODAY = date.today()


def _emi_date(months_back: int) -> str:
    """The 5th of this month (0), last month (1), ..."""
    y, m = TODAY.year, TODAY.month - months_back
    while m < 1:
        y, m = y - 1, m + 12
    return date(y, m, 5).isoformat()


def _next_emi_date() -> str:
    d = date(TODAY.year, TODAY.month, 5)
    if d <= TODAY:
        d = date(TODAY.year + (TODAY.month == 12), TODAY.month % 12 + 1, 5)
    return d.isoformat()


BORROWERS: dict[tuple[str, str], dict[str, Any]] = {
    ("4321", "1990-05-14"): {
        "first_name": "Asha",
        "loan_type": "home loan",
        "emi": 24850.00,
        "overdue": 0.0,
        "charges": 0.0,
        "last_emi": {"date": _emi_date(0), "status": "paid"},
        "debits": [
            {"date": _emi_date(0), "amount": 24850.00, "mode": "NACH auto-debit"},
            {"date": _emi_date(0), "amount": 24850.00, "mode": "NACH auto-debit"},
            {"date": _emi_date(1), "amount": 24850.00, "mode": "NACH auto-debit"},
            {"date": _emi_date(2), "amount": 24850.00, "mode": "NACH auto-debit"},
        ],
    },
    ("8765", "1985-11-02"): {
        "first_name": "Ravi",
        "loan_type": "personal loan",
        "emi": 12400.00,
        "overdue": 12400.00,
        "charges": 590.00,  # bounce charge incl. GST
        "last_emi": {"date": _emi_date(0), "status": "bounced (insufficient balance)"},
        "debits": [
            {"date": _emi_date(1), "amount": 12400.00, "mode": "NACH auto-debit"},
            {"date": _emi_date(2), "amount": 12400.00, "mode": "NACH auto-debit"},
        ],
    },
    ("1111", "1978-01-30"): {
        "first_name": "Meera",
        "loan_type": "car loan",
        "emi": 9150.00,
        "overdue": 0.0,
        "charges": 0.0,
        "last_emi": {"date": _emi_date(0), "status": "paid"},
        "debits": [
            {"date": _emi_date(0), "amount": 9150.00, "mode": "UPI"},
            {"date": _emi_date(1), "amount": 9150.00, "mode": "UPI"},
        ],
    },
}

FAQ = [
    (("late", "fee", "penal", "charge"), "If an EMI isn't paid on the due date, penal charges of 2% a month apply on "
     "the overdue EMI, from the due date until it's paid. Paying as soon as possible keeps the charges low."),
    (("bounce", "nach", "return", "insufficient"), "If an auto-debit (NACH) fails, a bounce charge of ₹500 plus GST "
     "(₹590) applies, and the bank tries again only on the next due date. Paying the EMI by link or in the app "
     "avoids further charges."),
    (("cibil", "credit score", "score"), "EMIs paid more than 30 days late are reported to credit bureaus and can "
     "lower the credit score. A single delay paid quickly usually has a small effect."),
    (("prepay", "foreclose", "foreclosure", "part"), "Part-prepayment is allowed after 6 EMIs, at least one EMI's "
     "amount at a time, with no charge on floating-rate home loans. Foreclosure needs a request in the app."),
    (("date", "change"), "The EMI date can be changed once a year to the 5th, 10th or 15th, from the app or by "
     "a request to the loans team."),
    (("double", "twice", "duplicate", "extra"), "If an EMI is debited twice, a dispute is raised and the extra "
     "amount is usually returned within 7 working days after the check."),
]


class NoMatch(ToolError):
    """The details don't match a borrower, or the request can't be done (the
    message goes back to the agent, so it can tell the caller)."""


def _digits(value: Any) -> str:
    return re.sub(r"\D", "", str(value or ""))


def _dob(value: Any) -> str:
    """'1990-05-14', '14/05/1990' or '14 05 1990' -> '1990-05-14'."""
    text = str(value or "").strip()
    if re.fullmatch(r"\d{4}-\d{2}-\d{2}", text):
        return text
    parts = re.findall(r"\d+", text)
    if len(parts) == 3 and len(parts[2]) == 4:
        return f"{parts[2]}-{int(parts[1]):02d}-{int(parts[0]):02d}"
    return text


def _borrower(loan_last4: Any, date_of_birth: Any) -> dict[str, Any]:
    b = BORROWERS.get((_digits(loan_last4)[-4:], _dob(date_of_birth)))
    if b is None:
        raise NoMatch("no loan matches those details")
    return b


def _ref(prefix: str) -> str:
    return f"{prefix}-{random.randint(100000, 999999)}"


# ── core: the loan system ──


def verify_borrower(loan_last4: str, date_of_birth: str) -> dict[str, Any]:
    """Check the caller's loan account (last 4 digits) and date of birth. Fails if they don't match a borrower."""
    b = _borrower(loan_last4, date_of_birth)
    return {"verified": True, "first_name": b["first_name"], "loan_type": b["loan_type"]}


def get_emi_status(loan_last4: str, date_of_birth: str) -> dict[str, Any]:
    """The loan's EMI amount, next due date, the last EMI and whether it was paid or bounced, and any overdue amount and charges (INR)."""
    b = _borrower(loan_last4, date_of_birth)
    return {
        "loan_type": b["loan_type"],
        "emi_amount": b["emi"],
        "next_due_date": _next_emi_date(),
        "last_emi_date": b["last_emi"]["date"],
        "last_emi_status": b["last_emi"]["status"],
        "overdue_amount": b["overdue"],
        "charges": b["charges"],
        "total_to_pay_now": round(b["overdue"] + b["charges"], 2),
        "currency": "INR",
    }


def check_recent_debits(loan_last4: str, date_of_birth: str) -> dict[str, Any]:
    """EMI debits from the caller's bank account in the last 3 months, newest first, and whether any EMI was debited twice."""
    b = _borrower(loan_last4, date_of_birth)
    seen: dict[tuple[str, float], int] = {}
    for d in b["debits"]:
        seen[(d["date"], d["amount"])] = seen.get((d["date"], d["amount"]), 0) + 1
    dup = next(((k, n) for k, n in seen.items() if n > 1), None)
    return {
        "debits": b["debits"],
        "duplicate_found": dup is not None,
        "duplicate_date": dup[0][0] if dup else "",
        "duplicate_amount": dup[0][1] if dup else 0.0,
        "currency": "INR",
    }


def raise_debit_dispute(loan_last4: str, date_of_birth: str, debit_date: str, amount: str = "") -> dict[str, Any]:
    """Raise a dispute for an EMI debited twice or an extra debit. Returns a reference and how long the check takes."""
    _borrower(loan_last4, date_of_birth)
    if not str(debit_date or "").strip():
        raise NoMatch("say when the extra debit happened")
    return {"reference": _ref("EMID"), "review_within": "7 working days"}


# ── helpdesk: services outside the bank's network ──


def emi_faq(question: str) -> dict[str, Any]:
    """Answer a general question about EMIs: late or penal charges, bounced auto-debits, credit score, prepayment, changing the EMI date, double debits."""
    q = str(question or "").lower()
    for keys, answer in FAQ:
        if any(k in q for k in keys):
            return {"answer": answer}
    return {"answer": "No help article matches that; offer a call back from the loans team."}


def send_payment_link(mobile: str, loan_last4: str, amount: str = "") -> dict[str, Any]:
    """Send the caller an SMS link to pay an overdue EMI (demo: nothing is actually sent). Returns a reference."""
    digits = _digits(mobile)
    if len(digits) < 10 or len(_digits(loan_last4)) != 4:
        raise NoMatch("need the caller's mobile number and the loan's last 4 digits")
    return {"reference": _ref("PAY"), "sent_to": "******" + digits[-4:], "valid_for": "24 hours"}


def request_callback(mobile: str, reason: str) -> dict[str, Any]:
    """Book a call back from a loan specialist. Returns a reference and when they will call."""
    if len(_digits(mobile)) < 10:
        raise NoMatch("need a mobile number to call back on")
    if not str(reason or "").strip():
        raise NoMatch("say what the call back is about")
    return {"reference": _ref("CB"), "call_within": "1 working day"}


ROLES = {
    "core": ("emi-core", "The bank's loan system: borrowers' EMIs and debits (made-up data, for test calls).",
             [verify_borrower, get_emi_status, check_recent_debits, raise_debit_dispute]),
    "helpdesk": ("emi-helpdesk", "Loan customer services: EMI help articles, payment links, call backs (demo).",
                 [emi_faq, send_payment_link, request_callback]),
}


def build_app(role: str, token: str):
    name, about, tools = ROLES[role]
    mcp = MCPServer(name=name, instructions=about)
    for fn in tools:
        mcp.tool()(fn)
    app = mcp.streamable_http_app(
        stateless_http=True,
        json_response=True,
        # Reached by private IP or a tunnel's host name; the token protects it.
        transport_security=TransportSecuritySettings(enable_dns_rebinding_protection=False),
    )

    async def health(_: Request) -> JSONResponse:
        return JSONResponse({"status": "ok", "role": role})

    app.add_route("/healthz", health, methods=["GET"])

    async def guarded(scope, receive, send) -> None:
        if scope["type"] == "http" and scope["path"] != "/healthz":
            given = dict(scope.get("headers") or []).get(b"authorization", b"").decode()
            if not hmac.compare_digest(given, f"Bearer {token}"):
                await JSONResponse({"error": "unauthorized"}, status_code=401)(scope, receive, send)
                return
        await app(scope, receive, send)

    return guarded


if __name__ == "__main__":
    role = os.getenv("EMI_MCP_ROLE", "").strip()
    token = os.getenv("EMI_MCP_TOKEN", "").strip()
    if role not in ROLES:
        sys.exit("set EMI_MCP_ROLE to core or helpdesk")
    if len(token) < 16:
        sys.exit("set EMI_MCP_TOKEN to a long random value (openssl rand -hex 24)")
    print(f"EMI MCP server, role {role}: http://0.0.0.0:{os.getenv('PORT', '8000')}/mcp", flush=True)
    # access_log off: paths only, but nothing about calls belongs in logs.
    uvicorn.run(build_app(role, token), host="0.0.0.0", port=int(os.getenv("PORT", "8000")), access_log=False)
