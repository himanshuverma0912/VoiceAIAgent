package flowspec

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// The messages below are the ones pydantic (Python) uses, so a flow team sees
// the same text whether the UI (this service) or the voice agent checks a flow.

const (
	identifierMessage = "must be lower-case letters, digits and _ , starting with a letter (max 48 characters)"
	languageMessage   = "must be a language code like 'hi', 'en', 'hi-IN', or 'multi'"
	defaultGoodbye    = "Thank the caller briefly, in the language they are speaking, and say goodbye."
)

var (
	identifierRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	languageRE   = regexp.MustCompile(`^[a-z]{2}(-[A-Za-z]{2})?$|^multi$`)
	valueTypes   = []string{"string", "number", "integer", "boolean"}
	simpleFields = map[string]bool{"phone": true, "email": true, "address": true, "date_of_birth": true}
)

// BuiltinFields are the field types a collect node can ask for without a description.
var BuiltinFields = []string{"name", "phone", "email", "address", "date_of_birth", "digits"}

// Flow is a flow that passed the shape checks.
type Flow struct {
	FlowID               string
	Name                 string
	Persona              string
	Guardrails           []string
	Greeting             string
	AllowEndCallAnywhere bool
	StartNode            string
	Nodes                []*Node // in file order
}

// Node is one step of a flow: type "agent", "collect" or "end".
type Node struct {
	ID           string
	Type         string
	Instructions string
	Transitions  []Transition // agent
	Fields       []Field      // collect
	Next         string       // collect
	Goodbye      string       // end
}

// Transition moves an agent node on to another node.
type Transition struct {
	To       string
	When     string
	ToolName string
	Capture  []Capture
}

// Capture is a value the LLM fills in when it takes a transition.
type Capture struct {
	Name string
	Type string
	Enum []string
}

// Field is one value a collect node asks for. Builtin is "" for custom fields.
type Field struct {
	Name         string
	Builtin      string
	Type         string
	Enum         []string
	Instructions string
}

type shape struct{ problems []string }

func (s *shape) add(loc, msg string) {
	if loc == "" {
		loc = "(top level)"
	}
	s.problems = append(s.problems, loc+": "+msg)
}

func at(loc, key string) string {
	if loc == "" {
		return key
	}
	return loc + "." + key
}

func idx(loc string, i int) string { return fmt.Sprintf("%s[%d]", loc, i) }

// model checks v is an object, reports unknown keys, and returns it.
func (s *shape) model(loc string, v any, class string, keys ...string) (*Object, bool) {
	o, ok := v.(*Object)
	if !ok {
		s.add(loc, "Input should be a valid dictionary or instance of "+class)
		return nil, false
	}
	s.extraKeys(loc, o, keys...)
	return o, true
}

func (s *shape) extraKeys(loc string, o *Object, keys ...string) {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for _, k := range o.Keys {
		if !allowed[k] {
			s.add(at(loc, k), "unknown key (check the spelling)")
		}
	}
}

// field returns the value of key; a missing required key is reported.
func (s *shape) field(loc string, o *Object, key string, required bool) (any, bool) {
	v, ok := o.Get(key)
	if !ok && required {
		s.add(at(loc, key), "Field required")
	}
	return v, ok
}

func (s *shape) str(loc string, v any, minLen1 bool) (string, bool) {
	str, ok := v.(string)
	if !ok {
		s.add(loc, "Input should be a valid string")
		return "", false
	}
	if minLen1 && str == "" {
		s.add(loc, "String should have at least 1 character")
		return "", false
	}
	return str, true
}

func (s *shape) identifier(loc string, v any) (string, bool) {
	str, ok := s.str(loc, v, false)
	if !ok {
		return "", false
	}
	if !identifierRE.MatchString(str) {
		s.add(loc, identifierMessage)
		return "", false
	}
	return str, true
}

func (s *shape) boolean(loc string, v any) (bool, bool) {
	b, ok := v.(bool)
	if !ok {
		s.add(loc, "Input should be a valid boolean")
	}
	return b, ok
}

// literal checks v is one of the allowed strings.
func (s *shape) literal(loc string, v any, allowed ...string) (string, bool) {
	if str, ok := v.(string); ok {
		for _, a := range allowed {
			if str == a {
				return str, true
			}
		}
	}
	quoted := make([]string, len(allowed))
	for i, a := range allowed {
		quoted[i] = "'" + a + "'"
	}
	msg := quoted[len(quoted)-1]
	if len(quoted) > 1 {
		msg = strings.Join(quoted[:len(quoted)-1], ", ") + " or " + msg
	}
	s.add(loc, "Input should be "+msg)
	return "", false
}

// integer checks v is a whole JSON number (not 4.0, not "4") within [lo, hi].
func (s *shape) integer(loc string, v any, lo, hi int64) {
	n, ok := v.(json.Number)
	var value *big.Int
	if ok {
		value, ok = new(big.Int).SetString(string(n), 10)
	}
	if !ok {
		s.add(loc, "Input should be a valid integer")
		return
	}
	if value.Cmp(big.NewInt(lo)) < 0 {
		s.add(loc, fmt.Sprintf("Input should be greater than or equal to %d", lo))
	} else if value.Cmp(big.NewInt(hi)) > 0 {
		s.add(loc, fmt.Sprintf("Input should be less than or equal to %d", hi))
	}
}

// number checks v is a JSON number (whole or decimal) within [lo, hi].
func (s *shape) number(loc string, v any, lo, hi float64) {
	n, ok := v.(json.Number)
	if !ok {
		s.add(loc, "Input should be a valid number")
		return
	}
	f, _ := strconv.ParseFloat(string(n), 64) // out of range gives ±Inf, compared below
	if f < lo {
		s.add(loc, "Input should be greater than or equal to "+pyNumber(lo))
	} else if f > hi {
		s.add(loc, "Input should be less than or equal to "+pyNumber(hi))
	}
}

func pyNumber(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// list checks v is a list (with at least one item if minLen1).
func (s *shape) list(loc string, v any, minLen1 bool) ([]any, bool) {
	l, ok := v.([]any)
	if !ok {
		s.add(loc, "Input should be a valid list")
		return nil, false
	}
	if minLen1 && len(l) == 0 {
		s.add(loc, "List should have at least 1 item after validation, not 0")
		return nil, false
	}
	return l, true
}

// dict checks v is an object whose keys are identifiers.
func (s *shape) dict(loc string, v any, minLen1 bool) (*Object, bool) {
	o, ok := v.(*Object)
	if !ok {
		s.add(loc, "Input should be a valid dictionary")
		return nil, false
	}
	for _, k := range o.Keys {
		if !identifierRE.MatchString(k) {
			s.add(at(at(loc, k), "[key]"), identifierMessage)
		}
	}
	if minLen1 && len(o.Keys) == 0 {
		s.add(loc, "Dictionary should have at least 1 item after validation, not 0")
		return nil, false
	}
	return o, true
}

// enum checks an optional list of allowed string values.
func (s *shape) enum(loc string, o *Object) []string {
	v, ok := o.Get("enum")
	if !ok || v == nil {
		return nil
	}
	loc = at(loc, "enum")
	items, ok := s.list(loc, v, true)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		if str, ok := s.str(idx(loc, i), item, false); ok {
			out = append(out, str)
		}
	}
	return out
}

// optionalString reads a key that may be missing, null or a string.
func (s *shape) optionalString(loc string, o *Object, key string, identifier bool) string {
	v, ok := o.Get(key)
	if !ok || v == nil {
		return ""
	}
	if identifier {
		str, _ := s.identifier(at(loc, key), v)
		return str
	}
	str, _ := s.str(at(loc, key), v, false)
	return str
}

// pyStr renders a JSON value the way Python's str() would (for "unknown type '5'").
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return t
	case json.Number:
		if _, ok := new(big.Int).SetString(string(t), 10); ok {
			return string(t)
		}
		f, _ := strconv.ParseFloat(string(t), 64)
		out := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(out, ".") {
			out += ".0"
		}
		return out
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func (s *shape) flow(v any) *Flow {
	o, ok := s.model("", v, "Flow",
		"schema_version", "flow_id", "name", "persona", "guardrails", "greeting",
		"allow_end_call_anywhere", "settings", "start_node", "nodes")
	if !ok {
		return nil
	}
	f := &Flow{AllowEndCallAnywhere: true}
	if v, ok := s.field("", o, "schema_version", true); ok {
		if n, isNum := v.(json.Number); !isNum || string(n) != "1" {
			s.add("schema_version", "Input should be 1")
		}
	}
	if v, ok := s.field("", o, "flow_id", true); ok {
		f.FlowID, _ = s.identifier("flow_id", v)
	}
	if v, ok := s.field("", o, "name", true); ok {
		f.Name, _ = s.str("name", v, true)
	}
	if v, ok := s.field("", o, "persona", true); ok {
		f.Persona, _ = s.str("persona", v, true)
	}
	if v, ok := s.field("", o, "guardrails", false); ok {
		if items, ok := s.list("guardrails", v, false); ok {
			for i, item := range items {
				g, _ := s.str(idx("guardrails", i), item, false)
				f.Guardrails = append(f.Guardrails, g)
			}
		}
	}
	f.Greeting = s.optionalString("", o, "greeting", false)
	if v, ok := s.field("", o, "allow_end_call_anywhere", false); ok {
		f.AllowEndCallAnywhere, _ = s.boolean("allow_end_call_anywhere", v)
	}
	if v, ok := s.field("", o, "settings", false); ok {
		s.settings(v)
	}
	if v, ok := s.field("", o, "start_node", true); ok {
		f.StartNode, _ = s.identifier("start_node", v)
	}
	if v, ok := s.field("", o, "nodes", true); ok {
		if nodes, ok := s.dict("nodes", v, true); ok {
			for _, id := range nodes.Keys {
				if n := s.node(at("nodes", id), id, nodes.Values[id]); n != nil {
					f.Nodes = append(f.Nodes, n)
				}
			}
		}
	}
	return f
}

func (s *shape) settings(v any) {
	const loc = "settings"
	o, ok := s.model(loc, v, "FlowSettings", "language", "llm_model", "llm_temperature", "tts_voice_id")
	if !ok {
		return
	}
	if v, ok := o.Get("language"); ok {
		if lang, ok := s.str(at(loc, "language"), v, false); ok && !languageRE.MatchString(lang) {
			s.add(at(loc, "language"), languageMessage)
		}
	}
	s.optionalString(loc, o, "llm_model", false)
	if v, ok := o.Get("llm_temperature"); ok {
		s.number(at(loc, "llm_temperature"), v, 0, 2)
	}
	s.optionalString(loc, o, "tts_voice_id", false)
}

func (s *shape) node(loc, id string, v any) *Node {
	o, ok := v.(*Object)
	if !ok {
		s.add(loc, "Input should be a valid dictionary or object to extract fields from")
		return nil
	}
	typ, ok := o.Get("type")
	if !ok {
		s.add(loc, "Unable to extract tag using discriminator 'type'")
		return nil
	}
	n := &Node{ID: id}
	switch typ {
	case "agent":
		n.Type = "agent"
		s.extraKeys(loc, o, "type", "instructions", "transitions", "on_enter")
		if v, ok := s.field(loc, o, "instructions", true); ok {
			n.Instructions, _ = s.str(at(loc, "instructions"), v, true)
		}
		if v, ok := o.Get("transitions"); ok {
			if items, ok := s.list(at(loc, "transitions"), v, false); ok {
				for i, item := range items {
					if t, ok := s.transition(idx(at(loc, "transitions"), i), item); ok {
						n.Transitions = append(n.Transitions, t)
					}
				}
			}
		}
		if v, ok := o.Get("on_enter"); ok {
			s.literal(at(loc, "on_enter"), v, "speak", "wait")
		}
	case "collect":
		n.Type = "collect"
		s.extraKeys(loc, o, "type", "instructions", "fields", "next")
		if v, ok := o.Get("instructions"); ok {
			n.Instructions, _ = s.str(at(loc, "instructions"), v, false)
		}
		if v, ok := s.field(loc, o, "fields", true); ok {
			if items, ok := s.list(at(loc, "fields"), v, true); ok {
				for i, item := range items {
					if f, ok := s.collectField(idx(at(loc, "fields"), i), item); ok {
						n.Fields = append(n.Fields, f)
					}
				}
			}
		}
		if v, ok := s.field(loc, o, "next", true); ok {
			n.Next, _ = s.identifier(at(loc, "next"), v)
		}
	case "end":
		n.Type = "end"
		s.extraKeys(loc, o, "type", "instructions", "goodbye")
		if v, ok := s.field(loc, o, "instructions", true); ok {
			n.Instructions, _ = s.str(at(loc, "instructions"), v, true)
		}
		n.Goodbye = defaultGoodbye
		if v, ok := o.Get("goodbye"); ok {
			n.Goodbye, _ = s.str(at(loc, "goodbye"), v, false)
		}
	default:
		s.add(loc, fmt.Sprintf("unknown node type '%s'; use 'agent', 'collect' or 'end'", pyStr(typ)))
		return nil
	}
	return n
}

func (s *shape) transition(loc string, v any) (Transition, bool) {
	var t Transition
	o, ok := s.model(loc, v, "Transition", "to", "when", "name", "capture")
	if !ok {
		return t, false
	}
	if v, ok := s.field(loc, o, "to", true); ok {
		t.To, _ = s.identifier(at(loc, "to"), v)
	}
	if v, ok := s.field(loc, o, "when", true); ok {
		t.When, _ = s.str(at(loc, "when"), v, true)
	}
	t.ToolName = s.optionalString(loc, o, "name", true)
	if t.ToolName == "" {
		t.ToolName = "go_to_" + t.To
	}
	if v, ok := o.Get("capture"); ok {
		if capture, ok := s.dict(at(loc, "capture"), v, false); ok {
			for _, name := range capture.Keys {
				t.Capture = append(t.Capture, s.capture(at(at(loc, "capture"), name), name, capture.Values[name]))
			}
		}
	}
	return t, true
}

func (s *shape) capture(loc, name string, v any) Capture {
	c := Capture{Name: name, Type: "string"}
	o, ok := s.model(loc, v, "CaptureField", "type", "description", "enum", "required")
	if !ok {
		return c
	}
	if v, ok := o.Get("type"); ok {
		c.Type, _ = s.literal(at(loc, "type"), v, valueTypes...)
	}
	if v, ok := s.field(loc, o, "description", true); ok {
		s.str(at(loc, "description"), v, true)
	}
	c.Enum = s.enum(loc, o)
	if v, ok := o.Get("required"); ok {
		s.boolean(at(loc, "required"), v)
	}
	return c
}

func (s *shape) collectField(loc string, v any) (Field, bool) {
	var f Field
	o, isObj := v.(*Object)
	var builtin any
	if isObj {
		builtin, _ = o.Get("builtin")
	}
	switch {
	case builtin == nil: // custom field (also when v isn't an object: reported as CustomField)
		o, ok := s.model(loc, v, "CustomField", "name", "type", "description", "enum", "instructions")
		if !ok {
			return f, false
		}
		f.Type = "string"
		if v, ok := o.Get("type"); ok {
			f.Type, _ = s.literal(at(loc, "type"), v, valueTypes...)
		}
		if v, ok := s.field(loc, o, "description", true); ok {
			s.str(at(loc, "description"), v, true)
		}
		f.Enum = s.enum(loc, o)
	case builtin == "name":
		s.extraKeys(loc, o, "name", "builtin", "instructions", "options")
		if v, ok := o.Get("options"); ok {
			if opts, ok := s.model(at(loc, "options"), v, "NameOptions", "last_name", "verify_spelling"); ok {
				for _, key := range []string{"last_name", "verify_spelling"} {
					if v, ok := opts.Get(key); ok {
						s.boolean(at(at(loc, "options"), key), v)
					}
				}
			}
		}
	case builtin == "digits":
		s.extraKeys(loc, o, "name", "builtin", "instructions", "options")
		if v, ok := s.field(loc, o, "options", true); ok {
			oloc := at(loc, "options")
			if opts, ok := s.model(oloc, v, "DigitsOptions", "num_digits", "ask_for_confirmation"); ok {
				if v, ok := s.field(oloc, opts, "num_digits", true); ok {
					s.integer(at(oloc, "num_digits"), v, 1, 32)
				}
				if v, ok := opts.Get("ask_for_confirmation"); ok {
					s.boolean(at(oloc, "ask_for_confirmation"), v)
				}
			}
		}
	default:
		name, isString := builtin.(string)
		if !isString || !simpleFields[name] {
			s.add(at(loc, "builtin"), fmt.Sprintf("unknown built-in '%s'; use one of %s",
				pyStr(builtin), strings.Join(BuiltinFields, ", ")))
			return f, false
		}
		s.extraKeys(loc, o, "name", "builtin", "instructions")
	}
	if b, ok := builtin.(string); ok {
		f.Builtin = b
	}
	if v, ok := s.field(loc, o, "name", true); ok {
		f.Name, _ = s.identifier(at(loc, "name"), v)
	}
	if v, ok := o.Get("instructions"); ok {
		f.Instructions, _ = s.str(at(loc, "instructions"), v, false)
	}
	return f, true
}
