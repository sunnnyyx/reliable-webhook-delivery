package delivery

import (
	"errors"
	"time"
)

var ErrReplayState = errors.New("delivery cannot be replayed")

type Replay struct {
	ID         string    `json:"id"`
	DeliveryID string    `json:"delivery_id"`
	Cycle      int       `json:"new_cycle_number"`
	CreatedAt  time.Time `json:"created_at"`
}
