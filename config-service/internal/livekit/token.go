// Package livekit creates LiveKit access tokens, so Flow Studio can join a room
// from the browser with the voice agent dispatched into it.
//
// A token is a JWT signed (HS256) with the LiveKit API secret. Its claims are
// the same as the official server SDKs produce: "video" grants for the room,
// and "roomConfig.agents" to dispatch an agent by name when the room is created
// (https://docs.livekit.io/agents/server/agent-dispatch/).
package livekit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"time"
)

type Config struct {
	URL       string // wss://<project>.livekit.cloud, what the browser connects to
	APIKey    string
	APISecret string
	AgentName string // the agent's registered name, e.g. VoiceAIAgentInbound
}

func (c Config) Enabled() bool {
	return c.URL != "" && c.APIKey != "" && c.APISecret != "" && c.AgentName != ""
}

type videoGrant struct {
	RoomJoin       bool   `json:"roomJoin"`
	Room           string `json:"room"`
	CanPublish     bool   `json:"canPublish"`
	CanSubscribe   bool   `json:"canSubscribe"`
	CanPublishData bool   `json:"canPublishData"`
}

type agentDispatch struct {
	AgentName string `json:"agentName"`
	Metadata  string `json:"metadata"`
}

type roomConfig struct {
	EmptyTimeout     int             `json:"emptyTimeout"`
	DepartureTimeout int             `json:"departureTimeout"`
	MaxParticipants  int             `json:"maxParticipants"`
	Agents           []agentDispatch `json:"agents"`
}

type claims struct {
	Issuer     string     `json:"iss"`
	Subject    string     `json:"sub"`
	Name       string     `json:"name"`
	NotBefore  int64      `json:"nbf"`
	Expires    int64      `json:"exp"`
	Video      videoGrant `json:"video"`
	RoomConfig roomConfig `json:"roomConfig"`
}

// AgentRoomToken lets one person join room (microphone and speaker) and has
// LiveKit dispatch the agent with metadata when the room is first created.
// ttl only limits when they can join, not how long they stay.
func (c Config) AgentRoomToken(identity, name, room, metadata string, ttl time.Duration, now time.Time) (string, error) {
	body := claims{
		Issuer:    c.APIKey,
		Subject:   identity,
		Name:      name,
		NotBefore: now.Unix(),
		Expires:   now.Add(ttl).Unix(),
		Video:     videoGrant{RoomJoin: true, Room: room, CanPublish: true, CanSubscribe: true, CanPublishData: true},
		RoomConfig: roomConfig{
			EmptyTimeout:     60, // close the room if nobody joins within a minute
			DepartureTimeout: 10, // and soon after the person leaves
			MaxParticipants:  3,  // the person, the agent, and room for a reconnect
			Agents:           []agentDispatch{{AgentName: c.AgentName, Metadata: metadata}},
		},
	}
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(c.APISecret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil)), nil
}
