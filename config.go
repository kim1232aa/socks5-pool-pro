package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const maxCredentialFileBytes = 4 << 10

type Config struct {
	ListenAddr        string
	StatusAddr        string
	SOCKSUser         string
	SOCKSPass         string
	AdminUser         string
	AdminPass         string
	DataDir           string
	ScrapeInterval    time.Duration
	CheckTimeout      time.Duration
	MaxConcurrent     int
	MaxCandidates     int
	RequireIPChange   bool
	credentialLoadErr error
}

func ParseConfig() *Config {
	cfg := &Config{}
	socksUser, socksUserErr := credentialDefault("SOCKS_USER", "SOCKS_USER_FILE")
	socksPass, socksPassErr := credentialDefault("SOCKS_PASS", "SOCKS_PASS_FILE")
	adminUser, adminUserErr := credentialDefault("ADMIN_USER", "ADMIN_USER_FILE")
	adminPass, adminPassErr := credentialDefault("ADMIN_PASS", "ADMIN_PASS_FILE")
	flag.StringVar(&cfg.ListenAddr, "listen", "127.0.0.1:1080", "local SOCKS5 listen address")
	flag.StringVar(&cfg.StatusAddr, "status", "127.0.0.1:8080", "HTTP status dashboard address")
	flag.StringVar(&cfg.SOCKSUser, "socks-user", socksUser, "username required by the local SOCKS5 listener (or SOCKS_USER/SOCKS_USER_FILE; also set its password)")
	flag.StringVar(&cfg.SOCKSPass, "socks-pass", socksPass, "password required by the local SOCKS5 listener (or SOCKS_PASS/SOCKS_PASS_FILE; also set its username)")
	flag.StringVar(&cfg.AdminUser, "admin-user", adminUser, "username required by the dashboard/API (or ADMIN_USER/ADMIN_USER_FILE; also set its password)")
	flag.StringVar(&cfg.AdminPass, "admin-pass", adminPass, "password required by the dashboard/API (or ADMIN_PASS/ADMIN_PASS_FILE; also set its username)")
	flag.StringVar(&cfg.DataDir, "data-dir", "./data", "directory for persisted sources/rules/groups config")
	flag.DurationVar(&cfg.ScrapeInterval, "scrape-interval", 20*time.Minute, "scrape interval")
	flag.DurationVar(&cfg.CheckTimeout, "check-timeout", 10*time.Second, "proxy check timeout")
	flag.IntVar(&cfg.MaxConcurrent, "max-concurrent", 20, "max concurrent health checks")
	flag.IntVar(&cfg.MaxCandidates, "max-candidates", 3000, "cap on total scraped candidates checked per refresh cycle (some sources return 100k+ entries; a random subset is sampled each cycle when over the cap)")
	flag.BoolVar(&cfg.RequireIPChange, "require-ip-change", true, "drop transparent proxies whose exit IP equals our own direct egress (i.e. that don't actually change your public IP)")
	flag.Parse()

	// HTTP-oriented cloud platforms expose one PORT. Keep SOCKS5 on its own
	// conventional port and put the dashboard/health endpoint on PORT, while
	// respecting any address the operator explicitly supplied on the CLI.
	explicit := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	var credentialErrors []error
	if !explicit["socks-user"] {
		credentialErrors = append(credentialErrors, socksUserErr)
	}
	if !explicit["socks-pass"] {
		credentialErrors = append(credentialErrors, socksPassErr)
	}
	if !explicit["admin-user"] {
		credentialErrors = append(credentialErrors, adminUserErr)
	}
	if !explicit["admin-pass"] {
		credentialErrors = append(credentialErrors, adminPassErr)
	}
	cfg.credentialLoadErr = errors.Join(credentialErrors...)
	if port := os.Getenv("PORT"); port != "" {
		if !explicit["listen"] {
			cfg.ListenAddr = "0.0.0.0:1080"
		}
		if !explicit["status"] {
			cfg.StatusAddr = net.JoinHostPort("0.0.0.0", port)
		}
	}

	return cfg
}

func (c *Config) Validate() error {
	if c.credentialLoadErr != nil {
		return fmt.Errorf("load credentials: %w", c.credentialLoadErr)
	}
	if (c.SOCKSUser == "") != (c.SOCKSPass == "") {
		return fmt.Errorf("socks-user and socks-pass must be set together")
	}
	if len(c.SOCKSUser) > 255 || len(c.SOCKSPass) > 255 {
		return fmt.Errorf("SOCKS5 username and password must each be at most 255 bytes")
	}
	if (c.AdminUser == "") != (c.AdminPass == "") {
		return fmt.Errorf("admin-user and admin-pass must be set together")
	}
	if len(c.AdminUser) > maxCredentialFileBytes || len(c.AdminPass) > maxCredentialFileBytes {
		return fmt.Errorf("admin username and password must each be at most %d bytes", maxCredentialFileBytes)
	}
	if c.ScrapeInterval <= 0 {
		return fmt.Errorf("scrape-interval must be positive")
	}
	if c.CheckTimeout <= 0 {
		return fmt.Errorf("check-timeout must be positive")
	}
	if c.MaxConcurrent <= 0 {
		return fmt.Errorf("max-concurrent must be greater than zero")
	}
	if c.MaxCandidates <= 0 {
		return fmt.Errorf("max-candidates must be greater than zero")
	}
	if c.DataDir == "" {
		return fmt.Errorf("data-dir must not be empty")
	}
	for name, addr := range map[string]string{"listen": c.ListenAddr, "status": c.StatusAddr} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("invalid %s address %q: %w", name, addr, err)
		}
	}
	return nil
}

// credentialDefault supports container/platform secrets without placing them
// in process arguments. Direct values and *_FILE are intentionally mutually
// exclusive so a stale environment variable cannot silently override a
// rotated mounted secret. Explicit command-line flags still override these
// defaults through flag.StringVar.
func credentialDefault(valueEnv, fileEnv string) (string, error) {
	value, valueSet := os.LookupEnv(valueEnv)
	path, fileSet := os.LookupEnv(fileEnv)
	if valueSet && fileSet {
		return "", fmt.Errorf("%s and %s cannot both be set", valueEnv, fileEnv)
	}
	if valueSet {
		return value, nil
	}
	if !fileSet {
		return "", nil
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s points to an empty path", fileEnv)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileEnv, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileEnv, err)
	}
	if len(data) > maxCredentialFileBytes {
		return "", fmt.Errorf("%s exceeds %d bytes", fileEnv, maxCredentialFileBytes)
	}
	secret := strings.TrimSuffix(string(data), "\n")
	secret = strings.TrimSuffix(secret, "\r")
	return secret, nil
}
