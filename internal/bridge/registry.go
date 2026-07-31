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

// registryFile is the on-disk form. Version 1 stored a bare aid per device;
// version 2 records the accessory shape alongside it.
type registryFile struct {
	Version  int                        `json:"version"`
	Next     uint64                     `json:"next"`
	Assigned map[string]json.RawMessage `json:"assigned"`
}

// entry is one device's accessory id and the shape it was issued for.
type entry struct {
	AID uint64 `json:"aid"`
	// Shape is the Spec.ShapeHash the aid was issued for. An empty value means
	// the entry predates shape tracking and is adopted as-is.
	Shape string `json:"shape,omitempty"`
}

// Registry maps Yandex device ids to stable HomeKit accessory ids.
type Registry struct {
	path string

	mu       sync.Mutex
	next     uint64
	assigned map[string]entry
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
		assigned: make(map[string]entry),
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

	for deviceID, raw := range f.Assigned {
		e, err := decodeEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: device %q in %s: %v", ErrCorruptRegistry, deviceID, path, err)
		}
		if e.AID < firstDeviceAID {
			return nil, fmt.Errorf("%w: device %q has reserved aid %d", ErrCorruptRegistry, deviceID, e.AID)
		}
		r.assigned[deviceID] = e
		if e.AID >= r.next {
			r.next = e.AID + 1
		}
	}
	// Trust the stored watermark when it is ahead: it records aids that were
	// handed out to devices since removed, which must never be handed out again.
	if f.Next > r.next {
		r.next = f.Next
	}
	return r, nil
}

// decodeEntry reads either on-disk form: a bare aid (version 1) or an object
// with the shape (version 2). Accepting both means an upgrade does not have to
// renumber anything.
func decodeEntry(raw json.RawMessage) (entry, error) {
	var aid uint64
	if err := json.Unmarshal(raw, &aid); err == nil {
		return entry{AID: aid}, nil
	}
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return entry{}, fmt.Errorf("expected an aid or {aid, shape}: %v", err)
	}
	if e.AID == 0 {
		return entry{}, errors.New("entry has no aid")
	}
	return e, nil
}

// Lookup returns the aid assigned to a device, if it has one.
func (r *Registry) Lookup(deviceID string) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.assigned[deviceID]
	return e.AID, ok
}

// Len returns how many devices have been assigned an aid, including devices no
// longer present in the account.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.assigned)
}

// Assign returns stable aids for the given specs, allocating and persisting
// ids for anything new or reshaped.
//
// The whole set is assigned in one call so that a poll returning thirty new
// devices costs one fsync rather than thirty. New ids are allocated in sorted
// device-id order, which keeps the result deterministic regardless of how
// Yandex happened to order its response.
//
// A device whose accessory shape changed — a kettle that became a thermostat,
// a lamp that gained a colour control — is issued a *new* aid on purpose.
// HomeKit caches an accessory's services and handles "same accessory, new
// layout" poorly: the Home app keeps showing the old controls indefinitely.
// Retiring the old aid and publishing a new one is a transition it does
// understand. Only the reshaped device is affected; every other aid is
// untouched, which is the protection this registry exists for.
//
// The file is written before the aids are returned: an aid that is in use but
// not on disk would be reassigned to a different device after a restart.
func (r *Registry) Assign(specs []Spec) (map[string]uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	type change struct {
		deviceID string
		shape    string
		previous entry
		existed  bool
	}

	var changes []change
	for _, spec := range specs {
		if spec.DeviceID == "" {
			continue
		}
		shape := spec.ShapeHash()
		current, ok := r.assigned[spec.DeviceID]

		switch {
		case !ok:
			changes = append(changes, change{deviceID: spec.DeviceID, shape: shape})
		case current.Shape == "":
			// Predates shape tracking. Adopt the shape without renumbering:
			// an upgrade of the bridge must not disturb a working home.
			changes = append(changes, change{
				deviceID: spec.DeviceID, shape: shape, previous: current, existed: true,
			})
		case current.Shape != shape:
			changes = append(changes, change{
				deviceID: spec.DeviceID, shape: shape, previous: current, existed: true,
			})
		}
	}

	if len(changes) > 0 {
		sort.Slice(changes, func(i, j int) bool { return changes[i].deviceID < changes[j].deviceID })

		allocated := 0
		for _, c := range changes {
			switch {
			case c.existed && c.previous.Shape == "":
				// Grandfathered: keep the aid, just record the shape.
				r.assigned[c.deviceID] = entry{AID: c.previous.AID, Shape: c.shape}
			default:
				r.assigned[c.deviceID] = entry{AID: r.next, Shape: c.shape}
				// Monotonic by construction: a freed aid is never reclaimed,
				// so a removed-and-readded device gets a new identity rather
				// than inheriting a stranger's room and automations.
				r.next++
				allocated++
			}
		}

		if err := r.saveLocked(); err != nil {
			// Roll back so memory cannot disagree with disk.
			for _, c := range changes {
				if c.existed {
					r.assigned[c.deviceID] = c.previous
				} else {
					delete(r.assigned, c.deviceID)
				}
			}
			r.next -= uint64(allocated)
			return nil, err
		}
	}

	out := make(map[string]uint64, len(specs))
	for _, spec := range specs {
		if e, ok := r.assigned[spec.DeviceID]; ok {
			out[spec.DeviceID] = e.AID
		}
	}
	return out, nil
}

// Reshaped reports devices whose aid changed because their accessory shape
// changed, so the caller can say so in the log. Call it after Assign.
func (r *Registry) Reshaped(specs []Spec, before map[string]uint64) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []string
	for _, spec := range specs {
		old, had := before[spec.DeviceID]
		if !had {
			continue
		}
		if e, ok := r.assigned[spec.DeviceID]; ok && e.AID != old {
			out = append(out, spec.DeviceID)
		}
	}
	return out
}

// Shape returns the accessory shape an aid was issued for.
func (r *Registry) Shape(deviceID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.assigned[deviceID]
	return e.Shape, ok
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
	assigned := make(map[string]json.RawMessage, len(r.assigned))
	for deviceID, e := range r.assigned {
		raw, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode entry for %q: %w", deviceID, err)
		}
		assigned[deviceID] = raw
	}

	data, err := json.MarshalIndent(registryFile{
		Version:  2,
		Next:     r.next,
		Assigned: assigned,
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
