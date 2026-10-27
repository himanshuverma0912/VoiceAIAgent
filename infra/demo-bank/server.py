"""Demo core-banking server: made-up credit card customers, for test calls.

It stands in for a bank's real systems so flows can be built and test-called
with real lookups before the bank's own API is connected. Everything here is
invented; nothing is stored (a "dispute" only returns a reference number).

The same tools are offered two ways, to exercise both kinds of integration:

  MCP   POST /mcp            (streamable HTTP; tools listed below)
  REST  POST /api/<tool>     JSON body in, JSON out; GET /api/search_faq?question=...

Tools:
  verify_customer(card_last4, date_of_birth)          -> first name, or an error
  get_card_balance(card_last4, date_of_birth)         -> amounts and due date       (sensitive)
  recent_transactions(card_last4, date_of_birth, count) -> latest card payments    (sensitive)
  raise_dispute(card_last4, date_of_birth, merchant, amount, date) -> reference    (sensitive)
  search_faq(question)                                -> a help-centre answer
  find_branch(city)                                   -> branches in a city

Details that don't match a customer are an error (MCP: isError; REST: 404).

Requests need "Authorization: Bearer $DEMO_BANK_TOKEN" when DEMO_BANK_TOKEN is
set. Bodies are never logged (they hold card digits and dates of birth).

Demo customers (card last 4 / date of birth):
  4321 / 1990-05-14   Asha    balance due soon, one payment she may not recognise
  8765 / 1985-11-02   Ravi    payment overdue
  1111 / 1978-01-30   Meera   nothing due
"""

from __future__ import annotations

import hmac
import os
import random
import re
from datetime import date, timedelta
from typing import Any

import uvicorn
from mcp.server.mcpserver import MCPServer
from mcp.server.transport_security import TransportSecuritySettings
from starlette.requests import Request
from starlette.responses import JSONResponse

TODAY = date.today()


def _d(days: int) -> str:
    return (TODAY + timedelta(days=days)).isoformat()


CUSTOMERS: dict[tuple[str, str], dict[str, Any]] = {
    ("4321", "1990-05-14"): {
        "first_name": "Asha",
        "credit_limit": 100000,
        "outstanding": 18450.75,
        "minimum_due": 922.54,
        "due_date": _d(9),
        "transactions": [
            {"date": _d(-1), "merchant": "Swiggy", "amount": 486.00},
            {"date": _d(-2), "merchant": "XYZ GLOBAL SERVICES", "amount": 4999.00},
            {"date": _d(-4), "merchant": "Amazon India", "amount": 2349.00},
            {"date": _d(-6), "merchant": "Indian Oil, Andheri", "amount": 3000.00},
            {"date": _d(-9), "merchant": "Netflix", "amount": 649.00},
            {"date": _d(-12), "merchant": "Apollo Pharmacy", "amount": 812.75},
        ],
    },
    ("8765", "1985-11-02"): {
        "first_name": "Ravi",
        "credit_limit": 50000,
        "outstanding": 42310.00,
        "minimum_due": 4231.00,
        "due_date": _d(-6),
        "late_fee": 750.00,
        "transactions": [
            {"date": _d(-3), "merchant": "Reliance Digital", "amount": 18999.00},
            {"date": _d(-8), "merchant": "MakeMyTrip", "amount": 12400.00},
            {"date": _d(-15), "merchant": "BigBasket", "amount": 2210.00},
        ],
    },
    ("1111", "1978-01-30"): {
        "first_name": "Meera",
        "credit_limit": 250000,
        "outstanding": 0.0,
        "minimum_due": 0.0,
        "due_date": None,
        "transactions": [],
    },
}

FAQ = [
    (("minimum", "min due"), "The minimum due is the smallest amount to pay by the due date to avoid a late fee. "
     "Interest is still charged on the rest of the balance."),
    (("late", "fee"), "A late fee is charged when less than the minimum due is paid by the due date. "
     "It depends on the balance: ₹0 up to ₹500, then ₹500 to ₹1,300."),
    (("interest", "finance charge"), "Interest of 3.5% a month is charged on balances carried over from the "
     "previous statement, and on cash withdrawals from the day they're taken."),
    (("emi",), "Purchases above ₹2,500 can usually be converted to EMIs of 3, 6, 9 or 12 months from the app "
     "(Cards, then Convert to EMI). Interest and a processing fee apply and are shown before you confirm."),
    (("block", "lost", "stolen"), "Block a lost or stolen card at once in the app (Cards, then Block card) or "
     "by calling the 24x7 helpline on the bank's website. A replacement arrives in about 7 working days."),
    (("pay", "payment", "upi", "auto"), "Pay the bill from the app, net banking, any UPI app using the card's "
     "biller name, auto-debit from a Demo Bank account, or at a branch."),
    (("statement", "bill date"), "Statements are generated on the 18th of each month; the due date is 20 days "
     "later. Statements are emailed and also in the app under Cards, then Statements."),
    (("reward", "points"), "Every ₹100 spent earns 2 reward points; 1 point is worth 25 paise when redeemed "
     "in the app."),
]

BRANCHES = {
    "mumbai": ["Andheri East, Chakala", "Fort, DN Road", "Powai, Hiranandani Gardens"],
    "bengaluru": ["MG Road", "Koramangala 5th Block", "Whitefield, ITPL Road"],
    "delhi": ["Connaught Place", "Saket, Select Citywalk", "Rajouri Garden"],
    "pune": ["FC Road", "Hinjewadi Phase 1"],
}


class NoMatch(Exception):
    """The details don't match a customer."""


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


def _customer(card_last4: Any, date_of_birth: Any) -> dict[str, Any]:
    c = CUSTOMERS.get((_digits(card_last4)[-4:], _dob(date_of_birth)))
    if c is None:
        raise NoMatch("no customer matches those details")
    return c


# ── The tools (plain functions, offered over MCP and REST) ──


def verify_customer(card_last4: str, date_of_birth: str) -> dict[str, Any]:
    """Check a caller's card (last 4 digits) and date of birth. Fails if they don't match a customer."""
    return {"verified": True, "first_name": _customer(card_last4, date_of_birth)["first_name"]}


def get_card_balance(card_last4: str, date_of_birth: str) -> dict[str, Any]:
    """The card's outstanding balance, minimum due, due date and available credit (amounts in INR)."""
    c = _customer(card_last4, date_of_birth)
    overdue = bool(c["due_date"]) and c["due_date"] < TODAY.isoformat() and c["minimum_due"] > 0
    return {
        "outstanding": c["outstanding"],
        "minimum_due": c["minimum_due"],
        "due_date": c["due_date"] or "nothing due",
        "overdue": overdue,
        "late_fee": c.get("late_fee", 0.0),
        "credit_limit": c["credit_limit"],
        "available_credit": round(c["credit_limit"] - c["outstanding"], 2),
        "currency": "INR",
    }


def recent_transactions(card_last4: str, date_of_birth: str, count: int = 5) -> dict[str, Any]:
    """The card's latest payments, newest first (at most 10)."""
    c = _customer(card_last4, date_of_birth)
    count = max(1, min(int(count or 5), 10))
    return {"transactions": c["transactions"][:count], "currency": "INR"}


def raise_dispute(card_last4: str, date_of_birth: str, merchant: str, amount: str = "", date: str = "") -> dict[str, Any]:
    """Raise a dispute for a payment the caller doesn't recognise. Returns a reference number."""
    _customer(card_last4, date_of_birth)
    if not str(merchant or "").strip():
        raise NoMatch("say which payment to dispute (the merchant name)")
    # Demo only: nothing is stored; the reference is made up.
    return {"reference": f"DSP-{random.randint(100000, 999999)}", "callback_within": "2 working days"}


def search_faq(question: str) -> dict[str, Any]:
    """Answer a general credit card question from the help centre (fees, interest, EMI, payments, rewards)."""
    q = str(question or "").lower()
    for keys, answer in FAQ:
        if any(k in q for k in keys):
            return {"answer": answer}
    return {"answer": "No help-centre article matches that; offer a call back from the team."}


def find_branch(city: str) -> dict[str, Any]:
    """Demo Bank branches in a city."""
    found = BRANCHES.get(str(city or "").strip().lower())
    return {"city": city, "branches": found or [], "note": "" if found else "No branch in that city; suggest the app."}


TOOLS = [verify_customer, get_card_balance, recent_transactions, raise_dispute, search_faq, find_branch]

# ── MCP ──

mcp = MCPServer(name="demo-bank", instructions="Demo Bank's core banking (made-up data, for test calls).")
for fn in TOOLS:
    mcp.tool()(fn)

# ── REST ──


async def rest_tool(request: Request) -> JSONResponse:
    fn = {f.__name__: f for f in TOOLS}.get(request.path_params["tool"])
    if fn is None:
        return JSONResponse({"error": "no such tool"}, status_code=404)
    if request.method == "GET":
        args = dict(request.query_params)
    else:
        try:
            args = await request.json()
        except ValueError:
            return JSONResponse({"error": "send a JSON object"}, status_code=400)
    if not isinstance(args, dict):
        return JSONResponse({"error": "send a JSON object"}, status_code=400)
    try:
        return JSONResponse(fn(**args))
    except NoMatch as e:
        return JSONResponse({"error": str(e)}, status_code=404)
    except TypeError:
        return JSONResponse({"error": "missing or unexpected inputs"}, status_code=422)


async def health(_: Request) -> JSONResponse:
    return JSONResponse({"status": "ok"})


TOKEN = os.getenv("DEMO_BANK_TOKEN", "").strip()


class RequireToken:
    """Checks the bearer token on every request except /healthz."""

    def __init__(self, app) -> None:
        self.app = app

    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] == "http" and TOKEN and scope["path"] != "/healthz":
            headers = dict(scope.get("headers") or [])
            given = headers.get(b"authorization", b"").decode()
            if not hmac.compare_digest(given, f"Bearer {TOKEN}"):
                await JSONResponse({"error": "unauthorized"}, status_code=401)(scope, receive, send)
                return
        await self.app(scope, receive, send)


def build_app():
    app = mcp.streamable_http_app(
        stateless_http=True,
        json_response=True,
        # Reached by container name (http://demo-bank:8000) inside the Docker network.
        transport_security=TransportSecuritySettings(enable_dns_rebinding_protection=False),
    )
    app.add_route("/api/{tool}", rest_tool, methods=["GET", "POST"])
    app.add_route("/healthz", health, methods=["GET"])
    return RequireToken(app)


if __name__ == "__main__":
    # access_log off: paths only, but nothing about calls belongs in logs.
    uvicorn.run(build_app(), host="0.0.0.0", port=int(os.getenv("PORT", "8000")), access_log=False)
