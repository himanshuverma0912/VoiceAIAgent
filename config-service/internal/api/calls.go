package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/flowspec"
	"github.com/himanshuverma0912/VoiceAIAgent/config-service/internal/store"
)

// Call history. The agent sends one record per call when it ends
// (POST /internal/v1/calls); Flow Studio shows a flow's calls and a summary.
//
// A record holds counts and timings only: never the caller's number, the room
// name, anything said, or any value the caller gave. Unknown fields are
// refused, so nothing else can be slipped in later by mistake.

const (
	maxCallBody     = 64 << 10 // a record is ~1-2 KB
	maxCallDuration = 6 * 3600 * 1000
	summaryMaxCalls = 20000
)

var (
	callIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	sourceRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

	channels = map[string]bool{"phone": true, "studio_test": true}
	runtimes = map[string]bool{"livekit": true, "pipecat": true}
	outcomes = []string{"completed", "ended_early", "caller_hung_up", "time_limit", "error", "other"}

	ist = time.FixedZone("IST", 5*3600+1800) // days in the summary are Indian days
)

func validOutcome(o string) bool {
	for _, x := range outcomes {
		if x == o {
			return true
		}
	}
	return false
}

func checkCall(c *store.Call, now time.Time) []string {
	var p []string
	bad := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	between := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			bad("%s: must be between %d and %d", name, lo, hi)
		}
	}
	if !callIDRE.MatchString(c.CallID) {
		bad("call_id: must be the job id (letters, digits, _ and -; max 64)")
	}
	if !identifierRE.MatchString(c.FlowID) {
		bad("flow_id: not a flow id")
	}
	if c.FlowVersion != nil {
		between("flow_version", *c.FlowVersion, 1, 1_000_000)
	}
	if !sourceRE.MatchString(c.FlowSource) {
		bad("flow_source: not a source name")
	}
	if !channels[c.Channel] {
		bad("channel: must be phone or studio_test")
	}
	if !runtimes[c.Runtime] {
		bad("runtime: must be livekit or pipecat")
	}
	if c.StartedAt.IsZero() || c.StartedAt.After(now.Add(5*time.Minute)) || c.StartedAt.Before(now.Add(-48*time.Hour)) {
		bad("started_at: must be within the last 2 days")
	}
	between("duration_ms", c.DurationMs, 0, maxCallDuration)
	if !validOutcome(c.Outcome) {
		bad("outcome: unknown")
	}
	if c.Steps == nil {
		c.Steps = []store.CallStep{}
	}
	if len(c.Steps) > 200 {
		bad("steps: at most 200")
	}
	for i, st := range c.Steps {
		if !identifierRE.MatchString(st.Node) {
			bad("steps[%d].node: not a node id", i)
		}
		between(fmt.Sprintf("steps[%d].ms", i), st.Ms, 0, maxCallDuration)
	}
	if c.Tools == nil {
		c.Tools = []store.CallTool{}
	}
	if len(c.Tools) > 200 {
		bad("tools: at most 200")
	}
	for i, t := range c.Tools {
		if !flowspec.ToolRE.MatchString(t.Tool) {
			bad("tools[%d].tool: not an '<integration>.<tool>' name", i)
		}
		between(fmt.Sprintf("tools[%d].ms", i), t.Ms, 0, 600_000)
	}
	between("turns.caller", c.Turns.Caller, 0, 10000)
	between("turns.agent", c.Turns.Agent, 0, 10000)
	if l := c.LatencyMs; l != nil {
		between("latency_ms.p50", l.P50, 0, 600_000)
		between("latency_ms.p95", l.P95, 0, 600_000)
		between("latency_ms.count", l.Count, 1, 10000)
	}
	u := c.Usage
	between("usage.llm_input_tokens", u.LLMInputTokens, 0, 100_000_000)
	between("usage.llm_output_tokens", u.LLMOutputTokens, 0, 100_000_000)
	between("usage.tts_characters", u.TTSCharacters, 0, 10_000_000)
	if u.STTAudioSeconds < 0 || u.STTAudioSeconds > 86400 || u.TTSAudioSeconds < 0 || u.TTSAudioSeconds > 86400 {
		bad("usage: audio seconds must be between 0 and 86400")
	}
	return p
}

// POST /internal/v1/calls — the agent, when a call ends.
func (s *Server) addCall(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(w, r, maxCallBody)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "call record is larger than 64 KB")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var c store.Call
	if err := dec.Decode(&c); err != nil || dec.More() {
		msg := "not a call record"
		if err != nil {
			msg += ": " + err.Error()
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if problems := checkCall(&c, time.Now()); len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "call record has problems", "problems": problems})
		return
	}
	created, err := s.Store.AddCall(r.Context(), &c)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK // already had it
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"call_id": c.CallID, "created": created})
}

func readLimited(w http.ResponseWriter, r *http.Request, n int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, n))
	return buf.Bytes(), err
}

// callQuery reads ?days= (1-90, default 7) and ?channel= (phone, studio_test
// or all; default phone).
func callQuery(w http.ResponseWriter, r *http.Request) (days int, channel string, ok bool) {
	days = 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			writeError(w, http.StatusBadRequest, "days must be between 1 and 90")
			return 0, "", false
		}
		days = n
	}
	switch channel = r.URL.Query().Get("channel"); channel {
	case "":
		channel = "phone"
	case "all":
		channel = ""
	case "phone", "studio_test":
	default:
		writeError(w, http.StatusBadRequest, "channel must be phone, studio_test or all")
		return 0, "", false
	}
	return days, channel, true
}

// GET /v1/flows/{flow_id}/calls?days=7&channel=phone&limit=50 — recent calls.
func (s *Server) listCalls(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toView)
	if !ok {
		return
	}
	days, channel, ok := callQuery(w, r)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	calls, err := s.Store.ListCalls(r.Context(), f.FlowID, channel, time.Now().AddDate(0, 0, -days), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": calls})
}

// ── Summary ──

type durationStats struct {
	Avg int `json:"avg"`
	P50 int `json:"p50"`
	P95 int `json:"p95"`
}

type stepStats struct {
	Node       string `json:"node"`
	Reached    int    `json:"reached"`      // calls that got to this step
	AvgMs      int    `json:"avg_ms"`       // time spent per visit
	EndedHere  int    `json:"ended_here"`   // calls whose last step this was
	HungUpHere int    `json:"hung_up_here"` // ...and the caller hung up there
}

type toolStats struct {
	Tool   string `json:"tool"`
	Uses   int    `json:"uses"`
	Failed int    `json:"failed"`
	AvgMs  int    `json:"avg_ms"`
	P95Ms  int    `json:"p95_ms"`
}

type versionStats struct {
	Version       *int   `json:"version"` // null: a draft or the fallback file
	Source        string `json:"source"`
	Calls         int    `json:"calls"`
	Completed     int    `json:"completed"`
	CallerHungUp  int    `json:"caller_hung_up"`
	AvgDurationMs int    `json:"avg_duration_ms"`
	LastCallAt    string `json:"last_call_at"`
}

type dayStats struct {
	Date      string `json:"date"`
	Calls     int    `json:"calls"`
	Completed int    `json:"completed"`
}

type callSummary struct {
	Days      int              `json:"days"`
	Channel   string           `json:"channel"`
	Calls     int              `json:"calls"`
	Truncated bool             `json:"truncated"` // more calls than were summarised
	Outcomes  map[string]int   `json:"outcomes"`
	Duration  *durationStats   `json:"duration_ms"`
	Latency   *durationStats   `json:"latency_ms"` // p50: typical reply; p95: slow replies
	Steps     []stepStats      `json:"steps"`
	Tools     []toolStats      `json:"tools"`
	Versions  []versionStats   `json:"versions"`
	PerDay    []dayStats       `json:"per_day"`
	Usage     store.CallUsage  `json:"usage"`
	Turns     *store.CallTurns `json:"avg_turns"`
}

func pct(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	k := float64(len(sorted)-1) * p
	lo := int(k)
	hi := min(lo+1, len(sorted)-1)
	return int(float64(sorted[lo]) + (float64(sorted[hi])-float64(sorted[lo]))*(k-float64(lo)) + 0.5)
}

func stats(values []int) *durationStats {
	if len(values) == 0 {
		return nil
	}
	sort.Ints(values)
	sum := 0
	for _, v := range values {
		sum += v
	}
	return &durationStats{Avg: (sum + len(values)/2) / len(values), P50: pct(values, 0.5), P95: pct(values, 0.95)}
}

func summarise(calls []store.Call, days int, channel string, now time.Time) callSummary {
	out := callSummary{Days: days, Channel: channel, Calls: len(calls), Outcomes: map[string]int{}}
	if out.Channel == "" {
		out.Channel = "all"
	}
	for _, o := range outcomes {
		out.Outcomes[o] = 0
	}

	var durations, p50s, p95s []int
	type stepAgg struct {
		reached, visits, ms, endedHere, hungUp int
	}
	steps := map[string]*stepAgg{}
	type versionAgg struct {
		versionStats
		ms   int
		last time.Time
	}
	versions := map[string]*versionAgg{}
	perDay := map[string]*dayStats{}
	turns := store.CallTurns{}
	toolMs := map[string][]int{}
	toolFailed := map[string]int{}

	for _, c := range calls {
		out.Outcomes[c.Outcome]++
		durations = append(durations, c.DurationMs)
		if c.LatencyMs != nil {
			p50s = append(p50s, c.LatencyMs.P50)
			p95s = append(p95s, c.LatencyMs.P95)
		}
		out.Usage.Add(c.Usage)
		turns.Caller += c.Turns.Caller
		turns.Agent += c.Turns.Agent

		for _, t := range c.Tools {
			toolMs[t.Tool] = append(toolMs[t.Tool], t.Ms)
			if !t.OK {
				toolFailed[t.Tool]++
			}
		}

		seen := map[string]bool{}
		for _, st := range c.Steps {
			a := steps[st.Node]
			if a == nil {
				a = &stepAgg{}
				steps[st.Node] = a
			}
			a.visits++
			a.ms += st.Ms
			if !seen[st.Node] {
				seen[st.Node] = true
				a.reached++
			}
		}
		if n := len(c.Steps); n > 0 {
			last := steps[c.Steps[n-1].Node]
			last.endedHere++
			if c.Outcome == "caller_hung_up" {
				last.hungUp++
			}
		}

		key := c.FlowSource
		if c.FlowVersion != nil {
			key = "v" + strconv.Itoa(*c.FlowVersion)
		}
		v := versions[key]
		if v == nil {
			v = &versionAgg{versionStats: versionStats{Version: c.FlowVersion, Source: c.FlowSource}}
			versions[key] = v
		}
		v.Calls++
		v.ms += c.DurationMs
		if c.Outcome == "completed" {
			v.Completed++
		}
		if c.Outcome == "caller_hung_up" {
			v.CallerHungUp++
		}
		if c.StartedAt.After(v.last) {
			v.last = c.StartedAt
		}

		day := c.StartedAt.In(ist).Format("2006-01-02")
		d := perDay[day]
		if d == nil {
			d = &dayStats{Date: day}
			perDay[day] = d
		}
		d.Calls++
		if c.Outcome == "completed" {
			d.Completed++
		}
	}

	out.Duration = stats(durations)
	if lat50, lat95 := stats(p50s), stats(p95s); lat50 != nil {
		out.Latency = &durationStats{Avg: lat50.Avg, P50: lat50.P50, P95: lat95.P95}
	}
	if len(calls) > 0 {
		out.Turns = &store.CallTurns{Caller: turns.Caller / len(calls), Agent: turns.Agent / len(calls)}
	}
	out.Usage.STTAudioSeconds = float64(int(out.Usage.STTAudioSeconds*10+0.5)) / 10
	out.Usage.TTSAudioSeconds = float64(int(out.Usage.TTSAudioSeconds*10+0.5)) / 10

	out.Steps = []stepStats{}
	for node, a := range steps {
		out.Steps = append(out.Steps, stepStats{
			Node: node, Reached: a.reached, AvgMs: (a.ms + a.visits/2) / a.visits, EndedHere: a.endedHere, HungUpHere: a.hungUp,
		})
	}
	sort.Slice(out.Steps, func(i, j int) bool {
		if out.Steps[i].Reached != out.Steps[j].Reached {
			return out.Steps[i].Reached > out.Steps[j].Reached
		}
		return out.Steps[i].Node < out.Steps[j].Node
	})

	out.Tools = []toolStats{}
	for tool, ms := range toolMs {
		st := stats(ms)
		out.Tools = append(out.Tools, toolStats{Tool: tool, Uses: len(ms), Failed: toolFailed[tool], AvgMs: st.Avg, P95Ms: st.P95})
	}
	sort.Slice(out.Tools, func(i, j int) bool {
		if out.Tools[i].Uses != out.Tools[j].Uses {
			return out.Tools[i].Uses > out.Tools[j].Uses
		}
		return out.Tools[i].Tool < out.Tools[j].Tool
	})

	out.Versions = []versionStats{}
	for _, v := range versions {
		v.AvgDurationMs = (v.ms + v.Calls/2) / v.Calls
		v.LastCallAt = v.last.UTC().Format(time.RFC3339)
		out.Versions = append(out.Versions, v.versionStats)
	}
	sort.Slice(out.Versions, func(i, j int) bool { return out.Versions[i].LastCallAt > out.Versions[j].LastCallAt })

	// Every day in the window, including days without calls, oldest first.
	out.PerDay = []dayStats{}
	today := now.In(ist)
	for i := days - 1; i >= 0; i-- {
		day := today.AddDate(0, 0, -i).Format("2006-01-02")
		if d := perDay[day]; d != nil {
			out.PerDay = append(out.PerDay, *d)
		} else {
			out.PerDay = append(out.PerDay, dayStats{Date: day})
		}
	}
	return out
}

// GET /v1/flows/{flow_id}/calls/summary?days=7&channel=phone
func (s *Server) callSummary(w http.ResponseWriter, r *http.Request) {
	f, ok := s.flowFor(w, r, toView)
	if !ok {
		return
	}
	days, channel, ok := callQuery(w, r)
	if !ok {
		return
	}
	now := time.Now()
	calls, err := s.Store.ListCalls(r.Context(), f.FlowID, channel, now.AddDate(0, 0, -days), summaryMaxCalls+1)
	if err != nil {
		s.fail(w, err)
		return
	}
	truncated := len(calls) > summaryMaxCalls
	if truncated {
		calls = calls[:summaryMaxCalls]
	}
	sum := summarise(calls, days, channel, now)
	sum.Truncated = truncated
	writeJSON(w, http.StatusOK, sum)
}
