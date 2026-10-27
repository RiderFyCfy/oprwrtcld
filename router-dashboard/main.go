package main

import (
	"bufio"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed static/*
var staticFS embed.FS

type DeviceRecord struct {
	DeviceID    string    `json:"device_id"`
	MAC         string    `json:"mac"`
	Hostname    string    `json:"hostname"`
	Model       string    `json:"model"`
	Arch        string    `json:"arch"`
	UptimeSec   int64     `json:"uptime_sec"`
	LoadAvg     string    `json:"load_avg"`
	MemFreeKB   int64     `json:"mem_free_kb"`
	MemTotalKB  int64     `json:"mem_total_kb"`
	RemoteIP    string    `json:"remote_ip"`
	LastSeen    time.Time `json:"last_seen"`
	Status      string    `json:"status"` // "online", "delayed", "offline"
	SessionAuth string    `json:"-"`

	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (d *DeviceRecord) SendJSON(v interface{}) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("device disconnected")
	}
	_ = d.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return d.conn.WriteJSON(v)
}

type ControlMessage struct {
	Type      string              `json:"type"`
	RequestID string              `json:"request_id,omitempty"`
	SessionID string              `json:"session_id,omitempty"`
	Method    string              `json:"method,omitempty"`
	Path      string              `json:"path,omitempty"`
	Headers   map[string][]string `json:"headers,omitempty"`
	BodyB64   string              `json:"body_b64,omitempty"`
	Status    int                 `json:"status,omitempty"`
	DataB64   string              `json:"data_b64,omitempty"`
	MsgType   int                 `json:"msg_type,omitempty"`
}

type Hub struct {
	mu       sync.RWMutex
	devices  map[string]*DeviceRecord
	httpWait map[string]chan ControlMessage
	termWait map[string]*websocket.Conn
	reqSeq   uint64
}

func NewHub() *Hub {
	return &Hub{
		devices:  make(map[string]*DeviceRecord),
		httpWait: make(map[string]chan ControlMessage),
		termWait: make(map[string]*websocket.Conn),
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func extractRemoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (h *Hub) handleDeviceWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	remoteIP := extractRemoteIP(r)
	var currentDeviceID string

	defer func() {
		if currentDeviceID != "" {
			h.mu.Lock()
			if dev, ok := h.devices[currentDeviceID]; ok && dev.conn == conn {
				dev.conn = nil
			}
			h.mu.Unlock()
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}

		switch base.Type {
		case "heartbeat":
			var hb struct {
				DeviceID    string `json:"device_id"`
				MAC         string `json:"mac"`
				Hostname    string `json:"hostname"`
				Model       string `json:"model"`
				Arch        string `json:"arch"`
				UptimeSec   int64  `json:"uptime_sec"`
				LoadAvg     string `json:"load_avg"`
				MemFreeKB   int64  `json:"mem_free_kb"`
				MemTotalKB  int64  `json:"mem_total_kb"`
				SessionAuth string `json:"session_auth"`
			}
			if err := json.Unmarshal(raw, &hb); err != nil || hb.DeviceID == "" {
				continue
			}
			currentDeviceID = hb.DeviceID
			h.mu.Lock()
			dev, ok := h.devices[hb.DeviceID]
			if !ok {
				dev = &DeviceRecord{DeviceID: hb.DeviceID}
				h.devices[hb.DeviceID] = dev
			}
			dev.MAC = hb.MAC
			dev.Hostname = hb.Hostname
			dev.Model = hb.Model
			dev.Arch = hb.Arch
			dev.UptimeSec = hb.UptimeSec
			dev.LoadAvg = hb.LoadAvg
			dev.MemFreeKB = hb.MemFreeKB
			dev.MemTotalKB = hb.MemTotalKB
			dev.RemoteIP = remoteIP
			dev.LastSeen = time.Now()
			if hb.SessionAuth != "" {
				dev.SessionAuth = hb.SessionAuth
			}
			dev.conn = conn
			h.mu.Unlock()

		case "http_response":
			var msg ControlMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			h.mu.RLock()
			ch, ok := h.httpWait[msg.RequestID]
			h.mu.RUnlock()
			if ok {
				select {
				case ch <- msg:
				default:
				}
			}

		case "term_data":
			var msg ControlMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			h.mu.RLock()
			uiConn, ok := h.termWait[msg.SessionID]
			h.mu.RUnlock()
			if ok {
				if data, err := base64.StdEncoding.DecodeString(msg.DataB64); err == nil {
					mt := msg.MsgType
					if mt == 0 {
						mt = websocket.BinaryMessage
					}
					_ = uiConn.WriteMessage(mt, data)
				}
			}

		case "term_close":
			var msg ControlMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			h.mu.Lock()
			uiConn, ok := h.termWait[msg.SessionID]
			delete(h.termWait, msg.SessionID)
			h.mu.Unlock()
			if ok && uiConn != nil {
				_ = uiConn.Close()
			}
		}
	}
}

func (h *Hub) handleDevicesAPI(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	h.mu.RLock()
	list := make([]DeviceRecord, 0, len(h.devices))
	for _, d := range h.devices {
		item := *d
		age := now.Sub(d.LastSeen)
		if d.conn == nil || age > 95*time.Second {
			item.Status = "offline"
		} else if age > 40*time.Second {
			item.Status = "delayed"
		} else {
			item.Status = "online"
		}
		list = append(list, item)
	}
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (h *Hub) handleLuCIProxy(w http.ResponseWriter, r *http.Request) {
	// Route format: /proxy/{device_id}/...
	trimmed := strings.TrimPrefix(r.URL.Path, "/proxy/")
	parts := strings.SplitN(trimmed, "/", 2)
	deviceID := parts[0]
	subPath := "/"
	if len(parts) > 1 && parts[1] != "" {
		subPath = "/" + parts[1]
	}
	if r.URL.RawQuery != "" {
		subPath += "?" + r.URL.RawQuery
	}

	h.mu.RLock()
	dev, ok := h.devices[deviceID]
	h.mu.RUnlock()
	if !ok || dev.conn == nil {
		http.Error(w, "Router is currently offline", http.StatusBadGateway)
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	reqID := fmt.Sprintf("req-%d", atomic.AddUint64(&h.reqSeq, 1))
	ch := make(chan ControlMessage, 1)

	h.mu.Lock()
	h.httpWait[reqID] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.httpWait, reqID)
		h.mu.Unlock()
	}()

	err := dev.SendJSON(ControlMessage{
		Type:      "http_request",
		RequestID: reqID,
		Method:    r.Method,
		Path:      subPath,
		Headers:   r.Header,
		BodyB64:   base64.StdEncoding.EncodeToString(bodyBytes),
	})
	if err != nil {
		http.Error(w, "Failed to tunnel request to router: "+err.Error(), http.StatusBadGateway)
		return
	}

	select {
	case resp := <-ch:
		prefix := "/proxy/" + deviceID
		if dev.SessionAuth != "" {
			http.SetCookie(w, &http.Cookie{
				Name:  "sysauth_http",
				Value: dev.SessionAuth,
				Path:  prefix,
			})
			http.SetCookie(w, &http.Cookie{
				Name:  "sysauth",
				Value: dev.SessionAuth,
				Path:  prefix,
			})
		}
		for k, vals := range resp.Headers {
			for _, v := range vals {
				if strings.EqualFold(k, "Location") && strings.HasPrefix(v, "/") {
					v = prefix + v
				}
				w.Header().Add(k, v)
			}
		}
		rawBody, _ := base64.StdEncoding.DecodeString(resp.BodyB64)
		ct := w.Header().Get("Content-Type")
		if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/javascript") {
			rewritten := strings.ReplaceAll(string(rawBody), `"/luci-static/`, `"`+prefix+`/luci-static/`)
			rewritten = strings.ReplaceAll(rewritten, `"/cgi-bin/luci`, `"`+prefix+`/cgi-bin/luci`)
			rewritten = strings.ReplaceAll(rewritten, `'/luci-static/`, `'`+prefix+`/luci-static/`)
			rewritten = strings.ReplaceAll(rewritten, `'/cgi-bin/luci`, `'`+prefix+`/cgi-bin/luci`)
			rewritten = strings.ReplaceAll(rewritten, `"/ubus"`, `"`+prefix+`/ubus"`)
			rawBody = []byte(rewritten)
			w.Header().Del("Content-Length")
		}
		if resp.Status == 0 {
			resp.Status = http.StatusOK
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(rawBody)
	case <-time.After(25 * time.Second):
		http.Error(w, "Gateway timeout waiting for router response", http.StatusGatewayTimeout)
	}
}

func (h *Hub) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/ws/terminal/")
	h.mu.RLock()
	dev, ok := h.devices[deviceID]
	h.mu.RUnlock()
	if !ok || dev.conn == nil {
		http.Error(w, "Router offline", http.StatusBadGateway)
		return
	}

	uiConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer uiConn.Close()

	sessionID := fmt.Sprintf("term-%d", atomic.AddUint64(&h.reqSeq, 1))
	h.mu.Lock()
	h.termWait[sessionID] = uiConn
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.termWait, sessionID)
		h.mu.Unlock()
		_ = dev.SendJSON(ControlMessage{
			Type:      "term_close",
			SessionID: sessionID,
		})
	}()

	if err := dev.SendJSON(ControlMessage{
		Type:      "term_open",
		SessionID: sessionID,
	}); err != nil {
		return
	}

	for {
		mt, data, err := uiConn.ReadMessage()
		if err != nil {
			return
		}
		_ = dev.SendJSON(ControlMessage{
			Type:      "term_data",
			SessionID: sessionID,
			MsgType:   mt,
			DataB64:   base64.StdEncoding.EncodeToString(data),
		})
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func pauseAndExit(err error) {
	fmt.Printf("\n[ERROR] %v\n", err)
	fmt.Println("\nPress Enter to exit...")
	_, _ = bufio.NewReader(os.Stdin).ReadBytes('\n')
	os.Exit(1)
}

func main() {
	addr := ":8080"
	uiURL := "http://localhost:8080"

	// Check if port 8080 is already in use
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("============================================================")
		fmt.Println("  OpenWrt Cloud Manager is ALREADY RUNNING!")
		fmt.Printf("  Opening %s in your browser...\n", uiURL)
		fmt.Println("============================================================")
		openBrowser(uiURL)
		fmt.Println("\nPress Enter to close this window...")
		_, _ = bufio.NewReader(os.Stdin).ReadBytes('\n')
		return
	}

	hub := NewHub()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/device", hub.handleDeviceWS)
	mux.HandleFunc("/api/devices", hub.handleDevicesAPI)
	mux.HandleFunc("/proxy/", hub.handleLuCIProxy)
	mux.HandleFunc("/ws/terminal/", hub.handleTerminalWS)
	subFS, err := fs.Sub(staticFS, "static")
	if err != nil {
		pauseAndExit(fmt.Errorf("failed to load embedded static UI: %v", err))
	}
	mux.Handle("/", http.FileServer(http.FS(subFS)))

	fmt.Println("============================================================")
	fmt.Println("       OpenWrt Cloud Manager Controller & Dashboard")
	fmt.Printf("  Web UI:         %s\n", uiURL)
	fmt.Println("  Controller WS:  ws://riderok.mooo.com:8080/ws/device")
	fmt.Println("============================================================")
	fmt.Println("Status: Server is running. Keep this window open!")
	fmt.Println("Opening dashboard in your default browser...")

	go func() {
		time.Sleep(600 * time.Millisecond)
		openBrowser(uiURL)
	}()

	server := &http.Server{Handler: mux}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		pauseAndExit(err)
	}
}
