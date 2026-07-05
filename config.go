package main

import (
	"flag"
	"os"
	"time"
)

type Config struct {
	ListenAddr     string
	StatusAddr     string
	DataDir        string
	ScrapeInterval time.Duration
	CheckTimeout   time.Duration
	MaxConcurrent  int
	MaxCandidates  int
}

func ParseConfig() *Config {
	cfg := &Config{}
	flag.StringVar(&cfg.ListenAddr, "listen", "127.0.0.1:1080", "local SOCKS5 listen address")
	flag.StringVar(&cfg.StatusAddr, "status", "127.0.0.1:8080", "HTTP status dashboard address")
	flag.StringVar(&cfg.DataDir, "data-dir", "./data", "directory for persisted sources/rules/groups config")
	flag.DurationVar(&cfg.ScrapeInterval, "scrape-interval", 20*time.Minute, "scrape interval")
	flag.DurationVar(&cfg.CheckTimeout, "check-timeout", 10*time.Second, "proxy check timeout")
	flag.IntVar(&cfg.MaxConcurrent, "max-concurrent", 20, "max concurrent health checks")
	flag.IntVar(&cfg.MaxCandidates, "max-candidates", 3000, "cap on total scraped candidates checked per refresh cycle (some sources return 100k+ entries; a random subset is sampled each cycle when over the cap)")
	flag.Parse()

	// Cloud deployment: always use fixed ports
	// SOCKS5 on 1080, status on 8080
	if os.Getenv("PORT") != "" {
		cfg.ListenAddr = "0.0.0.0:1080"
		cfg.StatusAddr = "0.0.0.0:8080"
	}

	return cfg
}
