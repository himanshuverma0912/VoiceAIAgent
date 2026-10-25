package auth

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/auth/authtest"
)

func verifier(kc *authtest.Keycloak) *Verifier {
	return &Verifier{Issuer: authtest.Issuer, Audience: "config-api", JWKSURL: kc.JWKSURL()}
}

func asha(t *testing.T, kc *authtest.Keycloak, changes map[string]any) string {
	return kc.Token(t, "asha@demo.corp", []string{"viewer", "flow-editor"}, []string{"emi"}, changes)
}

func TestValidTokenGivesRolesAndTeams(t *testing.T) {
	kc := authtest.New(t)
	token := asha(t, kc, map[string]any{"groups": []string{"/teams/emi", "/other", "/teams/emi/sub"}})
	p, err := verifier(kc).Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Email != "asha@demo.corp" || strings.Join(p.Roles, ",") != "flow-editor,viewer" || strings.Join(p.Teams, ",") != "emi" {
		t.Fatalf("got %+v", p)
	}
	if !p.Can(RoleEditor, "emi") || p.Can(RolePublisher, "emi") || p.Can(RoleEditor, "complaints") || p.IsAdmin() {
		t.Fatalf("permissions wrong for %+v", p)
	}
}

func TestRejectedTokens(t *testing.T) {
	kc := authtest.New(t)
	other := authtest.New(t) // same kid, different key: a forged token
	cases := map[string]string{
		"wrong issuer":    asha(t, kc, map[string]any{"iss": "https://evil.example/auth/realms/voice-platform"}),
		"wrong audience":  asha(t, kc, map[string]any{"aud": "account"}),
		"no audience":     asha(t, kc, map[string]any{"aud": nil}),
		"expired":         asha(t, kc, map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()}),
		"no expiry":       asha(t, kc, map[string]any{"exp": nil}),
		"not yet valid":   asha(t, kc, map[string]any{"nbf": time.Now().Add(5 * time.Minute).Unix()}),
		"id token":        asha(t, kc, map[string]any{"typ": "ID"}),
		"forged":          asha(t, other, nil),
		"unknown key":     kc.Sign(t, map[string]any{"alg": "RS256", "kid": "nope"}, map[string]any{"iss": authtest.Issuer}),
		"alg none":        kc.Sign(t, map[string]any{"alg": "none", "kid": "key-1"}, map[string]any{"iss": authtest.Issuer}),
		"not a jwt":       "abc.def",
		"tampered claims": tamper(asha(t, kc, nil)),
	}
	v := verifier(kc)
	for name, token := range cases {
		if p, err := v.Verify(context.Background(), token); err == nil {
			t.Errorf("%s: accepted as %+v", name, p)
		}
	}
}

// tamper swaps the payload for one claiming platform-admin, keeping the signature.
func tamper(token string) string {
	parts := strings.Split(token, ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	s := strings.Replace(string(b), `"flow-editor"`, `"platform-admin"`, 1)
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(s)) + "." + parts[2]
}

func TestKeysAreCachedAndRefetchedForANewKey(t *testing.T) {
	kc := authtest.New(t)
	v := verifier(kc)
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(context.Background(), asha(t, kc, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if n := kc.Fetches.Load(); n != 1 {
		t.Fatalf("fetched keys %d times, want 1", n)
	}
	// Keycloak rotates its key; the first token with the new kid triggers a refetch.
	kc.Rotate(t, "key-2")
	v.fetchedAt = time.Now().Add(-time.Minute) // past the 30 s refetch guard
	if _, err := v.Verify(context.Background(), asha(t, kc, nil)); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
}

func TestScriptAdmin(t *testing.T) {
	p := ScriptAdmin()
	if !p.Can(RolePublisher, "any") || p.Actor() != "admin-token" {
		t.Fatalf("%+v", p)
	}
}
