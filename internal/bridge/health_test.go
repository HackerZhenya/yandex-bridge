package bridge

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"yandex-bridge/internal/auth"
)

type stubTokens struct{ status auth.Status }

func (s stubTokens) Status() auth.Status { return s.status }

func TestHealthTripsOnSustainedPollFailure(t *testing.T) {
	h := NewHealth("Health", nil, true, testLogger())

	// A single blip is noise; waking somebody's phone for it would train them
	// to ignore the notification.
	h.PollFailed(1, errors.New("boom"))
	if !h.Healthy() {
		t.Error("a single failed poll tripped the health accessory")
	}
	if h.contact.Value() != contactDetected {
		t.Error("contact sensor opened after one failure")
	}

	h.PollFailed(3, errors.New("boom"))
	if h.Healthy() {
		t.Error("sustained failure did not trip the health accessory")
	}
	if h.contact.Value() != contactNotDetected {
		t.Error("contact sensor did not open on sustained failure")
	}
	if h.fault.Value() != faultGeneral {
		t.Error("StatusFault was not raised")
	}

	h.PollSucceeded()
	if !h.Healthy() {
		t.Error("health did not recover after a successful poll")
	}
	if h.contact.Value() != contactDetected {
		t.Error("contact sensor stayed open after recovery")
	}
}

func TestHealthReportsTokenProblems(t *testing.T) {
	h := NewHealth("Health", stubTokens{status: auth.Status{
		State:       auth.StateReauthRequired,
		Detail:      "waiting for confirmation",
		PendingCode: "1234567",
		PendingURL:  "https://ya.ru/device",
	}}, true, testLogger())

	h.Refresh()
	if h.Healthy() {
		t.Error("an unusable token did not trip the health accessory")
	}
	// The code belongs in the summary: it is the one thing the user has to act on.
	if got := h.Summary(); got == "ok" {
		t.Errorf("Summary = %q, want the pending code", got)
	}
}

// TestReauthButtonRestsOn covers the presentation fix: a momentary control has
// to sit in one position and be pushed out of it, and a switch parked in "off"
// made the accessory look permanently broken.
func TestReauthButtonRestsOn(t *testing.T) {
	h := NewHealth("Health", nil, true, testLogger())

	if h.reauth == nil {
		t.Fatal("re-auth switch was not created")
	}
	if !h.reauth.Value() {
		t.Error("re-auth switch rests in the off position")
	}

	var triggered atomic.Bool
	h.SetReauthFunc(func() { triggered.Store(true) })

	// Turning it off is the gesture that asks for re-authorization.
	if err := h.onReauthToggled(false); err != nil {
		t.Fatalf("onReauthToggled: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !triggered.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !triggered.Load() {
		t.Fatal("switching off did not start re-authorization")
	}

	// And it springs back, so it never sits in the alarming position.
	for time.Now().Before(deadline) {
		if h.reauth.Value() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("re-auth switch did not spring back to on")
}

func TestReauthButtonIgnoresBeingTurnedOn(t *testing.T) {
	h := NewHealth("Health", nil, true, testLogger())

	var triggered atomic.Bool
	h.SetReauthFunc(func() { triggered.Store(true) })

	if err := h.onReauthToggled(true); err != nil {
		t.Fatalf("onReauthToggled: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if triggered.Load() {
		t.Error("springing back to on re-triggered authorization")
	}
}

func TestReauthButtonCanBeDisabled(t *testing.T) {
	h := NewHealth("Health", nil, false, testLogger())

	if h.reauth != nil {
		t.Error("re-auth switch was created despite being disabled")
	}
	// The contact sensor must survive: it is the part that notifies.
	if h.contact == nil {
		t.Error("disabling the button removed the health sensor too")
	}
}

// TestHealthStateSurvivesRebuild matters because the HAP server is rebuilt
// whenever the device set changes, and a bridge that reported itself healthy
// for a moment on every rebuild would be misleading.
func TestHealthStateSurvivesRebuild(t *testing.T) {
	h := NewHealth("Health", nil, true, testLogger())
	h.Report(problemPoll, "yandex is down")

	h.Rebuild()

	if h.Healthy() {
		t.Error("the problem was forgotten across a rebuild")
	}
	if h.contact.Value() != contactNotDetected {
		t.Error("the rebuilt contact sensor reports healthy")
	}
	if !h.reauth.Value() {
		t.Error("the rebuilt re-auth switch does not rest in the on position")
	}
}
