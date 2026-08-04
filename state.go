package main

import "sync"

// state is now only the admin WebSocket hub. Desired and observed room state
// belongs exclusively to controller.go; the former command state machine was
// removed because it could create a second, conflicting source of truth.
type state struct {
	mu           sync.RWMutex
	adminClients map[*adminClient]struct{}
}

func newState() *state {
	return &state{adminClients: make(map[*adminClient]struct{})}
}

func (s *state) registerAdmin(client *adminClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminClients[client] = struct{}{}
}

func (s *state) unregisterAdmin(client *adminClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.adminClients, client)
}

func (s *state) broadcastAdmins(data []byte) {
	s.mu.RLock()
	clients := make([]*adminClient, 0, len(s.adminClients))
	for client := range s.adminClients {
		clients = append(clients, client)
	}
	s.mu.RUnlock()
	for _, client := range clients {
		client.enqueue(data)
	}
}

func (s *state) closeAdmins() {
	s.mu.RLock()
	clients := make([]*adminClient, 0, len(s.adminClients))
	for client := range s.adminClients {
		clients = append(clients, client)
	}
	s.mu.RUnlock()
	for _, client := range clients {
		client.stop()
	}
}
