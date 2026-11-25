package config

import (
	"os"
	"testing"
)

func TestLoad_LibrivoxEnabled(t *testing.T) {
	t.Run("defaults to enabled so existing deployments are unchanged", func(t *testing.T) {
		os.Unsetenv("LIBRARR_LIBRIVOX_ENABLED")
		t.Setenv("SETTINGS_FILE", "/nonexistent/path/settings.json")
		if !Load().LibrivoxEnabled {
			t.Error("LibrivoxEnabled = false with no env set, want true")
		}
	})

	t.Run("env var turns it off", func(t *testing.T) {
		t.Setenv("LIBRARR_LIBRIVOX_ENABLED", "false")
		t.Setenv("SETTINGS_FILE", "/nonexistent/path/settings.json")
		if Load().LibrivoxEnabled {
			t.Error("LibrivoxEnabled = true with LIBRARR_LIBRIVOX_ENABLED=false, want false")
		}
	})

	t.Run("settings file overrides env", func(t *testing.T) {
		settingsPath := t.TempDir() + "/settings.json"
		if err := os.WriteFile(settingsPath, []byte(`{"librivox_enabled": true}`), 0600); err != nil {
			t.Fatalf("write settings file: %v", err)
		}
		t.Setenv("LIBRARR_LIBRIVOX_ENABLED", "false")
		t.Setenv("SETTINGS_FILE", settingsPath)
		if !Load().LibrivoxEnabled {
			t.Error("settings file librivox_enabled=true should override env false")
		}
	})
}
