// Package yandex is a typed client for the Yandex smart home user API at
// https://api.iot.yandex.net.
//
// The API is small — six endpoints — but its capability and property states are
// polymorphic on the capability type, so states arrive as json.RawMessage and
// are decoded through the typed accessors in capability.go. That keeps the
// mapping layer from having to type-assert its way through interface{}.
package yandex

import (
	"encoding/json"
	"strings"
)

// BaseURL is the Yandex smart home user API host.
const BaseURL = "https://api.iot.yandex.net"

// DeviceType identifies a Yandex device class, e.g. "devices.types.light.ceiling".
type DeviceType string

// Device types this bridge knows how to map. The list is not exhaustive:
// unknown types are skipped rather than guessed at.
const (
	DeviceTypeLight         DeviceType = "devices.types.light"
	DeviceTypeSocket        DeviceType = "devices.types.socket"
	DeviceTypeSwitch        DeviceType = "devices.types.switch"
	DeviceTypeSensor        DeviceType = "devices.types.sensor"
	DeviceTypeSensorClimate DeviceType = "devices.types.sensor.climate"
	// DeviceTypeCooking covers kettles, coffee makers and multicookers.
	DeviceTypeCooking DeviceType = "devices.types.cooking"
	// DeviceTypeThermostat is a heater with a target temperature.
	DeviceTypeThermostat DeviceType = "devices.types.thermostat"
	// DeviceTypeThermostatAC is an air conditioner. It carries a temperature
	// range like a thermostat but also cools, so it is treated separately.
	DeviceTypeThermostatAC DeviceType = "devices.types.thermostat.ac"
)

// Base returns the type without its subtype, so that
// "devices.types.light.ceiling" and "devices.types.light" compare equal.
// Yandex nests subtypes with a dot and adds new ones over time; matching on the
// base keeps a new subtype from silently dropping a device.
func (t DeviceType) Base() DeviceType {
	const prefix = "devices.types."
	rest, ok := strings.CutPrefix(string(t), prefix)
	if !ok {
		return t
	}
	if i := strings.Index(rest, "."); i >= 0 {
		rest = rest[:i]
	}
	return DeviceType(prefix + rest)
}

// ConnectionState reports whether Yandex can currently reach a device.
type ConnectionState string

const (
	ConnectionOnline  ConnectionState = "online"
	ConnectionOffline ConnectionState = "offline"
)

// Offline reports whether the device is known to be unreachable. An empty
// state means the endpoint did not report one, which is not an error:
// GET /v1.0/user/info omits it while GET /v1.0/devices/{id} includes it.
func (s ConnectionState) Offline() bool { return s == ConnectionOffline }

// Envelope is the status header every API response carries.
type Envelope struct {
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
	Message   string `json:"message"`
}

// OK reports whether the API considered the request successful.
func (e Envelope) OK() bool { return e.Status == "ok" }

// Device is a single device in the user's smart home.
type Device struct {
	Envelope

	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Aliases     []string        `json:"aliases"`
	Type        DeviceType      `json:"type"`
	State       ConnectionState `json:"state"`
	ExternalID  string          `json:"external_id"`
	SkillID     string          `json:"skill_id"`
	HouseholdID string          `json:"household_id"`
	Room        string          `json:"room"`
	Groups      []string        `json:"groups"`

	Capabilities []Capability `json:"capabilities"`
	Properties   []Property   `json:"properties"`
}

// Capability finds the first capability of the given type, if any.
func (d Device) Capability(t CapabilityType) (Capability, bool) {
	for _, c := range d.Capabilities {
		if c.Type == t {
			return c, true
		}
	}
	return Capability{}, false
}

// HasCapability reports whether the device exposes the given capability type.
func (d Device) HasCapability(t CapabilityType) bool {
	_, ok := d.Capability(t)
	return ok
}

// RangeCapability finds the range capability for a given instance, e.g.
// RangeBrightness. A device may expose several ranges at once.
func (d Device) RangeCapability(instance RangeInstance) (Capability, bool) {
	for _, c := range d.Capabilities {
		if c.Type != CapabilityRange {
			continue
		}
		if p, err := c.RangeParameters(); err == nil && p.Instance == instance {
			return c, true
		}
	}
	return Capability{}, false
}

// FloatProperty finds the float property for a given instance, e.g.
// FloatTemperature.
func (d Device) FloatProperty(instance FloatInstance) (Property, bool) {
	for _, p := range d.Properties {
		if p.Type != PropertyFloat {
			continue
		}
		if params, err := p.FloatParameters(); err == nil && params.Instance == instance {
			return p, true
		}
	}
	return Property{}, false
}

// Room is a room in a household.
type Room struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	HouseholdID string   `json:"household_id"`
	Devices     []string `json:"devices"`
}

// Group is a device group.
type Group struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Aliases      []string     `json:"aliases"`
	HouseholdID  string       `json:"household_id"`
	Type         DeviceType   `json:"type"`
	Devices      []string     `json:"devices"`
	Capabilities []Capability `json:"capabilities"`
}

// Household is a home.
type Household struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Scenario is a user-defined scenario.
type Scenario struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
}

// UserInfo is the response of GET /v1.0/user/info: the whole smart home in one
// request, which is why it is the bridge's primary poll.
type UserInfo struct {
	Envelope

	Rooms      []Room      `json:"rooms"`
	Groups     []Group     `json:"groups"`
	Devices    []Device    `json:"devices"`
	Scenarios  []Scenario  `json:"scenarios"`
	Households []Household `json:"households"`
}

// RoomName resolves a room id to its name, returning "" when unknown.
func (u UserInfo) RoomName(id string) string {
	for _, r := range u.Rooms {
		if r.ID == id {
			return r.Name
		}
	}
	return ""
}

// Action is one capability change requested on a device.
type Action struct {
	Type  CapabilityType `json:"type"`
	State ActionState    `json:"state"`
}

// ActionState carries the target value. Values are absolute, never relative:
// "set brightness to 40" is safe to retry, "increase brightness by 10" is not,
// and every write this bridge makes must be idempotent.
type ActionState struct {
	Instance string `json:"instance"`
	Value    any    `json:"value"`
}

// DeviceActions groups the actions requested on a single device.
type DeviceActions struct {
	ID      string   `json:"id"`
	Actions []Action `json:"actions"`
}

// ActionRequest is the body of POST /v1.0/devices/actions.
type ActionRequest struct {
	Devices []DeviceActions `json:"devices"`
}

// ActionOutcome is the per-capability result of an action.
type ActionOutcome struct {
	Status       string `json:"status"` // "DONE" or "ERROR"
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

// Done reports whether the capability was applied successfully.
func (o ActionOutcome) Done() bool { return o.Status == "DONE" }

// ActionCapabilityResult is the result for one capability of one device.
type ActionCapabilityResult struct {
	Type  CapabilityType `json:"type"`
	State struct {
		Instance string        `json:"instance"`
		Result   ActionOutcome `json:"action_result"`
	} `json:"state"`
}

// ActionDeviceResult is the result for one device.
type ActionDeviceResult struct {
	ID           string                   `json:"id"`
	Capabilities []ActionCapabilityResult `json:"capabilities"`
}

// ActionResponse is the response of POST /v1.0/devices/actions.
//
// An HTTP 200 here does not mean the device did anything: each capability
// carries its own action_result, and a failure shows up as ERROR with a device
// error code. Err collapses that into a single error.
type ActionResponse struct {
	Envelope

	Devices []ActionDeviceResult `json:"devices"`
}

// Err returns a non-nil error if any requested capability failed.
func (r ActionResponse) Err() error {
	var errs []error
	for _, d := range r.Devices {
		for _, c := range d.Capabilities {
			if c.State.Result.Done() {
				continue
			}
			errs = append(errs, &DeviceError{
				DeviceID:   d.ID,
				Capability: c.Type,
				Instance:   c.State.Instance,
				Code:       c.State.Result.ErrorCode,
				Message:    c.State.Result.ErrorMessage,
			})
		}
	}
	return joinErrors(errs)
}

// rawOrNull reports whether a raw JSON message is absent or literal null.
func rawOrNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
