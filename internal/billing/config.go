package billing

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultStateFile = "plugins/cpa-key-billing-state-v1.db"

type Config struct {
	Enabled               bool    `yaml:"enabled"`
	Debug                 bool    `yaml:"debug"`
	StateFile             string  `yaml:"state_file"`
	CodexFastModeBilling  bool    `yaml:"codex_fast_mode_billing"`
	BillingMultiplier     float64 `yaml:"billing_multiplier"`
	AccountAPIBaseURL     string  `yaml:"account_api_base_url"`
	MaskAPIKeyViewEmails  bool    `yaml:"mask_api_key_view_emails"`
	AllowAPIKeyQuotaReset bool    `yaml:"allow_api_key_quota_reset"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:              false,
		StateFile:            DefaultStateFile,
		BillingMultiplier:    1,
		CodexFastModeBilling: true,
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		document := struct {
			Config `yaml:",inline"`
			// These fields belong to the host and are ignored by the plugin.
			Priority int       `yaml:"priority"`
			Store    yaml.Node `yaml:"store"`
		}{Config: cfg}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if errDecode := decoder.Decode(&document); errDecode != nil {
			return Config{}, fmt.Errorf("Parse plugin configuration: %w", errDecode)
		}
		if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
			return Config{}, fmt.Errorf("Plugin configuration must contain exactly one YAML document")
		}
		cfg = document.Config
	}
	cfg = cfg.normalized()
	if err := cfg.validateBillingMultiplier(); err != nil {
		return Config{}, err
	}
	origin, err := ValidateAccountAPIBaseURL(cfg.AccountAPIBaseURL)
	if err != nil {
		return Config{}, err
	}
	cfg.AccountAPIBaseURL = origin
	return cfg, nil
}

func (c Config) validateBillingMultiplier() error {
	if c.BillingMultiplier <= 0 || math.IsNaN(c.BillingMultiplier) || math.IsInf(c.BillingMultiplier, 0) {
		return fmt.Errorf("billing_multiplier must be a finite number greater than zero")
	}
	return nil
}

// Config returns a snapshot without holding the store lock during HTTP calls.
func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// ValidateAccountAPIBaseURL confines user-key checks to an explicitly configured
// loopback origin. Hostnames, URL credentials and request-controlled paths are
// deliberately unsupported: the user's key must never leave this machine.
func ValidateAccountAPIBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	invalid := func() (string, error) {
		return "", fmt.Errorf("account_api_base_url must be an HTTP or HTTPS numeric loopback origin without credentials, path, query, or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return invalid()
	}
	address, err := netip.ParseAddr(u.Hostname())
	if err != nil || address.Zone() != "" || !address.Unmap().IsLoopback() {
		return invalid()
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return invalid()
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return invalid()
	}
	return u.Scheme + "://" + u.Host, nil
}

func (c Config) describe() string {
	if c.Enabled {
		return "enabled"
	}
	return "disabled"
}

func (c Config) normalized() Config {
	c.AccountAPIBaseURL = strings.TrimSpace(c.AccountAPIBaseURL)
	c.StateFile = strings.TrimSpace(c.StateFile)
	if c.StateFile == "" {
		c.StateFile = DefaultStateFile
	}
	return c
}
