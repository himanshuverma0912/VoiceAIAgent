// Package builder turns a business team's written description of a call
// (it can be long: 20,000 tokens of rules and conditions) into a flow.
//
// It asks an LLM through LiteLLM (an OpenAI-compatible API) to write the flow,
// checks the result with the same validator as Flow Studio, and if there are
// problems sends them back to the LLM to fix, up to MaxAttempts times. The
// result is only returned, never saved: Flow Studio opens it as a draft that a
// maker reviews, test-calls and submits like any other change.
//
// The description is only held in memory while building and is never logged.
package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
)

type Config struct {
	URL         string // LiteLLM, e.g. http://litellm:4000
	APIKey      string
	Model       string // LiteLLM alias, e.g. flow-builder
	Timeout     time.Duration
	MaxAttempts int
	HTTP        *http.Client
}

type Builder struct{ cfg Config }

func New(cfg Config) *Builder {
	if cfg.Timeout == 0 {
		cfg.Timeout = 6 * time.Minute
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{}
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &Builder{cfg: cfg}
}

func (b *Builder) Model() string { return b.cfg.Model }

type Request struct {
	FlowID      string
	Name        string
	Description string
}

type Result struct {
	Flow         json.RawMessage `json:"flow"`     // the flow, ready to save as a draft
	Notes        []string        `json:"notes"`    // for the business team: assumptions, simplifications
	Problems     []string        `json:"problems"` // problems still left after the last attempt (normally none)
	Attempts     int             `json:"attempts"`
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
	Steps        int             `json:"steps"`
}

// Build writes a flow for req. check validates a flow (returns its problems);
// progress is told what's happening, for the UI.
func (b *Builder) Build(ctx context.Context, req Request, check func([]byte) []string, progress func(string)) (*Result, error) {
	system := strings.Replace(systemPrompt, "%EXAMPLE%", exampleFlow, 1)
	messages := []message{
		{Role: "system", Content: system},
		{Role: "user", Content: fmt.Sprintf(
			"Flow id: %s\nName: %s\n\nThe team's description of the call, between the markers:\n<<<DESCRIPTION\n%s\nDESCRIPTION>>>",
			req.FlowID, req.Name, req.Description)},
	}
	res := &Result{Notes: []string{}, Problems: []string{}}
	for attempt := 1; attempt <= b.cfg.MaxAttempts; attempt++ {
		res.Attempts = attempt
		if attempt == 1 {
			progress("Reading the description and writing the flow")
		} else {
			progress(fmt.Sprintf("Fixing %d problem(s) the checker found (attempt %d of %d)", len(res.Problems), attempt, b.cfg.MaxAttempts))
		}
		reply, usage, err := b.complete(ctx, messages)
		if err != nil {
			return nil, err
		}
		res.InputTokens += usage.PromptTokens
		res.OutputTokens += usage.CompletionTokens

		flow, notes, perr := parseReply(reply, req)
		if perr != nil {
			res.Problems = []string{"(top level): " + perr.Error()}
		} else {
			progress("Checking the flow")
			res.Flow, res.Notes, res.Steps = flow, notes, countSteps(flow)
			res.Problems = check(flow)
			if len(res.Problems) == 0 {
				return res, nil
			}
		}
		messages = append(messages,
			message{Role: "assistant", Content: reply},
			message{Role: "user", Content: "The flow checker found these problems:\n- " + strings.Join(res.Problems, "\n- ") +
				"\n\nFix all of them and answer again with the complete JSON object {\"flow\": ..., \"notes\": [...]} and nothing else. Keep everything that was right."},
		)
	}
	if res.Flow == nil {
		return nil, errors.New("the AI didn't return a usable flow; try again, or shorten or clarify the description")
	}
	return res, nil // with problems left: the maker can fix them in the editor
}

// ── The LLM call (OpenAI-compatible chat completions through LiteLLM) ─────────

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (b *Builder) complete(ctx context.Context, messages []message) (string, usage, error) {
	body, _ := json.Marshal(map[string]any{
		"model":           b.cfg.Model,
		"messages":        messages,
		"response_format": map[string]string{"type": "json_object"},
	})
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.cfg.APIKey)
	resp, err := b.cfg.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", usage{}, fmt.Errorf("the AI took longer than %s; try a shorter description", b.cfg.Timeout)
		}
		return "", usage{}, errors.New("couldn't reach the AI service (LiteLLM)")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", usage{}, fmt.Errorf("the AI service answered %d: %s", resp.StatusCode, firstLine(raw, 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", usage{}, errors.New("the AI service sent an answer that isn't a chat completion")
	}
	if out.Choices[0].FinishReason == "length" {
		return "", out.Usage, errors.New("the flow was too long for the AI to finish in one answer; split the description into smaller flows")
	}
	return out.Choices[0].Message.Content, out.Usage, nil
}

func firstLine(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// ── Reading the reply ─────────────────────────────────────────────────────────

// parseReply extracts the flow (with the given id and name, key order kept)
// and the notes from the model's answer.
func parseReply(reply string, req Request) (json.RawMessage, []string, error) {
	reply = strings.TrimSpace(reply)
	if strings.HasPrefix(reply, "```") { // tolerate a fenced answer
		reply = strings.TrimPrefix(strings.TrimPrefix(reply, "```json"), "```")
		reply = strings.TrimSuffix(strings.TrimSpace(reply), "```")
	}
	decoded, err := flowspec.Decode([]byte(reply))
	if err != nil {
		return nil, nil, fmt.Errorf("the answer isn't valid JSON (%v)", err)
	}
	root, ok := decoded.(*flowspec.Object)
	if !ok {
		return nil, nil, errors.New("the answer must be a JSON object {\"flow\": ..., \"notes\": [...]}")
	}
	flow := root
	if f, ok := root.Values["flow"].(*flowspec.Object); ok {
		flow = f
	} else if _, isFlow := root.Values["nodes"]; !isFlow {
		return nil, nil, errors.New("the answer has no \"flow\"")
	}
	notes := []string{}
	if list, ok := root.Values["notes"].([]any); ok {
		for _, n := range list {
			if s, ok := n.(string); ok && strings.TrimSpace(s) != "" && len(notes) < 10 {
				notes = append(notes, strings.TrimSpace(s))
			}
		}
	}
	set(flow, "schema_version", json.Number("1"), 0)
	set(flow, "flow_id", req.FlowID, 1)
	if name, _ := flow.Values["name"].(string); strings.TrimSpace(name) == "" || req.Name != "" {
		set(flow, "name", req.Name, 2)
	}
	var buf bytes.Buffer
	encode(&buf, flow, "")
	buf.WriteByte('\n')
	return json.RawMessage(buf.Bytes()), notes, nil
}

// set puts key = value into o, at position pos if the key is new.
func set(o *flowspec.Object, key string, value any, pos int) {
	if _, ok := o.Values[key]; !ok {
		if pos > len(o.Keys) {
			pos = len(o.Keys)
		}
		o.Keys = append(o.Keys[:pos], append([]string{key}, o.Keys[pos:]...)...)
	}
	o.Values[key] = value
}

func countSteps(flow json.RawMessage) int {
	var f struct {
		Nodes map[string]json.RawMessage `json:"nodes"`
	}
	json.Unmarshal(flow, &f) //nolint:errcheck
	return len(f.Nodes)
}

// encode writes v as indented JSON, keeping object keys in their order.
func encode(buf *bytes.Buffer, v any, indent string) {
	switch t := v.(type) {
	case *flowspec.Object:
		if len(t.Keys) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteString("{\n")
		for i, k := range t.Keys {
			buf.WriteString(indent + "  ")
			writeString(buf, k)
			buf.WriteString(": ")
			encode(buf, t.Values[k], indent+"  ")
			if i < len(t.Keys)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(indent + "}")
	case []any:
		if len(t) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteString("[\n")
		for i, item := range t {
			buf.WriteString(indent + "  ")
			encode(buf, item, indent+"  ")
			if i < len(t)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(indent + "]")
	case string:
		writeString(buf, t)
	case json.Number:
		buf.WriteString(t.String())
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case nil:
		buf.WriteString("null")
	default: // not produced by flowspec.Decode
		b, _ := json.Marshal(t)
		buf.Write(b)
	}
}

func writeString(buf *bytes.Buffer, s string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s) //nolint:errcheck
	buf.Write(bytes.TrimRight(b.Bytes(), "\n"))
}
