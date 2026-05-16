package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTemplatesAPILifecycle(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Create.
	body := mustJSON(t, map[string]any{
		"name":       "alert-tpl",
		"parse_mode": "MarkdownV2",
		"body":       "*{{ .title }}* host={{ .host }}",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/templates"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created templateView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == 0 {
		t.Fatal("id is zero")
	}

	// List.
	resp, err = client.Get(h.fullURL("/api/v1/templates"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if items, _ := list["items"].([]any); len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	// Update parse_mode.
	upd := mustJSON(t, map[string]any{"parse_mode": "HTML"})
	req, _ = http.NewRequest(http.MethodPut, h.fullURL("/api/v1/templates/"+strInt64(created.ID)), bytes.NewReader(upd))
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()

	// Delete.
	req, _ = http.NewRequest(http.MethodDelete, h.fullURL("/api/v1/templates/"+strInt64(created.ID)), nil)
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTemplatesAPIRejectsBadBody(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Unbalanced braces — parse should fail at create time.
	body := mustJSON(t, map[string]any{
		"name":       "bad",
		"parse_mode": "None",
		"body":       "{{ .title ",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/templates"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTemplatesPreviewRenders(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"body":       "Hello {{ .name }}!",
		"parse_mode": "None",
		"sample":     map[string]any{"name": "Wendi"},
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/templates/preview"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["rendered"] != "Hello Wendi!" {
		t.Fatalf("rendered = %v", out["rendered"])
	}
}

func TestTemplatesPreviewSurfacesError(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Reference a function that is not in FuncMap → parse error.
	body := mustJSON(t, map[string]any{
		"body":       "{{ nosuchfunc .name }}",
		"parse_mode": "None",
		"sample":     map[string]any{"name": "x"},
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/templates/preview"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUITemplatesRenders(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	resp, err := client.Get(h.fullURL("/ui/templates"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bs), "消息模板") {
		t.Fatalf("body missing heading: %s", string(bs)[:200])
	}
}
