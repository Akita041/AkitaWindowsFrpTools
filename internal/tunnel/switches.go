package tunnel

import (
	"sync"
)

// MappingSwitches stores mapping enablement. The FRP service is reloaded when
// a switch changes so the upstream implementation owns the actual proxy state.
type MappingSwitches struct {
	mu             sync.Mutex
	enabled        map[string]bool
	serverMappings []Mapping
	clientMappings []LocalMapping
}

func (s *MappingSwitches) SetServerMappings(mappings []Mapping) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.serverMappings = append([]Mapping(nil), mappings...)
	s.mu.Unlock()
}

func (s *MappingSwitches) ServerMappings(fallback []Mapping) []Mapping {
	if s == nil {
		return append([]Mapping(nil), fallback...)
	}
	s.mu.Lock()
	mappings := append([]Mapping(nil), s.serverMappings...)
	s.mu.Unlock()
	return mappings
}

// SetClientMappings publishes the latest GUI configuration to the running FRP
// client, which can add/remove proxies without replacing its control session.
func (s *MappingSwitches) SetClientMappings(mappings []LocalMapping) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.clientMappings = append([]LocalMapping(nil), mappings...)
	s.mu.Unlock()
}

func (s *MappingSwitches) ClientMappings(fallback []LocalMapping) []LocalMapping {
	if s == nil {
		return append([]LocalMapping(nil), fallback...)
	}
	s.mu.Lock()
	mappings := append([]LocalMapping(nil), s.clientMappings...)
	s.mu.Unlock()
	return mappings
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
