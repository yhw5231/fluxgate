package router

import (
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
)

func TestMemorySelectorPrefersHighestPriorityAndMapsModel(t *testing.T) {
	selector := NewMemorySelector([]domain.Channel{
		{
			ID:           "lower",
			Enabled:      true,
			Priority:     10,
			Weight:       1,
			ModelMapping: map[string]string{"gpt-*": "lower-model"},
		},
		{
			ID:           "higher",
			Enabled:      true,
			Priority:     20,
			Weight:       1,
			ModelMapping: map[string]string{"gpt-*": "higher-model"},
		},
	})

	selection, err := selector.Select("gpt-4.1", nil)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "higher" {
		t.Fatalf("selected channel = %q, want higher", selection.Channel.ID)
	}
	if selection.Model != "higher-model" {
		t.Fatalf("selected model = %q, want higher-model", selection.Model)
	}
}

func TestMemorySelectorUsesSmoothWeightedRoundRobinWithinPriority(t *testing.T) {
	selector := NewMemorySelector([]domain.Channel{
		{ID: "heavy", Enabled: true, Priority: 10, Weight: 3},
		{ID: "light", Enabled: true, Priority: 10, Weight: 1},
	})

	counts := map[string]int{}
	for index := 0; index < 8; index++ {
		selection, err := selector.Select("model", nil)
		if err != nil {
			t.Fatalf("Select() error = %v", err)
		}
		counts[selection.Channel.ID]++
	}

	if counts["heavy"] != 6 || counts["light"] != 2 {
		t.Fatalf("weighted counts = %#v, want heavy:6 light:2", counts)
	}
}

func TestMemorySelectorHonorsExclusionsAndDisabledChannels(t *testing.T) {
	selector := NewMemorySelector([]domain.Channel{
		{ID: "disabled", Enabled: false, Priority: 30, Weight: 1},
		{ID: "primary", Enabled: true, Priority: 20, Weight: 1},
		{ID: "fallback", Enabled: true, Priority: 10, Weight: 1},
	})

	selection, err := selector.Select("model", map[string]struct{}{"primary": {}})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "fallback" {
		t.Fatalf("selected channel = %q, want fallback", selection.Channel.ID)
	}
}

func TestMemorySelectorReturnsErrorWhenNoChannelIsEligible(t *testing.T) {
	selector := NewMemorySelector([]domain.Channel{
		{ID: "disabled", Enabled: false, Priority: 10, Weight: 1},
	})

	_, err := selector.Select("model", nil)
	if err != ErrNoChannel {
		t.Fatalf("Select() error = %v, want ErrNoChannel", err)
	}
}
