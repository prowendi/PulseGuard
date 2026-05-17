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
	apiBase    string
	httpC      *http.Client
	logger     *slog.Logger
	dispatcher CommandDispatcher
	catalog    CommandCatalog
	remover    SubscriberRemover
	acker      AlertAcker
	silences   SilenceManager
	health     HealthHook
}

// FactoryOptions bundles the optional knobs. apiBase=="" defaults to
// https://api.telegram.org inside Listener.New so production wire-ups
// can leave it empty; tests redirect to an httptest server.
//
// Dispatcher, when non-nil, is plumbed into every Listener the factory
// builds so per-tenant custom commands get dispatched.
//
// Catalog, when non-nil, lets each Listener publish its slash menu via
// setMyCommands on startup and back the built-in /commands helper.
//
// Remover, when non-nil, powers the built-in /unsubscribe command.
//
// Acker, when non-nil, powers the built-in /ack <fingerprint> command.
//
// Health, when its callbacks are non-nil, bridges listener hot-path
// events (update batches, dispatches, errors) to the Manager's
// in-memory health panel (V6-2). Each callback is nil-checked at the
// invocation site so partial wire-ups are safe.
type FactoryOptions struct {
	APIBase    string
	HTTP       *http.Client
	Logger     *slog.Logger
	Dispatcher CommandDispatcher
	Catalog    CommandCatalog
	Remover    SubscriberRemover
	Acker      AlertAcker
	Silences   SilenceManager
	Health     HealthHook
}

// NewFactory constructs a Factory. Pass FactoryOptions{} for defaults.
func NewFactory(opts FactoryOptions) *Factory {
	return &Factory{
		apiBase:    opts.APIBase,
		httpC:      opts.HTTP,
		logger:     opts.Logger,
		dispatcher: opts.Dispatcher,
		catalog:    opts.Catalog,
		remover:    opts.Remover,
		acker:      opts.Acker,
		silences:   opts.Silences,
		health:     opts.Health,
	}
}

// Platform identifies this factory as the Telegram adapter.
func (f *Factory) Platform() string { return domain.PlatformTelegram }

// Build constructs a Listener using the Factory's shared HTTP client.
// The returned platform.Listener is owned by the Manager.
func (f *Factory) Build(bot *domain.Bot) (platform.Listener, error) {
	return New(bot, Options{
		APIBase:    f.apiBase,
		HTTP:       f.httpC,
		Logger:     f.logger,
		Dispatcher: f.dispatcher,
		Catalog:    f.catalog,
		Remover:    f.remover,
		Acker:      f.acker,
		Silences:   f.silences,
		Health:     f.health,
	})
}

// Ensure compile-time conformance.
var _ platform.Factory = (*Factory)(nil)
