package tak

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/K13094/skylens/internal/processor"
)

// PublisherConfig holds TAK publisher settings.
type PublisherConfig struct {
	Enabled         bool
	RateLimitSec    int  // Minimum seconds between updates per drone
	StaleSeconds    int  // CoT stale time
	SendControllers bool // Whether to send controller detections
}

// Publisher subscribes to StateManager events and pushes CoT to a TAK server.
type Publisher struct {
	client *Client
	cfg    PublisherConfig
	mu     sync.RWMutex

	eventCh  chan processor.StateEvent
	lastSent map[string]time.Time // per-drone rate limiter
	rateMu   sync.Mutex

	state *processor.StateManager
	done  chan struct{}
}

// NewPublisher creates a TAK publisher.
func NewPublisher(client *Client, state *processor.StateManager, cfg PublisherConfig) *Publisher {
	if cfg.RateLimitSec < 1 {
		cfg.RateLimitSec = 3
	}
	if cfg.StaleSeconds < 10 {
		cfg.StaleSeconds = 30
	}

	return &Publisher{
		client:   client,
		cfg:      cfg,
		eventCh:  make(chan processor.StateEvent, 256),
		lastSent: make(map[string]time.Time),
		state:    state,
		done:     make(chan struct{}),
	}
}

// EventChannel returns the channel to register with StateManager.Subscribe().
func (p *Publisher) EventChannel() chan processor.StateEvent {
	return p.eventCh
}

// Start begins consuming events and pushing to TAK. Blocks until ctx cancelled.
func (p *Publisher) Start(ctx context.Context) {
	// Periodic cleanup of rate limiter map
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return

		case ev := <-p.eventCh:
			p.handleEvent(ev)

		case <-cleanupTicker.C:
			p.cleanupRateMap()
		}
	}
}

// Stop signals the publisher to stop.
func (p *Publisher) Stop() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	// Unsubscribe
	if p.state != nil {
		p.state.Unsubscribe(p.eventCh)
	}
}

// GetConfig returns a copy of the publisher config.
func (p *Publisher) GetConfig() PublisherConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// UpdateConfig updates the publisher config at runtime.
func (p *Publisher) UpdateConfig(cfg PublisherConfig) {
	if cfg.RateLimitSec < 1 {
		cfg.RateLimitSec = 3
	}
	if cfg.StaleSeconds < 10 {
		cfg.StaleSeconds = 30
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

func (p *Publisher) handleEvent(ev processor.StateEvent) {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	if !cfg.Enabled {
		return
	}

	if !p.client.IsConnected() {
		return
	}

	switch ev.Type {
	case "drone_new", "drone_update":
		d, ok := ev.Data.(*processor.Drone)
		if !ok {
			return
		}

		// Skip controllers unless configured
		if d.IsController && !cfg.SendControllers {
			return
		}

		// Skip drones without position
		if d.Latitude == 0 && d.Longitude == 0 {
			return
		}

		// Rate limit per drone
		if !p.shouldSend(d.Identifier) {
			return
		}

		// Build and send drone CoT
		event := BuildDroneEvent(d, cfg.StaleSeconds)
		data, err := MarshalEvent(event)
		if err != nil {
			slog.Warn("TAK: failed to marshal drone event", "id", d.Identifier, "error", err)
			return
		}

		if err := p.client.Send(data); err != nil {
			slog.Debug("TAK: send failed", "id", d.Identifier, "error", err)
			return
		}

		// Send operator position if available
		if d.OperatorLatitude != 0 || d.OperatorLongitude != 0 {
			oprEvent := BuildOperatorEvent(d, cfg.StaleSeconds)
			oprData, err := MarshalEvent(oprEvent)
			if err == nil {
				p.client.Send(oprData)
			}
		}

	case "drone_lost":
		d, ok := ev.Data.(*processor.Drone)
		if !ok {
			return
		}

		if d.IsController && !cfg.SendControllers {
			return
		}

		// Send drop-track event
		event := BuildDropEvent(d.Identifier)
		data, err := MarshalEvent(event)
		if err != nil {
			slog.Warn("TAK: failed to marshal drop event", "id", d.Identifier, "error", err)
			return
		}

		if err := p.client.Send(data); err != nil {
			slog.Debug("TAK: drop send failed", "id", d.Identifier, "error", err)
		}

		// Also drop operator marker (UID must match skylens-opr-{id})
		oprEvent := BuildDropOperatorEvent(d.Identifier)
		oprData, _ := MarshalEvent(oprEvent)
		p.client.Send(oprData)

		// Clean up rate limiter entry
		p.rateMu.Lock()
		delete(p.lastSent, d.Identifier)
		p.rateMu.Unlock()
	}
}

// shouldSend checks rate limiter and updates timestamp if allowed.
func (p *Publisher) shouldSend(id string) bool {
	p.mu.RLock()
	limit := time.Duration(p.cfg.RateLimitSec) * time.Second
	p.mu.RUnlock()

	now := time.Now()

	p.rateMu.Lock()
	defer p.rateMu.Unlock()

	if last, ok := p.lastSent[id]; ok {
		if now.Sub(last) < limit {
			return false
		}
	}
	p.lastSent[id] = now
	return true
}

func (p *Publisher) cleanupRateMap() {
	p.mu.RLock()
	limit := time.Duration(p.cfg.RateLimitSec) * time.Second
	p.mu.RUnlock()

	cutoff := time.Now().Add(-limit * 3)

	p.rateMu.Lock()
	defer p.rateMu.Unlock()

	for id, t := range p.lastSent {
		if t.Before(cutoff) {
			delete(p.lastSent, id)
		}
	}
}
