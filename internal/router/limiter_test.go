package router

import (
	"errors"
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// busyLimiter reports the lines whose key is at its concurrency limit, which is
// the one question the selector asks a limiter.
type busyLimiter struct {
	busy map[string]bool
}

func (f busyLimiter) Busy(channel domain.Channel) bool {
	return f.busy[channel.ID]
}

// A request is placed on a key that has room rather than queueing behind a key at
// its limit, so two keys of one upstream serve two requests at once.
func TestMemorySelectorSkipsAKeyThatIsAtItsLimit(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		channel("first-key", 0, 1),
		channel("second-key", 0, 1),
	)})
	selector.SetKeyLimiter(busyLimiter{busy: map[string]bool{"first-key": true}})

	for index := 0; index < 5; index++ {
		selection := selectChannel(t, selector, "model-a", domain.RoutingPolicy{})
		if selection.Channel.ID != "second-key" {
			t.Fatalf("selected channel = %q, want the key with room", selection.Channel.ID)
		}
	}
}

// Every line being at its limit is a throttle, not a refusal: nothing about the
// configuration is wrong, so it is reported apart from "no eligible channel" — and
// the model stays listed, because it answers again as soon as a request finishes.
func TestMemorySelectorReportsAThrottledModelAsBusy(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		channel("only-key", 0, 1),
	)})
	selector.SetKeyLimiter(busyLimiter{busy: map[string]bool{"only-key": true}})

	_, err := selector.Select(domain.SelectionRequest{Model: "model-a"})
	if !errors.Is(err, domain.ErrChannelsBusy) {
		t.Fatalf("Select() error = %v, want ErrChannelsBusy", err)
	}
	if !selector.HasCandidate("model-a", domain.RoutingPolicy{}) {
		t.Fatal("HasCandidate() = false while the only key is merely busy")
	}

	// A line held out of rotation for another reason is still a refusal, and stays
	// distinguishable from a throttle.
	inactive := channel("only-key", 0, 1)
	inactive.Enabled = false
	selector = NewMemorySelector([]domain.Route{route(1, "*", inactive)})
	selector.SetKeyLimiter(busyLimiter{busy: map[string]bool{"only-key": true}})
	if _, err := selector.Select(domain.SelectionRequest{Model: "model-a"}); !errors.Is(err, ErrNoChannel) {
		t.Fatalf("Select() error = %v, want ErrNoChannel for a disabled line", err)
	}
}
