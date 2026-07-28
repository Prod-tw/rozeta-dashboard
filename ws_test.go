package main

import "testing"

func TestAgentHelloRejectsDuplicateRoom(t *testing.T) {
	a := &app{state: newState()}

	first := &agentClient{}
	if err := a.onAgentMessage(first, []byte(`{"type":"agent_hello","room_name":"alpha","agent_id":"agent-1"}`)); err != nil {
		t.Fatalf("first hello failed: %v", err)
	}

	second := &agentClient{}
	if err := a.onAgentMessage(second, []byte(`{"type":"agent_hello","room_name":"alpha","agent_id":"agent-2"}`)); err == nil {
		t.Fatal("expected second hello to fail for occupied room")
	}

	a.state.unregisterAgent(first)
	if err := a.onAgentMessage(second, []byte(`{"type":"agent_hello","room_name":"alpha","agent_id":"agent-2"}`)); err != nil {
		t.Fatalf("hello should succeed after release: %v", err)
	}
}
