package sessions

import (
	"context"
	"fmt"
	"time"

	"encore.dev/rlog"
	"github.com/wiselead-ai/pkg/idutil"
	"github.com/wiselead-ai/trello"
)

const (
	newLeadsTrelloLane = "6765c8d942977be5554e82d8"
	brLocation         = "America/Sao_Paulo"
)

type createLeadInput struct {
	Name    string
	Phone   string
	Title   string
	Summary string
}

func (sm *SessionManager) createLead(ctx context.Context, input *createLeadInput) error {
	id, err := idutil.NewID()
	if err != nil {
		return fmt.Errorf("could not generate ID: %w", err)
	}

	if _, err := sm.db.Exec(ctx, `
		INSERT INTO leads (id, name, phone)
		VALUES ($1, $2, $3)
	`, id, input.Name, input.Phone); err != nil {
		return fmt.Errorf("could not insert lead: %w", err)
	}

	go func() {
		loc, err := time.LoadLocation(brLocation)
		if err != nil {
			loc = time.UTC
		}

		description := fmt.Sprintf(
			"Nome: %s\n"+
				"Telefone: %s\n\n"+
				"Interesse: %s\n"+
				"Detalhes: %s\n\n"+
				"Data: %s",
			input.Name,
			input.Phone,
			input.Title,
			input.Summary,
			time.Now().In(loc).Format("02/01/2006 às 15:04"),
		)

		if err := sm.trelloCli.CreateCard(ctx, trello.TrelloCard{
			Name:        fmt.Sprintf("%s - %s", input.Name, input.Title),
			Description: description,
			ListID:      newLeadsTrelloLane,
		}); err != nil {
			rlog.Error("Could not create Trello card", "error", err)
			return
		}
	}()
	return nil
}
