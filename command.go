package reed

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"

	"github.com/heedle-ai/draft/registry"
	"github.com/heedle-ai/shuttle"
	"github.com/heedle-ai/shuttle/plan"
	"github.com/heedle-ai/weft"
)

const (
	// CommandName is the draft command: (browse "URL" "also.allowed.host" …).
	CommandName = "browse"
	// ActivityName is its Temporal activity.
	ActivityName = "Browse"
)

// Opener opens a page; Browser is the real one.
type Opener interface {
	Open(ctx context.Context, v Visit) (Page, error)
}

// Command is the browse step: Args[0] is the URL, Args[1:] are further
// hosts the page may load from. The allowlist is therefore part of the
// program, compiled into the plan and recorded with every run.
//
// Its output is the Page as JSON: what the browser saw. While it runs, it
// heartbeats each live Frame to Temporal, where drawdown reads the latest
// one to show the agent's browser.
func Command(o Opener) weft.Arrow[plan.Input, string] {
	return func(ctx context.Context, in plan.Input) (string, error) {
		if len(in.Args) == 0 {
			return "", temporal.NewNonRetryableApplicationError("browse needs a URL", "InvalidInput", nil)
		}
		v := Visit{URL: in.Args[0], Allow: in.Args[1:]}
		if activity.IsActivity(ctx) {
			v.OnFrame = func(f Frame) { activity.RecordHeartbeat(ctx, f) }
		}
		p, err := o.Open(ctx, v)
		switch {
		case errors.Is(err, ErrInvalid):
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "InvalidInput", nil)
		case errors.Is(err, ErrSetup):
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "BrowserSetup", nil)
		case errors.Is(err, ErrBlocked):
			// The page went off the allowlist: it will again.
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "Blocked", nil)
		case err != nil:
			return "", err // retryable: a fresh browser next time
		}
		b, err := json.Marshal(p)
		return string(b), err
	}
}

// BuiltinSpec is the draft builtin: a URL, then any number of hosts.
func BuiltinSpec() registry.BuiltinSpec {
	return registry.BuiltinSpec{
		Name:    CommandName,
		AgentID: ActivityName,
		ArgSchema: registry.ArgSchema{
			Params:       []registry.ParamSpec{{Name: "url", Type: registry.StringT}},
			Variadic:     true,
			VariadicType: registry.StringT,
		},
		Backends: []registry.Backend{registry.BackendTemporal},
	}
}

// Activities is the part of a Temporal worker Register needs.
type Activities interface {
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}

// Register adds browse to a draft registry and its activity to a worker.
func Register(reg *registry.Registry, w Activities, o Opener) error {
	if err := reg.Register(BuiltinSpec()); err != nil {
		return err
	}
	w.RegisterActivityWithOptions(shuttle.ArrowAsActivity(Command(o)), activity.RegisterOptions{Name: ActivityName})
	return nil
}

// HeartbeatThrottle is how often a worker should pass browse's live frames
// on to Temporal. The Go SDK sends heartbeats at most every 30 seconds
// unless told otherwise, which is too slow for a live view.
const HeartbeatThrottle = 2 * time.Second

// WorkerOptions returns o with heartbeats passed on every
// HeartbeatThrottle, for workers that run browse.
func WorkerOptions(o worker.Options) worker.Options {
	o.DefaultHeartbeatThrottleInterval = HeartbeatThrottle
	o.MaxHeartbeatThrottleInterval = HeartbeatThrottle
	return o
}

// ParsePage reads a browse output back. ok is false when out isn't one.
func ParsePage(out string) (p Page, ok bool) {
	if json.Unmarshal([]byte(out), &p) != nil || p.Reed != 1 {
		return Page{}, false
	}
	return p, true
}
