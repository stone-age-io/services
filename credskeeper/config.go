package credskeeper

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ConfigFromEnv reads the configuration from environment variables. getenv
// is os.Getenv outside tests.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	env := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	cfg := Config{
		URL:       getenv("PLATFORM_URL"),
		Identity:  getenv("PLATFORM_IDENTITY"),
		Password:  getenv("PLATFORM_PASSWORD"),
		CredsFile: getenv("NATS_CREDS"),
	}

	if cfg.URL == "" {
		return cfg, errors.New("PLATFORM_URL is required")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" {
		return cfg, fmt.Errorf("PLATFORM_URL %q is not a URL", cfg.URL)
	}
	insecure, err := strconv.ParseBool(env("PLATFORM_ALLOW_INSECURE", "false"))
	if err != nil {
		return cfg, fmt.Errorf("PLATFORM_ALLOW_INSECURE must be true or false, got %q", getenv("PLATFORM_ALLOW_INSECURE"))
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && insecure:
	default:
		return cfg, fmt.Errorf("PLATFORM_URL must be https://: the first sync sends a password and receives a private key (set PLATFORM_ALLOW_INSECURE=true only for development)")
	}

	if cfg.Identity == "" {
		return cfg, errors.New("PLATFORM_IDENTITY is required: the Thing's login email")
	}
	if cfg.CredsFile == "" {
		return cfg, errors.New("NATS_CREDS is required: the creds file to write, the same path the service reads")
	}
	cfg.SessionFile = env("SESSION_FILE", filepath.Join(filepath.Dir(cfg.CredsFile), "platform-session.json"))
	if filepath.Clean(cfg.SessionFile) == filepath.Clean(cfg.CredsFile) {
		return cfg, errors.New("SESSION_FILE must not be the creds file")
	}

	mode, err := strconv.ParseUint(env("CREDS_FILE_MODE", "0600"), 8, 32)
	if err != nil || mode > 0o777 {
		return cfg, fmt.Errorf("CREDS_FILE_MODE must be an octal mode such as 0600 or 0640, got %q", getenv("CREDS_FILE_MODE"))
	}
	if mode&0o007 != 0 {
		return cfg, fmt.Errorf("CREDS_FILE_MODE %04o gives other users access; the file holds a private key", mode)
	}
	cfg.CredsMode = os.FileMode(mode)

	cfg.SyncInterval, err = time.ParseDuration(env("SYNC_INTERVAL", "5m"))
	if err != nil || cfg.SyncInterval <= 0 {
		return cfg, fmt.Errorf("SYNC_INTERVAL must be a positive duration, got %q", getenv("SYNC_INTERVAL"))
	}
	return cfg, nil
}
