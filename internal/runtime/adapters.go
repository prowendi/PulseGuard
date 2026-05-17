package runtime

import (
	"context"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/platform/telegram"
)

// commandCatalogAdapter projects domain.CommandRepo into the narrow
// telegram.CommandCatalog interface the listener consumes. Keeping the
// projection here (not in the telegram package) means the listener
// never imports domain types beyond what it owns, and the runtime
// retains the freedom to swap repo implementations without rippling
// changes into the platform adapter.
type commandCatalogAdapter struct {
	commands domain.CommandRepo
}

// ListByBot implements telegram.CommandCatalog. Disabled commands are
// already filtered out by the repo (the listener treats disabled =
// hidden), so this adapter is a pure 1:1 projection.
func (a commandCatalogAdapter) ListByBot(ctx context.Context, botID int64) ([]telegram.CommandSummary, error) {
	rows, err := a.commands.ListByBot(ctx, botID)
	if err != nil {
		return nil, err
	}
	out := make([]telegram.CommandSummary, 0, len(rows))
	for _, c := range rows {
		if c == nil {
			continue
		}
		out = append(out, telegram.CommandSummary{
			Name:        c.Name,
			Description: c.Description,
		})
	}
	return out, nil
}

// Ensure compile-time conformance.
var _ telegram.CommandCatalog = commandCatalogAdapter{}
