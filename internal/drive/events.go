package drive

import (
	"context"
	"log/slog"
	"time"

	proton "github.com/henrybear327/go-proton-api"
)

// Event is one remote change, already decrypted as far as possible.
type Event struct {
	Type     proton.LinkEventType
	LinkID   string
	ParentID string // new parent for moves/creates
	IsDir    bool
	Refresh  bool // server asked for a full resync; other fields empty
}

// Events polls volume events every interval and calls fn for each one until ctx is done.
// It returns after ctx is cancelled. Errors are logged and polling continues.
// When paused is non-nil and returns true, the tick is skipped without calling the API; the
// next unpaused tick picks up from the last event seen, so nothing is lost.
func (c *Client) Events(ctx context.Context, interval time.Duration, fn func(Event), paused func() bool) {
	last, err := c.api.GetLatestVolumeEventID(ctx, c.volumeID)
	if err != nil {
		slog.Warn("getting latest volume event id failed", "err", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if paused != nil && paused() {
			continue
		}

		if last == "" {
			last, err = c.api.GetLatestVolumeEventID(ctx, c.volumeID)
			if err != nil {
				slog.Warn("getting latest volume event id failed", "err", err)
				continue
			}
		}

		for {
			ev, err := c.api.GetVolumeEvent(ctx, c.volumeID, last)
			if err != nil {
				slog.Warn("getting volume event failed", "event_id", last, "err", err)
				break
			}
			slog.Debug("volume event", "event_id", ev.EventID, "count", len(ev.Events), "refresh", ev.Refresh)

			for _, e := range ev.Events {
				fn(Event{
					Type:     e.EventType,
					LinkID:   e.Link.LinkID,
					ParentID: e.Link.ParentLinkID,
					IsDir:    e.Link.Type == proton.LinkTypeFolder,
				})
			}

			last = ev.EventID

			if ev.Refresh {
				fn(Event{Refresh: true})
			}

			if len(ev.Events) == 0 {
				break
			}
		}
	}
}
