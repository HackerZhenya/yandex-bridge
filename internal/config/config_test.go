package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setEnv sets the minimum environment a valid configuration needs.
func setEnv(t *testing.T) {
	t.Helper()
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	t.Setenv("YANDEX_CLIENT_SECRET", "client-secret")
	t.Setenv("HOMEKIT_PIN", "010-20-030")
}

func TestLoadWithoutConfigFile(t *testing.T) {
	setEnv(t)
	t.Setenv("DATA_DIR", t.TempDir())

	// Running with no config file at all is the intended default, not an edge
	// case: everything supported is exported automatically.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval.Std() != 15*time.Second {
		t.Errorf("PollInterval = %s, want the 15s default", cfg.PollInterval.Std())
	}
	if len(cfg.Devices) != 0 {
		t.Errorf("Devices = %v, want empty", cfg.Devices)
	}
	if cfg.HomeKit.Pin != "01020030" {
		t.Errorf("Pin = %q, want the dashes stripped", cfg.HomeKit.Pin)
	}
}

func TestLoadMergesOverDefaults(t *testing.T) {
	setEnv(t)
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	yaml := `
poll_interval: 30s
homekit:
  name: "Мой мост"
devices:
  "socket-1":
    type: fan
    name: "Вентилятор"
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval.Std() != 30*time.Second {
		t.Errorf("PollInterval = %s, want 30s", cfg.PollInterval.Std())
	}
	if cfg.HomeKit.Name != "Мой мост" {
		t.Errorf("Name = %q, want the configured one", cfg.HomeKit.Name)
	}
	// Keys the file did not mention must keep their defaults rather than
	// becoming zero values.
	if cfg.HomeKit.Port != 51826 {
		t.Errorf("Port = %d, want the 51826 default", cfg.HomeKit.Port)
	}
	if cfg.TopologyConfirmations != 3 {
		t.Errorf("TopologyConfirmations = %d, want the default 3", cfg.TopologyConfirmations)
	}

	o := cfg.Override("socket-1")
	if o.Type != TypeFan || o.Name != "Вентилятор" {
		t.Errorf("Override = %+v, want the fan override", o)
	}
}

func TestMalformedConfigIsAnError(t *testing.T) {
	setEnv(t)
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("poll_interval: [nope"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// Silently ignoring a broken config would leave the user wondering why
	// their override does nothing.
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a malformed config file")
	}
}

func TestMissingExplicitConfigIsAnError(t *testing.T) {
	setEnv(t)
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	// An absent default file is fine; an absent file the user explicitly
	// pointed at is a typo worth reporting.
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a missing explicit CONFIG_FILE")
	}
}

func TestEnvironmentOverridesFile(t *testing.T) {
	setEnv(t)
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("HOMEKIT_PORT", "51999")
	t.Setenv("HOMEKIT_INTERFACES", "eth0, wlan0")
	t.Setenv("LOG_LEVEL", "debug")

	yaml := "homekit:\n  port: 51826\n  interfaces: [docker0]\nlog_level: info\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HomeKit.Port != 51999 {
		t.Errorf("Port = %d, want the env value 51999", cfg.HomeKit.Port)
	}
	if len(cfg.HomeKit.Interfaces) != 2 || cfg.HomeKit.Interfaces[0] != "eth0" {
		t.Errorf("Interfaces = %v, want [eth0 wlan0] from the env", cfg.HomeKit.Interfaces)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug from the env", cfg.LogLevel)
	}
}

func TestTogglesAndHealthDefaultOn(t *testing.T) {
	setEnv(t)
	t.Setenv("DATA_DIR", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ExposeToggles {
		t.Error("ExposeToggles = false, want toggles exported by default")
	}
	if !cfg.Health.Enabled || !cfg.Health.ReauthButton {
		t.Errorf("Health = %+v, want both on by default", cfg.Health)
	}
	if cfg.SettleWindow.Std() != 10*time.Second {
		t.Errorf("SettleWindow = %s, want 10s", cfg.SettleWindow.Std())
	}
	if cfg.CoalesceDelay.Std() != 60*time.Millisecond {
		t.Errorf("CoalesceDelay = %s, want 60ms", cfg.CoalesceDelay.Std())
	}
}

// TestBooleansCanBeTurnedOff guards the decode-over-defaults trick: a key set
// to false must win over a default of true.
func TestBooleansCanBeTurnedOff(t *testing.T) {
	setEnv(t)
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	yaml := "expose_toggles: false\nhealth:\n  enabled: false\n  reauth_button: false\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ExposeToggles {
		t.Error("expose_toggles: false was ignored")
	}
	if cfg.Health.Enabled || cfg.Health.ReauthButton {
		t.Errorf("Health = %+v, want both off", cfg.Health)
	}
}

func TestHideTogglesIsPerDevice(t *testing.T) {
	setEnv(t)
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	yaml := "devices:\n  \"kettle\":\n    type: thermostat\n    hide_toggles: [backlight]\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	o := cfg.Override("kettle")
	if o.Type != TypeThermostat {
		t.Errorf("Type = %q, want thermostat", o.Type)
	}
	if !o.TogglesHidden("backlight") {
		t.Error("backlight is not reported as hidden")
	}
	if o.TogglesHidden("keep_warm") {
		t.Error("keep_warm was hidden without being listed")
	}
}

func TestValidateRejectsAbsurdCoalesceDelay(t *testing.T) {
	cfg := Defaults()
	cfg.Yandex = Yandex{ClientID: "a", ClientSecret: "b"}
	cfg.HomeKit.Pin = "01020030"
	// Beyond a second a tap on a light switch starts to feel broken.
	cfg.CoalesceDelay = Duration(5 * time.Second)

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a 5s coalesce delay")
	}
}

func TestValidatePin(t *testing.T) {
	tests := []struct {
		pin     string
		wantErr bool
	}{
		{"01020030", false},
		{"010-20-030", false},
		{"", true},
		{"1234567", true},   // too short
		{"123456789", true}, // too long
		{"0102003a", true},  // not digits
		{"12345678", true},  // rejected by HomeKit itself
		{"11111111", true},
		{"00000000", true},
	}

	for _, tt := range tests {
		t.Run(tt.pin, func(t *testing.T) {
			err := validatePin(normalizePin(tt.pin))
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePin(%q) error = %v, wantErr %v", tt.pin, err, tt.wantErr)
			}
		})
	}
}

func TestMissingCredentialsAreReportedTogether(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("YANDEX_CLIENT_ID", "")
	t.Setenv("YANDEX_CLIENT_SECRET", "")
	t.Setenv("HOMEKIT_PIN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an empty environment")
	}
	// Reporting one missing variable at a time turns setup into a guessing
	// game, so all of them are joined into a single error.
	for _, want := range []string{"YANDEX_CLIENT_ID", "YANDEX_CLIENT_SECRET", "HOMEKIT_PIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestValidateRejectsBadOverrides(t *testing.T) {
	cfg := Defaults()
	cfg.Yandex = Yandex{ClientID: "a", ClientSecret: "b"}
	cfg.HomeKit.Pin = "01020030"
	cfg.Devices = map[string]DeviceOverride{
		"d1": {Type: "toaster"},
		"d2": {ColorMode: "sepia"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted unknown override values")
	}
	for _, want := range []string{"toaster", "sepia"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestValidateRejectsTooFastPolling(t *testing.T) {
	cfg := Defaults()
	cfg.Yandex = Yandex{ClientID: "a", ClientSecret: "b"}
	cfg.HomeKit.Pin = "01020030"
	cfg.PollInterval = Duration(100 * time.Millisecond)

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a sub-second poll interval")
	}
}

func TestDurationParsing(t *testing.T) {
	setEnv(t)
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("poll_interval: 90\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// A bare number is ambiguous — 90 what? — so it is rejected rather than
	// guessed at.
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a duration without a unit")
	}
}

func TestPathsLiveInDataDir(t *testing.T) {
	cfg := Defaults()
	cfg.DataDir = "/data"

	for name, got := range map[string]string{
		"token":    cfg.TokenPath(),
		"aids":     cfg.AIDPath(),
		"hapstore": cfg.HAPStorePath(),
	} {
		if !strings.HasPrefix(got, "/data/") {
			t.Errorf("%s path = %q, want it under the data dir", name, got)
		}
	}
}
