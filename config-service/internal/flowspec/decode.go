// Package flowspec checks flow definitions (schema version 1).
//
// It is a port of flow_engine/spec.py in the voice agent. Both must report the
// same problems for the same flow; tests/flow_conformance.json (generated from
// the Python validator) holds the cases both test suites check.
package flowspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Object is a JSON object that keeps its keys in file order. Python dicts keep
// order too, and some messages ("already defined at ...") depend on it.
type Object struct {
	Keys   []string
	Values map[string]any
}

// Get returns the value for key and whether the key is present.
func (o *Object) Get(key string) (any, bool) {
	v, ok := o.Values[key]
	return v, ok
}

// Decode parses JSON into nil, bool, string, json.Number, []any and *Object.
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil // nil, bool, string or json.Number
	}
	switch delim {
	case '{':
		obj := &Object{Values: map[string]any{}}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := keyTok.(string)
			val, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			// A repeated key keeps its first position and its last value, like Python.
			if _, seen := obj.Values[key]; !seen {
				obj.Keys = append(obj.Keys, key)
			}
			obj.Values[key] = val
		}
		if _, err := dec.Token(); err != nil { // closing }
			return nil, err
		}
		return obj, nil
	case '[':
		list := []any{}
		for dec.More() {
			val, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, val)
		}
		if _, err := dec.Token(); err != nil { // closing ]
			return nil, err
		}
		return list, nil
	}
	return nil, fmt.Errorf("unexpected %q", delim)
}
