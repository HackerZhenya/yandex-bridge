package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"

	"yandex-bridge/internal/auth"
)

// HomeKit contact sensor states.
const (
	contactDetected    = 0 // "Closed" in the Home app — everything is fine
	contactNotDetected = 1 // "Open" — something needs attention
)

// HomeKit fault states.
const (
	faultNone    = 0
	faultGeneral = 1
)

// Problem categories tracked by the health accessory.
const (
	problemAuth = "auth"
	problemPoll = "poll"
)

// TokenSource reports the state of the Yandex authorization.
type TokenSource interface {
	Status() auth.Status
}

// Health is the "Bridge Health" accessory.
//
// It exists because a bridge that quietly stops working is worse than one that
// fails loudly: the user finds out weeks later, when an automation does not
// fire. A contact sensor is the vehicle because the Home app can send push
// notifications for one out of the box, with no code and no extra service —
// the user just turns on notifications for this accessory.
//
// It keeps a fixed accessory id and is present in every build of the accessory
// set, so it survives the rebuilds that device changes trigger.
type Health struct {
	A *accessory.A

	contact *characteristic.ContactSensorState
	fault   *characteristic.StatusFault
	reauth  *characteristic.On

	name   string
	tokens TokenSource
	logger *slog.Logger

	mu       sync.Mutex
	problems map[string]string

	// reauthFn restarts the device flow. It is invoked from the Home app, so
	// it must not block the HAP request.
	reauthFn func()
}

// NewHealth builds the health accessory.
func NewHealth(name string, tokens TokenSource, logger *slog.Logger) *Health {
	h := &Health{
		name:     name,
		tokens:   tokens,
		logger:   logger,
		problems: make(map[string]string),
	}
	h.Rebuild()
	return h
}

// Rebuild constructs a fresh HomeKit accessory carrying the current state.
//
// It is called on every HAP server restart rather than reusing the existing
// accessory, because hap.NewServer appends a value-update callback to every
// characteristic it is given: passing the same accessory to a second server
// would make each change notify paired controllers twice, then three times.
func (h *Health) Rebuild() {
	h.A = accessory.New(accessory.Info{
		Name:         h.name,
		Manufacturer: manufacturer,
		Model:        "yandex-bridge",
		SerialNumber: "bridge-health",
		Firmware:     Version,
	}, accessory.TypeSensor)
	h.A.Id = HealthAID

	sensor := service.NewContactSensor()
	h.contact = sensor.ContactSensorState
	h.contact.SetValue(contactDetected)

	// StatusFault gives the Home app a second, subtler signal: the accessory
	// shows a fault badge even when the user has notifications turned off.
	h.fault = characteristic.NewStatusFault()
	sensor.AddC(h.fault.C)
	h.A.AddS(sensor.S)

	// A switch the user can flip to re-run the device flow without SSHing
	// into the Pi. It springs back to off once the flow has been kicked off.
	reauthSwitch := service.NewSwitch()
	h.reauth = reauthSwitch.On
	h.reauth.OnSetRemoteValue(h.onReauthToggled)
	h.A.AddS(reauthSwitch.S)

	// Carry any problems recorded before the rebuild into the new
	// characteristics, so a restart does not briefly report a healthy bridge.
	h.refresh()
}

// SetReauthFunc installs the callback that restarts device authorization.
func (h *Health) SetReauthFunc(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reauthFn = fn
}

func (h *Health) onReauthToggled(on bool) error {
	if !on {
		return nil
	}
	h.mu.Lock()
	fn := h.reauthFn
	h.mu.Unlock()

	if fn == nil {
		return fmt.Errorf("re-authorization is not available")
	}
	h.logger.Info("re-authorization requested from the Home app")
	go fn()

	// Reset the switch so it reads as a button rather than a state. The reset
	// runs in the background because hap is still inside this write.
	go func() {
		time.Sleep(time.Second)
		h.reauth.SetValue(false)
	}()
	return nil
}

// PollSucceeded implements Observer.
func (h *Health) PollSucceeded() { h.Clear(problemPoll) }

// PollFailed implements Observer.
func (h *Health) PollFailed(consecutive int, err error) {
	// One failed poll is noise. Only sustained failure is worth waking
	// somebody's phone for.
	if consecutive < 2 {
		return
	}
	h.Report(problemPoll, fmt.Sprintf("%d consecutive failed polls: %v", consecutive, err))
}

// Report records a problem and updates the accessory.
func (h *Health) Report(category, detail string) {
	h.mu.Lock()
	changed := h.problems[category] != detail
	h.problems[category] = detail
	h.mu.Unlock()

	if changed {
		h.logger.Warn("bridge health problem",
			slog.String("category", category),
			slog.String("detail", detail))
	}
	h.refresh()
}

// Clear removes a problem category.
func (h *Health) Clear(category string) {
	h.mu.Lock()
	_, existed := h.problems[category]
	delete(h.problems, category)
	h.mu.Unlock()

	if existed {
		h.logger.Info("bridge health problem resolved", slog.String("category", category))
	}
	h.refresh()
}

// Refresh re-reads the token status and updates the accessory. It is called
// periodically so a token problem shows up even when nothing else happens.
func (h *Health) Refresh() {
	if h.tokens != nil {
		status := h.tokens.Status()
		if status.Healthy() {
			h.Clear(problemAuth)
		} else {
			detail := status.Detail
			if status.PendingCode != "" {
				detail = fmt.Sprintf("enter code %s at %s", status.PendingCode, status.PendingURL)
			}
			h.Report(problemAuth, detail)
			return
		}
	}
	h.refresh()
}

// refresh pushes the current problem set into the characteristics.
func (h *Health) refresh() {
	h.mu.Lock()
	healthy := len(h.problems) == 0
	h.mu.Unlock()

	state, fault := contactNotDetected, faultGeneral
	if healthy {
		state, fault = contactDetected, faultNone
	}
	if h.contact.Value() != state {
		_ = h.contact.SetValue(state)
	}
	if h.fault.Value() != fault {
		_ = h.fault.SetValue(fault)
	}
}

// Healthy reports whether the bridge currently believes it is working.
func (h *Health) Healthy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.problems) == 0
}

// Problems returns the current problems, sorted by category.
func (h *Health) Problems() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make(map[string]string, len(h.problems))
	for k, v := range h.problems {
		out[k] = v
	}
	return out
}

// Summary renders the current problems as one line.
func (h *Health) Summary() string {
	problems := h.Problems()
	if len(problems) == 0 {
		return "ok"
	}
	categories := make([]string, 0, len(problems))
	for k := range problems {
		categories = append(categories, k)
	}
	sort.Strings(categories)

	parts := make([]string, 0, len(categories))
	for _, c := range categories {
		parts = append(parts, fmt.Sprintf("%s: %s", c, problems[c]))
	}
	return strings.Join(parts, "; ")
}

// Run refreshes the health state periodically until ctx is done.
func (h *Health) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	h.Refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.Refresh()
		}
	}
}
