// "Talk to this draft": a voice call from the browser to the agent, running the
// flow's saved draft. The config service creates a private LiveKit room and has
// the agent dispatched into it; we join with the microphone.
//
// The transcript arrives on LiveKit's "lk.transcription" text streams (the
// agent sends both sides) and lives only in this panel: closing it forgets it.
import {
  ParticipantKind,
  Room,
  RoomEvent,
  Track,
  type RemoteParticipant,
  type RemoteTrack,
  type TextStreamReader,
} from "livekit-client";
import { useEffect, useRef, useState } from "react";
import { ApiError, api } from "../api";

type Phase = "starting" | "waiting" | "live" | "ended";
interface Line {
  id: string;
  who: "you" | "agent";
  text: string;
}

const STATE_LABEL: Record<string, string> = {
  initializing: "getting ready",
  listening: "listening",
  thinking: "thinking",
  speaking: "speaking",
};

const clock = (s: number) => `${Math.floor(s / 60)}:${String(Math.max(0, s) % 60).padStart(2, "0")}`;

export function TestCall(props: { flowId: string; onNode: (id: string | null) => void; onClose: () => void }) {
  const [phase, setPhase] = useState<Phase>("starting");
  const [error, setError] = useState<string | null>(null);
  const [agentState, setAgentState] = useState("");
  const [muted, setMuted] = useState(false);
  const [lines, setLines] = useState<Line[]>([]);
  const [left, setLeft] = useState<number | null>(null);
  const [blockedAudio, setBlockedAudio] = useState(false);
  const roomRef = useRef<Room | null>(null);
  const endsAt = useRef<number | null>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const onNode = useRef(props.onNode);
  onNode.current = props.onNode;

  useEffect(() => {
    let cancelled = false;
    const room = new Room({ adaptiveStream: true });
    roomRef.current = room;
    const audio: HTMLMediaElement[] = [];

    const maxSeconds = { current: null as number | null };
    const isAgent = (p: RemoteParticipant) => p.kind === ParticipantKind.AGENT;
    const readAgent = (p: RemoteParticipant) => {
      const a = p.attributes;
      if (a["lk.agent.state"]) setAgentState(a["lk.agent.state"]);
      if (a["flow.node"]) onNode.current(a["flow.node"]);
    };
    const agentJoined = (p: RemoteParticipant) => {
      if (!isAgent(p)) return;
      setPhase("live");
      endsAt.current ??= Date.now() + (maxSeconds.current ?? 300) * 1000;
      readAgent(p);
    };

    room
      .on(RoomEvent.TrackSubscribed, (track: RemoteTrack) => {
        if (track.kind !== Track.Kind.Audio) return;
        const el = track.attach();
        el.style.display = "none";
        document.body.appendChild(el);
        audio.push(el);
      })
      .on(RoomEvent.TrackUnsubscribed, (track: RemoteTrack) => {
        track.detach().forEach((el) => el.remove());
      })
      .on(RoomEvent.ParticipantConnected, agentJoined)
      .on(RoomEvent.ParticipantAttributesChanged, (_changed, p) => {
        if (p !== room.localParticipant && isAgent(p as RemoteParticipant)) readAgent(p as RemoteParticipant);
      })
      .on(RoomEvent.AudioPlaybackStatusChanged, () => setBlockedAudio(!room.canPlaybackAudio))
      .on(RoomEvent.Disconnected, () => {
        setPhase("ended");
        onNode.current(null);
      });

    room.registerTextStreamHandler("lk.transcription", async (reader: TextStreamReader, from: { identity: string }) => {
      const id = reader.info.attributes?.["lk.segment_id"] ?? reader.info.id;
      const who = from.identity === room.localParticipant.identity ? "you" : "agent";
      let text = "";
      const show = () =>
        setLines((ls) => {
          const i = ls.findIndex((l) => l.id === id);
          if (i < 0) return [...ls, { id, who, text }];
          const copy = ls.slice();
          copy[i] = { ...copy[i], text };
          return copy;
        });
      for await (const chunk of reader) {
        text += chunk;
        show();
      }
    });

    (async () => {
      try {
        const session = await api.startTest(props.flowId);
        if (cancelled) return;
        maxSeconds.current = session.max_seconds;
        await room.connect(session.url, session.token);
        if (cancelled) return room.disconnect();
        setPhase("waiting");
        try {
          await room.localParticipant.setMicrophoneEnabled(true);
        } catch {
          throw new Error("Flow Studio needs your microphone. Allow it in the browser's address bar, then try again.");
        }
        await room.startAudio();
        room.remoteParticipants.forEach(agentJoined);
      } catch (e) {
        if (cancelled) return;
        setError(e instanceof ApiError && e.problems.length ? "Fix the draft's problems first." : (e as Error).message);
        setPhase("ended");
        room.disconnect();
      }
    })();

    return () => {
      cancelled = true;
      room.unregisterTextStreamHandler("lk.transcription");
      room.disconnect();
      audio.forEach((el) => el.remove());
      onNode.current(null);
    };
  }, [props.flowId]);

  // Countdown; the agent also ends the call at the limit.
  useEffect(() => {
    if (phase !== "live") return;
    const t = setInterval(() => {
      if (endsAt.current === null) return;
      const s = Math.round((endsAt.current - Date.now()) / 1000);
      setLeft(s);
      if (s <= 0) roomRef.current?.disconnect();
    }, 500);
    return () => clearInterval(t);
  }, [phase]);

  useEffect(() => {
    scroller.current?.scrollTo({ top: scroller.current.scrollHeight });
  }, [lines]);

  const toggleMute = async () => {
    const room = roomRef.current;
    if (!room) return;
    await room.localParticipant.setMicrophoneEnabled(muted);
    setMuted(!muted);
  };

  const status =
    phase === "starting"
      ? "Starting…"
      : phase === "waiting"
        ? "Waiting for the agent…"
        : phase === "live"
          ? `Agent ${STATE_LABEL[agentState] ?? "connected"}`
          : error
            ? "Couldn't start"
            : left !== null && left <= 0
              ? "Ended: 5-minute limit"
              : "Call ended";

  return (
    <div className="test-call" role="region" aria-label="Test call">
      <div className="test-call-head">
        <span className={`dot ${phase === "live" ? agentState || "live" : phase}`} />
        <strong className="grow">{status}</strong>
        {phase === "live" && left !== null && <span className="mono muted">{clock(left)}</span>}
        <button className="icon" aria-label="Close" onClick={props.onClose}>
          ✕
        </button>
      </div>
      {error && <div className="notice bad" style={{ margin: "8px 12px" }}>{error}</div>}
      {blockedAudio && (
        <div className="notice warn" style={{ margin: "8px 12px" }}>
          The browser blocked the agent's audio.{" "}
          <button className="small" onClick={() => roomRef.current?.startAudio()}>
            Play audio
          </button>
        </div>
      )}
      <div className="transcript" ref={scroller}>
        {lines.length === 0 && !error && (
          <p className="muted" style={{ margin: 0 }}>
            {phase === "live" ? "Say something. The conversation appears here." : "The agent joins in a few seconds."}
          </p>
        )}
        {lines.map((l) => (
          <div key={l.id} className={`line ${l.who}`}>
            <span className="who">{l.who === "you" ? "You" : "Agent"}</span>
            <span>{l.text}</span>
          </div>
        ))}
      </div>
      <div className="test-call-foot">
        <span className="hint grow" style={{ margin: 0 }}>
          Runs your saved draft. Nothing is recorded or kept.
        </span>
        {phase !== "ended" ? (
          <>
            <button onClick={toggleMute} disabled={phase !== "live"}>
              {muted ? "Unmute" : "Mute"}
            </button>
            <button className="danger" onClick={() => roomRef.current?.disconnect()}>
              Hang up
            </button>
          </>
        ) : (
          <button onClick={props.onClose}>Close</button>
        )}
      </div>
    </div>
  );
}
