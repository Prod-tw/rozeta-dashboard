package main

import (
	"encoding/json"
	"errors"
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

const roomOccupiedCloseCode = 4409

var errRoomOccupied = errors.New("room already has an agent connected")

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
			if errors.Is(err, errRoomOccupied) {
				_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(roomOccupiedCloseCode, err.Error()), time.Now().Add(time.Second))
			} else {
				_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error()), time.Now().Add(time.Second))
			}
			// A rejected hello must terminate the websocket so the browser stops
			// reconnecting after the server has already denied the room claim.
			return
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
		roomName, _ := msg["room_name"].(string)
		agentID, _ := msg["agent_id"].(string)
		if roomName == "" {
			return errors.New("missing room_name in agent_hello")
		}
		if agentID == "" {
			agentID = newID("agent")
		}
		room, claimed := a.state.claimRoom(roomName, client)
		if !claimed {
			return errRoomOccupied
		}
		client.roomName = roomName
		client.agentID = agentID
		a.broadcastToAdmins(adminEnvelope{Type: "agent_connected", Message: "agent connected for " + roomName, Room: room.snapshot(a.state.meetingNames), Timestamp: time.Now().UTC()})
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
