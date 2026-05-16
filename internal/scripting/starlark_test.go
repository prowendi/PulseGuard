package scripting

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustExec(t *testing.T, e *Executor, code string, args []string) *Result {
	t.Helper()
	res, err := e.Execute(context.Background(), code, args)
	if err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	return res
}

func TestExecute_HandleReturnsString(t *testing.T) {
	e := &Executor{}
	res := mustExec(t, e, `def handle(args): return "hello " + args[0]`, []string{"world"})
	if res.Return != "hello world" {
		t.Fatalf("Return = %q, want %q", res.Return, "hello world")
	}
	if res.Output != "" {
		t.Fatalf("Output should be empty, got %q", res.Output)
	}
}

func TestExecute_PrintCollected(t *testing.T) {
	e := &Executor{}
	res := mustExec(t, e, `
def handle(args):
    print("hi")
    print("there")
    return "done"
`, nil)
	if !strings.Contains(res.Output, "hi") || !strings.Contains(res.Output, "there") {
		t.Fatalf("Output = %q, want both 'hi' and 'there'", res.Output)
	}
	if res.Return != "done" {
		t.Fatalf("Return = %q", res.Return)
	}
}

func TestExecute_ArgsPassedAsList(t *testing.T) {
	e := &Executor{}
	res := mustExec(t, e, `
def handle(args):
    return ":".join(args)
`, []string{"a", "b", "c"})
	if res.Return != "a:b:c" {
		t.Fatalf("Return = %q", res.Return)
	}
}

func TestExecute_HandleMissing(t *testing.T) {
	e := &Executor{}
	_, err := e.Execute(context.Background(), `x = 1`, nil)
	if !errors.Is(err, ErrMissingHandle) {
		t.Fatalf("err = %v, want ErrMissingHandle", err)
	}
}

func TestExecute_TimeoutInfiniteLoop(t *testing.T) {
	e := &Executor{Timeout: 500 * time.Millisecond}
	start := time.Now()
	// Starlark forbids while loops with our FileOptions; use recursion
	// via a list comprehension that the step cap will hit eventually.
	// But the wall-clock cancel must trip first.
	_, err := e.Execute(context.Background(), `
def loop(n):
    for i in range(n):
        for j in range(n):
            pass
def handle(args):
    loop(10000000)
    return "done"
`, nil)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("execute should bail within 2s, took %v", elapsed)
	}
}

func TestExecute_JSONRoundtrip(t *testing.T) {
	e := &Executor{}
	res := mustExec(t, e, `
def handle(args):
    obj = {"name": "alice", "n": 42, "items": [1, 2, 3]}
    s = json.encode(obj)
    back = json.decode(s)
    return back["name"] + ":" + str(back["n"]) + ":" + str(back["items"][2])
`, nil)
	if res.Return != "alice:42:3" {
		t.Fatalf("Return = %q, want alice:42:3", res.Return)
	}
}

func TestExecute_TimeNow(t *testing.T) {
	e := &Executor{}
	res := mustExec(t, e, `def handle(args): return time.now()`, nil)
	if _, err := time.Parse(time.RFC3339, res.Return); err != nil {
		t.Fatalf("time.now() returned %q which is not RFC3339: %v", res.Return, err)
	}
}

func TestExecute_LoadDisabled(t *testing.T) {
	e := &Executor{}
	_, err := e.Execute(context.Background(), `
load("anywhere.star", "x")
def handle(args):
    return "nope"
`, nil)
	if err == nil {
		t.Fatalf("expected error from load()")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "load") {
		// Starlark may report it as a syntax error if load is disabled
		// at parse time — accept either path so long as it fails.
		t.Logf("load denied via: %v", err)
	}
}

func TestExecute_OutputCapExceeded(t *testing.T) {
	e := &Executor{OutputCap: 32}
	res, err := e.Execute(context.Background(), `
def handle(args):
    for i in range(1000):
        print("line that is going to overflow the cap")
    return "done"
`, nil)
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("err = %v, want ErrOutputTooLarge", err)
	}
	if res == nil || len(res.Output) > 32 {
		t.Fatalf("Output should be truncated to <=32 bytes, got %d bytes", len(res.Output))
	}
}

func TestExecute_NonStringReturnCoerced(t *testing.T) {
	e := &Executor{}
	res := mustExec(t, e, `def handle(args): return 7`, nil)
	if res.Return != "7" {
		t.Fatalf("Return = %q, want 7", res.Return)
	}
}

func TestExecute_NoneReturnEmpty(t *testing.T) {
	e := &Executor{}
	res := mustExec(t, e, `def handle(args): return None`, nil)
	if res.Return != "" {
		t.Fatalf("Return = %q, want empty for None", res.Return)
	}
}

func TestExecute_NoFileIO(t *testing.T) {
	e := &Executor{}
	// `open` is not part of Starlark's standard environment; any
	// attempt to use it must fail at compile-time as a NameError.
	_, err := e.Execute(context.Background(), `
def handle(args):
    f = open("/etc/passwd")
    return f.read()
`, nil)
	if err == nil {
		t.Fatalf("expected open() to be denied")
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "open") && !strings.Contains(low, "undefined") {
		t.Fatalf("expected NameError-ish message, got %v", err)
	}
}

func TestExecute_NoOSEnviron(t *testing.T) {
	e := &Executor{}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return os.environ
`, nil)
	if err == nil {
		t.Fatalf("expected os.environ to be denied")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "os") {
		t.Fatalf("expected os-undefined error, got %v", err)
	}
}

func TestExecute_HTTPModuleAbsentWhenNoHTTP(t *testing.T) {
	e := &Executor{}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("https://example.com")
`, nil)
	if err == nil {
		t.Fatalf("expected http to be undefined when HTTP nil")
	}
}
