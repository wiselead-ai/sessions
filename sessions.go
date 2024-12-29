package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wiselead-ai/openai"
	"github.com/wiselead-ai/trello"
)

const (
	cleanupInterval = 1 * time.Hour
	sessionTimeout  = 24 * time.Hour
	roleAssistant   = "assistant"
	messageTextType = "text"
)

type (
	openaiClient interface {
		GetAssistant(ctx context.Context, assistantID string) (*openai.Assistant, error)
		AddMessage(ctx context.Context, in openai.CreateMessageInput) error
		RunThread(ctx context.Context, threadID, assistantID string) (*openai.Run, error)
		WaitForRun(ctx context.Context, threadID, runID string) error
		GetMessages(ctx context.Context, threadID string) (*openai.ThreadMessageList, error)
		CreateThread(ctx context.Context) (*openai.Thread, error)
		GetRun(ctx context.Context, threadID, runID string) (*openai.Run, error)
		SubmitToolOutputs(ctx context.Context, threadID, runID string, outputs []openai.ToolOutput) error
		GetRunSteps(ctx context.Context, threadID, runID string) (*openai.RunSteps, error)
	}

	trelloClient interface {
		CreateCard(ctx context.Context, card trello.TrelloCard) error
	}

	db interface {
		ExecContext(ctx context.Context, query string, args ...any) (_ sql.Result, err error)
	}

	Session struct {
		ThreadID       string
		UserID         string
		LastAccessedAt time.Time
		NameCollected  bool
		CollectedName  string
	}
)

type SessionManager struct {
	mu              sync.RWMutex
	sessions        map[string]*Session
	assistantID     string
	openaiCli       openaiClient
	trelloCli       trelloClient
	db              db
	cleanupInterval time.Duration
	sessionTimeout  time.Duration
}

func NewSessionManager(assistantID string, db db, openaiCli openaiClient, trelloCli trelloClient) (*SessionManager, error) {
	sm := &SessionManager{
		sessions:        make(map[string]*Session),
		assistantID:     assistantID,
		openaiCli:       openaiCli,
		trelloCli:       trelloCli,
		db:              db,
		cleanupInterval: 1 * time.Hour,
		sessionTimeout:  2 * time.Hour,
	}

	go sm.cleanupLoop()
	return sm, nil
}

func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(sm.cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		sm.cleanup()
	}
}

func (sm *SessionManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	threshold := time.Now().Add(-sm.sessionTimeout)
	for userID, session := range sm.sessions {
		if session.LastAccessedAt.Before(threshold) {
			delete(sm.sessions, userID)
		}
	}
}

func (sm *SessionManager) SendMessage(ctx context.Context, userID, message string) (string, error) {
	session, err := sm.getOrCreateSession(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("could not get or create session: %w", err)
	}
	session.LastAccessedAt = time.Now()

	if session.NameCollected {
		message = fmt.Sprintf("(Context: User's name is %s) %s", session.CollectedName, message)
	}

	if err := sm.openaiCli.AddMessage(ctx, openai.CreateMessageInput{
		ThreadID: session.ThreadID,
		Message: openai.ThreadMessage{
			Role:    openai.RoleUser,
			Content: message,
		},
	}); err != nil {
		// Exploration: Do not try this at home
		if strings.Contains(err.Error(), "Can't add messages to thread") {
			time.Sleep(3 * time.Second)
			if err := sm.openaiCli.AddMessage(ctx, openai.CreateMessageInput{
				ThreadID: session.ThreadID,
				Message: openai.ThreadMessage{
					Role:    openai.RoleUser,
					Content: message,
				},
			}); err != nil {
				return "", fmt.Errorf("could not add message: %w", err)
			}
		} else {
			return "", fmt.Errorf("could not add message: %w", err)
		}
	}

	run, err := sm.openaiCli.RunThread(ctx, session.ThreadID, sm.assistantID)
	if err != nil {
		return "", fmt.Errorf("could not run thread: %w", err)
	}

	if err := sm.processRun(ctx, session.ThreadID, run.ID); err != nil {
		return "", err
	}

	response, err := sm.getAssistantResponse(ctx, session.ThreadID)
	if err != nil {
		return "", err
	}

	session.LastAccessedAt = time.Now()
	return response, nil
}

func (sm *SessionManager) processRun(ctx context.Context, threadID, runID string) error {
	for {
		currentRun, err := sm.openaiCli.GetRun(ctx, threadID, runID)
		if err != nil {
			return fmt.Errorf("could not get run status: %w", err)
		}

		switch currentRun.Status {
		case openai.RunStatusCompleted:
			return nil
		case openai.RunStatusRequiresAction:
			if currentRun.RequiredAction == nil {
				return fmt.Errorf("invalid state: requires_action but no action specified")
			}
			if err := sm.handleFunctionCalling(ctx, threadID, currentRun); err != nil {
				return fmt.Errorf("could not handle function calling: %w", err)
			}
			time.Sleep(1 * time.Second)
		case openai.RunStatusFailed, openai.RunStatusCancelled, openai.RunStatusExpired:
			return fmt.Errorf("run failed with status: %s and error: %v", currentRun.Status, currentRun.LastError)
		}

		if currentRun.Status != openai.RunStatusCompleted {
			if err := sm.openaiCli.WaitForRun(ctx, threadID, runID); err != nil {
				if strings.Contains(err.Error(), "requires_action") {
					continue
				}
				return fmt.Errorf("could not wait for run: %w", err)
			}
		}
	}
}

func (sm *SessionManager) getAssistantResponse(ctx context.Context, threadID string) (string, error) {
	messages, err := sm.openaiCli.GetMessages(ctx, threadID)
	if err != nil {
		return "", fmt.Errorf("could not get messages: %w", err)
	}

	if len(messages.Data) == 0 {
		return "", fmt.Errorf("no messages returned")
	}

	var finalResponse strings.Builder
	mostRecentMsg := messages.Data[0]
	if mostRecentMsg.Role == roleAssistant && len(mostRecentMsg.Content) > 0 {
		for _, content := range mostRecentMsg.Content {
			if content.Type == messageTextType {
				finalResponse.WriteString(content.Text.Value)
				finalResponse.WriteString("\n")
			}
		}
	}
	return finalResponse.String(), nil
}

func (sm *SessionManager) handleFunctionCalling(ctx context.Context, threadID string, run *openai.Run) error {
	if run.RequiredAction == nil {
		return nil
	}

	steps, err := sm.openaiCli.GetRunSteps(ctx, threadID, run.ID)
	if err != nil {
		return fmt.Errorf("could not get run steps: %w", err)
	}

	var toolCalls []openai.ToolCall
	if len(run.RequiredAction.ToolCalls) > 0 {
		toolCalls = run.RequiredAction.ToolCalls
	} else if len(steps.Data) > 0 && steps.Data[0].StepDetails != nil {
		toolCalls = steps.Data[0].StepDetails.ToolCalls
	}

	if len(toolCalls) == 0 {
		return nil
	}

	var toolOutputs []openai.ToolOutput

	for _, toolCall := range toolCalls {
		if toolCall.Type != openai.ToolTypeFunction {
			continue
		}

		switch toolCall.Function.Name {
		case "lead":
			var args struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
				return fmt.Errorf("could not parse lead arguments: %w", err)
			}

			// Find session by threadID
			sm.mu.Lock()
			var userPhone string
			var session *Session
			for _, sess := range sm.sessions {
				if sess.ThreadID == threadID {
					userPhone = sess.UserID // UserID contains the WhatsApp number
					session = sess
					break
				}
			}
			sm.mu.Unlock()

			if session == nil {
				return fmt.Errorf("no session found for thread %s", threadID)
			}

			// Update session with name information
			session.NameCollected = true
			session.CollectedName = args.Name

			if userPhone == "" {
				return fmt.Errorf("no session found for thread %s", threadID)
			}

			if err := sm.createLead(ctx, &createLeadInput{
				Name: args.Name,
				// Clean up the phone number by removing the WhatsApp suffix.
				Phone: strings.Split(strings.Split(userPhone, "@")[0], ":")[0],
			}); err != nil {
				return fmt.Errorf("could not create lead: %w", err)
			}

			toolOutputs = append(toolOutputs, openai.ToolOutput{
				ToolCallID: toolCall.ID,
				Output:     "Lead created successfully",
			})
		}
	}

	if len(toolOutputs) > 0 {
		if err := sm.openaiCli.SubmitToolOutputs(ctx, threadID, run.ID, toolOutputs); err != nil {
			return fmt.Errorf("could not submit tool outputs: %w", err)
		}
	}
	return nil
}

func (sm *SessionManager) getOrCreateSession(ctx context.Context, userID string) (*Session, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if session, exists := sm.sessions[userID]; exists {
		session.LastAccessedAt = time.Now()
		return session, nil
	}

	thread, err := sm.openaiCli.CreateThread(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not create thread: %w", err)
	}

	sess := Session{
		ThreadID:       thread.ID,
		UserID:         userID,
		LastAccessedAt: time.Now(),
	}
	sm.sessions[userID] = &sess

	return &sess, nil
}
