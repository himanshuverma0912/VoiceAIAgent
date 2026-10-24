package flowspec

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The same cases the Python validator is tested with (generated from it by
// tests/make_flow_conformance.py). Both must report exactly these problems.
func TestConformanceWithPythonValidator(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "flow_conformance.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	cases := decoded.([]any)
	if len(cases) < 50 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		c := c.(*Object)
		name := c.Values["name"].(string)
		var want []string
		for _, p := range c.Values["problems"].([]any) {
			want = append(want, p.(string))
		}
		t.Run(name, func(t *testing.T) {
			got := Validate(c.Values["flow"]).Problems
			sort.Strings(got)
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("problems differ\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

func TestRealFlowFilesAreValid(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "..", "flows", "*.json"))
	if len(files) == 0 {
		t.Fatal("no flow files found")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		res, err := ValidateJSON(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Problems) > 0 || res.FlowID == "" {
			t.Errorf("%s: %v", file, res.Problems)
		}
	}
}

func TestDecodeKeepsKeyOrderAndRejectsTrailingData(t *testing.T) {
	v, err := Decode([]byte(`{"b": 1, "a": {"y": 2, "x": 3}, "b": 4}`))
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*Object)
	if strings.Join(o.Keys, ",") != "b,a" || pyStr(o.Values["b"]) != "4" {
		t.Errorf("got keys %v, b=%v", o.Keys, o.Values["b"])
	}
	if inner := o.Values["a"].(*Object); strings.Join(inner.Keys, ",") != "y,x" {
		t.Errorf("inner keys %v", inner.Keys)
	}
	for _, bad := range []string{`{"a":1} x`, `{"a":1,}`, `{`, ``} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}
