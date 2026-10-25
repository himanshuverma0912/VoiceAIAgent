// Package authtest is a fake Keycloak for tests: it signs access tokens and
// serves its public key the way Keycloak's JWKS endpoint does.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const Issuer = "https://studio.example/auth/realms/voice-platform"

type Keycloak struct {
	mu      sync.Mutex
	key     *rsa.PrivateKey
	kid     string
	Fetches atomic.Int32
	Server  *httptest.Server
}

func New(t testing.TB) *Keycloak {
	t.Helper()
	k := &Keycloak{}
	k.Rotate(t, "key-1")
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.Fetches.Add(1)
		k.mu.Lock()
		pub, kid := k.key.PublicKey, k.kid
		k.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{ //nolint:errcheck
			"kid": kid, "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	}))
	t.Cleanup(k.Server.Close)
	return k
}

// Rotate switches to a new signing key, as Keycloak does on key rotation.
func (k *Keycloak) Rotate(t testing.TB, kid string) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	k.key, k.kid = key, kid
	k.mu.Unlock()
}

func (k *Keycloak) JWKSURL() string { return k.Server.URL }

// Token is a signed access token for a user; changes override (nil deletes) claims.
func (k *Keycloak) Token(t testing.TB, email string, roles, teams []string, changes map[string]any) string {
	t.Helper()
	groups := []string{}
	for _, team := range teams {
		groups = append(groups, "/teams/"+team)
	}
	c := map[string]any{
		"iss": Issuer, "sub": "sub-" + email, "aud": []string{"config-api", "account"}, "typ": "Bearer",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(),
		"email": email, "name": email, "preferred_username": email,
		"realm_access": map[string]any{"roles": append([]string{"offline_access", "default-roles-voice-platform"}, roles...)},
		"groups":       groups,
	}
	for key, v := range changes {
		if v == nil {
			delete(c, key)
		} else {
			c[key] = v
		}
	}
	k.mu.Lock()
	kid := k.kid
	k.mu.Unlock()
	return k.Sign(t, map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"}, c)
}

// Sign signs any header and claims with the current key.
func (k *Keycloak) Sign(t testing.TB, header, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	unsigned := enc(header) + "." + enc(claims)
	digest := sha256.Sum256([]byte(unsigned))
	k.mu.Lock()
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, digest[:])
	k.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
}
