package processor

import (
	"log/slog"
	"time"

	"github.com/K13094/skylens/internal/intel"
)

// ControllerLink represents a correlation between a controller and a UAV.
type ControllerLink struct {
	ControllerID string    `json:"controller_id"`
	UAVID        string    `json:"uav_id"`
	Confidence   float64   `json:"confidence"`
	LastUpdated  time.Time `json:"last_updated"`
	Method       string    `json:"method"` // How link was established (e.g. "manufacturer+tap+operator")
}

// ControllerCorrelator periodically scans active drones and controllers
// to establish links between them.
type ControllerCorrelator struct {
	state    *StateManager
	interval time.Duration
	stopCh   chan struct{}
}

// NewControllerCorrelator creates a correlator that runs every interval.
func NewControllerCorrelator(state *StateManager, interval time.Duration) *ControllerCorrelator {
	return &ControllerCorrelator{
		state:    state,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the periodic correlation loop.
func (c *ControllerCorrelator) Start() {
	go c.run()
}

// Stop halts the correlator.
func (c *ControllerCorrelator) Stop() {
	close(c.stopCh)
}

func (c *ControllerCorrelator) run() {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.correlate()
		}
	}
}

func (c *ControllerCorrelator) correlate() {
	now := time.Now()
	activeWindow := 60 * time.Second

	allDrones := c.state.GetAllDrones()

	// Separate controllers and UAVs
	var controllers []*Drone
	var uavs []*Drone
	for _, d := range allDrones {
		if d.Status == "lost" {
			continue
		}
		if now.Sub(d.LastSeen) > activeWindow {
			continue
		}
		if d.IsController {
			controllers = append(controllers, d)
		} else {
			uavs = append(uavs, d)
		}
	}

	if len(controllers) == 0 || len(uavs) == 0 {
		return
	}

	// Score all controller-UAV pairs, then assign greedily (1:1 links only)
	type candidate struct {
		ctrl   *Drone
		uav    *Drone
		score  float64
		method string
	}
	var candidates []candidate
	for _, ctrl := range controllers {
		for _, uav := range uavs {
			score, method := c.scoreLink(ctrl, uav, now)
			if score >= 0.4 {
				candidates = append(candidates, candidate{ctrl, uav, score, method})
			}
		}
	}

	// Sort by score descending — best pairs first
	for i := 0; i < len(candidates)-1; i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	// Greedy 1:1 assignment
	claimedUAVs := make(map[string]bool)
	claimedCtrls := make(map[string]bool)
	for _, cand := range candidates {
		ctrlID := cand.ctrl.Identifier
		uavID := cand.uav.Identifier
		if claimedUAVs[uavID] || claimedCtrls[ctrlID] {
			continue
		}
		claimedUAVs[uavID] = true
		claimedCtrls[ctrlID] = true

		existing := c.state.GetControllerLinkByController(ctrlID)
		if existing != nil && existing.UAVID == uavID && cand.score <= existing.Confidence {
			existing.LastUpdated = now
			c.state.SetControllerLink(existing)
			continue
		}

		link := &ControllerLink{
			ControllerID: ctrlID,
			UAVID:        uavID,
			Confidence:   cand.score,
			LastUpdated:  now,
			Method:       cand.method,
		}
		c.state.SetControllerLink(link)

		c.state.updateDroneField(ctrlID, func(d *Drone) {
			d.LinkedUAVID = uavID
		})
		c.state.updateDroneField(uavID, func(d *Drone) {
			d.LinkedControllerID = ctrlID
		})

		if existing == nil || existing.UAVID != uavID {
			slog.Info("Controller-UAV link established",
				"controller", ctrlID,
				"uav", uavID,
				"confidence", cand.score,
				"method", cand.method,
			)
		}
	}

	// Clear links for unmatched controllers
	for _, ctrl := range controllers {
		if !claimedCtrls[ctrl.Identifier] {
			if existing := c.state.GetControllerLinkByController(ctrl.Identifier); existing != nil {
				c.state.RemoveControllerLink(ctrl.Identifier)
				c.state.updateDroneField(ctrl.Identifier, func(d *Drone) {
					d.LinkedUAVID = ""
				})
			}
		}
	}
}

// scoreLink computes a correlation score (0-1) between a controller and a UAV.
func (c *ControllerCorrelator) scoreLink(ctrl, uav *Drone, now time.Time) (float64, string) {
	// Required: same manufacturer
	if ctrl.Manufacturer != uav.Manufacturer {
		return 0, ""
	}
	if ctrl.Manufacturer == "" {
		return 0, ""
	}

	score := 0.0
	methods := "manufacturer"

	// Manufacturer match baseline
	score += 0.25

	// Same TAP detection
	if ctrl.TapID == uav.TapID && ctrl.TapID != "" {
		score += 0.20
		methods += "+tap"
	}

	// Temporal overlap (both seen recently)
	ctrlAge := now.Sub(ctrl.LastSeen).Seconds()
	uavAge := now.Sub(uav.LastSeen).Seconds()
	if ctrlAge < 30 && uavAge < 30 {
		score += 0.15
		methods += "+temporal"
	}

	// Operator position proximity: UAV's operator_latitude/longitude should be
	// close to the controller's position (or estimated position).
	// The UAV's operator position IS where the pilot (controller) is standing.
	if uav.OperatorLatitude != 0 && uav.OperatorLongitude != 0 {
		// If controller has a position estimate (from RSSI trilateration or range rings),
		// check proximity. Otherwise just the fact that operator position exists is a signal.
		if ctrl.Latitude != 0 && ctrl.Longitude != 0 {
			dist := intel.HaversineDistance(ctrl.Latitude, ctrl.Longitude,
				uav.OperatorLatitude, uav.OperatorLongitude)
			if dist < 500 {
				score += 0.30
				methods += "+operator_proximity"
			} else if dist < 2000 {
				score += 0.10
				methods += "+operator_nearby"
			}
		} else if ctrl.DistanceEstM > 0 {
			// Controller only has RSSI distance estimate — weaker signal but still useful
			score += 0.10
			methods += "+operator_exists"
		}
	}

	return score, methods
}

// updateDroneField applies a mutation function to a drone by identifier.
func (s *StateManager) updateDroneField(identifier string, fn func(d *Drone)) {
	shard := s.drones.getShard(identifier)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if d, ok := shard.drones[identifier]; ok {
		fn(d)
	}
}
