package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefiix92/coachd/internal/auth"
	"github.com/stefiix92/coachd/internal/store"
)

// maxMessageRunes caps a message: it is read on a watch.
const maxMessageRunes = 300

// registerMessages adds send_message: display-only text for the runner's
// watch. A message never carries or changes a workout.
func (s *svc) registerMessages(srv *mcp.Server) {
	addTool(s, srv, &mcp.Tool{Name: "send_message", Annotations: writeAnnotations(false), Description: "Send the " +
		"runner a short message, shown on their watch by coachd's watch app the next time it checks (within about " +
		"15 minutes when its background checks are on), and on coachd's page; " +
		"when coachd has ntfy configured the runner also gets a push that says only that a message waits, at most one " +
		"per 15 minutes. text is 1 to 300 characters: keep it short enough to read at a glance. A message is display-only: it cannot carry, start or change a workout. " +
		"coachd keeps the newest 20 and the watch shows those from the last 7 days." + localWrite + descSuffix},
		auth.ScopeWrite, s.sendMessage)
}

type sendMessageInput struct {
	Text string `json:"text" jsonschema:"Message text, 1 to 300 characters"`
}

type messageOut struct {
	ID        int64  `json:"id" jsonschema:"Message ID"`
	Text      string `json:"text" jsonschema:"Message text"`
	CreatedAt string `json:"created_at" jsonschema:"When the message was stored, RFC 3339 in the runner's timezone"`
}

type sendMessageOut struct {
	Message messageOut `json:"message" jsonschema:"The stored message"`
}

func (s *svc) sendMessage(ctx context.Context, in sendMessageInput) (sendMessageOut, error) {
	if in.Text == "" {
		return sendMessageOut{}, invalidf("text: required")
	}
	if err := checkText("text", in.Text, maxMessageRunes); err != nil {
		return sendMessageOut{}, err
	}
	id, _ := auth.IdentityFrom(ctx)
	m, err := s.DB.InsertMessage(ctx, store.Message{Text: in.Text, CreatedBy: agentCreator(id), CreatedAt: s.Now().UTC()})
	if err != nil {
		return sendMessageOut{}, err
	}
	if s.Messages != nil {
		s.Messages.AgentMessage()
	}
	return sendMessageOut{Message: messageOut{ID: m.ID, Text: m.Text, CreatedAt: s.ts(m.CreatedAt)}}, nil
}
