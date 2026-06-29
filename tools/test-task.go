//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

type request struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func main() {
	u := url.URL{Scheme: "ws", Host: "localhost:18900", Path: "/ws"}
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer c.Close()

	// Test 1: Create task
	fmt.Println("=== Test 1: task.create ===")
	body, _ := json.Marshal(map[string]interface{}{
		"goal":    "在当前目录创建一个 test-daemon.txt 文件，内容为 'daemon works'",
		"workdir": ".",
	})
	c.WriteJSON(request{Type: "task.create", ID: "t1", Payload: body})

	// Test 2: List tasks
	time.Sleep(500 * time.Millisecond)
	fmt.Println("=== Test 2: task.list ===")
	c.WriteJSON(request{Type: "task.list", ID: "t2"})

	// Read responses
	deadline := time.Now().Add(30 * time.Second)
	c.SetReadDeadline(deadline)
	for time.Now().Before(deadline) {
		_, raw, err := c.ReadMessage()
		if err != nil {
			break
		}
		var m map[string]interface{}
		json.Unmarshal(raw, &m)
		typ, _ := m["type"].(string)
		fmt.Printf("[%s] %s\n", typ, string(raw))
		if typ == "task.list" {
			break
		}
	}
	fmt.Println("=== done ===")
}
