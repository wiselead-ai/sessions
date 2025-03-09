package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"encore.dev/rlog"
	"encore.dev/storage/sqldb"
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
		Exec(ctx context.Context, query string, args ...any) (_ sqldb.ExecResult, _ error)
	}

	Session struct {
		ThreadID       string
		UserID         string
		LastAccessedAt time.Time
		NameCollected  bool
		CollectedName  string
		LeadRegistered bool
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
	rlog.Info("Sending message", "userID", userID, "message", message)
	session, err := sm.getOrCreateSession(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("could not get or create session: %w", err)
	}
	session.LastAccessedAt = time.Now()

	// Check for active runs before proceeding
	active, runID, err := sm.hasActiveRun(ctx, session.ThreadID)
	if err != nil {
		return "", fmt.Errorf("could not check active runs: %w", err)
	}

	if active {
		rlog.Info("Cancelling active run", "runID", runID)
		// Wait a bit before proceeding
		time.Sleep(2 * time.Second)
	}

	if session.NameCollected {
		message = fmt.Sprintf("(Context: User's name is %s) %s", session.CollectedName, message)
	}

	// Try to add message with retries
	maxRetries := 10
	backoff := 2 * time.Second
	var addMessageErr error

	for i := 0; i < maxRetries; i++ {
		addMessageErr = sm.openaiCli.AddMessage(ctx, openai.CreateMessageInput{
			ThreadID: session.ThreadID,
			Message: openai.ThreadMessage{
				Role:    openai.RoleUser,
				Content: message,
			},
		})

		if addMessageErr == nil {
			break
		}

		if !strings.Contains(addMessageErr.Error(), "Can't add messages to thread") {
			return "", fmt.Errorf("could not add message: %w", addMessageErr)
		}

		// Wait before retrying
		time.Sleep(backoff)
		backoff *= 2 // Exponential backoff
	}

	if addMessageErr != nil {
		return "", fmt.Errorf("failed to add message after retries: %w", addMessageErr)
	}

	run, err := sm.openaiCli.RunThread(ctx, session.ThreadID, sm.assistantID)
	if err != nil {
		return "", fmt.Errorf("could not run thread: %w", err)
	}

	// Use a separate context for run processing with longer timeout
	runCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	maxRetries = 10
	var processErr error
	for i := 0; i < maxRetries; i++ {
		processErr = sm.processRun(runCtx, session.ThreadID, run.ID)
		if processErr == nil {
			break
		}
		if i < maxRetries-1 {
			// Add exponential backoff
			backoff := time.Second * time.Duration(1<<uint(i))
			time.Sleep(backoff)
			continue
		}
	}
	if processErr != nil {
		return "", fmt.Errorf("failed to process run after retries: %w", processErr)
	}

	response, err := sm.getAssistantResponse(ctx, session.ThreadID)
	if err != nil {
		return "", err
	}

	session.LastAccessedAt = time.Now()
	return response, nil
}

// Add new helper method to check for active runs
func (sm *SessionManager) hasActiveRun(ctx context.Context, threadID string) (bool, string, error) {
	messages, err := sm.openaiCli.GetMessages(ctx, threadID)
	if err != nil {
		return false, "", fmt.Errorf("could not get messages: %w", err)
	}

	// If no messages, no active runs
	if len(messages.Data) == 0 {
		return false, "", nil
	}

	// Get all runs for the thread and check their status
	latestMsg := messages.Data[0]

	// If there's no run associated with the message, assume no active run
	if latestMsg.RunID == "" {
		return false, "", nil
	}

	run, err := sm.openaiCli.GetRun(ctx, threadID, latestMsg.RunID)
	if err != nil {
		return false, "", fmt.Errorf("could not get run status: %w", err)
	}

	// Consider these statuses as active
	active := run.Status == openai.RunStatusQueued ||
		run.Status == openai.RunStatusInProgress ||
		run.Status == openai.RunStatusRequiresAction

	return active, latestMsg.RunID, nil
}

func (sm *SessionManager) processRun(ctx context.Context, threadID, runID string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			currentRun, err := sm.openaiCli.GetRun(ctx, threadID, runID)
			if err != nil {
				return fmt.Errorf("could not get run status: %w", err)
			}

			switch currentRun.Status {
			case openai.RunStatusCompleted:
				return nil
			case openai.RunStatusRequiresAction:
				if err := sm.handleFunctionCalling(ctx, threadID, currentRun); err != nil {
					return fmt.Errorf("could not handle function calling: %w", err)
				}
			case openai.RunStatusFailed, openai.RunStatusCancelled, openai.RunStatusExpired:
				return fmt.Errorf("run failed with status: %s and error: %v", currentRun.Status, currentRun.LastError)
			case openai.RunStatusQueued, openai.RunStatusInProgress:
				continue
			default:
				return fmt.Errorf("unknown run status: %s", currentRun.Status)
			}
		}
	}
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

	var (
		toolOutputs       []openai.ToolOutput
		leadToolProcessed bool
	)

	for _, toolCall := range toolCalls {
		if toolCall.Type != openai.ToolTypeFunction {
			continue
		}

		switch toolCall.Function.Name {
		case "lead":
			if leadToolProcessed {
				rlog.Info("Lead tool already processed in this run, skipping", "threadID", threadID, "runID", run.ID)
				continue
			}

			// Get session once at the beginning
			sm.mu.Lock()

			var (
				userPhone string
				session   *Session
			)

			for _, sess := range sm.sessions {
				if sess.ThreadID == threadID {
					userPhone = sess.UserID
					session = sess
					break
				}
			}

			// Check for already registered lead
			if session != nil && session.LeadRegistered {
				sm.mu.Unlock()
				rlog.Info("Lead already registered for this session, skipping", "threadID", threadID)
				return nil
			}
			sm.mu.Unlock()

			if session == nil {
				return fmt.Errorf("no session found for thread %s", threadID)
			}

			var args struct {
				Name    string `json:"name"`
				Title   string `json:"title"`
				Summary string `json:"summary"`
			}
			rlog.Info("Received lead function call",
				"arguments", toolCall.Function.Arguments,
				"threadID", threadID)

			if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
				return fmt.Errorf("could not parse lead arguments: %w", err)
			}

			rlog.Info("Parsed lead data",
				"name", args.Name,
				"title", args.Title,
				"summary", args.Summary)

			leadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()

			if err := sm.createLead(leadCtx, &createLeadInput{
				Name:    args.Name,
				Phone:   strings.Split(strings.Split(userPhone, "@")[0], ":")[0],
				Title:   args.Title,
				Summary: args.Summary,
			}); err != nil {
				return fmt.Errorf("could not create lead: %w", err)
			}

			// Update session after successful lead creation
			sm.mu.Lock()
			session.NameCollected = true
			session.CollectedName = args.Name
			session.LeadRegistered = true
			sm.mu.Unlock()

			toolOutputs = append(toolOutputs, openai.ToolOutput{
				ToolCallID: toolCall.ID,
				Output:     "Lead created successfully",
			})
			leadToolProcessed = true
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

func (sm *SessionManager) isRunActive(ctx context.Context, threadID, runID string) (bool, error) {
	run, err := sm.openaiCli.GetRun(ctx, threadID, runID)
	if err != nil {
		return false, fmt.Errorf("could not get run status: %w", err)
	}

	switch run.Status {
	case openai.RunStatusCompleted, openai.RunStatusFailed, openai.RunStatusCancelled, openai.RunStatusExpired:
		return false, nil
	default:
		return true, nil
	}
}

func (sm *SessionManager) getAssistantResponse(ctx context.Context, threadID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

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
