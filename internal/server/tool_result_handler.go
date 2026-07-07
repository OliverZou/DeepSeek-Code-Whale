package server

import (
	"encoding/json"

	"github.com/usewhale/whale/internal/core"
)

// handleSessionGetToolResult returns the full output of a specific tool call.
func (d *Daemon) handleSessionGetToolResult(w MessageWriter, req wsRequest) {
	var p struct {
		SessionID  string `json:"session_id"`
		ToolCallID string `json:"tool_call_id"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		w.SendResponse(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	msgs, err := d.store.List(nil, p.SessionID)
	if err != nil {
		w.SendResponse(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}
	for _, m := range msgs {
		for _, tr := range m.ToolResults {
			if tr.ToolCallID == p.ToolCallID {
				output := core.ToolResultModelText(tr)
				outcome := string(tr.Outcome)
				w.SendResponse(wsResponse{Type: "session.getToolResult", ID: req.ID, Payload: map[string]interface{}{
					"output":  output,
					"outcome": outcome,
					"failed":  outcome != "" && outcome != "success" && outcome != "no_result",
				}})
				return
			}
		}
	}
	w.SendResponse(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "tool result not found"}})
}
