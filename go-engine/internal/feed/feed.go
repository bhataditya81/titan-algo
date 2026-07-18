package feed

import "log"

// Manager handles WebSocket connections and tick distribution
type Manager struct {
	// TODO: Add channels for tick data
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) Start() {
	log.Println("Starting Feed Manager...")
}
