package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

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

type TunnelManager struct {
	cfg       Config
	httpCli   *http.Client
	writeJSON func(v interface{}) error
	mu        sync.Mutex
	terms     map[string]*websocket.Conn
}

func NewTunnelManager(cfg Config, writeJSON func(v interface{}) error) *TunnelManager {
	return &TunnelManager{
		cfg:     cfg,
		httpCli: &http.Client{Timeout: 25 * time.Second},
		writeJSON: writeJSON,
		terms:   make(map[string]*websocket.Conn),
	}
}

func (tm *TunnelManager) HandleHTTPProxy(msg ControlMessage, sysauthToken string) {
	relPath := msg.Path
	if relPath == "/ubus" || relPath == "/ubus/" {
		relPath = "/cgi-bin/luci/admin/ubus"
	}
	targetURL := strings.TrimRight(tm.cfg.LocalLuCIAddr, "/") + relPath
	var bodyReader io.Reader
	if msg.BodyB64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(msg.BodyB64); err == nil {
			bodyReader = bytes.NewReader(raw)
		}
	}

	req, err := http.NewRequest(msg.Method, targetURL, bodyReader)
	if err != nil {
		tm.sendHTTPError(msg.RequestID, http.StatusBadRequest, err.Error())
		return
	}

	for k, vals := range msg.Headers {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Cookie") {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	req.Host = "127.0.0.1"

	if sysauthToken != "" {
		req.AddCookie(&http.Cookie{Name: "sysauth", Value: sysauthToken})
		req.AddCookie(&http.Cookie{Name: "sysauth_http", Value: sysauthToken})
		req.AddCookie(&http.Cookie{Name: "sysauth_https", Value: sysauthToken})
	}

	resp, err := tm.httpCli.Do(req)
	if err != nil {
		tm.sendHTTPError(msg.RequestID, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	_ = tm.writeJSON(ControlMessage{
		Type:      "http_response",
		RequestID: msg.RequestID,
		Status:    resp.StatusCode,
		Headers:   resp.Header,
		BodyB64:   base64.StdEncoding.EncodeToString(respBody),
	})
}

func (tm *TunnelManager) sendHTTPError(reqID string, code int, text string) {
	_ = tm.writeJSON(ControlMessage{
		Type:      "http_response",
		RequestID: reqID,
		Status:    code,
		Headers:   map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}},
		BodyB64:   base64.StdEncoding.EncodeToString([]byte(text)),
	})
}

func (tm *TunnelManager) OpenTerminalBridge(sessionID string) {
	dialer := websocket.Dialer{
		Subprotocols:     []string{"tty"},
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialer.Dial(tm.cfg.LocalTtydAddr, nil)
	if err != nil {
		return
	}

	tm.mu.Lock()
	tm.terms[sessionID] = conn
	tm.mu.Unlock()

	go func() {
		defer func() {
			tm.CloseTerminalBridge(sessionID)
		}()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				_ = tm.writeJSON(ControlMessage{
					Type:      "term_close",
					SessionID: sessionID,
				})
				return
			}
			_ = tm.writeJSON(ControlMessage{
				Type:      "term_data",
				SessionID: sessionID,
				MsgType:   mt,
				DataB64:   base64.StdEncoding.EncodeToString(data),
			})
		}
	}()
}

func (tm *TunnelManager) WriteTerminalBridge(sessionID string, msgType int, dataB64 string) {
	tm.mu.Lock()
	conn, ok := tm.terms[sessionID]
	tm.mu.Unlock()
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return
	}
	if msgType == 0 {
		msgType = websocket.BinaryMessage
	}
	_ = conn.WriteMessage(msgType, raw)
}

func (tm *TunnelManager) CloseTerminalBridge(sessionID string) {
	tm.mu.Lock()
	conn, ok := tm.terms[sessionID]
	if ok {
		delete(tm.terms, sessionID)
	}
	tm.mu.Unlock()
	if ok && conn != nil {
		_ = conn.Close()
	}
}

func (tm *TunnelManager) CloseAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for id, c := range tm.terms {
		_ = c.Close()
		delete(tm.terms, id)
	}
}

// FetchLocalUbusSession creates an authenticated root session via OpenWrt ubus.
func FetchLocalUbusSession(localLuCIAddr string) string {
	// 1. Create session via /bin/ubus
	out, err := exec.Command("/bin/ubus", "call", "session", "create", `{"timeout":7200}`).Output()
	if err != nil {
		return ""
	}
	var res struct {
		Token string `json:"ubus_rpc_session"`
	}
	if err := json.Unmarshal(out, &res); err != nil || res.Token == "" {
		return ""
	}
	token := res.Token

	// 2. Grant all required scopes (luci, ubus, uci, file, access-group)
	scopes := []string{"luci", "ubus", "uci", "file", "access-group"}
	for _, sc := range scopes {
		grantArg := fmt.Sprintf(`{"ubus_rpc_session":"%s","scope":"%s","objects":[["*","*"]]}`, token, sc)
		_ = exec.Command("/bin/ubus", "call", "session", "grant", grantArg).Run()
	}

	// 3. Set root user and session token for LuCI ucode dispatcher
	randToken := fmt.Sprintf("%016x%016x", time.Now().UnixNano(), time.Now().UnixNano())
	setArg := fmt.Sprintf(`{"ubus_rpc_session":"%s","values":{"username":"root","token":"%s"}}`, token, randToken)
	_ = exec.Command("/bin/ubus", "call", "session", "set", setArg).Run()

	return token
}
