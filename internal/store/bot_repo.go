package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	"github.com/wendi/pulseguard/internal/domain"
)

// BotRepo persists tenant-owned bots. The plaintext bot_token is
// encrypted on write and decrypted on read using the injected Cipher.
type BotRepo struct {
	db     *sql.DB
	clock  domain.Clock
	cipher *Cipher
}

// NewBotRepo binds the repo to a DB handle, clock, and cipher.
func NewBotRepo(db *sql.DB, clock domain.Clock, cipher *Cipher) *BotRepo {
	return &BotRepo{db: db, clock: clock, cipher: cipher}
}

// Insert writes a new bot row, encrypting the token. An empty Platform
// defaults to PlatformTelegram so callers (and existing tests) can omit
// the field; invalid values are rejected. Enabled is back-filled to
// true when the caller leaves the zero value (false) so freshly-created
// bots match the column DEFAULT and the listener spawns immediately —
// callers that want a paused bot must SetEnabled(false) after Insert
// or pre-populate Enabled=false AND tolerate the listener being absent
// until the operator re-enables.
//
// For Lark application bots (Platform="lark", BotKind="app") the
// BotToken column carries a derived URL of the form
// "lark-app://<app_id>?secret=<plaintext-secret>" assembled in
// scanBot on read. On write we ignore whatever the caller put in
// BotToken and let validateBot enforce that AppID + AppSecret are
// present. The encrypted column app_secret_enc holds the secret;
// bot_token_enc on app rows holds the encrypted empty string so the
// NOT NULL constraint stays satisfied.
func (r *BotRepo) Insert(ctx context.Context, b *domain.Bot) error {
	if err := validateBot(b); err != nil {
		return err
	}
	// bot_token_enc stays the encrypted form of BotToken for webhook
	// rows (telegram + lark-webhook). For lark-app rows it carries the
	// encrypted empty string so the NOT NULL constraint passes; the
	// real secret lives in app_secret_enc and BotToken is derived on
	// read.
	tokenPlain := b.BotToken
	if b.Platform == domain.PlatformLark && b.BotKind == domain.BotKindApp {
		tokenPlain = ""
	}
	encTok, err := r.cipher.Encrypt([]byte(tokenPlain))
	if err != nil {
		return fmt.Errorf("encrypt bot token: %w", err)
	}
	var encSecret []byte
	if b.AppSecret != "" {
		encSecret, err = r.cipher.Encrypt([]byte(b.AppSecret))
		if err != nil {
			return fmt.Errorf("encrypt app secret: %w", err)
		}
	}
	// Default the in-memory struct so the row we wrote matches what the
	// caller will read back. Most call sites omit the field entirely;
	// matching the column DEFAULT keeps round-trip semantics intuitive.
	if !b.Enabled {
		b.Enabled = true
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO bots (tenant_id, name, platform, bot_kind, bot_token_enc, description, enabled,
		                  app_id, app_secret_enc, verify_token, encrypt_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.TenantID, b.Name, b.Platform, b.BotKind, encTok, b.Description, boolToInt(b.Enabled),
		b.AppID, encSecret, b.VerifyToken, b.EncryptKey, now, now)
	if err != nil {
		return fmt.Errorf("insert bot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	b.ID = id
	b.CreatedAt = toTime(now)
	b.UpdatedAt = toTime(now)
	// For lark-app rows, surface the derived BotToken to the caller so
	// in-memory round-trips match what GetByID would return.
	if b.Platform == domain.PlatformLark && b.BotKind == domain.BotKindApp {
		b.BotToken = appBotToken(b.AppID, b.AppSecret)
	}
	return nil
}

// Update mutates an existing bot (name/platform/token/description/enabled). Tenant id
// is used as a guard to prevent cross-tenant edits. The web layer routes
// enabled toggles through SetEnabled instead, but mass-edit tools and
// import paths legitimately persist the flag here.
//
// Lark-app rows follow the same dual-token convention as Insert:
// bot_token_enc holds the encrypted empty string and the real
// AppSecret lives in app_secret_enc. When the caller passes
// AppSecret="" on an existing app row we PRESERVE the previously
// stored secret (so "edit only the name" flows don't have to re-type
// the secret); pass an explicit value to rotate it.
func (r *BotRepo) Update(ctx context.Context, b *domain.Bot) error {
	if err := validateBot(b); err != nil {
		return err
	}
	if b.ID == 0 {
		return fmt.Errorf("%w: bot id is zero", domain.ErrValidation)
	}
	tokenPlain := b.BotToken
	if b.Platform == domain.PlatformLark && b.BotKind == domain.BotKindApp {
		tokenPlain = ""
	}
	encTok, err := r.cipher.Encrypt([]byte(tokenPlain))
	if err != nil {
		return fmt.Errorf("encrypt bot token: %w", err)
	}
	// AppSecret preservation: if the caller left AppSecret empty AND
	// the row is an app bot, fetch the existing encrypted blob so the
	// secret round-trips. This keeps the "edit name only" flow simple
	// (the UI surfaces a "leave blank to keep" placeholder).
	var encSecret []byte
	if b.AppSecret != "" {
		encSecret, err = r.cipher.Encrypt([]byte(b.AppSecret))
		if err != nil {
			return fmt.Errorf("encrypt app secret: %w", err)
		}
	} else if b.Platform == domain.PlatformLark && b.BotKind == domain.BotKindApp {
		row := r.db.QueryRowContext(ctx,
			`SELECT app_secret_enc FROM bots WHERE id = ? AND tenant_id = ?`, b.ID, b.TenantID)
		if err := row.Scan(&encSecret); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("select existing app secret: %w", err)
		}
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		UPDATE bots SET name = ?, platform = ?, bot_kind = ?, bot_token_enc = ?, description = ?, enabled = ?,
		                app_id = ?, app_secret_enc = ?, verify_token = ?, encrypt_key = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		b.Name, b.Platform, b.BotKind, encTok, b.Description, boolToInt(b.Enabled),
		b.AppID, encSecret, b.VerifyToken, b.EncryptKey, now, b.ID, b.TenantID)
	if err != nil {
		return fmt.Errorf("update bot: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	b.UpdatedAt = toTime(now)
	if b.Platform == domain.PlatformLark && b.BotKind == domain.BotKindApp {
		// Backfill AppSecret on the struct if we preserved it from disk,
		// so subsequent in-memory uses don't see an empty value.
		if b.AppSecret == "" && len(encSecret) > 0 {
			plain, decErr := r.cipher.Decrypt(encSecret)
			if decErr == nil {
				b.AppSecret = string(plain)
			}
		}
		b.BotToken = appBotToken(b.AppID, b.AppSecret)
	}
	return nil
}

// Delete removes a bot owned by tenantID. Missing rows return ErrNotFound.
func (r *BotRepo) Delete(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM bots WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete bot: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// GetByID fetches a bot owned by tenantID, decrypting the token.
func (r *BotRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Bot, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, platform, bot_kind, bot_token_enc, description, enabled,
		       app_id, app_secret_enc, verify_token, encrypt_key, created_at, updated_at
		  FROM bots WHERE id = ? AND tenant_id = ?`, id, tenantID)
	return r.scanBot(row)
}

// ListByTenant returns every bot of a tenant (smallest id first).
func (r *BotRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.Bot, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, platform, bot_kind, bot_token_enc, description, enabled,
		       app_id, app_secret_enc, verify_token, encrypt_key, created_at, updated_at
		  FROM bots WHERE tenant_id = ? ORDER BY id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	defer rows.Close()
	var out []*domain.Bot
	for rows.Next() {
		b, err := r.scanBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListAll returns every bot across all tenants, smallest id first
// (regardless of enabled state — runtime startup decides per-row whether
// to spawn a listener so an operator can re-enable a bot without
// restarting the process). Only the startup wire-up and admin tooling
// should call this — it deliberately skips the tenantID guard.
func (r *BotRepo) ListAll(ctx context.Context) ([]*domain.Bot, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, platform, bot_kind, bot_token_enc, description, enabled,
		       app_id, app_secret_enc, verify_token, encrypt_key, created_at, updated_at
		  FROM bots ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all bots: %w", err)
	}
	defer rows.Close()
	var out []*domain.Bot
	for rows.Next() {
		b, err := r.scanBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetEnabled flips the bots.enabled column for the (tenantID, id) row.
// Returns ErrNotFound when no row matches so the 401 auto-disable path
// stays safe against a row that has just been deleted (the listener
// goroutine may outlive its DB row by a few microseconds).
func (r *BotRepo) SetEnabled(ctx context.Context, tenantID, id int64, enabled bool) error {
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		UPDATE bots SET enabled = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		boolToInt(enabled), now, id, tenantID)
	if err != nil {
		return fmt.Errorf("set bot enabled: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *BotRepo) scanBot(s interface {
	Scan(dest ...any) error
}) (*domain.Bot, error) {
	b := &domain.Bot{}
	var encTok, encSecret []byte
	var createdMs, updatedMs int64
	var enabledInt int
	err := s.Scan(&b.ID, &b.TenantID, &b.Name, &b.Platform, &b.BotKind,
		&encTok, &b.Description, &enabledInt,
		&b.AppID, &encSecret, &b.VerifyToken, &b.EncryptKey,
		&createdMs, &updatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan bot: %w", err)
	}
	// Default BotKind for legacy rows persisted before migration 0010
	// landed (defensive — the schema default is already "webhook", so
	// this branch is effectively unreachable but keeps the in-memory
	// invariant explicit).
	if b.BotKind == "" {
		b.BotKind = domain.BotKindWebhook
	}
	plainTok, err := r.cipher.Decrypt(encTok)
	if err != nil {
		return nil, fmt.Errorf("decrypt bot token: %w", err)
	}
	b.BotToken = string(plainTok)
	if len(encSecret) > 0 {
		plainSec, err := r.cipher.Decrypt(encSecret)
		if err != nil {
			return nil, fmt.Errorf("decrypt app secret: %w", err)
		}
		b.AppSecret = string(plainSec)
	}
	// Derive BotToken for lark-app rows so the runtime sender_router
	// (which only sees BotToken) can route via the lark-app:// prefix.
	if b.Platform == domain.PlatformLark && b.BotKind == domain.BotKindApp {
		b.BotToken = appBotToken(b.AppID, b.AppSecret)
	}
	b.Enabled = enabledInt != 0
	b.CreatedAt = toTime(createdMs)
	b.UpdatedAt = toTime(updatedMs)
	return b, nil
}

// appBotToken assembles the lark-app:// pseudo-URL the runtime
// sender_router uses to identify an application-bot row. The format
// is
//
//	lark-app://<app_id>?secret=<plaintext-secret>
//
// chosen so a plain url.Parse can recover both halves without bespoke
// string surgery. The secret is URL-query-escaped to tolerate values
// containing reserved characters. Callers must NOT log or display the
// returned string; treat it with the same care as the underlying
// secret it embeds.
func appBotToken(appID, secret string) string {
	q := url.Values{}
	q.Set("secret", secret)
	return "lark-app://" + appID + "?" + q.Encode()
}

func validateBot(b *domain.Bot) error {
	if b == nil {
		return errors.New("bot is nil")
	}
	if b.TenantID == 0 {
		return fmt.Errorf("%w: bot tenant_id is zero", domain.ErrValidation)
	}
	if b.Name == "" {
		return fmt.Errorf("%w: bot name is empty", domain.ErrValidation)
	}
	// Default empty Platform to telegram so existing callers/tests stay
	// compatible. Reject anything else so a typo can never silently
	// disable the listener.
	if b.Platform == "" {
		b.Platform = domain.PlatformTelegram
	}
	if !domain.IsValidPlatform(b.Platform) {
		return fmt.Errorf("%w: unknown bot platform %q", domain.ErrValidation, b.Platform)
	}
	// Default the kind to "webhook" so pre-LB1 callers continue to
	// work unchanged — every telegram bot and every Phase A lark
	// webhook bot is implicitly a "webhook" kind.
	if b.BotKind == "" {
		b.BotKind = domain.BotKindWebhook
	}
	if !domain.IsValidBotKind(b.BotKind) {
		return fmt.Errorf("%w: unknown bot kind %q", domain.ErrValidation, b.BotKind)
	}
	// BotKind="app" is only meaningful on Platform="lark" — reject the
	// telegram+app combination loudly so a misconfig cannot silently
	// route a telegram token through the lark IM API client.
	if b.BotKind == domain.BotKindApp && b.Platform != domain.PlatformLark {
		return fmt.Errorf("%w: bot kind %q requires platform %q (got %q)",
			domain.ErrValidation, b.BotKind, domain.PlatformLark, b.Platform)
	}
	if b.BotKind == domain.BotKindApp {
		if b.AppID == "" {
			return fmt.Errorf("%w: app bot requires app_id", domain.ErrValidation)
		}
		// AppSecret is allowed to be empty on Update (preserve-existing
		// semantics — see Update). On Insert the higher-level web
		// handler is expected to require it; we cannot enforce it here
		// without a separate code path, and the wire-up already does so.
	} else {
		// Webhook-kind bots: BotToken is mandatory. Telegram tokens are
		// the canonical "<id>:<secret>" shape; Lark webhook tokens are
		// the full https:// URL.
		if b.BotToken == "" {
			return fmt.Errorf("%w: bot token is empty", domain.ErrValidation)
		}
	}
	return nil
}

// Ensure interface compliance at compile time.
var _ domain.BotRepo = (*BotRepo)(nil)
