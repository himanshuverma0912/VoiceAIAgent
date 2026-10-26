"""Inbound/outbound voice agent: runs a conversation flow.

The conversation itself (nodes, prompts, what to collect, how to end) is a flow;
this file only sets up the call and the speech/LLM models.

Which flow: at the start of each call the agent asks the config service which
flow the dialled number runs (CONFIG_SERVICE_URL in .env), so a published change
applies from the next call. Without the config service, or when it can't answer,
FLOW_FILE in .env is used (default: flows/complaint-helpline.json).

Test calls from Flow Studio ("Talk to this draft") come from a browser instead
of a phone: the job metadata says {"kind": "studio_test", "flow_id": ...}, and
the agent runs that flow's current draft for at most 5 minutes.

Production start command (see Dockerfile):
    python -m livekit.agents start agent.py
"""

import asyncio
import json
import logging
import os

import log_privacy

# First thing: mask phone numbers and LiveKit's "lk.pii.*" fields in every log line
# (ours and LiveKit's), so no personal data ends up in the container logs.
log_privacy.install()

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
from flow_engine.calllog import CallRecorder, send_record
from flow_engine.language import LanguageHint
from flow_engine.runtime import CallState, default_end_call, start_agent
from flow_engine.spec import EndNode
from flow_engine.source import FlowSource

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


# The fallback flow. It's loaded and checked when the container starts, so a
# broken flow file stops the deploy (the old container keeps taking calls).
FLOW_FILE = _env("FLOW_FILE", "flows/complaint-helpline.json")
FALLBACK_FLOW: Flow = load_flow(FLOW_FILE)

# The config service (e.g. http://config-service:8080) and its runtime token.
# Not set: every call runs FLOW_FILE, as before.
FLOWS = FlowSource(
    fallback=FALLBACK_FLOW,
    url=_env("CONFIG_SERVICE_URL"),
    token=_env("CONFIG_SERVICE_TOKEN"),
    timeout=float(_env("CONFIG_SERVICE_TIMEOUT", "2")),
    # Flow configuration only (no call data); lets a fresh job process use the
    # last known flow if the config service is down.
    cache_dir=_env("FLOW_CACHE_DIR", "/tmp/flow-cache"),
)

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


class FlowLLM(openai.LLM):
    """The OpenAI-compatible LLM, plus a line before every request saying which
    language to answer in, worked out from the caller's latest words (see
    flow_engine/language.py). Covers every step, including LiveKit's built-in
    questions, because they all go through this one LLM."""

    def __init__(self, *, flow_language: str, **kwargs) -> None:
        super().__init__(**kwargs)
        self._language = LanguageHint(flow_language)

    def chat(self, *, chat_ctx, **kwargs):
        return super().chat(chat_ctx=self._language.apply(chat_ctx), **kwargs)


def build_llm(flow: Flow) -> openai.LLM:
    s = flow.settings
    if LITELLM_URL:
        # The flow's model alias wins; LLM_MODEL in .env is the fallback.
        model = s.llm_model or _env("LLM_MODEL", "voice-fast")
        return FlowLLM(
            flow_language=s.language,
            model=model,
            base_url=f"{LITELLM_URL}/v1",
            api_key=_litellm_key(),
            temperature=s.llm_temperature,
        )
    # Without LiteLLM, model aliases don't exist; use a real OpenAI model name.
    return FlowLLM(flow_language=s.language, model=_env("LLM_MODEL", "gpt-4o-mini"), temperature=s.llm_temperature)


def build_stt(flow: Flow) -> deepgram.STT:
    # The flow's Deepgram model (chosen from the model catalog in Flow Studio);
    # nova-3 by default. Nova-3 Hindi also copes with Hinglish.
    model = flow.settings.stt_model or "nova-3"
    language = flow.settings.language
    if LITELLM_URL:
        return deepgram.STT(
            model=model,
            language=language,
            base_url=f"{LITELLM_URL}/deepgram/v1/listen",
            api_key=_litellm_key(),
            http_session=_BearerAuthSession(utils.http_context.http_session()),
        )
    return deepgram.STT(model=model, language=language)


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


def _job_metadata(ctx: agents.JobContext) -> dict:
    """The dispatch metadata ({} for phone calls)."""
    try:
        meta = json.loads(ctx.job.metadata or "{}")
    except ValueError:
        return {}
    return meta if isinstance(meta, dict) else {}


# Flow Studio test calls end after this long, whatever is happening.
STUDIO_TEST_MAX_SECONDS = 300


def _report_steps(room: rtc.Room):
    """Publish the current step as the agent's "flow.node" attribute, so Flow
    Studio can highlight it on the canvas during a test call."""
    pending: set[asyncio.Task] = set()

    def report(node_id: str) -> None:
        if room.isconnected():
            task = asyncio.create_task(room.local_participant.set_attributes({"flow.node": node_id}))
            pending.add(task)
            task.add_done_callback(pending.discard)

    return report


def _both(*hooks):
    def call(node_id: str) -> None:
        for hook in hooks:
            hook(node_id)

    return call


@server.rtc_session(agent_name=AGENT_NAME)
async def entrypoint(ctx: agents.JobContext) -> None:
    meta = _job_metadata(ctx)
    studio_test = meta.get("kind") == "studio_test"

    # Wait for the caller (the SIP participant, or the browser in a test call).
    await ctx.connect()
    caller = await ctx.wait_for_participant()
    attrs = caller.attributes or {}
    http = utils.http_context.http_session()

    if studio_test:
        resolved = await FLOWS.draft_for(str(meta.get("flow_id", "")), http)
        if resolved is None:
            logger.error("Flow Studio test call: the draft couldn't be loaded; ending it")
            await ctx.delete_room()
            return
        caller_number = dialed_number = ""
    else:
        # Our Vobiz number: the one dialled on inbound calls, the one we call from
        # on outbound calls. It decides which flow runs.
        dialed_number = attrs.get("sip.trunkPhoneNumber", "")
        caller_number = attrs.get("sip.phoneNumber", "")
        resolved = await FLOWS.flow_for(dialed_number, http)
    flow = resolved.flow

    # Call history: counts and timings only (no number, no words, no values);
    # sent to the config service when the call ends. See flow_engine/calllog.py.
    recorder = CallRecorder(
        call_id=ctx.job.id,  # never the room name: for phone calls it contains the caller's number
        channel="studio_test" if studio_test else "phone",
        flow_id=flow.flow_id,
        flow_version=resolved.version,
        flow_source=resolved.source,
        end_node_ids={k for k, n in flow.nodes.items() if isinstance(n, EndNode)},
    )

    state = CallState(
        flow=flow,
        caller_number=caller_number,
        dialed_number=dialed_number,
        on_node=_both(_report_steps(ctx.room), recorder.node_entered),
        end_call=lambda goodbye, desc: default_end_call(goodbye, desc, on_called=recorder.agent_ended_call),
    )
    logger.info(
        "call started",
        extra={
            "flow_id": flow.flow_id,
            "flow_version": resolved.version,
            "flow_source": resolved.source,
            "draft_checksum": resolved.checksum,
        },
    )

    if studio_test:
        limit = min(int(meta.get("max_seconds") or STUDIO_TEST_MAX_SECONDS), STUDIO_TEST_MAX_SECONDS)

        async def end_at_time_limit() -> None:
            await asyncio.sleep(limit)
            logger.info("Flow Studio test call reached its time limit", extra={"seconds": limit})
            recorder.time_limit_reached()
            await ctx.delete_room()

        timer = asyncio.create_task(end_at_time_limit())

        async def stop_timer() -> None:
            timer.cancel()

        ctx.add_shutdown_callback(stop_timer)

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

    recorder.attach(session)

    async def send_call_record() -> None:
        await send_record(_env("CONFIG_SERVICE_URL"), _env("CONFIG_SERVICE_TOKEN"), recorder.record(session.usage))

    ctx.add_shutdown_callback(send_call_record)

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
