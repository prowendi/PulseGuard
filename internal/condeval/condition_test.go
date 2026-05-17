package condeval

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    Condition
		wantErr bool
	}{
		{name: "empty string is zero condition", src: "", want: Condition{}},
		{name: "whitespace only is zero condition", src: "   ", want: Condition{}},
		{name: "eq", src: "level eq critical", want: Condition{Field: "level", Op: OpEq, Value: "critical"}},
		{name: "ne", src: "level ne info", want: Condition{Field: "level", Op: OpNe, Value: "info"}},
		{name: "in", src: "level in critical,fatal", want: Condition{Field: "level", Op: OpIn, Value: "critical,fatal"}},
		{name: "startswith", src: "host startswith db-", want: Condition{Field: "host", Op: OpStartsWith, Value: "db-"}},
		{name: "endswith", src: "host endswith .prod", want: Condition{Field: "host", Op: OpEndsWith, Value: ".prod"}},
		{name: "contains", src: "msg contains panic", want: Condition{Field: "msg", Op: OpContains, Value: "panic"}},
		{name: "gt", src: "value gt 90", want: Condition{Field: "value", Op: OpGt, Value: "90"}},
		{name: "lt", src: "value lt 10", want: Condition{Field: "value", Op: OpLt, Value: "10"}},
		{name: "ge", src: "value ge 50", want: Condition{Field: "value", Op: OpGe, Value: "50"}},
		{name: "le", src: "value le 50", want: Condition{Field: "value", Op: OpLe, Value: "50"}},
		{name: "op case is normalised", src: "level EQ critical", want: Condition{Field: "level", Op: OpEq, Value: "critical"}},
		{name: "value with spaces survives", src: "msg contains hello world", want: Condition{Field: "msg", Op: OpContains, Value: "hello world"}},
		{name: "extra leading spaces", src: "  level eq critical", want: Condition{Field: "level", Op: OpEq, Value: "critical"}},
		{name: "missing value rejected", src: "level eq", wantErr: true},
		{name: "single token rejected", src: "level", wantErr: true},
		{name: "unknown op rejected", src: "level zz critical", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.src)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestEmptyConditionAlwaysMatches(t *testing.T) {
	c := Condition{}
	if !c.IsEmpty() {
		t.Fatalf("zero condition should be IsEmpty")
	}
	if !c.Match(nil) {
		t.Fatalf("empty condition with nil payload must match")
	}
	if !c.Match(map[string]any{"x": 1}) {
		t.Fatalf("empty condition must match any payload")
	}
}

func TestMatchEq(t *testing.T) {
	c := mustParse(t, "level eq critical")
	if !c.Match(map[string]any{"level": "critical"}) {
		t.Fatalf("string eq should match")
	}
	if !c.Match(map[string]any{"level": "Critical"}) {
		t.Fatalf("eq should be case-insensitive")
	}
	if c.Match(map[string]any{"level": "info"}) {
		t.Fatalf("eq should not match different value")
	}
	if c.Match(map[string]any{}) {
		t.Fatalf("missing field must not match eq")
	}
}

func TestMatchEqNumeric(t *testing.T) {
	c := mustParse(t, "value eq 90")
	// JSON numbers decode to float64.
	if !c.Match(map[string]any{"value": float64(90)}) {
		t.Fatalf("float64 90 should eq 90")
	}
	if !c.Match(map[string]any{"value": 90}) {
		t.Fatalf("int 90 should eq 90")
	}
	if !c.Match(map[string]any{"value": "90"}) {
		t.Fatalf("string 90 should eq 90 via numeric coercion")
	}
	if !c.Match(map[string]any{"value": 90.0}) {
		t.Fatalf("90.0 should eq 90 (trailing zeros stripped)")
	}
	if c.Match(map[string]any{"value": 91}) {
		t.Fatalf("91 must not eq 90")
	}
}

func TestMatchNe(t *testing.T) {
	c := mustParse(t, "level ne info")
	if !c.Match(map[string]any{"level": "critical"}) {
		t.Fatalf("ne should match different value")
	}
	if c.Match(map[string]any{"level": "info"}) {
		t.Fatalf("ne should not match same value")
	}
	// Missing field: ne holds (absence cannot equal anything).
	if !c.Match(map[string]any{}) {
		t.Fatalf("ne with missing field should match")
	}
}

func TestMatchIn(t *testing.T) {
	c := mustParse(t, "level in critical,fatal,error")
	for _, v := range []string{"critical", "fatal", "error", "Critical"} {
		if !c.Match(map[string]any{"level": v}) {
			t.Fatalf("in should match %q", v)
		}
	}
	if c.Match(map[string]any{"level": "info"}) {
		t.Fatalf("in should not match value outside the list")
	}
	// Numeric in.
	num := mustParse(t, "code in 200,404,500")
	if !num.Match(map[string]any{"code": float64(404)}) {
		t.Fatalf("numeric in should match 404")
	}
	if num.Match(map[string]any{"code": float64(301)}) {
		t.Fatalf("numeric in should not match 301")
	}
}

func TestMatchStringOps(t *testing.T) {
	if !mustParse(t, "host startswith db-").Match(map[string]any{"host": "db-prod-01"}) {
		t.Fatalf("startswith should match")
	}
	if !mustParse(t, "host startswith DB-").Match(map[string]any{"host": "db-prod-01"}) {
		t.Fatalf("startswith should be case-insensitive")
	}
	if mustParse(t, "host startswith db-").Match(map[string]any{"host": "web-01"}) {
		t.Fatalf("startswith should not match web-01")
	}
	if !mustParse(t, "host endswith .prod").Match(map[string]any{"host": "db.prod"}) {
		t.Fatalf("endswith should match")
	}
	if !mustParse(t, "msg contains panic").Match(map[string]any{"msg": "runtime PANIC inside loop"}) {
		t.Fatalf("contains should be case-insensitive")
	}
	if mustParse(t, "msg contains panic").Match(map[string]any{"msg": "all good"}) {
		t.Fatalf("contains should not match unrelated text")
	}
}

func TestMatchNumericComparisons(t *testing.T) {
	tests := []struct {
		src     string
		payload map[string]any
		want    bool
	}{
		{"value gt 90", map[string]any{"value": float64(91)}, true},
		{"value gt 90", map[string]any{"value": float64(90)}, false},
		{"value gt 90", map[string]any{"value": "91"}, true}, // string coercion
		{"value lt 10", map[string]any{"value": float64(9)}, true},
		{"value lt 10", map[string]any{"value": float64(10)}, false},
		{"value ge 50", map[string]any{"value": float64(50)}, true},
		{"value ge 50", map[string]any{"value": float64(49)}, false},
		{"value le 50", map[string]any{"value": float64(50)}, true},
		{"value le 50", map[string]any{"value": float64(51)}, false},
		{"value gt 90", map[string]any{"value": "not-a-number"}, false}, // non-numeric -> false
		{"value gt 90", map[string]any{}, false},                        // missing field -> false
	}
	for _, tc := range tests {
		c := mustParse(t, tc.src)
		if got := c.Match(tc.payload); got != tc.want {
			t.Fatalf("%q vs %+v: got %v want %v", tc.src, tc.payload, got, tc.want)
		}
	}
}

func TestStringifyVariousTypes(t *testing.T) {
	// bool / int / nil all flow through stringify; via eq we can assert
	// they produce the textual form callers expect.
	if !mustParse(t, "ok eq true").Match(map[string]any{"ok": true}) {
		t.Fatalf("bool true should stringify to \"true\"")
	}
	if !mustParse(t, "ok eq false").Match(map[string]any{"ok": false}) {
		t.Fatalf("bool false should stringify to \"false\"")
	}
	if !mustParse(t, "count eq 7").Match(map[string]any{"count": int64(7)}) {
		t.Fatalf("int64 7 should match eq 7")
	}
}

func TestUnknownOpAfterConstructionIsFalse(t *testing.T) {
	// Defensive: an op manually mutated to garbage must not panic or
	// silently match — Match returns false.
	c := Condition{Field: "x", Op: "bogus", Value: "v"}
	if c.Match(map[string]any{"x": "v"}) {
		t.Fatalf("unknown op should not match")
	}
}

func mustParse(t *testing.T, s string) Condition {
	t.Helper()
	c, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return c
}
