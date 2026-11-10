package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the plugin configuration (plugins.configs.budget-guard in CPA's config.yaml).
type Config struct {
	// StateFile persists calibration and per-key accounting across restarts.
	StateFile string `yaml:"state_file"`
	// PollInterval is how often each account's usage endpoint is polled.
	PollInterval time.Duration `yaml:"poll_interval"`
	// CalibrationStep is the utilization rise (percentage points) that closes a calibration segment.
	CalibrationStep float64 `yaml:"calibration_step"`
	// Keys are the budgeted client API keys. Keys not listed here are unrestricted.
	Keys []KeyConfig `yaml:"keys"`
	// Pricing rules are checked before the built-in defaults (first match wins).
	Pricing []PriceRule `yaml:"pricing"`
	// RequestLogDays is how many days of the per-request log to keep (default 90; -1 disables the log).
	RequestLogDays int `yaml:"request_log_days"`
}

// RequestLogDir is where the request log lives: next to the state file.
func (c Config) RequestLogDir() string {
	return filepath.Join(filepath.Dir(c.StateFile), "budget-guard-requests")
}

// KeyConfig is one budgeted client key (one machine or CLI).
type KeyConfig struct {
	Name    string                    `yaml:"name"`
	Key     string                    `yaml:"key"`
	Budgets map[string]ProviderBudget `yaml:"budgets"` // provider ("claude", "codex", ...) -> budget
}

// ProviderBudget holds per-window limits in percent. Window IDs look like "5h", "7d" or "7d:fable";
// "*" applies to every window without an explicit entry; a window set to -1 (NoLimit) has no
// limit even when "*" gives one.
type ProviderBudget struct {
	// OwnShare caps this key's own estimated usage, as a percent of the whole pool's window.
	OwnShare map[string]float64 `yaml:"own_share"`
	// PoolCeiling stops serving this key once the pool's total usage of a window reaches the percent.
	PoolCeiling map[string]float64 `yaml:"pool_ceiling"`
}

// PriceRule weights tokens by API-equivalent USD per million tokens. Only ratios matter:
// calibration learns how much cost fills each window.
type PriceRule struct {
	Provider   string  `yaml:"provider" json:"provider,omitempty"` // optional; empty matches any provider
	Match      string  `yaml:"match" json:"match,omitempty"`       // lowercase substring of the model; empty matches any model
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cache_read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"` // 5-minute cache writes
	// CacheWrite1h prices 1-hour cache writes; 0 means twice Input.
	CacheWrite1h float64 `yaml:"cache_write_1h" json:"cache_write_1h,omitempty"`
}

const callerScopePrefix = "cli-proxy-api:caller-scope:v1\x00"

// CallerScope mirrors CPA's opaque caller handle (sdk/cliproxy/session.CallerScope).
func CallerScope(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(callerScopePrefix + apiKey))
	return hex.EncodeToString(sum[:])
}

// ParseConfig parses and validates the plugin YAML.
func ParseConfig(raw []byte) (Config, error) {
	cfg := Config{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse budget-guard config: %w", err)
		}
	}
	if cfg.StateFile == "" {
		cfg.StateFile = "plugins/budget-guard-state.json"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Minute
	}
	if cfg.PollInterval < 30*time.Second {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.CalibrationStep <= 0 {
		cfg.CalibrationStep = 3
	}
	if cfg.RequestLogDays == 0 {
		cfg.RequestLogDays = 90
	}
	seenNames := map[string]bool{}
	seenKeys := map[string]bool{}
	for i := range cfg.Keys {
		k := &cfg.Keys[i]
		k.Name = strings.TrimSpace(k.Name)
		k.Key = strings.TrimSpace(k.Key)
		if k.Name == "" || k.Key == "" {
			return cfg, fmt.Errorf("keys[%d]: name and key are required", i)
		}
		if seenNames[k.Name] || seenKeys[k.Key] {
			return cfg, fmt.Errorf("keys[%d]: duplicate name or key %q", i, k.Name)
		}
		seenNames[k.Name], seenKeys[k.Key] = true, true
		norm := map[string]ProviderBudget{}
		for provider, b := range k.Budgets {
			for _, m := range []map[string]float64{b.OwnShare, b.PoolCeiling} {
				for w, pct := range m {
					if (pct < 0 || pct > 100) && pct != NoLimit {
						return cfg, fmt.Errorf("keys[%s].budgets.%s: %s=%v must be between 0 and 100, or -1 for no limit", k.Name, provider, w, pct)
					}
				}
			}
			norm[strings.ToLower(strings.TrimSpace(provider))] = ProviderBudget{
				OwnShare:    lowerKeys(b.OwnShare),
				PoolCeiling: lowerKeys(b.PoolCeiling),
			}
		}
		k.Budgets = norm
	}
	for i := range cfg.Pricing {
		cfg.Pricing[i].Provider = strings.ToLower(strings.TrimSpace(cfg.Pricing[i].Provider))
		cfg.Pricing[i].Match = strings.ToLower(strings.TrimSpace(cfg.Pricing[i].Match))
	}
	return cfg, nil
}

func lowerKeys(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out
}

// limitFor returns the limit configured for window (falling back to "*").
// NoLimit as a window's value turns that window's limit off, overriding the "*" fallback.
const NoLimit = -1

func limitFor(m map[string]float64, window string) (float64, bool) {
	v, ok := m[window]
	if !ok {
		v, ok = m["*"]
	}
	if v == NoLimit {
		return 0, false
	}
	return v, ok
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
