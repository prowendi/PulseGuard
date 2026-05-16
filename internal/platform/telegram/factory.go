package telegram

import (
	"log/slog"
	"net/http"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/platform"
)

// Factory is the platform.Factory implementation for Telegram.
// Construct one at process start and pass it into platform.NewManager
// so every domain.Bot with Platform=="telegram" can boot a Listener.
type Factory struct {
	apiBase string
	httpC   *http.Client
	logger  *slog.Logger
}

// FactoryOptions bundles the optional knobs. apiBase=="" defaults to
// https://api.telegram.org inside Listener.New so production wire-ups
// can leave it empty; tests redirect to an httptest server.
type FactoryOptions struct {
	APIBase string
	HTTP    *http.Client
	Logger  *slog.Logger
}

// NewFactory constructs a Factory. Pass FactoryOptions{} for defaults.
func NewFactory(opts FactoryOptions) *Factory {
	return &Factory{
		apiBase: opts.APIBase,
		httpC:   opts.HTTP,
		logger:  opts.Logger,
	}
}

// Platform identifies this factory as the Telegram adapter.
func (f *Factory) Platform() string { return domain.PlatformTelegram }

// Build constructs a Listener using the Factory's shared HTTP client.
// The returned platform.Listener is owned by the Manager.
func (f *Factory) Build(bot *domain.Bot) (platform.Listener, error) {
	return New(bot, Options{APIBase: f.apiBase, HTTP: f.httpC, Logger: f.logger})
}

// Ensure compile-time conformance.
var _ platform.Factory = (*Factory)(nil)
