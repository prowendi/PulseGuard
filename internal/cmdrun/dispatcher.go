// Package cmdrun is the dispatcher that bridges the Telegram listener
// to per-tenant Starlark commands. The listener calls Dispatch(); we
// resolve the command via the repos, execute the script in the
// sandboxed Executor, upsert the subscriber row, and return the
// stitched-together reply text (or an ErrDispatch* sentinel the
// listener maps to a user-friendly message).
//
// The dispatcher is intentionally thin so the listener does not depend
// on internal/scripting or any repo directly — every collaborator goes
// through a small interface so unit tests can substitute fakes.
package cmdrun

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/platform/telegram"
	"github.com/wendi/pulseguard/internal/scripting"
)

// CommandResolver finds the command behind a bot-scoped slash name.
// The runtime layer plugs in *store.CommandRepo here.
type CommandResolver interface {
	GetByBotAndName(ctx context.Context, botID int64, name string) (*domain.Command, error)
}

// SubscriberRecorder upserts a (command, bot, chat) tuple. The runtime
// layer plugs in *store.SubscriberRepo.
type SubscriberRecorder interface {
	Upsert(ctx context.Context, s *domain.Subscriber) error
}

// Dispatcher implements telegram.CommandDispatcher by composing a
// CommandResolver, an Executor, and a SubscriberRecorder.
//
// All three fields are required; New panics if any is nil so wire-up
// bugs surface at startup rather than at the first inbound message.
type Dispatcher struct {
	resolver CommandResolver
	executor *scripting.Executor
	recorder SubscriberRecorder
}

// New builds a Dispatcher. All three deps are mandatory.
func New(resolver CommandResolver, executor *scripting.Executor, recorder SubscriberRecorder) *Dispatcher {
	if resolver == nil || executor == nil || recorder == nil {
		panic("cmdrun: New requires non-nil resolver, executor, recorder")
	}
	return &Dispatcher{
		resolver: resolver,
		executor: executor,
		recorder: recorder,
	}
}

// Dispatch implements telegram.CommandDispatcher.
//
// Resolution rules:
//   - The listener strips the leading "/", so we try both "/<name>" and
//     "<name>" against the store. Operators are free to define either.
//   - Unknown / disabled commands surface as telegram.ErrDispatchSkip
//     so the listener stays silent.
//   - Executor errors map to telegram.ErrDispatch* sentinels so the
//     listener can render a Chinese-friendly message without ever
//     touching scripting.* directly.
func (d *Dispatcher) Dispatch(ctx context.Context, in telegram.DispatchInput) (telegram.DispatchOutput, error) {
	cmd, err := d.resolve(ctx, in.BotID, in.Name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return telegram.DispatchOutput{}, telegram.ErrDispatchSkip
		}
		return telegram.DispatchOutput{}, err
	}

	// Record the subscriber BEFORE executing so a slow / failed script
	// still leaves an audit trail of "this chat tried to use that
	// command". Upsert errors are non-fatal; the listener should still
	// run the script.
	_ = d.recorder.Upsert(ctx, &domain.Subscriber{
		TenantID:  cmd.TenantID,
		CommandID: cmd.ID,
		BotID:     in.BotID,
		ChatID:    strconv.FormatInt(in.ChatID, 10),
		Platform:  domain.PlatformTelegram,
	})

	res, runErr := d.executor.Execute(ctx, cmd.Code, in.Args)
	if runErr != nil {
		switch {
		case errors.Is(runErr, scripting.ErrTimeout):
			return telegram.DispatchOutput{}, telegram.ErrDispatchTimeout
		case errors.Is(runErr, scripting.ErrUnsafeHost):
			return telegram.DispatchOutput{}, telegram.ErrDispatchUnsafeHost
		case errors.Is(runErr, scripting.ErrUnsupportedScheme):
			return telegram.DispatchOutput{}, telegram.ErrDispatchUnsupportedScheme
		default:
			// Don't leak the raw error to the chat — the listener
			// shows a generic "命令执行失败" for unknown failure modes.
			return telegram.DispatchOutput{}, runErr
		}
	}
	return telegram.DispatchOutput{Text: stitch(res)}, nil
}

func (d *Dispatcher) resolve(ctx context.Context, botID int64, name string) (*domain.Command, error) {
	// Try the slash form first since that is what /commands UI tends
	// to suggest, then the bare form.
	candidates := []string{"/" + name, name}
	for _, n := range candidates {
		c, err := d.resolver.GetByBotAndName(ctx, botID, n)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
	}
	return nil, domain.ErrNotFound
}

// stitch joins Output + Return with newline. Empty pieces are skipped.
func stitch(r *scripting.Result) string {
	if r == nil {
		return ""
	}
	var parts []string
	if s := strings.TrimSpace(r.Output); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(r.Return); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// compile-time conformance
var _ telegram.CommandDispatcher = (*Dispatcher)(nil)
