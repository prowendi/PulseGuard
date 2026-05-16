package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedacts(t *testing.T) {
	var buf bytes.Buffer
	l := newForTest(&buf, "info", "json")
	l.Info("login attempt",
		"email", "x@y.com",
		"password", "hunter2",
		"bot_token", "12345:secret",
		"push_token", "abc-xyz",
		"master_key", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
	)
	out := buf.String()
	for _, leak := range []string{"hunter2", "12345:secret", "abc-xyz", "AAAAAAA"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q in log: %s", leak, out)
		}
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("missing REDACTED marker: %s", out)
	}
	// still valid JSON
	var any map[string]any
	if err := json.Unmarshal(buf.Bytes(), &any); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
}

func TestLevel(t *testing.T) {
	var buf bytes.Buffer
	l := newForTest(&buf, "warn", "text")
	l.Info("noise")
	l.Warn("danger")
	out := buf.String()
	if strings.Contains(out, "noise") {
		t.Fatalf("info should be filtered at warn level: %s", out)
	}
	if !strings.Contains(out, "danger") {
		t.Fatalf("warn missing: %s", out)
	}
}
