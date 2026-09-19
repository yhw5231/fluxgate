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
	selector := NewMemorySelectorWithFilter([]domain.Channel{
		{ID: "primary", Enabled: true, Priority: 20, Weight: 1},
		{ID: "fallback", Enabled: true, Priority: 10, Weight: 1},
	}, blockingFilter{blocked: map[string]bool{"primary:model-a": true}})

	selection, err := selector.Select("model-a", nil)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "fallback" {
		t.Fatalf("selected channel = %q, want fallback", selection.Channel.ID)
	}
}

func TestMemorySelectorFilterIsModelSpecific(t *testing.T) {
	selector := NewMemorySelectorWithFilter([]domain.Channel{
		{ID: "primary", Enabled: true, Priority: 20, Weight: 1},
		{ID: "fallback", Enabled: true, Priority: 10, Weight: 1},
	}, blockingFilter{blocked: map[string]bool{"primary:model-a": true}})

	selection, err := selector.Select("model-b", nil)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "primary" {
		t.Fatalf("selected channel = %q, want primary", selection.Channel.ID)
	}
}

func TestMemorySelectorReturnsErrorWhenFilterBlocksAllChannels(t *testing.T) {
	selector := NewMemorySelectorWithFilter([]domain.Channel{
		{ID: "only", Enabled: true, Priority: 10, Weight: 1},
	}, blockingFilter{blocked: map[string]bool{"only:model-a": true}})

	_, err := selector.Select("model-a", nil)
	if err != ErrNoChannel {
		t.Fatalf("Select() error = %v, want ErrNoChannel", err)
	}
}
