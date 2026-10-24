package flowspec

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// BuiltinVariables can be used as {{name}} in any text of any flow.
var BuiltinVariables = []string{"caller_number", "dialed_number", "now_ist", "today_ist"}

var variableRE = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// Result of checking a flow. Problems is empty when the flow is valid; then
// FlowID and Name are set.
type Result struct {
	Problems []string
	FlowID   string
	Name     string
}

// ValidateJSON checks a flow given as JSON text.
func ValidateJSON(data []byte) (Result, error) {
	v, err := Decode(data)
	if err != nil {
		return Result{}, err
	}
	return Validate(v), nil
}

// Validate checks a decoded flow (see Decode) and reports every problem found.
func Validate(data any) Result {
	s := &shape{}
	flow := s.flow(data)
	if len(s.problems) > 0 {
		// Also report broken links and unknown variables now, so the flow team
		// sees every problem in one round instead of only after fixing the shape.
		problems := s.problems
		seen := map[string]bool{}
		for _, p := range problems {
			seen[p] = true
		}
		for _, p := range bestEffortChecks(data) {
			if !seen[p] {
				problems = append(problems, p)
			}
		}
		return Result{Problems: problems}
	}
	if problems := checkFlow(flow); len(problems) > 0 {
		return Result{Problems: problems}
	}
	return Result{FlowID: flow.FlowID, Name: flow.Name}
}

func successors(n *Node) []string {
	switch n.Type {
	case "agent":
		out := make([]string, len(n.Transitions))
		for i, t := range n.Transitions {
			out[i] = t.To
		}
		return out
	case "collect":
		return []string{n.Next}
	}
	return nil
}

type located struct{ where, text string }

func texts(f *Flow) []located {
	out := []located{{"persona", f.Persona}}
	for i, g := range f.Guardrails {
		out = append(out, located{fmt.Sprintf("guardrails[%d]", i), g})
	}
	if f.Greeting != "" {
		out = append(out, located{"greeting", f.Greeting})
	}
	for _, n := range f.Nodes {
		base := "nodes." + n.ID
		out = append(out, located{base + ".instructions", n.Instructions})
		switch n.Type {
		case "agent":
			for i, t := range n.Transitions {
				out = append(out, located{fmt.Sprintf("%s.transitions[%d].when", base, i), t.When})
			}
		case "collect":
			for i, fl := range n.Fields {
				out = append(out, located{fmt.Sprintf("%s.fields[%d].instructions", base, i), fl.Instructions})
			}
		default:
			out = append(out, located{base + ".goodbye", n.Goodbye})
		}
	}
	return out
}

func unknownVariable(where, name string) string {
	return fmt.Sprintf("%s: unknown variable {{%s}}; built-ins are %s, plus any captured or collected field",
		where, name, strings.Join(BuiltinVariables, ", "))
}

func isBuiltinVariable(name string) bool {
	for _, b := range BuiltinVariables {
		if b == name {
			return true
		}
	}
	return false
}

func checkFlow(f *Flow) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	byID := map[string]*Node{}
	var ids []string
	for _, n := range f.Nodes {
		byID[n.ID] = n
		ids = append(ids, n.ID)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)

	// Every reference points to a real node.
	if byID[f.StartNode] == nil {
		add("start_node: '%s' is not a node (nodes: %s)", f.StartNode, strings.Join(sorted, ", "))
	}
	for _, n := range f.Nodes {
		base := "nodes." + n.ID
		switch n.Type {
		case "agent":
			count := map[string]int{}
			for i, t := range n.Transitions {
				if byID[t.To] == nil {
					add("%s.transitions[%d].to: '%s' is not a node", base, i, t.To)
				}
				if t.To == n.ID {
					add("%s.transitions[%d].to: a node can't transition to itself", base, i)
				}
				count[t.ToolName]++
			}
			var dups []string
			for name, c := range count {
				if c > 1 {
					dups = append(dups, name)
				}
			}
			sort.Strings(dups)
			for _, d := range dups {
				add("%s.transitions: two transitions are both named '%s'; give them different 'name's", base, d)
			}
			if len(n.Transitions) == 0 && !f.AllowEndCallAnywhere {
				add("%s: has no transitions, and allow_end_call_anywhere is false, so the call gets stuck", base)
			}
		case "collect":
			if byID[n.Next] == nil {
				add("%s.next: '%s' is not a node", base, n.Next)
			}
			if n.Next == n.ID {
				add("%s.next: a node can't move to itself", base)
			}
			for i, fl := range n.Fields {
				if fl.Builtin == "" && len(fl.Enum) > 0 && fl.Type != "string" {
					add("%s.fields[%d].enum: only string fields can have an enum", base, i)
				}
			}
		}
	}
	for _, n := range f.Nodes {
		for i, t := range n.Transitions {
			for _, c := range t.Capture {
				if len(c.Enum) > 0 && c.Type != "string" {
					add("nodes.%s.transitions[%d].capture.%s.enum: only string values can have an enum", n.ID, i, c.Name)
				}
			}
		}
	}
	hasEnd := false
	for _, n := range f.Nodes {
		hasEnd = hasEnd || n.Type == "end"
	}
	if !hasEnd {
		add("nodes: the flow needs at least one node of type 'end'")
	}

	// Graph checks only make sense once every reference is valid.
	if len(problems) == 0 {
		reachable := map[string]bool{f.StartNode: true}
		queue := []string{f.StartNode}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			for _, next := range successors(byID[id]) {
				if !reachable[next] {
					reachable[next] = true
					queue = append(queue, next)
				}
			}
		}
		for _, id := range sorted {
			if !reachable[id] {
				add("nodes.%s: can't be reached from start_node '%s'", id, f.StartNode)
			}
		}

		// Every node must have some path to an end node.
		finishes := map[string]bool{}
		for _, n := range f.Nodes {
			finishes[n.ID] = n.Type == "end"
		}
		for changed := true; changed; {
			changed = false
			for _, n := range f.Nodes {
				if finishes[n.ID] {
					continue
				}
				for _, next := range successors(n) {
					if finishes[next] {
						finishes[n.ID] = true
						changed = true
						break
					}
				}
			}
		}
		for _, id := range sorted {
			if reachable[id] && !finishes[id] {
				add("nodes.%s: no path from here leads to an 'end' node", id)
			}
		}
	}

	// Variables: each is defined once, doesn't hide a built-in, and every
	// {{reference}} is a built-in or defined somewhere in the flow.
	seen := map[string]string{}
	for _, n := range f.Nodes {
		var places []located // (variable name, where)
		for i, t := range n.Transitions {
			for _, c := range t.Capture {
				places = append(places, located{c.Name, fmt.Sprintf("nodes.%s.transitions[%d].capture.%s", n.ID, i, c.Name)})
			}
		}
		for i, fl := range n.Fields {
			places = append(places, located{fl.Name, fmt.Sprintf("nodes.%s.fields[%d]", n.ID, i)})
		}
		for _, p := range places {
			name, where := p.where, p.text
			switch {
			case isBuiltinVariable(name):
				add("%s: '%s' is a built-in variable; choose another name", where, name)
			case seen[name] != "":
				add("%s: '%s' is already defined at %s; use a different name", where, name, seen[name])
			default:
				seen[name] = where
			}
		}
	}
	for _, t := range texts(f) {
		for _, m := range variableRE.FindAllStringSubmatch(t.text, -1) {
			if !isBuiltinVariable(m[1]) && seen[m[1]] == "" {
				problems = append(problems, unknownVariable(t.where, m[1]))
			}
		}
	}
	return problems
}

// pyTruthy is Python's truth test for a decoded JSON value.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case *Object:
		return len(t.Keys) > 0
	default:
		return pyStr(t) != "0" && pyStr(t) != "0.0"
	}
}

// bestEffortChecks runs the link and variable checks on a flow that failed the
// shape checks, looking only at the parts that have the expected shape.
func bestEffortChecks(data any) []string {
	root, ok := data.(*Object)
	if !ok {
		return nil
	}
	nodesValue, _ := root.Get("nodes")
	nodes, ok := nodesValue.(*Object)
	if !ok {
		return nil
	}
	var problems []string
	isNode := func(v any) bool { _, ok := nodes.Get(v.(string)); return ok }
	if start, ok := root.Values["start_node"].(string); ok && !isNode(start) {
		sorted := append([]string(nil), nodes.Keys...)
		sort.Strings(sorted)
		problems = append(problems, fmt.Sprintf("start_node: '%s' is not a node (nodes: %s)", start, strings.Join(sorted, ", ")))
	}

	defined := map[string]bool{}
	for _, b := range BuiltinVariables {
		defined[b] = true
	}
	var found []located
	for _, key := range []string{"persona", "greeting"} {
		if text, ok := root.Values[key].(string); ok {
			found = append(found, located{key, text})
		}
	}
	for _, id := range nodes.Keys {
		node, ok := nodes.Values[id].(*Object)
		if !ok {
			continue
		}
		base := "nodes." + id
		for _, key := range []string{"instructions", "goodbye"} {
			if text, ok := node.Values[key].(string); ok {
				found = append(found, located{base + "." + key, text})
			}
		}
		// The common misspelling is accepted here so its links still get checked.
		transitions := node.Values["transitions"]
		if !pyTruthy(transitions) {
			transitions = node.Values["transitons"]
		}
		list, _ := transitions.([]any)
		for i, item := range list {
			t, ok := item.(*Object)
			if !ok {
				continue
			}
			if to, ok := t.Values["to"].(string); ok && !isNode(to) {
				problems = append(problems, fmt.Sprintf("%s.transitions[%d].to: '%s' is not a node", base, i, to))
			}
			if capture, ok := t.Values["capture"].(*Object); ok {
				for _, k := range capture.Keys {
					defined[k] = true
				}
			}
			if when, ok := t.Values["when"].(string); ok {
				found = append(found, located{fmt.Sprintf("%s.transitions[%d].when", base, i), when})
			}
		}
		if next, ok := node.Values["next"].(string); ok && !isNode(next) {
			problems = append(problems, fmt.Sprintf("%s.next: '%s' is not a node", base, next))
		}
		fields, _ := node.Values["fields"].([]any)
		for i, item := range fields {
			f, ok := item.(*Object)
			if !ok {
				continue
			}
			if name, ok := f.Values["name"].(string); ok {
				defined[name] = true
			}
			if text, ok := f.Values["instructions"].(string); ok {
				found = append(found, located{fmt.Sprintf("%s.fields[%d].instructions", base, i), text})
			}
		}
	}
	for _, t := range found {
		for _, m := range variableRE.FindAllStringSubmatch(t.text, -1) {
			if !defined[m[1]] {
				problems = append(problems, unknownVariable(t.where, m[1]))
			}
		}
	}
	return problems
}
