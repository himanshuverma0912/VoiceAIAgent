"""Inbound/outbound voice agent: runs a conversation flow from flows/.

The conversation itself (nodes, prompts, what to collect, how to end) lives in
a flow file; this file only sets up the call and the speech/LLM models.
FLOW_FILE in .env picks the flow (default: flows/complaint-helpline.json).

Production start command (see Dockerfile):
    python -m livekit.agents start agent.py
"""

import logging
import os

import aiohttp
from dotenv import load_dotenv

load_dotenv(".env")

from livekit import agents, rtc
from livekit.agents import (
    AgentServer,
    AgentSession,
    TurnHandlingOptions,
    inference,
    room_io,
    utils,
)
from livekit.plugins import cartesia, deepgram, noise_cancellation, openai

from flow_engine import Flow, load_flow
from flow_engine.runtime import CallState, start_agent

logger = logging.getLogger("voice-agent")

# Same dispatch name as before, so the existing SIP dispatch rule
# and outbound dispatches keep reaching this agent.
AGENT_NAME = "VoiceAIAgentInbound"


def _env(name: str, default: str | None = None) -> str | None:
    """Read a setting from the environment, ignoring stray spaces or a Windows
    line ending pasted into .env (a trailing space in an API key makes every
    request fail with "Illegal header value")."""
    value = os.getenv(name)
    value = value.strip() if value is not None else None
    return value or default


# The flow this agent runs. It's loaded and checked when the container starts, so
# a broken flow file stops the deploy (the old container keeps taking calls).
FLOW_FILE = _env("FLOW_FILE", "flows/complaint-helpline.json")
FLOW: Flow = load_flow(FLOW_FILE)

# Cartesia voice used when the flow doesn't set settings.tts_voice_id.
CARTESIA_VOICE_ID = _env("CARTESIA_VOICE_ID", "9626c31c-bec5-4cca-baa8-f8ba9e84c8bc")

# LiteLLM gateway. When LITELLM_URL is set (e.g. http://litellm:4000), the LLM and
# Deepgram speech-to-text both go through LiteLLM with one LiteLLM key, so usage
# and cost are tracked in one place:
#   LLM -> {LITELLM_URL}/v1 using a model alias (e.g. "voice-fast"); which real
#          model sits behind the alias is set in infra/litellm-config.yaml.
#   STT -> {LITELLM_URL}/deepgram/v1/listen, LiteLLM's streaming Deepgram pass-through.
# Cartesia text-to-speech is not supported by LiteLLM, so it stays direct.
# When LITELLM_URL isn't set, the agent calls OpenAI and Deepgram directly as
# before (handy for rollback).
LITELLM_URL = (_env("LITELLM_URL") or "").rstrip("/") or None


def _litellm_key() -> str:
    key = _env("LITELLM_API_KEY")
    if not key:
        raise RuntimeError("LITELLM_URL is set but LITELLM_API_KEY is missing from .env")
    return key


class _BearerAuthSession:
    """Wraps an aiohttp session for the Deepgram plugin when it talks to LiteLLM.

    The Deepgram plugin always sends "Authorization: Token <key>" (Deepgram's own
    format), but LiteLLM only accepts "Authorization: Bearer <key>". This rewrites
    that one header and passes everything else straight through.
    """

    def __init__(self, session: aiohttp.ClientSession) -> None:
        self._session = session

    @staticmethod
    def _bearer(headers: dict | None) -> dict:
        headers = dict(headers or {})
        auth = headers.get("Authorization", "")
        if auth.startswith("Token "):
            headers["Authorization"] = "Bearer " + auth[len("Token ") :]
        return headers

    def ws_connect(self, url, *, headers=None, **kwargs):
        return self._session.ws_connect(url, headers=self._bearer(headers), **kwargs)

    def post(self, url, *, headers=None, **kwargs):
        return self._session.post(url, headers=self._bearer(headers), **kwargs)

    def __getattr__(self, name):
        return getattr(self._session, name)


def build_llm(flow: Flow) -> openai.LLM:
    s = flow.settings
    if LITELLM_URL:
        # The flow's model alias wins; LLM_MODEL in .env is the fallback.
        model = s.llm_model or _env("LLM_MODEL", "voice-fast")
        return openai.LLM(
            model=model, base_url=f"{LITELLM_URL}/v1", api_key=_litellm_key(), temperature=s.llm_temperature
        )
    # Without LiteLLM, model aliases don't exist; use a real OpenAI model name.
    return openai.LLM(model=_env("LLM_MODEL", "gpt-4o-mini"), temperature=s.llm_temperature)


def build_stt(flow: Flow) -> deepgram.STT:
    # Nova-3 Hindi also copes with Hinglish (Hindi mixed with English words).
    language = flow.settings.language
    if LITELLM_URL:
        return deepgram.STT(
            model="nova-3",
            language=language,
            base_url=f"{LITELLM_URL}/deepgram/v1/listen",
            api_key=_litellm_key(),
            http_session=_BearerAuthSession(utils.http_context.http_session()),
        )
    return deepgram.STT(model="nova-3", language=language)


def build_tts(flow: Flow) -> cartesia.TTS:
    s = flow.settings
    # Cartesia takes a two-letter language; for multilingual flows speak Hindi by default.
    language = "hi" if s.language == "multi" else s.language[:2]
    # word_timestamps=False: Cartesia only returns word timings for en/de/es/fr,
    # and a phone call doesn't need them.
    return cartesia.TTS(
        model="sonic-3", voice=s.tts_voice_id or CARTESIA_VOICE_ID, language=language, word_timestamps=False
    )


# drain_timeout: when Docker sends SIGTERM during a deploy, the server stops
# accepting new calls and lets live calls finish for up to 900 s (15 min).
# deploy.yml waits the same 900 s before force-stopping the old container.
server = AgentServer(drain_timeout=900)


@server.rtc_session(agent_name=AGENT_NAME)
async def entrypoint(ctx: agents.JobContext) -> None:
    flow = FLOW

    # Wait for the caller (the SIP participant) to read their phone numbers.
    await ctx.connect()
    caller = await ctx.wait_for_participant()
    attrs = caller.attributes or {}
    state = CallState(
        flow=flow,
        caller_number=attrs.get("sip.phoneNumber", ""),
        dialed_number=attrs.get("sip.trunkPhoneNumber", ""),
    )
    logger.info("call started", extra={"flow_id": flow.flow_id})

    session = AgentSession(
        stt=build_stt(flow),
        llm=build_llm(flow),
        tts=build_tts(flow),
        userdata=state,
        turn_handling=TurnHandlingOptions(
            # Audio-based end-of-turn model: listens to how the caller speaks, not just
            # for silence. "v1-mini" runs inside this container (supports Hindi), so it
            # costs nothing for an agent hosted on EC2.
            turn_detection=inference.TurnDetector(version="v1-mini"),
            endpointing={
                # Wait after the caller sounds finished. 0.6 s (not 0.3 s) because
                # speech-to-text goes through LiteLLM, and on real calls the transcript
                # sometimes arrived after the turn was already committed, so the LLM
                # answered half a sentence ("transcript arrives after turn has been
                # committed" in the logs).
                "min_delay": 0.6,
                "max_delay": 2.5,  # longest wait when the caller sounds mid-sentence
            },
            interruption={
                # "adaptive" mode is billed for self-hosted agents, so use VAD-based
                # interruptions. The caller must speak for 0.5 s to interrupt, and
                # if it was only noise the agent resumes what it was saying.
                "mode": "vad",
                "min_duration": 0.5,
                "resume_false_interruption": True,
            },
        ),
    )

    # The flow's start node becomes the first agent; it plays the flow's greeting.
    await session.start(
        room=ctx.room,
        agent=start_agent(state),
        # Don't upload this call's audio, transcript, traces or logs to LiveKit Cloud.
        record=False,
        room_options=room_io.RoomOptions(
            participant_identity=caller.identity,
            # When the caller hangs up the session closes; deleting the room then
            # ends this job instead of leaving it running.
            delete_room_on_close=True,
            audio_input=room_io.AudioInputOptions(
                noise_cancellation=lambda params: (
                    noise_cancellation.BVCTelephony()
                    if params.participant.kind == rtc.ParticipantKind.PARTICIPANT_KIND_SIP
                    else noise_cancellation.BVC()
                ),
            ),
        ),
    )
