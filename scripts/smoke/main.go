// Command smoke runs a real-network sanity check against the Telegram
// Bot API. It reuses internal/tg.Client for the SendMessage call so we
// exercise the same code path the worker uses in production.
//
// Three steps:
//  1. GET getMe — verifies the bot token is valid and prints the bot
//     username (helpful to confirm you wired up the right bot).
//  2. If -chat-id is empty, GET getUpdates and list the chat_ids from
//     the last 5 updates so the operator can pick one. We exit non-zero
//     when no updates exist (user must DM the bot first).
//  3. POST sendMessage with a timestamped marker text. On success we
//     print the resulting message_id.
//
// Inputs:
//   -bot-token / PULSEGUARD_SMOKE_BOT_TOKEN
//   -chat-id   / PULSEGUARD_SMOKE_CHAT_ID
//   -base-url  (default https://api.telegram.org)
//
// Exit code is 0 on success, 1 on any failure (auth, network, no
// updates, send refused, etc.). This script is intended to live in
// scripts/ and is excluded from the production build.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prowendi/PulseGuard/internal/config"
	"github.com/prowendi/PulseGuard/internal/tg"
)

func main() {
	botToken := flag.String("bot-token", os.Getenv("PULSEGUARD_SMOKE_BOT_TOKEN"),
		"Telegram bot token (or env PULSEGUARD_SMOKE_BOT_TOKEN)")
	chatID := flag.String("chat-id", os.Getenv("PULSEGUARD_SMOKE_CHAT_ID"),
		"Target chat id (or env PULSEGUARD_SMOKE_CHAT_ID). If empty we list recent updates and exit.")
	baseURL := flag.String("base-url", "https://api.telegram.org",
		"Telegram API base (override for local mocks)")
	flag.Parse()

	if *botToken == "" {
		fmt.Fprintln(os.Stderr, "error: -bot-token or PULSEGUARD_SMOKE_BOT_TOKEN required")
		os.Exit(1)
	}
	base := strings.TrimRight(*baseURL, "/")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── Step 1: getMe ──────────────────────────────────────────────
	me, err := callGetMe(ctx, base, *botToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "getMe failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("step 1: getMe OK — bot @%s (id=%d, name=%q)\n", me.Username, me.ID, me.FirstName)

	// ── Step 2: if no chat-id, list recent chats ────────────────────
	if *chatID == "" {
		ups, err := callGetUpdates(ctx, base, *botToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "getUpdates failed: %v\n", err)
			os.Exit(1)
		}
		if len(ups) == 0 {
			fmt.Fprintln(os.Stderr, "no recent updates — send '/start' or any message to the bot from your Telegram client, then re-run this script.")
			os.Exit(1)
		}
		fmt.Println("step 2: recent updates (pick one chat_id and pass via -chat-id):")
		limit := 5
		if len(ups) < limit {
			limit = len(ups)
		}
		for i := len(ups) - limit; i < len(ups); i++ {
			u := ups[i]
			fmt.Printf("   chat_id=%d type=%s title=%q from=%q text=%q\n",
				u.Message.Chat.ID, u.Message.Chat.Type, u.Message.Chat.Title,
				u.Message.From.Username, truncate(u.Message.Text, 60))
		}
		fmt.Fprintln(os.Stderr, "exit 1: -chat-id not supplied")
		os.Exit(1)
	}

	// ── Step 3: send the smoke message via internal/tg ──────────────
	client := tg.New(config.Telegram{
		APIBase:     base,
		HTTPTimeout: config.Duration(10 * time.Second),
	})
	stamp := time.Now().UTC().Format(time.RFC3339)
	text := fmt.Sprintf("PulseGuard smoke test @ %s", stamp)
	msgID, err := client.Send(ctx, *botToken, *chatID, "None", text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sendMessage failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("step 3: sendMessage OK — chat_id=%s message_id=%d text=%q\n", *chatID, msgID, text)
	fmt.Println("smoke: ALL STEPS OK")
}

// ── Telegram REST plumbing (kept minimal; only what smoke needs) ─────

type tgEnvelope struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

type meResult struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	IsBot     bool   `json:"is_bot"`
}

type updateResult struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		From struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
		Chat struct {
			ID    int64  `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

func callGetMe(ctx context.Context, base, token string) (*meResult, error) {
	u := fmt.Sprintf("%s/bot%s/getMe", base, url.PathEscape(token))
	body, err := httpJSON(ctx, u)
	if err != nil {
		return nil, err
	}
	var env tgEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse envelope: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("api returned ok=false: %s", env.Description)
	}
	var me meResult
	if err := json.Unmarshal(env.Result, &me); err != nil {
		return nil, fmt.Errorf("parse result: %w", err)
	}
	if !me.IsBot {
		return nil, fmt.Errorf("token does not identify a bot (is_bot=false)")
	}
	return &me, nil
}

func callGetUpdates(ctx context.Context, base, token string) ([]updateResult, error) {
	u := fmt.Sprintf("%s/bot%s/getUpdates", base, url.PathEscape(token))
	body, err := httpJSON(ctx, u)
	if err != nil {
		return nil, err
	}
	var env tgEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse envelope: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("api returned ok=false: %s", env.Description)
	}
	var ups []updateResult
	if err := json.Unmarshal(env.Result, &ups); err != nil {
		return nil, fmt.Errorf("parse result: %w", err)
	}
	return ups, nil
}

func httpJSON(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
