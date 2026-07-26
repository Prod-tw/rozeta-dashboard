package main

import (
	"errors"
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

type agentClient struct {
	conn     *websocket.Conn
	send     chan []byte
	roomName string
	agentID  string
}

type adminClient struct {
	conn *websocket.Conn
	send chan []byte
}

func newAgentClient(conn *websocket.Conn) *agentClient {
	return &agentClient{conn: conn, send: make(chan []byte, 32)}
}

func newAdminClient(conn *websocket.Conn) *adminClient {
	return &adminClient{conn: conn, send: make(chan []byte, 32)}
}

func (c *agentClient) enqueue(data []byte) {
	select {
	case c.send <- data:
	default:
		log.Printf("dropping agent message for %s", c.roomName)
	}
}

func (c *adminClient) enqueue(data []byte) {
	select {
	case c.send <- data:
	default:
		log.Printf("dropping admin message")
	}
}

func (c *agentClient) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.enqueue(data)
}

func (c *adminClient) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.enqueue(data)
}

func (c *agentClient) writePump() {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

func (c *adminClient) writePump() {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

func (c *agentClient) readPump(a *app) {
	defer func() {
		a.state.unregisterAgent(c)
		close(c.send)
		_ = c.conn.Close()
	}()

	_ = c.conn.SetReadDeadline(time.Time{})
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if err := a.onAgentMessage(c, msg); err != nil {
			log.Printf("agent message error: %v", err)
		}
	}
}

func (c *adminClient) readPump(a *app) {
	defer func() {
		a.state.unregisterAdmin(c)
		close(c.send)
		_ = c.conn.Close()
	}()

	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (a *app) onAgentMessage(client *agentClient, payload []byte) error {
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}
	typeName, _ := msg["type"].(string)
	switch typeName {
	case "agent_hello":
		client.roomName, _ = msg["room_name"].(string)
		client.agentID, _ = msg["agent_id"].(string)
		if client.roomName == "" {
			return errors.New("missing room_name in agent_hello")
		}
		if client.agentID == "" {
			client.agentID = newID("agent")
		}
		a.state.ensureRoom(client.roomName)
		a.broadcastToAdmins(adminEnvelope{Type: "agent_connected", Message: "agent connected for " + client.roomName, Timestamp: time.Now().UTC()})
		return nil
	case "agent_heartbeat":
		var hb snapshotHeartbeat
		if err := json.Unmarshal(payload, &hb); err != nil {
			return err
		}
		if hb.RoomName == "" {
			hb.RoomName = client.roomName
		}
		if hb.AgentID == "" {
			hb.AgentID = client.agentID
		}
		room, recovered := a.state.applyHeartbeat(hb)
		a.broadcastToAdmins(adminEnvelope{Type: "room_snapshot", Room: room, Timestamp: time.Now().UTC()})
		if recovered {
			a.broadcastToAdmins(adminEnvelope{Type: "alert", Level: "info", Message: "room " + hb.RoomName + " recovered", Room: room, Timestamp: time.Now().UTC()})
		}
		return nil
	default:
		return nil
	}
}
