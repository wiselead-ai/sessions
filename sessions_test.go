package sessions

import (
	"context"
	"testing"

	"encore.dev/storage/sqldb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wiselead-ai/openai"
	"github.com/wiselead-ai/trello"
)

type mockOpenAIClient struct {
	addMessageFn        func(ctx context.Context, in openai.CreateMessageInput) error
	createThreadFn      func(ctx context.Context) (*openai.Thread, error)
	getAssistantFn      func(ctx context.Context, assistantID string) (*openai.Assistant, error)
	runThreadFn         func(ctx context.Context, threadID, assistantID string) (*openai.Run, error)
	waitForRunFn        func(ctx context.Context, threadID, runID string) error
	getMessagesFn       func(ctx context.Context, threadID string) (*openai.ThreadMessageList, error)
	getRunFn            func(ctx context.Context, threadID, runID string) (*openai.Run, error)
	submitToolOutputsFn func(ctx context.Context, threadID, runID string, outputs []openai.ToolOutput) error
	getRunStepsFn       func(ctx context.Context, threadID, runID string) (*openai.RunSteps, error)
}

func (m *mockOpenAIClient) AddMessage(ctx context.Context, in openai.CreateMessageInput) error {
	return m.addMessageFn(ctx, in)
}

func (m *mockOpenAIClient) CreateThread(ctx context.Context) (*openai.Thread, error) {
	return m.createThreadFn(ctx)
}

func (m *mockOpenAIClient) GetAssistant(ctx context.Context, assistantID string) (*openai.Assistant, error) {
	return m.getAssistantFn(ctx, assistantID)
}

func (m *mockOpenAIClient) RunThread(ctx context.Context, threadID, assistantID string) (*openai.Run, error) {
	return m.runThreadFn(ctx, threadID, assistantID)
}

func (m *mockOpenAIClient) WaitForRun(ctx context.Context, threadID, runID string) error {
	return m.waitForRunFn(ctx, threadID, runID)
}

func (m *mockOpenAIClient) GetMessages(ctx context.Context, threadID string) (*openai.ThreadMessageList, error) {
	return m.getMessagesFn(ctx, threadID)
}

func (m *mockOpenAIClient) GetRun(ctx context.Context, threadID, runID string) (*openai.Run, error) {
	return m.getRunFn(ctx, threadID, runID)
}

func (m *mockOpenAIClient) SubmitToolOutputs(ctx context.Context, threadID, runID string, outputs []openai.ToolOutput) error {
	return m.submitToolOutputsFn(ctx, threadID, runID, outputs)
}

func (m *mockOpenAIClient) GetRunSteps(ctx context.Context, threadID, runID string) (*openai.RunSteps, error) {
	return m.getRunStepsFn(ctx, threadID, runID)
}

type mockTrelloClient struct {
	createCardFn func(ctx context.Context, card trello.TrelloCard) error
}

func (m *mockTrelloClient) CreateCard(ctx context.Context, card trello.TrelloCard) error {
	return m.createCardFn(ctx, card)
}

type mockDB struct {
	execFn func(ctx context.Context, query string, args ...any) (_ sqldb.ExecResult, _ error)
}

func (m *mockDB) Exec(ctx context.Context, query string, args ...any) (sqldb.ExecResult, error) {
	return m.execFn(ctx, query, args...)
}

func TestNewSessionManager(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		assistantID string
		expectError bool
	}{
		{"valid assistant", "validID", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			openaiCli := mockOpenAIClient{
				getAssistantFn: func(ctx context.Context, assistantID string) (*openai.Assistant, error) {
					require.Equal(t, tc.assistantID, assistantID)
					return &openai.Assistant{}, nil
				},
			}

			trelloCli := mockTrelloClient{}

			db := mockDB{}

			sm, err := NewSessionManager(tc.assistantID, &db, &openaiCli, &trelloCli)
			require.NoError(t, err, tc.expectError)

			assert.NotNil(t, sm)
		})
	}
}

func TestSessionManager_SendMessage(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		userID      string
		message     string
		expectError bool
	}{
		{
			name:        "success",
			userID:      "validID",
			message:     "validMessage",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			openaiCli := mockOpenAIClient{
				getAssistantFn: func(ctx context.Context, assistantID string) (*openai.Assistant, error) {
					return &openai.Assistant{}, nil
				},
				addMessageFn: func(ctx context.Context, in openai.CreateMessageInput) error {
					return nil
				},
				createThreadFn: func(ctx context.Context) (*openai.Thread, error) {
					return &openai.Thread{}, nil
				},
				runThreadFn: func(ctx context.Context, threadID, assistantID string) (*openai.Run, error) {
					return &openai.Run{}, nil
				},
				getRunFn: func(ctx context.Context, threadID, runID string) (*openai.Run, error) {
					return &openai.Run{
						Status: openai.RunStatusCompleted,
					}, nil
				},
				// TODO: Not called if run is completed. Assert that it is not called.
				// and add a test case where it is called.
				waitForRunFn: func(ctx context.Context, threadID, runID string) error {
					return nil
				},
				getMessagesFn: func(ctx context.Context, threadID string) (*openai.ThreadMessageList, error) {
					return &openai.ThreadMessageList{
						Data: []openai.MessageContent{
							{
								ID: "dummyID",
							},
						},
					}, nil
				},
			}

			trelloCli := mockTrelloClient{
				createCardFn: func(ctx context.Context, card trello.TrelloCard) error {
					return nil
				},
			}

			db := mockDB{
				execFn: func(ctx context.Context, query string, args ...any) (_ sqldb.ExecResult, _ error) {
					return nil, nil
				},
			}

			sm, err := NewSessionManager("validID", &db, &openaiCli, &trelloCli)
			require.NoError(t, err)

			_, err = sm.SendMessage(context.TODO(), tc.userID, tc.message)
			require.NoError(t, err, tc.expectError)
		})
	}
}

func TestSessionManager_getOrCreateSession(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		userID      string
		expectError bool
	}{
		{
			name:        "success",
			userID:      "validID",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			openaiCli := mockOpenAIClient{
				getAssistantFn: func(ctx context.Context, assistantID string) (*openai.Assistant, error) {
					return &openai.Assistant{}, nil
				},
				createThreadFn: func(ctx context.Context) (*openai.Thread, error) {
					return &openai.Thread{}, nil
				},
			}

			trelloCli := mockTrelloClient{}

			db := mockDB{}

			sm, err := NewSessionManager("validID", &db, &openaiCli, &trelloCli)
			require.NoError(t, err)

			session, err := sm.getOrCreateSession(context.TODO(), tc.userID)
			require.NoError(t, err, tc.expectError)

			assert.NotNil(t, session)
		})
	}
}
