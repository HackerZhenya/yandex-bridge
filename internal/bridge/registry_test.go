package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func tempRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aids.json")
	r, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	return r, path
}

func assign(t *testing.T, r *Registry, ids ...string) map[string]uint64 {
	t.Helper()
	got, err := r.Assign(ids)
	if err != nil {
		t.Fatalf("Assign(%v): %v", ids, err)
	}
	return got
}

func TestAssignSkipsReservedIDs(t *testing.T) {
	r, _ := tempRegistry(t)
	got := assign(t, r, "device-a")

	// aid 1 is the bridge and aid 2 is Bridge Health; devices start at 3.
	if got["device-a"] < firstDeviceAID {
		t.Errorf("aid = %d, want at least %d so the bridge and health accessories keep theirs",
			got["device-a"], firstDeviceAID)
	}
}

func TestAssignIsStableWithinAProcess(t *testing.T) {
	r, _ := tempRegistry(t)
	first := assign(t, r, "a", "b", "c")
	second := assign(t, r, "a", "b", "c")

	for id, aid := range first {
		if second[id] != aid {
			t.Errorf("device %q: aid changed from %d to %d", id, aid, second[id])
		}
	}
}

// TestAIDsSurviveRestart is the primary regression test. The reference
// TypeScript bridge lost its accessories over time, and hap assigning aids from
// slice order is the mechanism: if the numbers are not persisted, every restart
// is a chance to renumber.
func TestAIDsSurviveRestart(t *testing.T) {
	r, path := tempRegistry(t)
	before := assign(t, r, "lamp-1", "socket-2", "sensor-3")

	reopened, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("LoadRegistry after restart: %v", err)
	}
	after := assign(t, reopened, "lamp-1", "socket-2", "sensor-3")

	for id, aid := range before {
		if after[id] != aid {
			t.Errorf("device %q: aid was %d before restart and %d after", id, aid, after[id])
		}
	}
}

// TestAIDsAreStableWhenYandexReordersDevices covers the trigger that needs no
// restart at all: Yandex is under no obligation to return devices in a stable
// order, and hap numbers by position.
func TestAIDsAreStableWhenYandexReordersDevices(t *testing.T) {
	r, _ := tempRegistry(t)
	before := assign(t, r, "lamp-1", "socket-2", "sensor-3")
	after := assign(t, r, "sensor-3", "lamp-1", "socket-2")

	for id, aid := range before {
		if after[id] != aid {
			t.Errorf("device %q: aid changed from %d to %d after a reorder", id, aid, after[id])
		}
	}
}

// TestAIDsAreStableWhenADeviceGoesMissing covers the nastiest variant: one
// device drops out of a response and every device after it shifts down by one,
// so several accessories change identity at once.
func TestAIDsAreStableWhenADeviceGoesMissing(t *testing.T) {
	r, _ := tempRegistry(t)
	before := assign(t, r, "lamp-1", "socket-2", "sensor-3", "switch-4")

	// socket-2 is missing from this poll.
	partial := assign(t, r, "lamp-1", "sensor-3", "switch-4")
	for _, id := range []string{"lamp-1", "sensor-3", "switch-4"} {
		if partial[id] != before[id] {
			t.Errorf("device %q: aid changed from %d to %d while another device was missing",
				id, before[id], partial[id])
		}
	}
	if _, present := partial["socket-2"]; present {
		t.Error("Assign returned an aid for a device that was not requested")
	}

	// And when it comes back it must get its own aid, not a neighbour's.
	restored := assign(t, r, "lamp-1", "socket-2", "sensor-3", "switch-4")
	for id, aid := range before {
		if restored[id] != aid {
			t.Errorf("device %q: aid changed from %d to %d after the missing device returned",
				id, aid, restored[id])
		}
	}
}

func TestNewDevicesDoNotDisturbExistingAIDs(t *testing.T) {
	r, _ := tempRegistry(t)
	before := assign(t, r, "lamp-1", "socket-2")

	// A new device sorts before the existing ones alphabetically; it must
	// still take a fresh aid rather than pushing anyone else along.
	after := assign(t, r, "aaa-new", "lamp-1", "socket-2")
	for id, aid := range before {
		if after[id] != aid {
			t.Errorf("device %q: aid changed from %d to %d when a new device appeared", id, aid, after[id])
		}
	}
	if after["aaa-new"] == 0 {
		t.Error("new device got no aid")
	}
	for id, aid := range before {
		if after["aaa-new"] == aid {
			t.Errorf("new device reused the aid of %q", id)
		}
	}
}

func TestForgottenAIDsAreNeverReused(t *testing.T) {
	r, _ := tempRegistry(t)
	before := assign(t, r, "old-device")

	if err := r.Forget([]string{"old-device"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	after := assign(t, r, "new-device")
	// Reusing the number would hand the new device the old one's room, name
	// and automations in the Home app.
	if after["new-device"] == before["old-device"] {
		t.Errorf("new device reused aid %d from a forgotten device", after["new-device"])
	}
}

func TestForgottenAIDsAreNotReusedAfterRestart(t *testing.T) {
	r, path := tempRegistry(t)
	before := assign(t, r, "old-device")
	if err := r.Forget([]string{"old-device"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	reopened, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	after := assign(t, reopened, "new-device")
	if after["new-device"] == before["old-device"] {
		t.Errorf("aid %d was reused after a restart", after["new-device"])
	}
}

func TestAssignIsDeterministicRegardlessOfInputOrder(t *testing.T) {
	forward, _ := tempRegistry(t)
	reverse, _ := tempRegistry(t)

	a := assign(t, forward, "c", "a", "b")
	b := assign(t, reverse, "b", "c", "a")

	for id, aid := range a {
		if b[id] != aid {
			t.Errorf("device %q got aid %d in one order and %d in another", id, aid, b[id])
		}
	}
}

func TestCorruptRegistryIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aids.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Starting fresh here would renumber every accessory in the user's home,
	// so this has to be a refusal rather than a recovery.
	_, err := LoadRegistry(path)
	if !errors.Is(err, ErrCorruptRegistry) {
		t.Fatalf("LoadRegistry error = %v, want ErrCorruptRegistry", err)
	}
}

func TestRegistryRejectsReservedAIDInFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aids.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"next":4,"assigned":{"d":2}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// aid 2 belongs to Bridge Health; a device holding it would collide.
	if _, err := LoadRegistry(path); !errors.Is(err, ErrCorruptRegistry) {
		t.Fatalf("LoadRegistry error = %v, want ErrCorruptRegistry", err)
	}
}

func TestMissingRegistryStartsEmpty(t *testing.T) {
	r, _ := tempRegistry(t)
	if r.Len() != 0 {
		t.Errorf("Len = %d, want 0 for a first run", r.Len())
	}
}

func TestLookupReportsUnknownDevices(t *testing.T) {
	r, _ := tempRegistry(t)
	assign(t, r, "known")

	if _, ok := r.Lookup("known"); !ok {
		t.Error("Lookup(known) = false, want true")
	}
	if _, ok := r.Lookup("unknown"); ok {
		t.Error("Lookup(unknown) = true, want false")
	}
}

func TestAssignIgnoresEmptyDeviceIDs(t *testing.T) {
	r, _ := tempRegistry(t)
	got := assign(t, r, "", "real")

	if _, present := got[""]; present {
		t.Error("an empty device id was assigned an aid")
	}
	if got["real"] < firstDeviceAID {
		t.Errorf("aid = %d, want at least %d", got["real"], firstDeviceAID)
	}
}

func TestRegistryFileSurvivesCorruptionViaBackup(t *testing.T) {
	r, path := tempRegistry(t)
	before := assign(t, r, "lamp-1")
	assign(t, r, "lamp-1", "socket-2") // second write creates the backup

	// The backup is what makes a crash mid-write recoverable by hand.
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("no backup written: %v", err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if len(backup) == 0 {
		t.Fatal("backup is empty")
	}
	if before["lamp-1"] == 0 {
		t.Fatal("lamp-1 got no aid")
	}
}
