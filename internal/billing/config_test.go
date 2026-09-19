package billing

import (
	"math"
	"strings"
	"testing"
)

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, errDecode := DecodeConfig([]byte("enabled: true\npriority: 10\nstore:\n  id: cpa-key-billing\n  version: 0.5.1\n"))
	if errDecode != nil {
		t.Fatalf("DecodeConfig: %v", errDecode)
	}
	if !cfg.Enabled || cfg.Debug || !cfg.CodexFastModeBilling || cfg.BillingMultiplier != 1 || cfg.StateFile != DefaultStateFile {
		t.Fatalf("config = %+v", cfg)
	}
	cfg, errDecode = DecodeConfig([]byte("enabled: true\ndebug: true\ncodex_fast_mode_billing: true\n"))
	if errDecode != nil || !cfg.Debug || !cfg.CodexFastModeBilling {
		t.Fatalf("config = %+v, error = %v", cfg, errDecode)
	}
}

func TestDecodeConfigBillingMultiplierAndFastOptOut(t *testing.T) {
	cfg, err := DecodeConfig([]byte("billing_multiplier: 0.2\ncodex_fast_mode_billing: false\n"))
	if err != nil || cfg.BillingMultiplier != 0.2 || cfg.CodexFastModeBilling {
		t.Fatalf("config = %+v, error = %v", cfg, err)
	}
	for _, raw := range []string{"0", "-0.2", ".nan", ".inf", "-.inf", "not-a-number"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := DecodeConfig([]byte("billing_multiplier: " + raw + "\n")); err == nil {
				t.Fatal("accepted invalid billing multiplier")
			}
		})
	}
}

func TestConfigureRejectsInvalidMultiplierBeforeChangingStore(t *testing.T) {
	store, _ := newStoreWithRepository(t)
	previous := store.Config()
	for _, multiplier := range []float64{0, -0.2, math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := previous
		cfg.BillingMultiplier = multiplier
		if err := store.Configure(cfg); err == nil {
			t.Fatalf("accepted invalid billing multiplier %v", multiplier)
		}
		if got := store.Config(); got != previous {
			t.Fatalf("invalid configuration changed live store: %+v", got)
		}
	}
}

func TestDecodeConfigAccountAPIOrigin(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:18316", "https://[::1]:8443", "http://127.0.0.2", "http://[::ffff:127.0.0.1]:8317"} {
		t.Run(origin, func(t *testing.T) {
			cfg, err := DecodeConfig([]byte("account_api_base_url: '" + origin + "/'\n"))
			if err != nil || cfg.AccountAPIBaseURL != origin {
				t.Fatalf("origin = %q, error = %v", cfg.AccountAPIBaseURL, err)
			}
		})
	}
	for _, origin := range []string{
		"http://localhost:18316", "http://192.168.1.2:18316", "https://example.com", "http://0.0.0.0:8317", "http://[::]:8317",
		"ftp://127.0.0.1", "//127.0.0.1", "http://dummy-secret@127.0.0.1", "http://127.0.0.1/v1", "http://127.0.0.1/%2f",
		"http://127.0.0.1?key=dummy-secret", "http://127.0.0.1?", "http://127.0.0.1#", "http://127.0.0.1#dummy-secret",
		"http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:abc", "http://127.0.0.1:", "http://[::1%25lo]:8317",
	} {
		t.Run(origin, func(t *testing.T) {
			_, err := DecodeConfig([]byte("account_api_base_url: '" + origin + "'\n"))
			if err == nil {
				t.Fatal("accepted a non-loopback origin or extra URL components")
			}
			if strings.Contains(err.Error(), "dummy-secret") {
				t.Fatal("configuration error included URL credentials")
			}
		})
	}
}

func TestDecodeConfigRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":  "enable: true\n",
		"extra document": "enabled: true\n---\nenabled: false\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, errDecode := DecodeConfig([]byte(raw)); errDecode == nil {
				t.Fatal("DecodeConfig accepted invalid configuration")
			}
		})
	}
}
