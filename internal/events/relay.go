package events

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/projectbooth/booth-api/internal/store"
)

// WriteCommitted is api.write.committed: one committed app write (ADR 0123). It names the API,
// the dataset, the table, the operation and the app, and nothing else: never row values, key
// values, the reported user, the idempotency key or the granting owner.
const WriteCommitted = "api.write.committed"

// PublishedBy is booth-api's manifest id, the envelope's publishedBy (ADR 0026).
const PublishedBy = "api"

// WriteCommittedData is api.write.committed's data.
type WriteCommittedData struct {
	APISlug string `json:"apiSlug"`
	// Dataset is the API's catalog dataset id.
	Dataset string `json:"dataset"`
	// Table is the API's table as schema.name.
	Table     string `json:"table"`
	Operation string `json:"operation"`
	// Rows is how many rows the request changed: always 1 today (one row per request).
	Rows int `json:"rows"`
	// Principal is the writing app's token subject, streamlit:<workspace>:<appId>.
	Principal string `json:"principal"`
}

// Envelope is the bus's fixed event envelope (ADR 0026).
type Envelope struct {
	Workspace   string          `json:"workspace"`
	EventType   string          `json:"eventType"`
	PublishedAt time.Time       `json:"publishedAt"`
	PublishedBy string          `json:"publishedBy"`
	Data        json.RawMessage `json:"data"`
}

// Subject is an event's NATS subject: booth.<workspace>.<event-type> (ADR 0026).
func Subject(workspace, eventType string) string { return "booth." + workspace + "." + eventType }

// Defaults for Relay.
const (
	// DefaultMaxAge: a queued event not delivered within 24 hours is dropped (ADR 0123 revision).
	// A pipeline that must not miss one keeps a slow interval trigger as well.
	DefaultMaxAge = 24 * time.Hour
	// DefaultEvery is how often the relay looks for queued events without being woken: events
	// another replica queued, and those left while the bus was down.
	DefaultEvery = 5 * time.Second
	// batchSize is how many events one pass sends.
	batchSize = 100
	// publishTimeout bounds waiting for one JetStream acknowledgement.
	publishTimeout = 5 * time.Second
)

// Outbox is the slice of internal/store the relay needs.
type Outbox interface {
	RelayOutbox(ctx context.Context, limit int, publish func(store.OutboxRow) error) (int, error)
	ExpireOutbox(ctx context.Context, maxAge time.Duration) (int64, error)
}

// Publisher publishes with a JetStream acknowledgement (Bus).
type Publisher interface {
	Connected() bool
	Publish(ctx context.Context, subject string, data []byte, msgID string) error
}

// Relay publishes queued events from the outbox. Every replica runs one.
type Relay struct {
	Outbox Outbox
	Bus    Publisher
	// MaxAge and Every default to DefaultMaxAge and DefaultEvery.
	MaxAge time.Duration
	Every  time.Duration
	Now    func() time.Time

	wake chan struct{}
}

// NewRelay returns a relay with the defaults.
func NewRelay(o Outbox, bus Publisher) *Relay {
	return &Relay{Outbox: o, Bus: bus, wake: make(chan struct{}, 1)}
}

// Wake asks the relay to look at the outbox now: called after a write queued an event. It never
// blocks.
func (r *Relay) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run relays until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = DefaultEvery
	}
	for {
		if n, err := r.Expire(ctx); err != nil {
			if ctx.Err() == nil {
				log.Printf("events: expiring the outbox: %v", err)
			}
		} else if n > 0 {
			log.Printf("events: dropped %d undelivered event(s) older than %s from the outbox", n, r.maxAge())
		}
		for r.Bus.Connected() {
			n, err := r.Flush(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("events: publishing from the outbox (%d sent first): %v", n, err)
				}
				break
			}
			if n < batchSize {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(every):
		}
	}
}

func (r *Relay) maxAge() time.Duration {
	if r.MaxAge > 0 {
		return r.MaxAge
	}
	return DefaultMaxAge
}

// Expire drops queued events older than MaxAge.
func (r *Relay) Expire(ctx context.Context) (int64, error) {
	return r.Outbox.ExpireOutbox(ctx, r.maxAge())
}

// Flush makes one pass: up to one batch of queued events, oldest first, each published with its
// outbox msg_id as the JetStream message id and deleted once acknowledged. It stops at the first
// failure, leaving that event and the rest queued.
func (r *Relay) Flush(ctx context.Context) (int, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	return r.Outbox.RelayOutbox(ctx, batchSize, func(o store.OutboxRow) error {
		b, err := json.Marshal(Envelope{Workspace: o.Workspace, EventType: o.EventType, PublishedAt: now().UTC(), PublishedBy: PublishedBy, Data: o.Data})
		if err != nil {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, publishTimeout)
		defer cancel()
		return r.Bus.Publish(pctx, Subject(o.Workspace, o.EventType), b, "api-"+o.MsgID)
	})
}
