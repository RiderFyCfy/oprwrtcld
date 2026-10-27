package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

const (
	// DefaultServerURL is the pre-configured controller address (supports DDNS).
	DefaultServerURL = "ws://riderok.mooo.com:8080"
	// DefaultUCIConfigPath is the path to the OpenWrt UCI configuration file.
	DefaultUCIConfigPath = "/etc/config/cloud_manager"
)

type Config struct {
	Enabled           bool
	ServerURL         string
	HeartbeatInterval int
	LocalLuCIAddr     string
	LocalTtydAddr     string
}

func LoadConfig(path string) Config {
	cfg := Config{
		Enabled:           true,
		ServerURL:         DefaultServerURL,
		HeartbeatInterval: 30,
		LocalLuCIAddr:     "http://127.0.0.1:80",
		LocalTtydAddr:     "ws://127.0.0.1:7681/ws",
	}

	f, err := os.Open(path)
	if err != nil {
		return cfg
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "option ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		key := fields[1]
		val := strings.Trim(strings.Join(fields[2:], " "), "'\"")
		switch key {
		case "enabled":
			cfg.Enabled = (val == "1" || strings.EqualFold(val, "true"))
		case "server_url":
			if val != "" {
				cfg.ServerURL = val
			}
		case "heartbeat_interval":
			if n, err := strconv.Atoi(val); err == nil && n >= 5 {
				cfg.HeartbeatInterval = n
			}
		case "local_luci_addr":
			if val != "" {
				cfg.LocalLuCIAddr = val
			}
		case "local_ttyd_addr":
			if val != "" {
				cfg.LocalTtydAddr = val
			}
		}
	}
	return cfg
}
