package runtime

import (
	"context"
	"errors"
	"time"

	"zende.local/harness/internal/protocol"
)

func (c *Core) waitForAnswers(ctx, persist context.Context, agent, task string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := c.Store.QuestionBarrier(persist, agent, task)
		if err != nil {
			var known *protocol.Error
			if errors.As(err, &known) && known.Data.Kind == "task_cancelled" {
				return err
			}
			return protocol.E("persistence_failed", "question delivery could not be confirmed; inspect the original task")
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
