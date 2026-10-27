package main

import (
	"context"
	"flag"
	"log"
	"math/rand"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	configPath := flag.String("config", DefaultUCIConfigPath, "Path to UCI configuration file")
	serverOverride := flag.String("server", "", "Override Controller WebSocket URL")
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	for {
		cfg := LoadConfig(*configPath)
		if *serverOverride != "" {
			cfg.ServerURL = *serverOverride
		}
		if !cfg.Enabled {
			time.Sleep(15 * time.Second)
			continue
		}

		runSession(cfg)

		// Add random jitter (3 to 8 seconds) before reconnecting and re-resolving DDNS
		backoff := time.Duration(3000+rand.Intn(5000)) * time.Millisecond
		time.Sleep(backoff)
	}
}

// freshDNSDialer forces a fresh DNS lookup on every connection cycle to support DDNS updates.
func freshDNSDialer() *websocket.Dialer {
	resolver := &net.Resolver{
		PreferGo: true,
	}
	netDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  resolver,
	}
	return &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return netDialer.DialContext(ctx, network, addr)
			}
			ips, err := resolver.LookupHost(ctx, host)
			if err != nil || len(ips) == 0 {
				return netDialer.DialContext(ctx, network, addr)
			}
			return netDialer.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
		},
		HandshakeTimeout: 10 * time.Second,
	}
}

func buildDeviceWSURL(base string) string {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/ws/device"
	}
	return u.String()
}

func runSession(cfg Config) {
	wsURL := buildDeviceWSURL(cfg.ServerURL)
	conn, _, err := freshDNSDialer().Dial(wsURL, nil)
	if err != nil {
		log.Printf("[cloud-manager] connection to %s failed: %v", wsURL, err)
		return
	}
	defer conn.Close()
	log.Printf("[cloud-manager] connected to controller %s", wsURL)

	var writeMu sync.Mutex
	writeJSON := func(v interface{}) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(v)
	}

	sysauthToken := FetchLocalUbusSession(cfg.LocalLuCIAddr)
	_ = writeJSON(CollectTelemetry(sysauthToken))

	tm := NewTunnelManager(cfg, writeJSON)
	defer tm.CloseAll()

	stopCh := make(chan struct{})
	defer close(stopCh)

	// Lightweight heartbeat loop with random jitter
	go func() {
		for {
			jitterMs := rand.Intn(6000) - 3000 // -3s .. +3s jitter
			interval := time.Duration(cfg.HeartbeatInterval)*time.Second + time.Duration(jitterMs)*time.Millisecond
			if interval < 5*time.Second {
				interval = 5 * time.Second
			}
			select {
			case <-stopCh:
				return
			case <-time.After(interval):
				if err := writeJSON(CollectTelemetry(sysauthToken)); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	for {
		var msg ControlMessage
		if err := conn.ReadJSON(&msg); err != nil {
			log.Printf("[cloud-manager] connection closed: %v", err)
			return
		}

		switch msg.Type {
		case "http_request":
			if sysauthToken == "" {
				sysauthToken = FetchLocalUbusSession(cfg.LocalLuCIAddr)
			}
			go tm.HandleHTTPProxy(msg, sysauthToken)
		case "term_open":
			go tm.OpenTerminalBridge(msg.SessionID)
		case "term_data":
			tm.WriteTerminalBridge(msg.SessionID, msg.MsgType, msg.DataB64)
		case "term_close":
			tm.CloseTerminalBridge(msg.SessionID)
		}
	}
}
