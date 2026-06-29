//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

type request struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type response struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

type push struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type chatChunk struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Done      bool   `json:"done"`
	Error     string `json:"error,omitempty"`
}

func main() {
	u := url.URL{Scheme: "ws", Host: "localhost:18900", Path: "/ws"}
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer c.Close()

	// Send chat.
	body, _ := json.Marshal(map[string]interface{}{
		"message":    "读取 go.mod 文件第一行并告诉我内容",
		"deep_think": false,
	})
	req := request{Type: "chat", ID: "1", Payload: body}
	c.WriteJSON(req)

	// Read responses.
	deadline := time.Now().Add(60 * time.Second)
	c.SetReadDeadline(deadline)

	for time.Now().Before(deadline) {
		_, raw, err := c.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err) {
				fmt.Println("connection closed:", err)
			}
			break
		}

		// Try push first.
		var p push
		if json.Unmarshal(raw, &p) == nil && p.Type == "chat.stream" {
			var cc chatChunk
			json.Unmarshal(p.Payload, &cc)
			if cc.Error != "" {
				fmt.Println("ERROR:", cc.Error)
			}
			if cc.Thinking != "" {
				fmt.Print("[思考] ", cc.Thinking)
			}
			if cc.Content != "" {
				fmt.Print(cc.Content)
			}
			if cc.Done {
				fmt.Println("\n--- DONE ---")
				os.Exit(0)
			}
			continue
		}

		// Try response.
		var r response
		if json.Unmarshal(raw, &r) == nil && r.ID != "" {
			fmt.Println("response:", string(raw))
		}
	}

	fmt.Println("\ntimeout")
	os.Exit(1)
}
