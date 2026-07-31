package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"

	"yandex-bridge/internal/atomicfile"
)

// Reserved accessory ids.
//
// HomeKit identifies an accessory inside a bridge by its aid, and it remembers
// that number: rooms, names, scenes and automations all hang off it. If a
// device's aid changes between runs, HomeKit does not see "the same lamp with a
// new number" — it sees a different accessory, and the old one vanishes along
// with everything the user configured on it.
//
// brutella/hap assigns aids sequentially from the order accessories are passed
// to NewServer whenever a.Id is zero (see Server.add). Building that slice
// straight from the Yandex /v1.0/user/info device array therefore renumbers
// everything the moment Yandex reorders its response or omits one device — and
// that is exactly how a bridge silently loses its accessories over time.
//
// This registry exists to make that impossible: every device gets an aid once,
// keeps it forever, and the assignment is written to disk before it is used.
const (
	// BridgeAID is the bridge accessory itself, which hap requires to be first.
	BridgeAID uint64 = 1
	// HealthAID is the Bridge Health accessory. It is reserved so that health
	// stays at a fixed aid no matter how the device set changes.
	HealthAID uint64 = 2
	// firstDeviceAID is where device numbering starts.
	firstDeviceAID uint64 = 3
)

const registryFileMode fs.FileMode = 0o600

// ErrCorruptRegistry means the registry file exists but could not be parsed.
// Starting fresh in that situation would renumber every accessory, so the
// bridge refuses and lets a human decide.
var ErrCorruptRegistry = errors.New("accessory id registry is corrupt")

// registryFile is the on-disk form.
type registryFile struct {
	Version  int               `json:"version"`
	Next     uint64            `json:"next"`
	Assigned map[string]uint64 `json:"assigned"`
}

// Registry maps Yandex device ids to stable HomeKit accessory ids.
type Registry struct {
	path string

	mu       sync.Mutex
	next     uint64
	assigned map[string]uint64
}

// LoadRegistry reads the registry at path, or starts an empty one if the file
// does not exist.
//
// A corrupt file is a hard error rather than a fresh start: silently
// renumbering every accessory is the failure this type exists to prevent, so
// the operator is asked to delete the file deliberately if that is what they
// want.
func LoadRegistry(path string) (*Registry, error) {
	r := &Registry{
		path:     path,
		next:     firstDeviceAID,
		assigned: make(map[string]uint64),
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var f registryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", ErrCorruptRegistry, path, err)
	}

	for deviceID, aid := range f.Assigned {
		if aid < firstDeviceAID {
			return nil, fmt.Errorf("%w: device %q has reserved aid %d", ErrCorruptRegistry, deviceID, aid)
		}
		r.assigned[deviceID] = aid
		if aid >= r.next {
			r.next = aid + 1
		}
	}
	// Trust the stored watermark when it is ahead: it records aids that were
	// handed out to devices since removed, which must never be handed out again.
	if f.Next > r.next {
		r.next = f.Next
	}
	return r, nil
}

// Lookup returns the aid assigned to a device, if it has one.
func (r *Registry) Lookup(deviceID string) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aid, ok := r.assigned[deviceID]
	return aid, ok
}

// Len returns how many devices have been assigned an aid, including devices no
// longer present in the account.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.assigned)
}

// Assign returns stable aids for the given devices, allocating and persisting
// ids for any that are new.
//
// The whole set is assigned in one call so that a poll returning thirty new
// devices costs one fsync rather than thirty. New ids are allocated in sorted
// device-id order, which keeps the result deterministic regardless of how
// Yandex happened to order its response.
//
// The file is written before the aids are returned: an aid that is in use but
// not on disk would be reassigned to a different device after a restart.
func (r *Registry) Assign(deviceIDs []string) (map[string]uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var fresh []string
	for _, id := range deviceIDs {
		if id == "" {
			continue
		}
		if _, ok := r.assigned[id]; !ok {
			fresh = append(fresh, id)
		}
	}

	if len(fresh) > 0 {
		sort.Strings(fresh)
		for _, id := range fresh {
			r.assigned[id] = r.next
			// Monotonic by construction: a freed aid is never reclaimed, so a
			// removed-and-readded device gets a new identity rather than
			// inheriting a stranger's room and automations.
			r.next++
		}
		if err := r.saveLocked(); err != nil {
			// Roll back so memory cannot disagree with disk.
			for _, id := range fresh {
				delete(r.assigned, id)
			}
			r.next -= uint64(len(fresh))
			return nil, err
		}
	}

	out := make(map[string]uint64, len(deviceIDs))
	for _, id := range deviceIDs {
		if aid, ok := r.assigned[id]; ok {
			out[id] = aid
		}
	}
	return out, nil
}

// Forget removes devices from the registry. It is not called during normal
// operation — a device missing from one poll must keep its aid — and exists
// only for an explicit operator-driven cleanup.
func (r *Registry) Forget(deviceIDs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	changed := false
	for _, id := range deviceIDs {
		if _, ok := r.assigned[id]; ok {
			delete(r.assigned, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	// r.next is deliberately left alone: the point of forgetting a device is
	// to stop tracking it, not to let the next one inherit its HomeKit identity.
	return r.saveLocked()
}

// saveLocked persists the registry. r.mu must be held.
func (r *Registry) saveLocked() error {
	data, err := json.MarshalIndent(registryFile{
		Version:  1,
		Next:     r.next,
		Assigned: r.assigned,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode accessory id registry: %w", err)
	}
	data = append(data, '\n')

	if err := atomicfile.WriteWithBackup(r.path, data, registryFileMode); err != nil {
		return fmt.Errorf("persist accessory id registry: %w", err)
	}
	return nil
}
