package main

import "testing"

func TestAdminQueueOverflowForcesReconnect(t *testing.T) {
	client := &adminClient{
		send: make(chan []byte, 1),
		done: make(chan struct{}),
	}
	client.send <- []byte("first")
	client.enqueue([]byte("second"))
	select {
	case <-client.done:
	default:
		t.Fatal("overloaded client was not stopped")
	}
}
