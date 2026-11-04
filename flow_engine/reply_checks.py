"""Reply checks: what the agent is about to say, checked before the caller hears it.

The LLM's reply streams in small pieces. Each agent step's llm_node (runtime.py)
passes it through `checked()`, which collects the pieces into sentences and runs
the flow's checks on each sentence before it goes on to text-to-speech. A
check returns one of:

    PASS               say the sentence as it is
    Verdict.fix(text)  say this instead and carry on (e.g. a number masked)
    Verdict.stop()     don't say it; say the flow's safe line and end the reply

Whatever is said (checked text, or the safe line) is also what the
conversation history keeps, so the LLM sees what the caller actually heard.

Which checks run: the flow's settings.reply_checks on every step, plus a
conversation step's own reply_checks. The names are listed in spec.py
(REPLY_CHECKS) and in the config service; adding a check means writing it here
with @reply_check, adding its name to both lists, and a line in Flow Studio.

Nothing is logged except the check's name and the step: never the sentence.

Cost: the caller hears nothing until the first sentence is complete (typically
a few hundred milliseconds more than streaming word by word); after that the
LLM keeps writing while the earlier sentences are spoken.
"""

from __future__ import annotations

import logging
import re
from collections.abc import AsyncIterable, Callable
from dataclasses import dataclass
from decimal import ROUND_CEILING, ROUND_FLOOR, ROUND_HALF_UP, Decimal, InvalidOperation
from typing import TYPE_CHECKING, Any

from livekit.agents import llm

if TYPE_CHECKING:
    from .runtime import CallState

logger = logging.getLogger("flow_engine")


# ── Verdicts and the registry ────────────────────────────────────────────────


@dataclass(frozen=True)
class Verdict:
    action: str  # "pass" | "fix" | "stop"
    text: str = ""
    retry: bool = False  # after a stop: let the LLM answer again once, with a reminder

    @staticmethod
    def fix(text: str) -> Verdict:
        return Verdict("fix", text)

    @staticmethod
    def stop(retry: bool = False) -> Verdict:
        return Verdict("stop", retry=retry)


PASS = Verdict("pass")

Check = Callable[[str, "CheckContext"], Verdict]
CHECKS: dict[str, Check] = {}


def reply_check(name: str) -> Callable[[Check], Check]:
    """Register a check under the name flows use for it."""

    def register(fn: Check) -> Check:
        CHECKS[name] = fn
        return fn

    return register


# ── What a check can see ─────────────────────────────────────────────────────


class CheckContext:
    """One reply on one step: the call (memory only) and the conversation so far."""

    def __init__(self, state: CallState, node_id: str, chat_ctx: llm.ChatContext | None = None) -> None:
        self.state = state
        self.node_id = node_id
        self.chat_ctx = chat_ctx
        self.said: list[str] = []  # sentences already let through in this reply
        self.stopped: Fired | None = None  # the check that stopped this reply, if any
        self.called_tools = False  # the reply also called a tool
        self._allowed: set[Decimal] | None = None

    @property
    def hindi(self) -> bool:
        return self.state.flow.settings.language.startswith("hi")

    @property
    def verified(self) -> bool:
        return bool(self.state.tools and self.state.tools.verified)

    def allowed_amounts(self) -> set[Decimal]:
        """Numbers the agent may say as amounts: what the bank's tools returned on
        this call, what the flow itself says (its fees, limits...), and what the
        caller said or gave."""
        if self._allowed is None:
            allowed = set(self.state.tool_numbers) | flow_numbers(self.state.flow)
            for value in self.state.values.values():
                allowed |= numbers_in(value)
            for item in getattr(self.chat_ctx, "items", None) or []:
                if getattr(item, "type", None) == "message" and getattr(item, "role", None) == "user":
                    allowed |= numbers_in(item.text_content or "")
            self._allowed = allowed
        return self._allowed


# ── Numbers ──────────────────────────────────────────────────────────────────

_NUMBER = re.compile(r"\d[\d,]*(?:\.\d+)?")
_SCALE = {"lakh": 100_000, "lakhs": 100_000, "lac": 100_000, "lacs": 100_000, "crore": 10_000_000, "crores": 10_000_000}


def _decimal(text: str) -> Decimal | None:
    try:
        return Decimal(text.replace(",", "")).quantize(Decimal("0.01"))
    except InvalidOperation:
        return None


def numbers_in(value: Any) -> set[Decimal]:
    """Every number in a value (tool results are dicts and lists; text is searched)."""
    out: set[Decimal] = set()
    if isinstance(value, bool) or value is None:
        return out
    if isinstance(value, (int, float)):
        d = _decimal(str(value))
        return {d} if d is not None else out
    if isinstance(value, dict):
        for v in value.values():
            out |= numbers_in(v)
        return out
    if isinstance(value, (list, tuple)):
        for v in value:
            out |= numbers_in(v)
        return out
    for m in _NUMBER.finditer(str(value)):
        d = _decimal(m.group())
        if d is not None:
            out.add(d)
    return out


_flow_numbers_cache: dict[int, set[Decimal]] = {}


def flow_numbers(flow: Any) -> set[Decimal]:
    """Numbers written in the flow itself (reviewed and approved text)."""
    key = id(flow)
    if key not in _flow_numbers_cache:
        if len(_flow_numbers_cache) > 64:
            _flow_numbers_cache.clear()
        _flow_numbers_cache[key] = numbers_in(flow.model_dump(mode="json"))
    return _flow_numbers_cache[key]


# An amount: ₹922.54, Rs. 1,500, INR 300, 5000 rupees, ₹1.5 lakh, 2 करोड़ रुपये...
_AMOUNT = re.compile(
    r"(?:₹|\brs\.?|\binr\b)\s*(?P<a>\d[\d,]*(?:\.\d+)?)(?:\s*(?P<as>lakhs?|lacs?|crores?)\b)?"
    r"|(?P<b>\d[\d,]*(?:\.\d+)?)(?:\s*(?P<bs>lakhs?|lacs?|crores?)\b)?\s*(?:rupees?\b|rs\b\.?|inr\b|रुपये|रुपए|rupaye\b)",
    re.IGNORECASE,
)


def amounts_in(sentence: str) -> list[Decimal]:
    out = []
    for m in _AMOUNT.finditer(sentence):
        number, scale = (m.group("a"), m.group("as")) if m.group("a") else (m.group("b"), m.group("bs"))
        d = _decimal(number)
        if d is None:
            continue
        if scale:
            d = (d * _SCALE[scale.lower()]).quantize(Decimal("0.01"))
        out.append(d)
    return out


def _matches(said: Decimal, known: set[Decimal]) -> bool:
    """Exact to the paisa, or a whole-rupee rounding of a known amount
    (personas often say "rounded to the rupee")."""
    if said in known:
        return True
    if said != said.to_integral_value():
        return False
    for k in known:
        for mode in (ROUND_HALF_UP, ROUND_FLOOR, ROUND_CEILING):
            if k.quantize(Decimal("1"), rounding=mode) == said:
                return True
    return False


# ── The built-in checks ──────────────────────────────────────────────────────

# 12 or more digits in a row (spaces or dashes between them allowed): a card,
# account or Aadhaar number. Phone numbers (10 digits) and toll-free numbers
# (11) pass; amounts have commas, so they aren't caught either.
_LONG_NUMBER = re.compile(r"(?<![\d])\d(?:[ -]?\d){11,}(?![\d])")


@reply_check("mask_long_numbers")
def _mask_long_numbers(sentence: str, ctx: CheckContext) -> Verdict:
    if not _LONG_NUMBER.search(sentence):
        return PASS

    def mask(m: re.Match) -> str:
        last4 = re.sub(r"\D", "", m.group())[-4:]
        return f"आखिरी चार अंक {last4}" if ctx.hindi else f"ending in {last4}"

    return Verdict.fix(_LONG_NUMBER.sub(mask, sentence))


@reply_check("amounts_from_tools")
def _amounts_from_tools(sentence: str, ctx: CheckContext) -> Verdict:
    said = amounts_in(sentence)
    if not said:
        return PASS
    allowed = ctx.allowed_amounts()
    if all(_matches(a, allowed) for a in said):
        return PASS
    return Verdict.stop(retry=True)


# Promises the agent may not make on a call. A negation shortly before, in the
# same clause ("I can't guarantee", "it won't be waived"), is fine; one in an
# earlier clause isn't ("Don't worry, it will be waived").
_PROMISE = re.compile(
    r"\b(?:guarantee[sd]?|waive[sd]?|waiver|"
    r"will be (?:refunded|reversed|waived|credited back)|"
    r"(?:will|'ll) (?:get|receive) (?:a |the |your )?refund|"
    r"(?:is|has been|have been|are) approved)\b"
    r"|गारंटी|माफ़ कर|माफ कर",
    re.IGNORECASE,
)
_CLAUSE = re.compile(r"[,;:.!?—–]|\s-\s|।")
_NEGATION = re.compile(r"\b(?:not|no|never|cannot|can't|can’t|won't|won’t|unable|don't|don’t)\b|नहीं", re.IGNORECASE)


@reply_check("no_promises")
def _no_promises(sentence: str, ctx: CheckContext) -> Verdict:
    for m in _PROMISE.finditer(sentence):
        before = _CLAUSE.split(sentence[max(0, m.start() - 40) : m.start()])[-1]
        if not _NEGATION.search(before):
            return Verdict.stop()
    return PASS


# What the caller hears when a check stops a reply (unless the flow sets its own line).
SAFE_LINES = {
    "amounts_from_tools": (
        "Let me just double-check that figure for you.",
        "कृपया एक पल रुकिए, मुझे वह राशि दोबारा देखनी होगी।",
    ),
    "no_promises": (
        "I can't confirm that on this call, but I can raise a request for you.",
        "इस कॉल पर इसकी पुष्टि नहीं हो सकती, लेकिन आपके लिए अनुरोध दर्ज किया जा सकता है।",
    ),
}
DEFAULT_SAFE_LINE = ("I'm sorry, I can't say that on this call.", "माफ़ कीजिए, यह जानकारी इस कॉल पर नहीं दी जा सकती।")

# Said to the LLM (not the caller) when an amount was stopped, for one more try.
AMOUNT_RETRY = (
    "Your last reply was stopped before the caller heard it, because an amount in it didn't match "
    "anything the bank's tools returned. Answer the caller again now, saying amounts in digits with ₹ "
    "exactly as the tool gave them (or rounded to the rupee). If you don't have the figure, look it up "
    "with your tools or say you can't check it right now."
)

# Added to a step's instructions when amounts are checked, so the LLM writes them checkably.
AMOUNT_RULE = (
    "Say amounts in digits with ₹ (for example ₹922.54), exactly as a tool gave them or rounded to the "
    "rupee. Never add up, estimate or make up an amount."
)


def safe_line(name: str, ctx: CheckContext) -> str:
    own = ctx.state.flow.settings.reply_check_line.strip()
    if own:
        return ctx.state.render(own)
    english, hindi = SAFE_LINES.get(name, DEFAULT_SAFE_LINE)
    return hindi if ctx.hindi else english


# ── Running the checks over a streamed reply ─────────────────────────────────

# A sentence ends at . ! ? or । followed by a space, or at a line break; not
# after "Rs." or "Mr." and the like.
_BOUNDARY = re.compile(r"[.!?।]+[\"'”’)]*\s+|\n+")
_ABBREVIATION = re.compile(r"(?:\b(?:rs|mr|mrs|ms|dr|no|sr|jr|st|vs|etc|approx|a\.m|p\.m|e\.g|i\.e)\.)\s*$", re.IGNORECASE)
_MAX_HELD = 400  # a "sentence" this long without an end is checked in parts


def split_sentences(buffer: str) -> tuple[list[str], str]:
    """Complete sentences (with their trailing space) and what's left over."""
    out: list[str] = []
    start = 0
    for m in _BOUNDARY.finditer(buffer):
        piece = buffer[start : m.end()]
        if _ABBREVIATION.search(piece):
            continue
        out.append(piece)
        start = m.end()
    rest = buffer[start:]
    if len(rest) > _MAX_HELD:
        cut = rest.rfind(" ", 0, _MAX_HELD)
        if cut > 0:
            out.append(rest[: cut + 1])
            rest = rest[cut + 1 :]
    return out, rest


@dataclass
class Fired:
    check: str
    action: str  # "fix" or "stop"
    retry: bool = False


def run_checks(sentence: str, names: list[str], ctx: CheckContext) -> tuple[str | None, Fired | None, list[Fired]]:
    """(text to say or None, the check that stopped it or None, every check that fired)."""
    fired: list[Fired] = []
    for name in names:
        check = CHECKS.get(name)
        if check is None:  # the config service refuses unknown names; just in case
            logger.warning("unknown reply check", extra={"check": name})
            continue
        try:
            verdict = check(sentence, ctx)
        except Exception:  # a broken check must never take the call down
            logger.exception("reply check failed", extra={"check": name})
            continue
        if verdict.action == "fix":
            fired.append(Fired(name, "fix"))
            sentence = verdict.text
        elif verdict.action == "stop":
            stop = Fired(name, "stop", verdict.retry)
            fired.append(stop)
            return None, stop, fired
    return sentence, None, fired


async def checked(
    stream: AsyncIterable[Any],
    names: list[str],
    ctx: CheckContext,
    on_fired: Callable[[Fired], None] | None = None,
) -> AsyncIterable[Any]:
    """The LLM's stream, with its text checked sentence by sentence. Tool calls
    and usage pass through untouched; text after a stop is dropped."""
    buffer = ""
    stopped: Fired | None = None

    def handle(sentence: str) -> str | None:
        nonlocal stopped
        text, stop, fired = run_checks(sentence, names, ctx)
        for f in fired:
            logger.info("reply check fired", extra={"check": f.check, "action": f.action, "node": ctx.node_id})
            if on_fired:
                try:
                    on_fired(f)
                except Exception:
                    logger.exception("reply check hook failed")
        if stop:
            stopped = ctx.stopped = stop
            return safe_line(stop.check, ctx) + " "
        if text is not None:
            ctx.said.append(text)
        return text

    async for chunk in stream:
        text: str | None = None
        passthrough: Any = chunk
        if isinstance(chunk, str):
            text, passthrough = chunk, None
        elif isinstance(chunk, llm.ChatChunk) and chunk.delta and chunk.delta.content:
            text = chunk.delta.content
            if chunk.delta.tool_calls or chunk.usage or chunk.delta.extra:
                passthrough = chunk.model_copy(update={"delta": chunk.delta.model_copy(update={"content": None})})
            else:
                passthrough = None
        if text and not stopped:
            buffer += text
            ready, buffer = split_sentences(buffer)
            for sentence in ready:
                out = handle(sentence)
                if out:
                    yield out
                if stopped:
                    buffer = ""
                    break
        if passthrough is not None:
            if isinstance(passthrough, llm.ChatChunk) and passthrough.delta and passthrough.delta.tool_calls:
                ctx.called_tools = True
            yield passthrough
    if buffer.strip() and not stopped:
        out = handle(buffer)
        if out:
            yield out
