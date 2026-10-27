package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// ── Call history ──────────────────────────────────────────────────────────────
//
// Content-free facts about each call (see migrations/004_calls.sql). The API
// validates every field before it gets here.

type CallStep struct {
	Node string `json:"node"`
	Ms   int    `json:"ms"`
}

// CallTool is one use of an integration's tool: which, whether it worked and
// how long it took. Never what was sent or what came back.
type CallTool struct {
	Tool string `json:"tool"` // "<integration>.<tool>"
	OK   bool   `json:"ok"`
	Ms   int    `json:"ms"`
}

type CallTurns struct {
	Caller int `json:"caller"`
	Agent  int `json:"agent"`
}

type CallLatency struct {
	P50   int `json:"p50"`
	P95   int `json:"p95"`
	Count int `json:"count"`
}

type CallUsage struct {
	LLMInputTokens  int     `json:"llm_input_tokens"`
	LLMOutputTokens int     `json:"llm_output_tokens"`
	STTAudioSeconds float64 `json:"stt_audio_seconds"`
	TTSCharacters   int     `json:"tts_characters"`
	TTSAudioSeconds float64 `json:"tts_audio_seconds"`
}

func (u *CallUsage) Add(o CallUsage) {
	u.LLMInputTokens += o.LLMInputTokens
	u.LLMOutputTokens += o.LLMOutputTokens
	u.STTAudioSeconds += o.STTAudioSeconds
	u.TTSCharacters += o.TTSCharacters
	u.TTSAudioSeconds += o.TTSAudioSeconds
}

type Call struct {
	CallID      string       `json:"call_id"`
	FlowID      string       `json:"flow_id"`
	FlowVersion *int         `json:"flow_version"`
	FlowSource  string       `json:"flow_source"`
	Channel     string       `json:"channel"`
	Runtime     string       `json:"runtime"`
	StartedAt   time.Time    `json:"started_at"`
	DurationMs  int          `json:"duration_ms"`
	Outcome     string       `json:"outcome"`
	Steps       []CallStep   `json:"steps"`
	Tools       []CallTool   `json:"tools"`
	Turns       CallTurns    `json:"turns"`
	LatencyMs   *CallLatency `json:"latency_ms"`
	Usage       CallUsage    `json:"usage"`
}

// AddCall stores a call. Sending the same call twice is fine: the second
// copy is ignored and created is false.
func (s *Store) AddCall(ctx context.Context, c *Call) (created bool, err error) {
	steps, err := json.Marshal(c.Steps)
	if err != nil {
		return false, err
	}
	usage, err := json.Marshal(c.Usage)
	if err != nil {
		return false, err
	}
	if c.Tools == nil {
		c.Tools = []CallTool{}
	}
	tools, err := json.Marshal(c.Tools)
	if err != nil {
		return false, err
	}
	var p50, p95 sql.NullInt64
	count := 0
	if c.LatencyMs != nil {
		p50 = sql.NullInt64{Int64: int64(c.LatencyMs.P50), Valid: true}
		p95 = sql.NullInt64{Int64: int64(c.LatencyMs.P95), Valid: true}
		count = c.LatencyMs.Count
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO calls (call_id, flow_id, flow_version, flow_source, channel, runtime, started_at,
			duration_ms, outcome, steps, caller_turns, agent_turns, latency_p50_ms, latency_p95_ms,
			latency_count, usage, tools)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (call_id) DO NOTHING`,
		c.CallID, c.FlowID, c.FlowVersion, c.FlowSource, c.Channel, c.Runtime, c.StartedAt,
		c.DurationMs, c.Outcome, string(steps), c.Turns.Caller, c.Turns.Agent, p50, p95, count, string(usage), string(tools))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ListCalls returns a flow's calls started at or after since, newest first.
// channel "" means every channel.
func (s *Store) ListCalls(ctx context.Context, flowID, channel string, since time.Time, limit int) ([]Call, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT call_id, flow_id, flow_version, flow_source, channel, runtime, started_at, duration_ms,
			outcome, steps, caller_turns, agent_turns, latency_p50_ms, latency_p95_ms, latency_count, usage, tools
		FROM calls
		WHERE flow_id = $1 AND started_at >= $2 AND ($3 = '' OR channel = $3)
		ORDER BY started_at DESC
		LIMIT $4`, flowID, since, channel, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		var c Call
		var version, p50, p95 sql.NullInt64
		var count int
		var steps, usage, tools []byte
		if err := rows.Scan(&c.CallID, &c.FlowID, &version, &c.FlowSource, &c.Channel, &c.Runtime,
			&c.StartedAt, &c.DurationMs, &c.Outcome, &steps, &c.Turns.Caller, &c.Turns.Agent,
			&p50, &p95, &count, &usage, &tools); err != nil {
			return nil, err
		}
		if version.Valid {
			v := int(version.Int64)
			c.FlowVersion = &v
		}
		if p50.Valid && p95.Valid {
			c.LatencyMs = &CallLatency{P50: int(p50.Int64), P95: int(p95.Int64), Count: count}
		}
		if err := json.Unmarshal(steps, &c.Steps); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(usage, &c.Usage); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(tools, &c.Tools); err != nil {
			return nil, err
		}
		c.StartedAt = c.StartedAt.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// PurgeCalls deletes calls that started before cutoff and says how many.
func (s *Store) PurgeCalls(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM calls WHERE started_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
