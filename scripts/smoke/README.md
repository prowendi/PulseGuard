# scripts/smoke — Real-Network Telegram Smoke Test

A tiny CLI that exercises `internal/tg.Client` against the live Telegram
Bot API to confirm a bot token is wired correctly and can deliver
messages to a target chat.

The unit tests in `internal/tg/` already cover error classification with
a stubbed `httptest.Server`. This smoke is for the human-in-the-loop
sanity check before flipping a production channel live.

## Prerequisites

1. A Telegram bot — talk to [@BotFather](https://t.me/BotFather) and
   take note of the `<bot-token>`.
2. From your Telegram client, send any message (e.g. `/start`) to the
   bot. The bot needs to have received at least one update before it
   can list candidate chat ids.

## Usage

Set the token in your shell:

```bash
export PULSEGUARD_SMOKE_BOT_TOKEN="123456:AAH-yourTokenHere"
```

Step 1 — discover the chat id (omit `-chat-id`):

```bash
go run ./scripts/smoke -bot-token "$PULSEGUARD_SMOKE_BOT_TOKEN"
```

The script prints the most recent 5 chat ids your bot has interacted
with. Pick one and re-run with `-chat-id`:

```bash
export PULSEGUARD_SMOKE_CHAT_ID="123456789"
make smoke
```

Or, when you already know the chat id:

```bash
go run ./scripts/smoke \
  -bot-token "$PULSEGUARD_SMOKE_BOT_TOKEN" \
  -chat-id   "$PULSEGUARD_SMOKE_CHAT_ID"
```

## Expected output

```text
step 1: getMe OK — bot @your_bot_name (id=123456, name="Your Bot")
step 3: sendMessage OK — chat_id=123456789 message_id=42 text="PulseGuard smoke test @ 2026-05-17T10:11:12Z"
smoke: ALL STEPS OK
```

A new message arrives in your Telegram chat with the timestamped marker.
The exit code is `0` on success, `1` on any failure (token invalid,
network down, no updates yet, send refused by Telegram, etc.).
