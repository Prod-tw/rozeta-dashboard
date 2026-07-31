package main

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

type adminClient struct {
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
	once sync.Once
}

func newAdminClient(conn *websocket.Conn) *adminClient {
	return &adminClient{
		conn: conn,
		send: make(chan []byte, 32),
		done: make(chan struct{}),
	}
}

func (c *adminClient) enqueue(data []byte) {
	select {
	case <-c.done:
		return
	case c.send <- data:
	default:
		// Dropping a completion snapshot left the browser spinner permanently pending.
		// Closing an overloaded client forces a reconnect and full state snapshot.
		log.Printf("closing overloaded admin websocket")
		c.stop()
	}
}

func (c *adminClient) sendJSON(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	c.enqueue(data)
}

func (c *adminClient) stop() {
	c.once.Do(func() {
		close(c.done)
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}

func (c *adminClient) writePump() {
	defer c.stop()
	for {
		select {
		case <-c.done:
			return
		case message := <-c.send:
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		}
	}
}

func (c *adminClient) readPump(a *app) {
	defer func() {
		a.state.unregisterAdmin(c)
		c.stop()
	}()
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}
