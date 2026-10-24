"""Inbound/outbound voice agent.

Production start command (see Dockerfile):
    python -m livekit.agents start agent.py
"""

import os

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
        # Nova-3 Hindi also copes with Hinglish (Hindi mixed with English words).
        stt=deepgram.STT(model="nova-3", language="hi"),
        llm=openai.LLM(model="gpt-4o-mini", temperature=0),
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
