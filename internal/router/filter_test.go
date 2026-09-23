package router

import (
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
)

type blockingFilter struct {
	blocked map[string]bool
}

func (f blockingFilter) IsBlocked(channel domain.Channel, model string) bool {
	return f.blocked[channel.ID+":"+model]
}

func TestMemorySelectorRoutesAroundBlockedChannel(t *testing.T) {
	selector := NewMemorySelectorWithFilter([]domain.Route{route(1, "*",
		channel("primary", 20, 1),
		channel("fallback", 10, 1),
	)}, blockingFilter{blocked: map[string]bool{"primary:model-a": true}})

	selection, err := selector.Select(domain.SelectionRequest{Model: "model-a"})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "fallback" {
		t.Fatalf("selected channel = %q, want fallback", selection.Channel.ID)
	}
}

func TestMemorySelectorFilterIsModelSpecific(t *testing.T) {
	selector := NewMemorySelectorWithFilter([]domain.Route{route(1, "*",
		// The two lines sit on different upstreams with different priorities, so
		// which of them answers is decided rather than drawn.
		domain.Channel{ID: "primary", Enabled: true, Weight: 1, SiteID: 1, SitePriority: 20},
		domain.Channel{ID: "fallback", Enabled: true, Weight: 1, SiteID: 2, SitePriority: 10},
	)}, blockingFilter{blocked: map[string]bool{"primary:model-a": true}})

	selection, err := selector.Select(domain.SelectionRequest{Model: "model-b"})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "primary" {
		t.Fatalf("selected channel = %q, want primary", selection.Channel.ID)
	}
}

// A per-model circuit is filed under the model the line is asked for, which is
// the line's own name for it when the route maps the exposed one. The filter has
// to be asked about that same name: asking about the requested one would look up
// a circuit that is never written, and a cooling line would keep answering.
func TestMemorySelectorAsksTheFilterAboutTheModelTheLineSends(t *testing.T) {
	selector := NewMemorySelectorWithFilter([]domain.Route{route(1, "model-a",
		domain.Channel{
			ID: "primary", Enabled: true, Weight: 1,
			SiteID: 1, SitePriority: 20, SourceModel: "cn:model-a",
		},
		domain.Channel{
			ID: "fallback", Enabled: true, Weight: 1,
			SiteID: 2, SitePriority: 10, SourceModel: "cn:model-a",
		},
	)}, blockingFilter{blocked: map[string]bool{"primary:cn:model-a": true}})

	selection, err := selector.Select(domain.SelectionRequest{Model: "model-a"})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "fallback" {
		t.Fatalf("selected channel = %q, want the line that is not cooling", selection.Channel.ID)
	}
	if selection.Model != "cn:model-a" {
		t.Fatalf("selected model = %q, want the model the line is asked for", selection.Model)
	}
}

func TestMemorySelectorReturnsErrorWhenFilterBlocksAllChannels(t *testing.T) {
	selector := NewMemorySelectorWithFilter([]domain.Route{route(1, "*",
		channel("only", 10, 1),
	)}, blockingFilter{blocked: map[string]bool{"only:model-a": true}})

	_, err := selector.Select(domain.SelectionRequest{Model: "model-a"})
	if err != ErrNoChannel {
		t.Fatalf("Select() error = %v, want ErrNoChannel", err)
	}
	if selector.HasCandidate("model-a", domain.RoutingPolicy{}) {
		t.Fatal("HasCandidate() = true while every channel is blocked")
	}
}
