from dotenv import load_dotenv
load_dotenv(".env")

import asyncio
from livekit import agents, rtc
from livekit.agents import AgentSession, Agent
from livekit.plugins import silero, deepgram, openai, noise_cancellation, cartesia
from prompts import AGENT_INSTRUCTION

class Assistant(Agent):
    def __init__(self):
        super().__init__(instructions=AGENT_INSTRUCTION)

async def entrypoint(ctx: agents.JobContext):

    session = AgentSession(
        stt=deepgram.STT(model="nova-2", language="hi"),
        llm=openai.LLM(model="gpt-4o-mini", temperature=0),
        tts="cartesia/sonic-3:9626c31c-bec5-4cca-baa8-f8ba9e84c8bc",
        vad=silero.VAD.load(
            activation_threshold=0.6,
            min_speech_duration=0.12,
            min_silence_duration=0.18,
            prefix_padding_duration=0.2,
        ),
        turn_detection="vad",
        min_endpointing_delay=0.1,
        max_endpointing_delay=2.0,
        allow_interruptions=True,
    )

    await session.start(
        room=ctx.room,
        agent=Assistant(),
        room_options=agents.room_io.RoomOptions(
            close_on_disconnect=False,
            audio_input=agents.room_io.AudioInputOptions(
                noise_cancellation=lambda params:
                    noise_cancellation.BVCTelephony()
                    if params.participant.kind
                    == rtc.ParticipantKind.PARTICIPANT_KIND_SIP
                    else noise_cancellation.BVC()
            ),
        ),
    )

    # 🔊 FIRST AUDIO MUST BE AUDIBLE
    await session.say(
        "Hello. You have reached the Complaint Department Helpline. "
        "Please tell us your issue in detail.",
        allow_interruptions=False,
    )

    while True:
        await asyncio.sleep(1)

if __name__ == "__main__":
    agents.cli.run_app(
        agents.WorkerOptions(
            agent_name="VoiceAIAgentInbound",
            entrypoint_fnc=entrypoint)
    )