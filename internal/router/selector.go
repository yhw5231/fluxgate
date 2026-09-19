package router

import (
	"errors"
	"sort"
	"sync"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/transform"
)

var ErrNoChannel = errors.New("no eligible upstream channel")

// MemorySelector is a deterministic first-slice selector. It prefers higher
// priority channels, then uses smooth weighted round-robin within a priority.
// ChannelFilter can remove temporarily or permanently blocked channels at selection time.
type ChannelFilter interface {
	IsBlocked(channel domain.Channel, model string) bool
}

type MemorySelector struct {
	mu       sync.Mutex
	channels []domain.Channel
	current  map[string]int
	filter   ChannelFilter
}

func NewMemorySelector(channels []domain.Channel) *MemorySelector {
	return NewMemorySelectorWithFilter(channels, nil)
}

// NewMemorySelectorWithFilter preserves the existing selector behavior while
// allowing runtime circuit-breaker filtering.
func NewMemorySelectorWithFilter(channels []domain.Channel, filter ChannelFilter) *MemorySelector {
	copied := append([]domain.Channel(nil), channels...)
	sort.SliceStable(copied, func(i, j int) bool {
		if copied[i].Priority != copied[j].Priority {
			return copied[i].Priority > copied[j].Priority
		}
		return copied[i].ID < copied[j].ID
	})
	return &MemorySelector{channels: copied, current: make(map[string]int), filter: filter}
}

func (s *MemorySelector) Select(model string, excluded map[string]struct{}) (domain.Selection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	highestPriority := 0
	prioritySet := false
	eligible := make([]domain.Channel, 0, len(s.channels))
	for _, channel := range s.channels {
		if !channel.Enabled {
			continue
		}
		if _, skip := excluded[channel.ID]; skip {
			continue
		}
		if s.filter != nil && s.filter.IsBlocked(channel, model) {
			continue
		}
		if !prioritySet {
			highestPriority = channel.Priority
			prioritySet = true
		}
		if channel.Priority != highestPriority {
			break
		}
		eligible = append(eligible, channel)
	}
	if len(eligible) == 0 {
		return domain.Selection{}, ErrNoChannel
	}

	totalWeight := 0
	selected := eligible[0]
	selectedScore := 0
	for index, channel := range eligible {
		weight := channel.Weight
		if weight <= 0 {
			weight = 1
		}
		totalWeight += weight
		s.current[channel.ID] += weight
		if index == 0 || s.current[channel.ID] > selectedScore {
			selected = channel
			selectedScore = s.current[channel.ID]
		}
	}
	s.current[selected.ID] -= totalWeight

	return domain.Selection{
		Channel: selected,
		Model:   transform.MapModel(model, selected.ModelMapping),
	}, nil
}
