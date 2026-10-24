"""Inbound/outbound voice agent.

Production start command (see Dockerfile):
    python -m livekit.agents start agent.py
"""

import os

import aiohttp
from dotenv import load_dotenv

load_dotenv(".env")

from livekit import agents, rtc
from livekit.agents import (
    Agent,
    AgentServer,
    AgentSession,
    TurnHandlingOptions,
    inference,
    room_io,
    utils,
)
from livekit.agents.beta.tools import EndCallTool
from livekit.plugins import cartesia, deepgram, noise_cancellation, openai

from prompts import GREETING, build_instructions

# Same dispatch name as before, so the existing SIP dispatch rule
# and outbound dispatches keep reaching this agent.
AGENT_NAME = "VoiceAIAgentInbound"

# Cartesia voice used for replies. Set CARTESIA_VOICE_ID in .env to a Hindi
# voice from the Cartesia voice library; the default is the voice used before.
CARTESIA_VOICE_ID = os.getenv("CARTESIA_VOICE_ID", "9626c31c-bec5-4cca-baa8-f8ba9e84c8bc")

# LiteLLM gateway. When LITELLM_URL is set (e.g. http://litellm:4000), the LLM and
# Deepgram speech-to-text both go through LiteLLM with one LiteLLM key, so usage
# and cost are tracked in one place:
#   LLM -> {LITELLM_URL}/v1 using a model alias (e.g. "voice-fast"); which real
#          model sits behind the alias is set in infra/litellm-config.yaml.
#   STT -> {LITELLM_URL}/deepgram/v1/listen, LiteLLM's streaming Deepgram pass-through.
# Cartesia text-to-speech is not supported by LiteLLM, so it stays direct.
# When LITELLM_URL isn't set, the agent calls OpenAI and Deepgram directly as
# before (handy for rollback).
LITELLM_URL = (os.getenv("LITELLM_URL") or "").rstrip("/") or None
LLM_MODEL = os.getenv("LLM_MODEL", "voice-fast" if LITELLM_URL else "gpt-4o-mini")


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


def build_llm() -> openai.LLM:
    if LITELLM_URL:
        return openai.LLM(
            model=LLM_MODEL,
            base_url=f"{LITELLM_URL}/v1",
            api_key=os.environ["LITELLM_API_KEY"],
            temperature=0,
        )
    return openai.LLM(model=LLM_MODEL, temperature=0)


def build_stt() -> deepgram.STT:
    # Nova-3 Hindi also copes with Hinglish (Hindi mixed with English words).
    if LITELLM_URL:
        return deepgram.STT(
            model="nova-3",
            language="hi",
            base_url=f"{LITELLM_URL}/deepgram/v1/listen",
            api_key=os.environ["LITELLM_API_KEY"],
            http_session=_BearerAuthSession(utils.http_context.http_session()),
        )
    return deepgram.STT(model="nova-3", language="hi")


class Assistant(Agent):
    def __init__(self) -> None:
        # Lets the agent say goodbye and hang up by itself. delete_room=True
        # closes the LiveKit room, which disconnects the phone call.
        end_call = EndCallTool(
            extra_description=(
                "Use it after the complaint has been registered and the caller has "
                "nothing more to add, or when the caller clearly wants to end the call."
            ),
            delete_room=True,
            end_instructions=(
                "Thank the caller briefly, in the language they are speaking, and say goodbye."
            ),
        )
        super().__init__(
            # Built when the call starts, so the IST time in the prompt is current.
            instructions=build_instructions(),
            tools=end_call.tools,
        )


# drain_timeout: when Docker sends SIGTERM during a deploy, the server stops
# accepting new calls and lets live calls finish for up to 900 s (15 min).
# deploy.yml waits the same 900 s before force-stopping the old container.
server = AgentServer(drain_timeout=900)


@server.rtc_session(agent_name=AGENT_NAME)
async def entrypoint(ctx: agents.JobContext) -> None:
    session = AgentSession(
        stt=build_stt(),
        llm=build_llm(),
        # Your own Cartesia key (CARTESIA_API_KEY) instead of LiveKit Inference credits.
        # word_timestamps=False: Cartesia only returns word timings for en/de/es/fr,
        # and a phone call doesn't need them.
        tts=cartesia.TTS(
            model="sonic-3", voice=CARTESIA_VOICE_ID, language="hi", word_timestamps=False
        ),
        turn_handling=TurnHandlingOptions(
            # Audio-based end-of-turn model: listens to how the caller speaks, not just
            # for silence. "v1-mini" runs inside this container (supports Hindi), so it
            # costs nothing for an agent hosted on EC2.
            turn_detection=inference.TurnDetector(version="v1-mini"),
            endpointing={
                "min_delay": 0.3,  # wait after the caller sounds finished
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

    await session.start(
        room=ctx.room,
        agent=Assistant(),
        # Don't upload this call's audio, transcript, traces or logs to LiveKit Cloud.
        record=False,
        room_options=room_io.RoomOptions(
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

    # The caller can't interrupt the greeting, so they always hear it fully.
    await session.say(GREETING, allow_interruptions=False)
