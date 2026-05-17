// Package condeval implements a tiny, dependency-free evaluator for a
// "field op value" mini-grammar used by channel template bindings to
// decide which template should render a given payload.
//
// Grammar (single expression, no boolean composition):
//
//	<field> <op> <value>
//
// Examples:
//
//	level eq critical
//	level in critical,fatal
//	host startswith db-
//	value gt 90
//
// An empty source string parses into a zero Condition whose Match always
// returns true, so callers can store the zero value as the "no condition
// — eligible as fallback / default" marker without special-casing nil.
//
// Operator semantics:
//
//	eq         string equality (case-insensitive) OR numeric equality
//	           when both sides parse as float64
//	ne         logical negation of eq
//	in         comma-separated list, eq against each element
//	startswith string HasPrefix (case-insensitive)
//	endswith   string HasSuffix (case-insensitive)
//	contains   string Contains  (case-insensitive)
//	gt,lt,ge,le numeric comparison; non-numeric operands evaluate to false
//
// The package intentionally has zero external dependencies (stdlib only:
// strings/strconv/fmt). It is consumed by the pipeline worker and the
// web layer; both must keep it that way to avoid pulling production
// dependencies into a hot path.
package condeval

import (
	"fmt"
	"strconv"
	"strings"
)

// Op is the enumerated operator vocabulary the evaluator understands.
type Op string

const (
	OpEq         Op = "eq"
	OpNe         Op = "ne"
	OpIn         Op = "in"
	OpStartsWith Op = "startswith"
	OpEndsWith   Op = "endswith"
	OpContains   Op = "contains"
	OpGt         Op = "gt"
	OpLt         Op = "lt"
	OpGe         Op = "ge"
	OpLe         Op = "le"
)

var knownOps = map[Op]bool{
	OpEq: true, OpNe: true, OpIn: true,
	OpStartsWith: true, OpEndsWith: true, OpContains: true,
	OpGt: true, OpLt: true, OpGe: true, OpLe: true,
}

// Condition is the parsed form of a "field op value" expression. The
// zero value is intentionally meaningful: an empty Field is treated as
// "no condition" and Match returns true.
type Condition struct {
	Field string
	Op    Op
	Value string
}

// IsEmpty reports whether the condition is the zero / fallback form.
// Callers that want "default-eligible" semantics check this before
// evaluating Match against a payload.
func (c Condition) IsEmpty() bool {
	return c.Field == "" && c.Op == "" && c.Value == ""
}

// Parse turns a source string into a Condition. Whitespace around each
// token is trimmed; the operator token is lower-cased so callers can
// store "EQ" or "Eq" in the database. An empty (or whitespace-only)
// input returns the zero Condition with no error — it is a valid
// "fallback / default" marker, not a parse failure.
func Parse(s string) (Condition, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Condition{}, nil
	}
	// Split into at most 3 tokens so the value is allowed to contain
	// spaces (e.g. `msg contains hello world`). Field and op are single
	// whitespace-delimited words.
	parts := strings.SplitN(s, " ", 3)
	// Tolerate multiple spaces between field and op by trimming the
	// second token; if it ends up empty we fall through to the error.
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Condition{}, fmt.Errorf("condeval: expected `<field> <op> <value>`, got %q", s)
	}
	op := Op(strings.ToLower(parts[1]))
	if !knownOps[op] {
		return Condition{}, fmt.Errorf("condeval: unknown op %q", parts[1])
	}
	return Condition{Field: parts[0], Op: op, Value: parts[2]}, nil
}

// Match evaluates the condition against payload. An empty / zero
// Condition always matches. A missing payload key evaluates to false
// for every operator except `ne` (which returns true — the field's
// absence is not equal to whatever value the operator demands).
func (c Condition) Match(payload map[string]any) bool {
	if c.IsEmpty() {
		return true
	}
	raw, ok := payload[c.Field]
	if !ok {
		// Missing field cannot equal anything; `ne` therefore holds.
		return c.Op == OpNe
	}
	left := stringify(raw)

	switch c.Op {
	case OpEq:
		return equals(left, c.Value)
	case OpNe:
		return !equals(left, c.Value)
	case OpIn:
		for _, want := range splitList(c.Value) {
			if equals(left, want) {
				return true
			}
		}
		return false
	case OpStartsWith:
		return strings.HasPrefix(strings.ToLower(left), strings.ToLower(c.Value))
	case OpEndsWith:
		return strings.HasSuffix(strings.ToLower(left), strings.ToLower(c.Value))
	case OpContains:
		return strings.Contains(strings.ToLower(left), strings.ToLower(c.Value))
	case OpGt, OpLt, OpGe, OpLe:
		ln, lok := toFloat(raw)
		rn, rok := toFloat64String(c.Value)
		if !lok || !rok {
			return false
		}
		switch c.Op {
		case OpGt:
			return ln > rn
		case OpLt:
			return ln < rn
		case OpGe:
			return ln >= rn
		case OpLe:
			return ln <= rn
		}
	}
	return false
}

// equals does case-insensitive string compare, but if BOTH sides parse
// as float64 it falls back to numeric equality so `value eq 90` matches
// the JSON number 90 (decoded as float64) as well as the literal "90".
func equals(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	if an, aok := toFloat64String(a); aok {
		if bn, bok := toFloat64String(b); bok {
			return an == bn
		}
	}
	return false
}

// splitList parses the right-hand side of an `in` operator. Surrounding
// whitespace on each element is trimmed; empty elements are dropped.
func splitList(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	return out
}

// stringify converts a payload value to its textual form using the same
// rules JSON decoding produces (numbers become float64, booleans become
// "true"/"false"). The float case strips trailing zeros so 90.0 prints
// as "90" — important so `level eq 90` and `level eq 90.0` agree.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 64)
	case int:
		return strconv.FormatInt(int64(t), 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint:
		return strconv.FormatUint(uint64(t), 10)
	case uint32:
		return strconv.FormatUint(uint64(t), 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// toFloat coerces a payload value to float64 for numeric comparisons.
// Strings are parsed via strconv.ParseFloat so a JSON string "90"
// participates in `gt`/`lt` against numbers.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case string:
		return toFloat64String(t)
	default:
		return 0, false
	}
}

// toFloat64String wraps strconv.ParseFloat so the rest of the package
// can call a single helper instead of repeating the import + signature.
func toFloat64String(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
