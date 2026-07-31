// Package config loads the bridge configuration from environment variables
// (secrets and paths) and an optional YAML file (per-device overrides).
//
// The YAML file is optional by design: with no file at all the bridge exports
// every supported device it finds in the Yandex account. The file exists only
// to hide, rename or re-shape individual devices.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// HomeKitType names the HomeKit accessory shape a Yandex device is mapped onto.
// The zero value means "decide automatically from the Yandex device type".
type HomeKitType string

const (
	TypeAuto      HomeKitType = ""
	TypeLightbulb HomeKitType = "lightbulb"
	TypeOutlet    HomeKitType = "outlet"
	TypeSwitch    HomeKitType = "switch"
	TypeFan       HomeKitType = "fan"
)

var validTypes = map[HomeKitType]bool{
	TypeAuto:      true,
	TypeLightbulb: true,
	TypeOutlet:    true,
	TypeSwitch:    true,
	TypeFan:       true,
}

// ColorMode selects which colour characteristics a light exposes. HomeKit
// misbehaves when a Lightbulb service carries ColorTemperature and
// Hue/Saturation at the same time, so exactly one of them is picked.
type ColorMode string

const (
	ColorAuto        ColorMode = ""
	ColorHSV         ColorMode = "hsv"
	ColorTemperature ColorMode = "temperature"
)

var validColorModes = map[ColorMode]bool{
	ColorAuto:        true,
	ColorHSV:         true,
	ColorTemperature: true,
}

// Duration is a time.Duration that unmarshals from a YAML string such as "15s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"15s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// DeviceOverride customises how a single Yandex device is exported.
type DeviceOverride struct {
	// Exclude hides the device from HomeKit entirely.
	Exclude bool `yaml:"exclude"`
	// Type forces a HomeKit accessory shape. The motivating case is a dumb
	// fan plugged into a smart socket: type "fan" turns the socket into a
	// HomeKit Fan with nothing but an on/off control.
	Type HomeKitType `yaml:"type"`
	// Name overrides the name shown in HomeKit.
	Name string `yaml:"name"`
	// ColorMode picks hue/saturation or colour temperature for lights.
	ColorMode ColorMode `yaml:"color_mode"`
}

// HomeKit holds the HAP server settings.
type HomeKit struct {
	// Name is the bridge name announced over mDNS.
	Name string `yaml:"name"`
	// Port is the fixed TCP port for the HAP server. A fixed port keeps the
	// service stable across restarts and can be allowed through a firewall.
	Port int `yaml:"port"`
	// Interfaces limits the mDNS announcement to the given interfaces.
	// Announcing on docker0 and wlan0 as well as eth0 is a known cause of
	// HomeKit losing track of a bridge, so pin this on a multi-homed host.
	Interfaces []string `yaml:"interfaces"`
	// Pin is the 8-digit HomeKit setup code. Read from HOMEKIT_PIN.
	Pin string `yaml:"-"`
}

// Yandex holds the OAuth application credentials, read from the environment.
type Yandex struct {
	ClientID     string
	ClientSecret string
}

// Config is the fully resolved bridge configuration.
type Config struct {
	// PollInterval is how often GET /v1.0/user/info is polled. The Yandex
	// user API offers no push mechanism, so polling is the only option.
	PollInterval Duration `yaml:"poll_interval"`
	// ConfirmWindow is how long a write is followed up with fast per-device
	// polls, to catch the real state settling through the vendor cloud.
	ConfirmWindow Duration `yaml:"confirm_window"`
	// ConfirmInterval is the fast poll interval inside ConfirmWindow.
	ConfirmInterval Duration `yaml:"confirm_interval"`
	// TopologyConfirmations is how many consecutive successful polls must
	// agree on a new device topology before the HAP server is rebuilt. This
	// is what stops a partial API response from destroying the accessory set.
	TopologyConfirmations int `yaml:"topology_confirmations"`
	// UnhealthyAfter is how many consecutive failed polls trip Bridge Health.
	UnhealthyAfter int `yaml:"unhealthy_after"`

	LogLevel   string  `yaml:"log_level"`
	HealthAddr string  `yaml:"health_addr"`
	HomeKit    HomeKit `yaml:"homekit"`

	// Devices maps a Yandex device id to its override.
	Devices map[string]DeviceOverride `yaml:"devices"`

	// DataDir holds the HAP store, token.json and aids.json. Not settable
	// from YAML: the YAML file itself lives in it.
	DataDir string `yaml:"-"`
	Yandex  Yandex `yaml:"-"`
}

// Defaults returns the configuration used when nothing is specified.
func Defaults() Config {
	return Config{
		PollInterval:          Duration(15 * time.Second),
		ConfirmWindow:         Duration(5 * time.Second),
		ConfirmInterval:       Duration(time.Second),
		TopologyConfirmations: 3,
		UnhealthyAfter:        4,
		LogLevel:              "info",
		HealthAddr:            ":8080",
		DataDir:               "/data",
		HomeKit: HomeKit{
			Name: "Yandex Bridge",
			Port: 51826,
		},
	}
}

// Override returns the override for a device id, or the zero value.
func (c Config) Override(deviceID string) DeviceOverride {
	return c.Devices[deviceID]
}

// TokenPath is where the OAuth token is persisted.
func (c Config) TokenPath() string { return filepath.Join(c.DataDir, "token.json") }

// AIDPath is where the stable accessory-id registry is persisted.
func (c Config) AIDPath() string { return filepath.Join(c.DataDir, "aids.json") }

// HAPStorePath is where the HAP pairing data is persisted.
func (c Config) HAPStorePath() string { return filepath.Join(c.DataDir, "hap") }

// Load resolves the configuration from the environment and the optional YAML
// file. The YAML path defaults to <DATA_DIR>/config.yaml; a missing file is not
// an error, a malformed one is.
func Load() (Config, error) {
	cfg := Defaults()

	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	path := os.Getenv("CONFIG_FILE")
	explicit := path != ""
	if !explicit {
		path = filepath.Join(cfg.DataDir, "config.yaml")
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Decode over the defaults so unspecified keys keep their value.
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
		// No config file: run fully automatic. This is the expected setup.
	default:
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}

	// Environment wins over YAML for everything it covers.
	cfg.Yandex.ClientID = os.Getenv("YANDEX_CLIENT_ID")
	cfg.Yandex.ClientSecret = os.Getenv("YANDEX_CLIENT_SECRET")
	cfg.HomeKit.Pin = normalizePin(os.Getenv("HOMEKIT_PIN"))

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("HEALTH_ADDR"); v != "" {
		cfg.HealthAddr = v
	}
	if v := os.Getenv("HOMEKIT_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("HOMEKIT_PORT: %w", err)
		}
		cfg.HomeKit.Port = port
	}
	if v := os.Getenv("HOMEKIT_INTERFACES"); v != "" {
		cfg.HomeKit.Interfaces = splitList(v)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports configuration that would fail later in a confusing way.
func (c Config) Validate() error {
	var errs []error

	if c.Yandex.ClientID == "" {
		errs = append(errs, errors.New("YANDEX_CLIENT_ID is not set"))
	}
	if c.Yandex.ClientSecret == "" {
		errs = append(errs, errors.New("YANDEX_CLIENT_SECRET is not set"))
	}
	if err := validatePin(c.HomeKit.Pin); err != nil {
		errs = append(errs, err)
	}
	if c.PollInterval.Std() < time.Second {
		errs = append(errs, fmt.Errorf("poll_interval %s is below the 1s minimum", c.PollInterval.Std()))
	}
	if c.ConfirmInterval.Std() <= 0 {
		errs = append(errs, errors.New("confirm_interval must be positive"))
	}
	if c.TopologyConfirmations < 1 {
		errs = append(errs, errors.New("topology_confirmations must be at least 1"))
	}
	if c.UnhealthyAfter < 1 {
		errs = append(errs, errors.New("unhealthy_after must be at least 1"))
	}
	if c.HomeKit.Port < 1 || c.HomeKit.Port > 65535 {
		errs = append(errs, fmt.Errorf("homekit.port %d is out of range", c.HomeKit.Port))
	}
	if c.HomeKit.Name == "" {
		errs = append(errs, errors.New("homekit.name must not be empty"))
	}

	for id, o := range c.Devices {
		if !validTypes[o.Type] {
			errs = append(errs, fmt.Errorf("device %q: unknown type %q", id, o.Type))
		}
		if !validColorModes[o.ColorMode] {
			errs = append(errs, fmt.Errorf("device %q: unknown color_mode %q", id, o.ColorMode))
		}
	}

	return errors.Join(errs...)
}

// normalizePin strips the conventional dashes from a setup code so that
// "010-20-030" and "01020030" are both accepted.
func normalizePin(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "-", "")
}

// trivialPins are rejected by HomeKit itself; catching them here turns a
// baffling pairing failure into a startup error.
var trivialPins = map[string]bool{
	"00000000": true, "11111111": true, "22222222": true, "33333333": true,
	"44444444": true, "55555555": true, "66666666": true, "77777777": true,
	"88888888": true, "99999999": true, "12345678": true, "87654321": true,
}

func validatePin(pin string) error {
	if pin == "" {
		return errors.New("HOMEKIT_PIN is not set (8 digits, e.g. 010-20-030)")
	}
	if len(pin) != 8 {
		return fmt.Errorf("HOMEKIT_PIN must be 8 digits, got %d", len(pin))
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return errors.New("HOMEKIT_PIN must contain digits only")
		}
	}
	if trivialPins[pin] {
		return fmt.Errorf("HOMEKIT_PIN %q is rejected by HomeKit as too simple", pin)
	}
	return nil
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
