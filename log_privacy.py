"""Mask personal data in every log line the agent writes.

The platform stores no call content, but LiveKit's own log lines include the
caller's phone number (room name "+9199…_abc", participant "sip_+9199…") and
fields it tags as personal data ("lk.pii.*", e.g. keypad digits). LiveKit only
removes those from its telemetry export, not from console logs, and Docker
keeps console logs on the EC2 disk.

install() masks them in every record, just before it is written:
  - phone numbers keep their last 4 digits:  +919990187773 -> +91******7773
  - any "lk.pii.*" field becomes "[redacted]"
It hooks logging.Handler.handle, the one step every log line passes through.
Lines from each call's job process are forwarded to the main process and
written there, so installing it in the main process covers all of them.
"""

from __future__ import annotations

import logging
import re

# 10–15 digits, optionally starting with "+", not part of a longer digit run.
_PHONE_RE = re.compile(r"(?<!\d)(\+?)(\d{6,11})(\d{4})(?!\d)")
_PII_PREFIX = "lk.pii."
REDACTED = "[redacted]"

# Record attributes that are never personal data and must stay as they are.
_SKIP = frozenset(
    {
        "name", "levelname", "levelno", "pathname", "filename", "module", "lineno",
        "funcName", "created", "msecs", "relativeCreated", "thread", "threadName",
        "processName", "process", "taskName", "stack_info",
    }
)


def mask_phone_numbers(text: str) -> str:
    def _mask(m: re.Match) -> str:
        plus, middle, last4 = m.groups()
        keep = middle[:2] if plus else ""  # keep the country code after "+"
        return f"{plus}{keep}{'*' * (len(middle) - len(keep))}{last4}"

    return _PHONE_RE.sub(_mask, text)


def _scrub(value):
    if isinstance(value, str):
        return mask_phone_numbers(value)
    if isinstance(value, dict):
        return {k: (REDACTED if str(k).startswith(_PII_PREFIX) else _scrub(v)) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return type(value)(_scrub(v) for v in value)
    return value


def scrub_record(record: logging.LogRecord) -> None:
    if getattr(record, "_privacy_scrubbed", False):
        return
    if isinstance(record.msg, str):
        record.msg = mask_phone_numbers(record.msg)
    if record.args:
        record.args = _scrub(record.args)
    for key, value in list(record.__dict__.items()):
        if key in _SKIP or key in ("msg", "args"):
            continue
        if key.startswith(_PII_PREFIX):
            record.__dict__[key] = REDACTED
        elif isinstance(value, (str, dict, list, tuple)):
            record.__dict__[key] = _scrub(value)
    record._privacy_scrubbed = True


_installed = False


def install() -> None:
    """Mask personal data in every log record from now on (safe to call twice)."""
    global _installed
    if _installed:
        return
    original_handle = logging.Handler.handle

    def handle(self, record):  # type: ignore[no-untyped-def]
        scrub_record(record)
        return original_handle(self, record)

    logging.Handler.handle = handle  # type: ignore[method-assign]
    _installed = True
