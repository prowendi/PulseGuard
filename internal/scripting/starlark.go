package scripting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Sentinel errors. Callers (e.g. the Telegram listener) map these to
// user-friendly messages so Starlark internals never leak to the chat.
var (
	// ErrMissingHandle means the script either failed to define a
	// top-level `handle` function or the symbol is not callable.
	ErrMissingHandle = errors.New("scripting: handle(args) function missing")
	// ErrTimeout means the script ran beyond Executor.Timeout and was
	// cancelled. Both compile-loop and run-loop hit this path.
	ErrTimeout = errors.New("scripting: execution timeout")
	// ErrOutputTooLarge means print() output exceeded Executor.OutputCap.
	// The truncated prefix is still returned via Result.Output.
	ErrOutputTooLarge = errors.New("scripting: output exceeded cap")
)

// Default tuning constants. Callers may override on the Executor struct.
const (
	defaultTimeout   = 10 * time.Second
	defaultOutputCap = 8 * 1024
)

// Executor runs short-lived Starlark scripts inside a hardened sandbox:
//
//   - no load(), no module imports, no file/exec/socket access
//   - timeout via Thread.Cancel + context deadline
//   - bounded print() output (truncated at OutputCap with ErrOutputTooLarge)
//   - injected helpers: `time` (now), `json` (encode/decode), `print`,
//     and (when HTTP is set) `http` with SSRF-guarded get/post
//
// The Execute method is safe for concurrent use — each call builds its
// own Thread and globals.
type Executor struct {
	// Timeout bounds total script wall-clock time. Defaults to 10 s.
	Timeout time.Duration
	// OutputCap is the maximum number of bytes captured from print().
	// Defaults to 8 KiB. Excess data triggers ErrOutputTooLarge.
	OutputCap int
	// HTTP, when non-nil, exposes the `http` module to scripts. When
	// nil, scripts referencing `http` get a NameError at compile time.
	HTTP *HTTPClient
}

// Result wraps the output of a successful Execute call.
type Result struct {
	// Output is the (possibly truncated) concatenation of every print()
	// statement, joined with newlines exactly as the script produced.
	Output string
	// Return is the return value of handle(args), stringified.
	Return string
}

// Execute compiles `code`, finds the top-level `handle` function, and
// calls it with `args` (each token wrapped as a Starlark string). The
// returned Result holds the script's print() output and the stringified
// return value of handle().
func (e *Executor) Execute(ctx context.Context, code string, args []string) (*Result, error) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	cap := e.OutputCap
	if cap <= 0 {
		cap = defaultOutputCap
	}

	// Per-call output buffer with cap enforcement. printSink.write is
	// invoked by the Starlark print hook below.
	sink := &printSink{cap: cap}

	// Hard deadline + cancel-on-timeout via Thread.Cancel.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	thread := &starlark.Thread{
		Name: "pulseguard-command",
		Print: func(_ *starlark.Thread, msg string) {
			sink.write(msg)
		},
		// Disable module loading entirely — every load() call fails
		// with this error so users cannot pull in sibling files.
		Load: func(_ *starlark.Thread, module string) (starlark.StringDict, error) {
			return nil, fmt.Errorf("scripting: load() is disabled (module %q)", module)
		},
	}

	// Cap CPU steps so a script cannot lock a worker indefinitely with
	// pure-Go arithmetic. 100M ops ≈ <1s of starlark interpreter work
	// on a laptop class CPU; the wall-clock timeout below is the real
	// safety net.
	thread.SetMaxExecutionSteps(100_000_000)

	// Cancel-on-timeout: spawn a watcher that pokes thread.Cancel as
	// soon as runCtx deadlines. The watcher exits when execution
	// returns (done channel closes).
	done := make(chan struct{})
	var timedOut atomicBool
	go func() {
		select {
		case <-runCtx.Done():
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				timedOut.Set(true)
			}
			thread.Cancel("scripting timeout")
		case <-done:
		}
	}()
	defer close(done)

	// Locked-down predeclared globals: only the helpers we explicitly
	// allow are visible. No `os`, no `open`, no `io`.
	predeclared := starlark.StringDict{
		"time": timeModule(),
		"json": jsonModule(),
	}
	if e.HTTP != nil {
		predeclared["http"] = e.HTTP.Module(runCtx)
	}

	// Compile + execute the top-level program. ExecFileOptions with
	// strict syntax options prevents Starlark "while" / "recursion"
	// features that could otherwise bypass the step cap.
	opts := &syntax.FileOptions{
		Set:             false,
		While:           false,
		TopLevelControl: false,
		GlobalReassign:  false,
		Recursion:       false,
	}
	globals, err := starlark.ExecFileOptions(opts, thread, "command.star", code, predeclared)
	if err != nil {
		if timedOut.Get() {
			return nil, ErrTimeout
		}
		return nil, classifyErr(err)
	}

	fn, ok := globals["handle"].(starlark.Callable)
	if !ok {
		return nil, ErrMissingHandle
	}

	// Build argv: each string argument wrapped as starlark.String,
	// joined into a single tuple positional arg.
	argv := make([]starlark.Value, 0, len(args))
	for _, a := range args {
		argv = append(argv, starlark.String(a))
	}
	argList := starlark.NewList(argv)
	ret, err := starlark.Call(thread, fn, starlark.Tuple{argList}, nil)
	if err != nil {
		if timedOut.Get() {
			return nil, ErrTimeout
		}
		return nil, classifyErr(err)
	}

	out := &Result{Output: sink.Output()}
	if ret != nil && ret.Type() != "NoneType" {
		switch v := ret.(type) {
		case starlark.String:
			out.Return = v.GoString()
		default:
			out.Return = ret.String()
		}
	}
	// If the sink overflowed, report both the truncated output and
	// the sentinel error.
	if sink.overflowed() {
		return out, ErrOutputTooLarge
	}
	return out, nil
}

// classifyErr maps starlark.EvalError and friends to our public
// sentinels so callers can branch on the error type without parsing
// strings. Anything we don't recognise is returned verbatim.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	// SSRF / HTTP sentinels surfaced as starlark errors must propagate
	// unchanged so the listener can render the right friendly message.
	if errors.Is(err, ErrUnsafeHost) {
		return err
	}
	if errors.Is(err, ErrUnsupportedScheme) {
		return err
	}
	// Starlark surfaces cancellations as EvalError("cancelled by ...");
	// we treat them as ErrTimeout because that is the only way we
	// trigger Cancel today.
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "cancelled") {
		return ErrTimeout
	}
	return err
}

// printSink collects print() output up to cap bytes.
type printSink struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	cap      int
	overflow bool
}

func (s *printSink) write(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	remaining := s.cap - s.buf.Len()
	if remaining <= 0 {
		s.overflow = true
		return
	}
	// Reserve room for the trailing newline.
	chunk := msg
	if len(chunk)+1 > remaining {
		s.overflow = true
		chunk = chunk[:max0(remaining-1, 0)]
	}
	if s.buf.Len() > 0 {
		s.buf.WriteByte('\n')
	}
	s.buf.WriteString(chunk)
}

func (s *printSink) Output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *printSink) overflowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overflow
}

func max0(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// atomicBool is a lightweight, lock-free flag used by the timeout
// watcher to flag a Cancel-induced abort.
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) Set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) Get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

// timeModule exposes a single now() function returning RFC3339 UTC.
// The returned value is a starlark.Value with HasAttrs so user code
// can write `time.now()` and the interpreter resolves through Attr.
func timeModule() starlark.Value {
	return &simpleModule{
		name: "time",
		attrs: map[string]*starlark.Builtin{
			"now": starlark.NewBuiltin("time.now", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
				return starlark.String(time.Now().UTC().Format(time.RFC3339)), nil
			}),
		},
	}
}

// jsonModule exposes encode/decode with native Go encoding/json. We
// roundtrip via Go interface{} so user data can move between Starlark
// and JSON cleanly.
func jsonModule() starlark.Value {
	encode := starlark.NewBuiltin("json.encode", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) != 1 || len(kwargs) != 0 {
			return nil, fmt.Errorf("encode: want 1 positional arg, got %d", len(args))
		}
		v, err := starlarkToGo(args[0])
		if err != nil {
			return nil, fmt.Errorf("encode: %w", err)
		}
		bs, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode: %w", err)
		}
		return starlark.String(string(bs)), nil
	})
	decode := starlark.NewBuiltin("json.decode", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) != 1 || len(kwargs) != 0 {
			return nil, fmt.Errorf("decode: want 1 positional arg, got %d", len(args))
		}
		s, ok := args[0].(starlark.String)
		if !ok {
			return nil, fmt.Errorf("decode: want string, got %s", args[0].Type())
		}
		var v any
		if err := json.Unmarshal([]byte(s.GoString()), &v); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		return goToStarlark(v)
	})
	return &simpleModule{
		name:  "json",
		attrs: map[string]*starlark.Builtin{"encode": encode, "decode": decode},
	}
}

// simpleModule is the receiver type for read-only modules whose only
// purpose is to expose a fixed set of attribute-named builtins. Used
// by the time/json modules above.
type simpleModule struct {
	name  string
	attrs map[string]*starlark.Builtin
}

func (m *simpleModule) String() string        { return "<module " + m.name + ">" }
func (m *simpleModule) Type() string          { return m.name + "_module" }
func (m *simpleModule) Freeze()               {}
func (m *simpleModule) Truth() starlark.Bool  { return starlark.True }
func (m *simpleModule) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: %s", m.Type()) }
func (m *simpleModule) Attr(name string) (starlark.Value, error) {
	if v, ok := m.attrs[name]; ok {
		return v, nil
	}
	return nil, nil
}
func (m *simpleModule) AttrNames() []string {
	out := make([]string, 0, len(m.attrs))
	for k := range m.attrs {
		out = append(out, k)
	}
	return out
}

// starlarkToGo / goToStarlark convert the value graphs used by
// json.encode/decode. We support the common cases (string, int, float,
// bool, list, dict, none).
func starlarkToGo(v starlark.Value) (any, error) {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(x), nil
	case starlark.String:
		return x.GoString(), nil
	case starlark.Int:
		if i, ok := x.Int64(); ok {
			return i, nil
		}
		// Fall back to a string for out-of-range bignums so json.encode
		// stays lossless.
		return x.String(), nil
	case starlark.Float:
		return float64(x), nil
	case *starlark.List:
		out := make([]any, 0, x.Len())
		for i := 0; i < x.Len(); i++ {
			gv, err := starlarkToGo(x.Index(i))
			if err != nil {
				return nil, err
			}
			out = append(out, gv)
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, 0, len(x))
		for _, item := range x {
			gv, err := starlarkToGo(item)
			if err != nil {
				return nil, err
			}
			out = append(out, gv)
		}
		return out, nil
	case *starlark.Dict:
		out := make(map[string]any, x.Len())
		for _, item := range x.Items() {
			k, ok := item[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("json keys must be string, got %s", item[0].Type())
			}
			gv, err := starlarkToGo(item[1])
			if err != nil {
				return nil, err
			}
			out[k.GoString()] = gv
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported starlark type: %s", v.Type())
}

func goToStarlark(v any) (starlark.Value, error) {
	switch x := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(x), nil
	case string:
		return starlark.String(x), nil
	case float64:
		// JSON numbers are float64 by default; coerce integers when
		// possible so user code can `args[0] == 1` semantics survive.
		if x == float64(int64(x)) {
			return starlark.MakeInt64(int64(x)), nil
		}
		return starlark.Float(x), nil
	case int64:
		return starlark.MakeInt64(x), nil
	case []any:
		items := make([]starlark.Value, 0, len(x))
		for _, item := range x {
			sv, err := goToStarlark(item)
			if err != nil {
				return nil, err
			}
			items = append(items, sv)
		}
		return starlark.NewList(items), nil
	case map[string]any:
		d := starlark.NewDict(len(x))
		for k, v := range x {
			sv, err := goToStarlark(v)
			if err != nil {
				return nil, err
			}
			_ = d.SetKey(starlark.String(k), sv)
		}
		return d, nil
	}
	return nil, fmt.Errorf("unsupported go type: %T", v)
}
