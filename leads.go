package sessions

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wiselead-ai/pkg/idutil"
	"github.com/wiselead-ai/trello"
)

const (
	newLeadsTrelloLane = "6765c8d942977be5554e82d8"
	brLocation         = "America/Sao_Paulo"
)

type createLeadInput struct {
	Name  string
	Phone string
}

func createLead(ctx context.Context, db db, trelloCli trelloClient, input *createLeadInput) error {
	id, err := idutil.NewID()
	if err != nil {
		return fmt.Errorf("could not generate ID: %w", err)
	}

	go func() {
		if _, err := db.Exec(ctx, `
			INSERT INTO leads (id, name, phone)
			VALUES ($1, $2, $3)
		`, id, input.Name, input.Phone); err != nil {
			fmt.Fprintf(os.Stderr, "could not insert lead: %v\n", err)
			return
		}

		loc, err := time.LoadLocation(brLocation)
		if err != nil {
			loc = time.UTC
		}

		now := time.Now().In(loc)
		description := fmt.Sprintf(
			"Nome: %s\nTelefone: %s\nCriado em: %s",
			input.Name,
			input.Phone,
			now.Format("02/01/2006 às 15:04"),
		)

		if err := trelloCli.CreateCard(trello.TrelloCard{
			Name:        input.Name,
			Description: description,
			ListID:      newLeadsTrelloLane,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "could not create Trello card: %v\n", err)
			return
		}

		fmt.Printf("Created lead - ID: %s, Name: %s, Phone: %s\n", id, input.Name, input.Phone)
	}()
	return nil
}
