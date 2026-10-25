package livekit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTokenMatchesLiveKitSDKShape(t *testing.T) {
	c := Config{URL: "wss://x.livekit.cloud", APIKey: "devkey", APISecret: "secret", AgentName: "VoiceAIAgentInbound"}
	now := time.Unix(1_800_000_000, 0)
	tok, err := c.AgentRoomToken("studio-abc", "asha@demo.corp", "studio-test-x", `{"kind":"studio_test"}`, 10*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %s", tok)
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) != parts[2] {
		t.Fatal("signature doesn't verify with the API secret")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var got map[string]any
	json.Unmarshal(payload, &got) //nolint:errcheck
	// The same claim names livekit-server-sdk 2.19 writes.
	agents := got["roomConfig"].(map[string]any)["agents"].([]any)
	video := got["video"].(map[string]any)
	if got["iss"] != "devkey" || got["sub"] != "studio-abc" || got["exp"].(float64) != float64(now.Add(10*time.Minute).Unix()) ||
		video["room"] != "studio-test-x" || video["roomJoin"] != true ||
		agents[0].(map[string]any)["agentName"] != "VoiceAIAgentInbound" || agents[0].(map[string]any)["metadata"] != `{"kind":"studio_test"}` {
		t.Fatalf("claims: %s", payload)
	}
	if (Config{URL: "x"}).Enabled() || !c.Enabled() {
		t.Fatal("Enabled() wrong")
	}
}
