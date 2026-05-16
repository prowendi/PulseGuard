package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a yaml-friendly wrapper around time.Duration.
// yaml.v3 does not natively parse strings like "10s" into time.Duration,
// so we implement UnmarshalYAML to support both Go duration strings and integer (nanoseconds).
type Duration time.Duration

// Std returns the value as a standard time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML accepts either a duration string ("10s", "5m") or an integer nanosecond count.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case "!!str":
		parsed, err := time.ParseDuration(node.Value)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", node.Value, err)
		}
		*d = Duration(parsed)
		return nil
	case "!!int":
		n, err := strconv.ParseInt(node.Value, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid duration int %q: %w", node.Value, err)
		}
		*d = Duration(time.Duration(n))
		return nil
	default:
		// Best-effort: try ParseDuration on the raw value.
		parsed, err := time.ParseDuration(node.Value)
		if err != nil {
			return fmt.Errorf("unsupported duration node tag %s value %q", node.Tag, node.Value)
		}
		*d = Duration(parsed)
		return nil
	}
}

type Config struct {
	Server    Server    `yaml:"server"`
	Database  Database  `yaml:"database"`
	Security  Security  `yaml:"security"`
	Worker    Worker    `yaml:"worker"`
	Telegram  Telegram  `yaml:"telegram"`
	Bootstrap Bootstrap `yaml:"bootstrap"`
	Logging   Logging   `yaml:"logging"`
	Cleanup   Cleanup   `yaml:"cleanup"`
}

type Server struct {
	ListenAddr      string   `yaml:"listen_addr"      env:"LISTEN_ADDR"`
	BaseURL         string   `yaml:"base_url"         env:"BASE_URL"`
	ReadTimeout     Duration `yaml:"read_timeout"     env:"READ_TIMEOUT"`
	WriteTimeout    Duration `yaml:"write_timeout"    env:"WRITE_TIMEOUT"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout" env:"SHUTDOWN_TIMEOUT"`
}

type Database struct {
	Path        string   `yaml:"path"         env:"PATH"`
	BusyTimeout Duration `yaml:"busy_timeout" env:"BUSY_TIMEOUT"`
}

type Security struct {
	MasterKeyB64 string   `yaml:"master_key_b64" env:"MASTER_KEY_B64"`
	SessionTTL   Duration `yaml:"session_ttl"    env:"SESSION_TTL"`
	CookieSecure bool     `yaml:"cookie_secure"  env:"COOKIE_SECURE"`
	BcryptCost   int      `yaml:"bcrypt_cost"    env:"BCRYPT_COST"`
}

type Worker struct {
	Count                int        `yaml:"count"                  env:"COUNT"`
	PollInterval         Duration   `yaml:"poll_interval"          env:"POLL_INTERVAL"`
	MaxAttempts          int        `yaml:"max_attempts"           env:"MAX_ATTEMPTS"`
	InflightReclaimAfter Duration   `yaml:"inflight_reclaim_after" env:"INFLIGHT_RECLAIM_AFTER"`
	BackoffSchedule      []Duration `yaml:"backoff_schedule"`
}

type Telegram struct {
	APIBase     string   `yaml:"api_base"     env:"API_BASE"`
	HTTPTimeout Duration `yaml:"http_timeout" env:"HTTP_TIMEOUT"`
}

type Bootstrap struct {
	InitialAdminEmail    string `yaml:"initial_admin_email"    env:"INITIAL_ADMIN_EMAIL"`
	InitialAdminPassword string `yaml:"initial_admin_password" env:"INITIAL_ADMIN_PASSWORD"`
}

type Logging struct {
	Level  string `yaml:"level"  env:"LEVEL"`
	Format string `yaml:"format" env:"FORMAT"`
}

type Cleanup struct {
	PushLogsKeepDays       int      `yaml:"push_logs_keep_days"       env:"PUSH_LOGS_KEEP_DAYS"`
	DedupKeysSweepInterval Duration `yaml:"dedup_keys_sweep_interval" env:"DEDUP_KEYS_SWEEP_INTERVAL"`
	SessionsSweepInterval  Duration `yaml:"sessions_sweep_interval"   env:"SESSIONS_SWEEP_INTERVAL"`
}

// Load reads a yaml config file, applies env overrides (prefix PULSEGUARD_) and returns the resolved config.
func Load(path string) (*Config, error) {
	cfg := defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := applyEnv(cfg, "PULSEGUARD"); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Server: Server{
			ListenAddr:      ":8080",
			BaseURL:         "http://localhost:8080",
			ReadTimeout:     Duration(10 * time.Second),
			WriteTimeout:    Duration(30 * time.Second),
			ShutdownTimeout: Duration(15 * time.Second),
		},
		Database: Database{Path: "./data/pulseguard.db", BusyTimeout: Duration(5 * time.Second)},
		Security: Security{
			SessionTTL:   Duration(14 * 24 * time.Hour),
			CookieSecure: true,
			BcryptCost:   10,
		},
		Worker: Worker{
			Count:                4,
			PollInterval:         Duration(time.Second),
			MaxAttempts:          6,
			InflightReclaimAfter: Duration(60 * time.Second),
			BackoffSchedule: []Duration{
				Duration(time.Second),
				Duration(5 * time.Second),
				Duration(15 * time.Second),
				Duration(60 * time.Second),
				Duration(5 * time.Minute),
				Duration(15 * time.Minute),
			},
		},
		Telegram: Telegram{APIBase: "https://api.telegram.org", HTTPTimeout: Duration(10 * time.Second)},
		Logging:  Logging{Level: "info", Format: "json"},
		Cleanup: Cleanup{
			PushLogsKeepDays:       30,
			DedupKeysSweepInterval: Duration(time.Hour),
			SessionsSweepInterval:  Duration(time.Hour),
		},
	}
}

// applyEnv walks struct fields tagged `env:"NAME"` and overrides with values of
// $<prefix>_<SECTION>_<NAME> if set. SECTION comes from the yaml tag of the parent field.
func applyEnv(cfg *Config, prefix string) error {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		section := strings.ToUpper(t.Field(i).Tag.Get("yaml"))
		sv := v.Field(i)
		st := sv.Type()
		for j := 0; j < sv.NumField(); j++ {
			tag := st.Field(j).Tag.Get("env")
			if tag == "" {
				continue
			}
			envName := prefix + "_" + section + "_" + tag
			raw, ok := os.LookupEnv(envName)
			if !ok {
				continue
			}
			if err := setField(sv.Field(j), raw); err != nil {
				return fmt.Errorf("env %s: %w", envName, err)
			}
		}
	}
	return nil
}

func setField(f reflect.Value, raw string) error {
	// Handle config.Duration (named int64).
	if f.Type() == reflect.TypeOf(Duration(0)) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		f.SetInt(int64(d))
		return nil
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		f.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		f.SetInt(n)
	default:
		return fmt.Errorf("unsupported kind %s", f.Kind())
	}
	return nil
}
