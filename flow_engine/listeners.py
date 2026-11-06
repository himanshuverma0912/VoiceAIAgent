"""Side listeners: they follow what the caller says, alongside the conversation,
and act when they hear something (flow format: settings.listeners, spec.py).

    secret_guard    The caller starts reading out an OTP, PIN, CVV, password or
                    a full card or account number. The agent warns them at once
                    (from the live, still-changing transcript), and that turn
                    is dropped: the AI never gets it and the conversation
                    history never keeps it. Every request to the LLM is also
                    masked (mask_chat_ctx), in case one was already on its way.
    keyword_watch   One of its words is said. When the caller's turn ends:
                    go_to a step (with what they said), say a line, flag the
                    call, or end it.
    promise_to_pay  After each thing the caller says, a small AI check asks
                    whether it was a promise to pay; if so the date and amount
                    are kept as variables (e.g. {{promised_date}}). It runs on
                    the side: the agent's reply doesn't wait for it.
    time_limit      A clock for the whole call: a few minutes before the limit
                    the agent tells the caller time is nearly up; at the limit
                    it ends the call politely, or goes to a step (a call-back
                    booking, say) and ends a few minutes after that.
    off_topic       After each thing the caller says, a small AI check asks
                    whether it was about the call. After a few off-topic turns
                    in a row the agent politely brings them back; if it goes on,
                    it ends the call, goes to a step or flags the call.

Each listener runs on every step, or only on the steps it lists. Nothing is
logged or reported except the listener's name, what it did and the step:
never what the caller said, never the values it kept.

How it hooks in (runtime.py): every agent and question of the flow calls
Listeners.on_turn() from on_user_turn_completed (the moment a caller's turn is
complete, before the AI answers), and Listeners.attach() follows the session's
live transcripts and new messages.
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
from collections.abc import Awaitable, Callable
from datetime import datetime
from typing import TYPE_CHECKING, Any
from zoneinfo import ZoneInfo

from livekit.agents import StopResponse, llm

from .spec import KeywordWatch, OffTopic, PromiseToPay, SecretGuard, TimeLimit

if TYPE_CHECKING:
    from livekit.agents import Agent, AgentSession

    from .runtime import CallState

logger = logging.getLogger("flow_engine")

IST = ZoneInfo("Asia/Kolkata")


# ── Secrets ──────────────────────────────────────────────────────────────────

# Words that say a secret is being read out. "PIN code" / "pincode" is an
# Indian postal code, not a secret, so it's taken out first.
_POSTAL = re.compile(r"\bpin\s*-?\s*codes?\b|पिन\s*कोड", re.IGNORECASE)
_SECRET_WORD = re.compile(
    r"\b(?:o\.?\s?t\.?\s?p|one[\s-]?time[\s-]?(?:password|pass\s?code|code)|m-?pin|t-?pin|pin|cvv|cvc|"
    r"pass\s?word|pass\s?code|security\s?code)\b|ओटीपी|ओ\s?टी\s?पी|पिन|पासवर्ड|सीवीवी",
    re.IGNORECASE,
)
# A digit as a numeral or a spoken word (English, Hindi, Hinglish).
_DIGIT_WORD = (
    r"zero|oh|one|two|three|four|five|six|seven|eight|nine|"
    r"shunya|ek|teen|char|chaar|paanch|panch|chhe|chheh|saat|aath|nau|"
    r"शून्य|एक|दो|तीन|चार|पांच|पाँच|छह|छः|सात|आठ|नौ"
)
_DIGIT = re.compile(rf"\d|\b(?:{_DIGIT_WORD})\b", re.IGNORECASE)
# 12 or more digits in a row (spaces or dashes allowed): a card or account number.
_LONG_NUMBER = re.compile(r"(?<!\d)\d(?:[ -]?\d){11,}(?!\d)")
_SECRET_DIGITS = 3  # with a secret word: this many digits start a secret


def secret_in(text: str) -> bool:
    """Is the caller reading out a secret? A secret word with digits, or a
    full card or account number."""
    if not text:
        return False
    if _LONG_NUMBER.search(text):
        return True
    cleaned = _POSTAL.sub(" ", text)
    return bool(_SECRET_WORD.search(cleaned)) and len(_DIGIT.findall(cleaned)) >= _SECRET_DIGITS


SECRET_REMOVED = "[the caller started to read out a secret code or card number; removed]"


def mask_text(text: str) -> str:
    return SECRET_REMOVED if secret_in(text) else text


def mask_chat_ctx(chat_ctx: llm.ChatContext) -> llm.ChatContext:
    """A copy of a request to the LLM with any secret the caller said taken out
    (a reply prepared while the caller was still talking can contain one)."""
    items = chat_ctx.items
    if not any(i.type == "message" and i.role == "user" and secret_in(i.text_content or "") for i in items):
        return chat_ctx
    out = chat_ctx.copy()
    for i, item in enumerate(out.items):
        if item.type == "message" and item.role == "user" and secret_in(item.text_content or ""):
            out.items[i] = item.model_copy(update={"content": [SECRET_REMOVED]})
    return out


SECRET_WARNING = (
    "Please don't share your OTP, PIN, CVV or card number with anyone, not even with us. We will never ask for it.",
    "कृपया अपना ओटीपी, पिन, सीवीवी या कार्ड नंबर किसी के साथ साझा न करें, हमारे साथ भी नहीं। हम कभी इसकी मांग नहीं करते।",
)
# To the AI (not the caller), after the warning: carry on where the call was.
SECRET_FOLLOW_UP = (
    "The caller just started to read out a one-time password, PIN, CVV or card number. It was removed and "
    "they have already been told not to share it. Don't repeat the warning and don't mention the code; in one "
    "short sentence carry on with the current step (for example, ask your question again)."
)
GOODBYE = ("Thank you for calling. Goodbye.", "कॉल करने के लिए धन्यवाद। नमस्ते।")


# ── Keywords ─────────────────────────────────────────────────────────────────


def _normal(text: str) -> str:
    return " ".join(re.sub(r"[^\w\s]", " ", text.lower()).split())


def keyword_in(words: list[str], text: str) -> bool:
    """Is one of the words or phrases in what was said? Whole words only, any
    case and punctuation ("O.T.P" doesn't need to match "OTP" exactly: list both)."""
    said = f" {_normal(text)} "
    return any(f" {w} " in said for w in (_normal(w) for w in words) if w)


# ── Promise to pay ───────────────────────────────────────────────────────────

PROMISE_PROMPT = """You check one thing a caller said on a loan helpline: did they promise to pay?
Today is {today}. The agent had just said: "{agent}"
A promise is a commitment to pay on or by a date ("I'll pay on the 10th", "Friday tak kar dunga",
"salary aate hi 5 tareekh ko"). A wish, a question or "I can't pay" is not a promise.
Answer with JSON only, no other text:
{{"promise": true or false, "date": "YYYY-MM-DD or empty", "amount": number or null}}"""

PROMISE_TIMEOUT = 8.0


def _json_object(text: str) -> dict | None:
    m = re.search(r"\{.*\}", text, re.DOTALL)
    if not m:
        return None
    try:
        data = json.loads(m.group())
    except ValueError:
        return None
    return data if isinstance(data, dict) else None


def _amount(value: Any) -> int | float | None:
    if isinstance(value, bool) or value is None:
        return None
    if isinstance(value, (int, float)):
        return value if value > 0 else None
    try:
        n = float(str(value).replace(",", "").replace("₹", "").strip())
    except ValueError:
        return None
    return (int(n) if n.is_integer() else n) if n > 0 else None


async def ask_llm(model: llm.LLM, system: str, user: str, timeout: float = PROMISE_TIMEOUT) -> str:
    """One short request to the LLM, the whole answer as text."""
    ctx = llm.ChatContext()
    ctx.add_message(role="system", content=system)
    ctx.add_message(role="user", content=user)

    async def run() -> str:
        out = []
        async with model.chat(chat_ctx=ctx) as stream:
            async for chunk in stream:
                if chunk.delta and chunk.delta.content:
                    out.append(chunk.delta.content)
        return "".join(out)

    return await asyncio.wait_for(run(), timeout)


# ── Time limit ───────────────────────────────────────────────────────────────

# To the AI: tell the caller, in their language and its own words.
TIME_WARNING = (
    "This call has to end in about {minutes} minute{s}. In one short, polite sentence tell the caller, and help "
    "them finish what they need. Don't mention it again."
)
TIME_UP = (
    "We've reached the time limit for this call. Thank you for calling, and please call again if you need more help. Goodbye.",
    "इस कॉल का समय पूरा हो गया है। कॉल करने के लिए धन्यवाद, और मदद चाहिए तो दोबारा कॉल करें। नमस्ते।",
)

# ── Off topic ────────────────────────────────────────────────────────────────

OFF_TOPIC_PROMPT = """You check one thing a caller said on a phone helpline: was it about the call?
The call is about: {topic}
Right now the agent is doing this: {step}
The agent had just said: "{agent}"
About the call: answering the agent; questions, details or worries about their account, loan, payment or
this call; greetings, thanks, "wait", "hello?", asking to repeat, short replies.
Off topic: chatting about unrelated things (sport, politics, films, jokes, other people's stories), or
going on about something that doesn't help the call. Give the caller the benefit of the doubt.
Answer with JSON only, no other text:
{{"off_topic": true or false}}"""

# To the AI: bring the caller back, briefly and politely.
STEER = (
    "The caller keeps talking about things that have nothing to do with this call. In one or two short, polite "
    "sentences bring them back: say what you can help with on this call, and ask your current question again. "
    "Don't lecture them."
)
OFF_TOPIC_END = (
    "I'm sorry, on this call we can only help with what it's for. Please call us again when you need that. Goodbye.",
    "माफ़ कीजिए, इस कॉल पर हम सिर्फ़ इसी काम में मदद कर सकते हैं। ज़रूरत हो तो दोबारा कॉल करें। नमस्ते।",
)


# ── The listeners of one call ────────────────────────────────────────────────

Report = Callable[[str, str, str, str], None]  # (listener, kind, action, node)


class Listeners:
    """The flow's listeners for one call. Reads the flow from the call state
    each time, so a flow without listeners costs nothing."""

    WARN_AGAIN_AFTER = 8.0  # seconds: one warning covers one attempt to read a code out
    SECONDS_PER_MINUTE = 60.0  # the time limit's clock (tests make it fast)
    GRACE_MINUTES = 3  # after a time limit's "go_to", the call still ends this much later

    def __init__(self, state: CallState) -> None:
        self.state = state
        self.session: AgentSession | None = None
        self._done: set[str] = set()  # keyword watches with "once" that already acted
        self._warned_at = -1e9
        self._follow_up_owed = False
        self._tasks: set[asyncio.Task] = set()
        self._off_topic_run = 0  # off-topic turns in a row

    # ── which listeners apply ──

    def _on(self, node_id: str) -> list:
        """The listeners on a step (a time limit has no steps: it's for the whole call)."""
        out = []
        for ls in self.state.flow.settings.listeners:
            steps = getattr(ls, "steps", None)
            if steps is not None and (not steps or node_id in steps):
                out.append(ls)
        return out

    def _current_node(self) -> str:
        return self.state.path[-1] if self.state.path else self.state.flow.start_node

    @property
    def _hindi(self) -> bool:
        return self.state.flow.settings.language.startswith("hi")

    def _report(self, ls, action: str, node_id: str) -> None:
        logger.info("listener acted", extra={"listener": ls.name, "kind": ls.kind, "action": action, "node": node_id})
        if self.state.on_listener:
            try:
                self.state.on_listener(ls.name, ls.kind, action, node_id)
            except Exception:
                logger.exception("listener report failed")

    def _spawn(self, coro: Awaitable[Any]) -> None:
        task = asyncio.ensure_future(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    # ── following the session ──

    def attach(self, session: AgentSession) -> None:
        """Follow the live transcript (secret guard) and what the caller said (promise to pay)."""
        self.session = session
        if not self.state.flow.settings.listeners:
            return
        session.on("user_input_transcribed", self._on_transcript)
        session.on("conversation_item_added", self._on_item)
        for ls in self.state.flow.settings.listeners:
            if isinstance(ls, TimeLimit):
                self._spawn(self._clock(ls))

    def _on_transcript(self, ev: Any) -> None:
        guard = self._guard(self._current_node())
        if guard and secret_in(ev.transcript or ""):
            self._warn(guard, self._current_node())

    def _on_item(self, ev: Any) -> None:
        item = getattr(ev, "item", None)
        if getattr(item, "type", None) != "message" or item.role != "user":
            return
        node_id = self._current_node()
        for ls in self._on(node_id):
            if isinstance(ls, PromiseToPay):
                self._spawn(self._check_promise(ls, item.text_content or "", node_id))
            elif isinstance(ls, OffTopic):
                self._spawn(self._check_topic(ls, item.text_content or "", node_id))

    # ── the end of the caller's turn ──

    async def on_turn(self, agent: Agent, node_id: str, message: llm.ChatMessage) -> None:
        """Called before the AI answers what the caller just said. Raises
        StopResponse when a listener takes over this turn."""
        listeners = self._on(node_id)
        if not listeners:
            return
        text = message.text_content or ""
        guard = next((ls for ls in listeners if isinstance(ls, SecretGuard)), None)
        if guard and secret_in(text):
            if not self._warn(guard, node_id) and self._follow_up_owed:
                self._follow_up_owed = False
                self._say_follow_up()
            raise StopResponse()  # the AI never gets it; the history never keeps it
        self._follow_up_owed = False
        for ls in listeners:
            if isinstance(ls, KeywordWatch) and not (ls.once and ls.name in self._done) and keyword_in(ls.words, text):
                if ls.once:
                    self._done.add(ls.name)
                await self._act(ls, agent, node_id, message)
                return

    # ── secret guard ──

    def _guard(self, node_id: str) -> SecretGuard | None:
        return next((ls for ls in self._on(node_id) if isinstance(ls, SecretGuard)), None)

    def _warn(self, guard: SecretGuard, node_id: str) -> bool:
        """Warn the caller now, unless they were just warned. True if warned."""
        now = time.monotonic()
        if now - self._warned_at < self.WARN_AGAIN_AFTER or self.session is None:
            return False
        self._warned_at = now
        self._report(guard, "warned", node_id)
        line = self.state.render(guard.say) if guard.say.strip() else SECRET_WARNING[self._hindi]
        # Not interruptible: the caller may still be talking. A turn that ends
        # while it plays is dropped by LiveKit, which is what we want here.
        handle = self.session.say(line, allow_interruptions=False)
        handle.add_done_callback(lambda _h: self._after_warning())
        return True

    def _after_warning(self) -> None:
        if self.session is None or self.session._is_closing():
            return
        if getattr(self.session, "user_state", None) == "speaking":
            self._follow_up_owed = True  # answered when their turn ends (on_turn)
        else:
            self._say_follow_up()

    def _say_follow_up(self) -> None:
        if self.session is not None and not self.session._is_closing():
            self.session.generate_reply(instructions=SECRET_FOLLOW_UP)

    # ── keyword watch ──

    async def _act(self, ls: KeywordWatch, agent: Agent, node_id: str, message: llm.ChatMessage) -> None:
        session = self.session
        if session is None:
            return
        line = self.state.render(ls.say).strip() if ls.say else ""
        if ls.then == "flag" or (ls.then == "go_to" and ls.to == node_id):
            self._report(ls, "flag", node_id)
            return  # the agent answers as usual
        self._report(ls, ls.then, node_id)
        if ls.then == "go_to":
            from .runtime import build_node_agent  # runtime imports this module

            # The next step hears the conversation so far, including what was just said.
            ctx = agent.chat_ctx.copy(exclude_instructions=True)
            ctx.items.append(message)
            self._keep(message)
            logger.info("flow transition", extra={"from": node_id, "to": ls.to, "listener": ls.name})
            if line:
                session.say(line)
            session.update_agent(build_node_agent(self.state, ls.to, ctx))  # type: ignore[arg-type]
            raise StopResponse()
        # "say" and "end_call": keep what the caller said in the conversation, then speak.
        ctx = agent.chat_ctx.copy()
        ctx.items.append(message)
        await agent.update_chat_ctx(ctx)
        self._keep(message)
        if ls.then == "say":
            session.say(line)
            raise StopResponse()
        handle = session.say(line or GOODBYE[self._hindi], allow_interruptions=False)
        self._spawn(self._hang_up_after(handle))
        raise StopResponse()

    def _keep(self, message: llm.ChatMessage) -> None:
        """A turn a listener took over still belongs to the conversation (call
        history counts it; a promise check hears it)."""
        add = getattr(self.session, "_conversation_item_added", None)
        if add:
            add(message)

    async def _hang_up_after(self, handle: Any) -> None:
        try:
            await handle.wait_for_playout()
        finally:
            if self.state.hang_up:
                await self.state.hang_up()

    # ── promise to pay ──

    async def _check_promise(self, ls: PromiseToPay, said: str, node_id: str) -> None:
        model = self.state.side_llm
        if model is None or not said.strip():
            return
        today = datetime.now(IST).strftime("%A, %d %B %Y")
        try:
            answer = await ask_llm(model, PROMISE_PROMPT.format(today=today, agent=self._agent_said()), mask_text(said))
        except Exception:  # a side check must never affect the call
            logger.warning("promise check failed", extra={"listener": ls.name, "node": node_id})
            return
        data = _json_object(answer)
        if not data or data.get("promise") is not True:
            return
        date = str(data.get("date") or "").strip()
        amount = _amount(data.get("amount"))
        if not date and amount is None:
            return
        if date:
            self.state.values[ls.save_date] = date
        if amount is not None:
            self.state.values[ls.save_amount] = amount
        self._report(ls, "saved", node_id)

    # ── time limit ──

    async def _clock(self, ls: TimeLimit) -> None:
        unit = self.SECONDS_PER_MINUTE
        if ls.warn_minutes:
            await asyncio.sleep((ls.minutes - ls.warn_minutes) * unit)
            self._time_warning(ls)
            await asyncio.sleep(ls.warn_minutes * unit)
        else:
            await asyncio.sleep(ls.minutes * unit)
        await self._time_up(ls)

    def _live(self) -> bool:
        return self.session is not None and not self.session._is_closing()

    def _time_warning(self, ls: TimeLimit) -> None:
        if not self._live():
            return
        self._report(ls, "time_warning", self._current_node())
        if ls.warn_say.strip():
            self.session.say(self.state.render(ls.warn_say))  # type: ignore[union-attr]
        else:
            n = ls.warn_minutes
            self.session.generate_reply(instructions=TIME_WARNING.format(minutes=n, s="" if n == 1 else "s"))  # type: ignore[union-attr]

    async def _time_up(self, ls: TimeLimit) -> None:
        if not self._live():
            return
        line = self.state.render(ls.say).strip() if ls.say else ""
        if ls.then == "go_to" and ls.to:
            self._go_to(ls, ls.to, line)
            await asyncio.sleep(self.GRACE_MINUTES * self.SECONDS_PER_MINUTE)
            if not self._live():
                return
            line = ""
        await self._end(ls, line or TIME_UP[self._hindi])

    # ── off topic ──

    async def _check_topic(self, ls: OffTopic, said: str, node_id: str) -> None:
        model = self.state.side_llm
        if model is None or not said.strip():
            return
        if len(said.split()) <= 2:  # "ok", "haan ji", "hello?": part of the call, no check needed
            self._off_topic_run = 0
            return
        flow = self.state.flow
        topic = ls.topic.strip() or f"{flow.name}. The agent: {flow.persona.strip()[:300]}"
        node = flow.nodes.get(node_id)
        step = (getattr(node, "instructions", "") or "").strip()[:300] or "calling one of the bank's systems"
        prompt = OFF_TOPIC_PROMPT.format(topic=topic, step=step, agent=self._agent_said())
        try:
            answer = await ask_llm(model, prompt, mask_text(said))
        except Exception:  # a side check must never affect the call
            logger.warning("off-topic check failed", extra={"listener": ls.name, "node": node_id})
            return
        data = _json_object(answer)
        if not data or data.get("off_topic") is not True:
            self._off_topic_run = 0
            return
        self._off_topic_run += 1
        run = self._off_topic_run
        if not self._live():
            return
        if ls.then_after and run >= ls.then_after:
            self._off_topic_run = 0
            line = self.state.render(ls.say).strip() if ls.say else ""
            if ls.then == "flag":
                self._report(ls, "flag", node_id)
            elif ls.then == "go_to" and ls.to:
                self._go_to(ls, ls.to, line)
            else:
                await self._end(ls, line or OFF_TOPIC_END[self._hindi])
        elif run % ls.steer_after == 0:
            self._report(ls, "steered", node_id)
            self.session.interrupt()  # type: ignore[union-attr]  # not an answer to the off-topic talk, then this
            self.session.generate_reply(instructions=STEER)  # type: ignore[union-attr]

    # ── shared: go to a step, end the call ──

    def _agent_said(self) -> str:
        if self.session is not None:
            for item in reversed(self.session.history.items):
                if item.type == "message" and item.role == "assistant":
                    return (item.text_content or "")[-300:]
        return ""

    def _go_to(self, ls, to: str, line: str) -> None:
        """Move the call to another step from outside a turn (time limit, off topic)."""
        from .runtime import build_node_agent  # runtime imports this module

        session = self.session
        if session is None or to not in self.state.flow.nodes:
            return
        node_id = self._current_node()
        self._report(ls, "go_to", node_id)
        logger.info("flow transition", extra={"from": node_id, "to": to, "listener": ls.name})
        session.interrupt()
        ctx = session.current_agent.chat_ctx.copy(exclude_instructions=True)
        if line:
            session.say(line)
        session.update_agent(build_node_agent(self.state, to, ctx))

    async def _end(self, ls, line: str) -> None:
        """Say a last line, then hang up."""
        session = self.session
        if session is None:
            return
        self._report(ls, "end_call", self._current_node())
        session.interrupt()
        handle = session.say(line, allow_interruptions=False)
        await self._hang_up_after(handle)

    async def aclose(self) -> None:
        for task in list(self._tasks):
            task.cancel()
