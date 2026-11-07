package tunnel

import (
	"sync"
)

// MappingSwitches stores mapping enablement. The FRP service is reloaded when
// a switch changes so the upstream implementation owns the actual proxy state.
type MappingSwitches struct {
	mu      sync.Mutex
	enabled map[string]bool
}

func NewMappingSwitches() *MappingSwitches {
	return &MappingSwitches{enabled: make(map[string]bool)}
}

func (s *MappingSwitches) SetEnabled(name string, enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.enabled[name] = enabled
	s.mu.Unlock()
}

func (s *MappingSwitches) Enabled(name string) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	enabled, known := s.enabled[name]
	s.mu.Unlock()
	return !known || enabled
}
