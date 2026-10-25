// Package auth works out who is calling the config service.
//
// People sign in to Flow Studio through Keycloak, which hands the browser an
// access token (a JWT signed by Keycloak). This package checks that token:
// the signature against Keycloak's published keys (JWKS), the issuer, the
// audience ("config-api"), and that it hasn't expired. Then it reads the
// person's roles and teams from it.
//
// Scripts and CI keep using ADMIN_TOKEN, which acts as a platform admin.
package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Roles, as defined in Keycloak's voice-platform realm.
const (
	RoleViewer        = "viewer"
	RoleEditor        = "flow-editor"
	RolePublisher     = "flow-publisher"
	RolePlatformAdmin = "platform-admin"
)

// Principal is the person (or script) making a request.
type Principal struct {
	Subject string   `json:"subject"`
	Email   string   `json:"email"`
	Name    string   `json:"name"`
	Roles   []string `json:"roles"` // only the platform's roles, sorted
	Teams   []string `json:"teams"` // from Keycloak groups /teams/<name>, sorted
	Script  bool     `json:"script"`
}

// ScriptAdmin is who ADMIN_TOKEN requests run as.
func ScriptAdmin() Principal {
	return Principal{Subject: "admin-token", Name: "admin token (scripts)", Roles: []string{RolePlatformAdmin}, Teams: []string{}, Script: true}
}

func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func (p Principal) IsAdmin() bool { return p.HasRole(RolePlatformAdmin) }

func (p Principal) InTeam(team string) bool {
	for _, t := range p.Teams {
		if t == team {
			return true
		}
	}
	return false
}

// Can reports whether p may act with role on a flow owned by team.
// Platform admins can do everything; others need the role and the team.
func (p Principal) Can(role, team string) bool {
	if p.IsAdmin() {
		return true
	}
	return team != "" && p.HasRole(role) && p.InTeam(team)
}

// Actor is how p appears in the audit log and in "published by".
func (p Principal) Actor() string {
	if p.Email != "" {
		return p.Email
	}
	return p.Subject
}

// Verifier checks Keycloak access tokens.
type Verifier struct {
	Issuer   string // e.g. https://voiceflow.duckdns.org/auth/realms/voice-platform (as in the token)
	Audience string // "config-api"
	JWKSURL  string // where to fetch keys; may be an internal URL (http://keycloak:8080/...)
	Client   *http.Client
	Now      func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

var errToken = errors.New("invalid token")

const (
	leeway       = 30 * time.Second // clock difference allowed between Keycloak and us
	refreshEvery = 10 * time.Minute // re-read keys this often (Keycloak rotates them)
	refetchAfter = 30 * time.Second // at most this often for an unknown key id
)

type claims struct {
	Issuer      string          `json:"iss"`
	Subject     string          `json:"sub"`
	Audience    json.RawMessage `json:"aud"`
	Expires     *float64        `json:"exp"`
	NotBefore   *float64        `json:"nbf"`
	Type        string          `json:"typ"`
	Email       string          `json:"email"`
	Name        string          `json:"name"`
	Username    string          `json:"preferred_username"`
	Groups      []string        `json:"groups"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Verify checks a raw JWT and returns who it belongs to.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Principal{}, errToken
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "RS256" {
		return Principal{}, errToken
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return Principal{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, errToken
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return Principal{}, errToken
	}

	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Principal{}, errToken
	}
	now := v.now()
	switch {
	case c.Issuer != v.Issuer:
		return Principal{}, fmt.Errorf("%w: wrong issuer", errToken)
	case !hasAudience(c.Audience, v.Audience):
		return Principal{}, fmt.Errorf("%w: not meant for %s", errToken, v.Audience)
	case c.Expires == nil || now.After(unix(*c.Expires).Add(leeway)):
		return Principal{}, fmt.Errorf("%w: expired", errToken)
	case c.NotBefore != nil && now.Add(leeway).Before(unix(*c.NotBefore)):
		return Principal{}, fmt.Errorf("%w: not valid yet", errToken)
	case c.Type != "" && c.Type != "Bearer": // reject ID tokens and refresh tokens
		return Principal{}, fmt.Errorf("%w: not an access token", errToken)
	}

	p := Principal{Subject: c.Subject, Email: c.Email, Name: c.Name, Roles: []string{}, Teams: []string{}}
	if p.Email == "" {
		p.Email = c.Username
	}
	for _, r := range c.RealmAccess.Roles {
		switch r {
		case RoleViewer, RoleEditor, RolePublisher, RolePlatformAdmin:
			p.Roles = append(p.Roles, r)
		}
	}
	for _, g := range c.Groups {
		if team, ok := strings.CutPrefix(g, "/teams/"); ok && team != "" && !strings.Contains(team, "/") {
			p.Teams = append(p.Teams, team)
		}
	}
	sort.Strings(p.Roles)
	sort.Strings(p.Teams)
	return p, nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func unix(f float64) time.Time { return time.Unix(int64(f), 0) }

func decodeSegment(seg string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func hasAudience(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// key returns the public key for kid, fetching Keycloak's keys when needed.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	stale := time.Since(v.fetchedAt) > refreshEvery
	if k, ok := v.keys[kid]; ok && !stale {
		return k, nil
	}
	if stale || time.Since(v.fetchedAt) > refetchAfter {
		if err := v.fetch(ctx); err != nil {
			if v.keys == nil {
				return nil, fmt.Errorf("can't read Keycloak's keys: %w", err)
			}
			// Keycloak briefly unreachable: keep the keys we have, try again in 30 s.
			v.fetchedAt = time.Now().Add(-refreshEvery + refetchAfter)
		}
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown signing key", errToken)
}

func (v *Verifier) fetch(ctx context.Context) error {
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS answered %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	v.keys, v.fetchedAt = keys, time.Now()
	return nil
}
