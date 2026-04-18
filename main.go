package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// =========== Types ===========

type Node struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	NetworkID    string   `json:"network_id"`
	LastSeen     int64    `json:"last_seen"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
}

type RegisterRequest struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	NetworkID    string   `json:"network_id"`
	Capabilities []string `json:"capabilities"`
}

type SignalRequest struct {
	Type    string                 `json:"type"`
	FromID  string                 `json:"from_id"`
	ToID    string                 `json:"to_id"`
	CallID  string                 `json:"call_id,omitempty"`
	SDP     string                 `json:"sdp,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

type Message struct {
	Type    string      `json:"type"`
	FromID  string      `json:"from_id"`
	ToID    string      `json:"to_id"`
	CallID  string      `json:"call_id,omitempty"`
	SDP     string      `json:"sdp,omitempty"`
	Payload interface{} `json:"payload,omitempty"`
}

// =========== Config ===========

type Config struct {
	NetworkID  string `json:"network_id"`
	NodeID     string `json:"node_id"`
	Name       string `json:"name"`
	ServerAddr string `json:"server_addr"`
	path       string
}

func NewConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		ServerAddr: "http://localhost:8080",
		path:       filepath.Join(home, ".zerophone-cli.json"),
	}
}

func (c *Config) Load() error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, c)
}

func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0600)
}

// =========== API Client ===========

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	NodeID     string
	wsConn     *websocket.Conn
	msgChan    chan Message
	closeChan  chan struct{}
}

func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		msgChan:    make(chan Message, 100),
		closeChan:  make(chan struct{}),
	}
}

func (c *Client) GetNodes(networkID string) ([]Node, error) {
	resp, err := c.HTTPClient.Get(c.BaseURL + "/nodes?network_id=" + networkID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var nodes []Node
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	for i := range nodes {
		if now-nodes[i].LastSeen < 60 {
			nodes[i].Status = "online"
		} else {
			nodes[i].Status = "offline"
		}
	}

	return nodes, nil
}

func (c *Client) Register(req RegisterRequest) (*Node, error) {
	body, _ := json.Marshal(req)
	resp, err := c.HTTPClient.Post(c.BaseURL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bs, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bs))
	}

	var node Node
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return nil, err
	}
	c.NodeID = node.ID
	return &node, nil
}

func (c *Client) Heartbeat(nodeID string) error {
	req, _ := http.NewRequest("POST", c.BaseURL+"/heartbeat?node_id="+nodeID, nil)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) InitiateCall(fromID, toID string) error {
	callID := fmt.Sprintf("call-%d", time.Now().UnixNano())
	req := SignalRequest{
		Type:   "CALL_REQUEST",
		FromID: fromID,
		ToID:   toID,
		CallID: callID,
	}
	return c.sendSignal(req)
}

func (c *Client) AnswerCall(fromID, toID, callID string) error {
	req := SignalRequest{
		Type:   "CALL_ACCEPT",
		FromID: fromID,
		ToID:   toID,
		CallID: callID,
	}
	return c.sendSignal(req)
}

func (c *Client) RejectCall(fromID, toID, callID string) error {
	req := SignalRequest{
		Type:   "CALL_REJECT",
		FromID: fromID,
		ToID:   toID,
		CallID: callID,
	}
	return c.sendSignal(req)
}

func (c *Client) EndCall(fromID, toID, callID string) error {
	req := SignalRequest{
		Type:   "CALL_END",
		FromID: fromID,
		ToID:   toID,
		CallID: callID,
	}
	return c.sendSignal(req)
}

func (c *Client) sendSignal(req SignalRequest) error {
	body, _ := json.Marshal(req)
	resp, err := c.HTTPClient.Post(c.BaseURL+"/signal", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bs, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("signal failed: %s", string(bs))
	}
	return nil
}

func (c *Client) ConnectWebSocket(nodeID string) (<-chan Message, error) {
	url := c.BaseURL + "/ws/" + nodeID
	url = "ws" + url[4:]

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	c.wsConn = conn

	go c.readPump()
	return c.msgChan, nil
}

func (c *Client) readPump() {
	for {
		var msg Message
		if err := c.wsConn.ReadJSON(&msg); err != nil {
			if io.EOF == err {
				close(c.closeChan)
				return
			}
			continue
		}
		select {
		case c.msgChan <- msg:
		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) Close() error {
	if c.wsConn != nil {
		return c.wsConn.Close()
	}
	return nil
}

// =========== TUI ===========

type App struct {
	client      *Client
	config      *Config
	state       int // 0=browse, 1=calling, 2=incall, 3=incoming
	nodes       []Node
	selectedIdx int
	activeCall  *CallInfo
	incoming    *CallInfo
	statusMsg   string
	statusTime  time.Time
	errMsg      string
	callTimer   *time.Ticker
	callSecs    int
	oldState    *term.State
}

type CallInfo struct {
	CallID string
	From   string
	To     string
}

const (
	StateBrowse = iota
	StateCalling
	StateInCall
	StateIncoming
)

func NewApp(client *Client, cfg *Config) *App {
	return &App{
		client:     client,
		config:     cfg,
		state:      StateBrowse,
		nodes:      []Node{},
		statusTime: time.Now(),
	}
}

func (a *App) isRegistered() bool {
	return a.config.NodeID != "" && a.config.NetworkID != ""
}

func (a *App) Register(nodeID, name, networkID string) {
	a.config.NodeID = nodeID
	a.config.Name = name
	a.config.NetworkID = networkID
	_ = a.config.Save()

	node := RegisterRequest{
		ID:           nodeID,
		Name:         name,
		NetworkID:    networkID,
		Capabilities: []string{"audio"},
	}
	registered, err := a.client.Register(node)
	if err != nil {
		a.errMsg = "Register failed: " + err.Error()
	} else {
		a.statusMsg = "Registered as " + registered.Name
		a.config.Save()
		go a.fetchNodes()
	}
	a.statusTime = time.Now().Add(3 * time.Second)
}

func (a *App) fetchNodes() {
	if a.config.NetworkID == "" {
		return
	}
	nodes, err := a.client.GetNodes(a.config.NetworkID)
	if err != nil {
		a.errMsg = "Fetch error: " + err.Error()
	} else {
		a.nodes = nodes
		online := 0
		for _, n := range nodes {
			if n.Status == "online" && n.ID != a.config.NodeID {
				online++
			}
		}
		if online == 0 {
			a.statusMsg = "No nodes online"
		} else {
			a.statusMsg = fmt.Sprintf("%d node(s) online", online)
		}
	}
	a.statusTime = time.Now().Add(2 * time.Second)
}

func (a *App) filterNodes() []Node {
	out := []Node{}
	for _, n := range a.nodes {
		if n.ID != a.config.NodeID {
			out = append(out, n)
		}
	}
	return out
}

func (a *App) getSelected() *Node {
	f := a.filterNodes()
	if a.selectedIdx >= 0 && a.selectedIdx < len(f) {
		return &f[a.selectedIdx]
	}
	return nil
}

func (a *App) callSelected() {
	if !a.isRegistered() {
		a.errMsg = "Register first"
		a.statusTime = time.Now().Add(2 * time.Second)
		return
	}
	node := a.getSelected()
	if node == nil || node.Status != "online" {
		return
	}
	a.state = StateCalling
	a.activeCall = &CallInfo{From: a.config.NodeID, To: node.ID}
	err := a.client.InitiateCall(a.config.NodeID, node.ID)
	if err != nil {
		a.errMsg = "Call error: " + err.Error()
		a.state = StateBrowse
		a.activeCall = nil
		a.statusTime = time.Now().Add(3 * time.Second)
	} else {
		a.statusMsg = "Calling " + node.ID + "..."
		a.statusTime = time.Now().Add(5 * time.Second)
	}
}

func (a *App) answerCall(from, callID string) {
	a.state = StateInCall
	a.incoming = nil
	a.activeCall = &CallInfo{CallID: callID, From: from, To: a.config.NodeID}
	err := a.client.AnswerCall(a.config.NodeID, from, callID)
	if err != nil {
		a.errMsg = "Answer failed: " + err.Error()
		a.state = StateBrowse
		a.activeCall = nil
		a.statusTime = time.Now().Add(3 * time.Second)
		return
	}
	a.callSecs = 0
	a.callTimer = time.NewTicker(1 * time.Second)
	go func() {
		for range a.callTimer.C {
			if a.state != StateInCall {
				return
			}
			a.callSecs++
		}
	}()
}

func (a *App) rejectCall(from, callID string) {
	_ = a.client.RejectCall(a.config.NodeID, from, callID)
	a.incoming = nil
	a.state = StateBrowse
}

func (a *App) endCall() {
	if a.activeCall == nil {
		return
	}
	_ = a.client.EndCall(a.config.NodeID, a.activeCall.To, a.activeCall.CallID)
	a.activeCall = nil
	if a.callTimer != nil {
		a.callTimer.Stop()
	}
	a.state = StateBrowse
	a.callSecs = 0
}

func (a *App) setIncoming(from, callID string) {
	a.incoming = &CallInfo{From: from, CallID: callID}
	a.state = StateIncoming
}

// =========== Rendering ===========

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorWhite  = "\033[37m"
	colorGray   = "\033[90m"
)

func clear() {
	fmt.Print("\033[H\033[2J")
}

func (a *App) Render() {
	clear()

	// Header
	status := colorGray + "[Disconnected]" + colorReset
	if a.isRegistered() {
		status = colorGreen + "[Connected]" + colorReset
	}
	fmt.Printf("%sZeroPhone CLI %s %s\n\n", colorCyan, status, colorReset)

	// Incoming call overlay
	if a.incoming != nil {
		a.renderIncoming()
		fmt.Println()
	}

	// Active call overlay
	if a.activeCall != nil && a.state == StateInCall {
		a.renderActiveCall()
		fmt.Println()
	}

	// Main content
	if !a.isRegistered() {
		a.renderRegister()
	} else {
		a.renderNodes()
	}

	// Status
	fmt.Println()
	if time.Now().Before(a.statusTime) {
		if a.errMsg != "" {
			fmt.Printf("%s%s%s\n", colorRed, a.errMsg, colorReset)
		} else if a.statusMsg != "" {
			fmt.Printf("%s%s%s\n", colorGreen, a.statusMsg, colorReset)
		}
	}

	// Help
	fmt.Printf("%s[r] Refresh  [↑↓] Select  [Enter] Call/End  [a] Answer  [R] Reject  [q] Quit%s\n", colorGray, colorReset)
}

func (a *App) renderRegister() {
	fmt.Println(colorYellow + "  Not Registered" + colorReset)
	fmt.Println()
	fmt.Println("  Set identity in ~/.zerophone-cli.json:")
	fmt.Println()
	fmt.Printf("  %sNetwork ID:%s %s\n", colorGray, colorReset, defaultIfEmpty(a.config.NetworkID))
	fmt.Printf("  %sNode ID:%s    %s\n", colorGray, colorReset, defaultIfEmpty(a.config.NodeID))
	fmt.Printf("  %sName:%s       %s\n", colorGray, colorReset, defaultIfEmpty(a.config.Name))
	fmt.Println()
	fmt.Println("  Example:")
	fmt.Println(`  {`)
	fmt.Println(`    "network_id": "a84ac5c123456789",`)
	fmt.Println(`    "node_id": "your-zt-node-id",`)
	fmt.Println(`    "name": "Your Name"`)
	fmt.Println(`  }`)
}

func defaultIfEmpty(s string) string {
	if s == "" {
		return "<not set>"
	}
	return s
}

func (a *App) renderNodes() {
	header := fmt.Sprintf("Nodes on %s", a.config.NetworkID)
	fmt.Println(colorCyan + header + colorReset)
	fmt.Println()

	filtered := a.filterNodes()
	onlineCount := 0
	for _, n := range filtered {
		if n.Status == "online" {
			onlineCount++
		}
	}

	if len(filtered) == 0 {
		fmt.Println(colorGray + "  No other nodes discovered" + colorReset)
	} else {
		for i, node := range filtered {
			sel := "  "
			if i == a.selectedIdx {
				sel = colorYellow + "▶ " + colorReset
			}

			status := colorGray + "offline" + colorReset
			if node.Status == "online" {
				status = colorGreen + "online " + colorReset
			}

			id := truncate(node.ID, 12)
			name := truncate(node.Name, 20)
			fmt.Printf("%s%s  %-20s  %s\n", sel, colorPurple+id+colorReset, name, status)
		}
	}
	fmt.Printf("\n  %sTotal: %d, Online: %d%s\n", colorGray, len(filtered), onlineCount, colorReset)
}

func (a *App) renderActiveCall() {
	fmt.Println(colorGreen + "  ╔═ Active Call ╗" + colorReset)
	fmt.Printf("  %sWith:%s %s\n", colorGray, colorReset, a.activeCall.To)
	fmt.Printf("  %sTime:%s %s\n", colorGray, colorReset, a.formatDuration(a.callSecs))
	fmt.Println("  " + colorGray + "[Press Enter to end call]" + colorReset)
}

func (a *App) renderIncoming() {
	fmt.Println(colorYellow + "  ╔═ Incoming Call ╗" + colorReset)
	fmt.Printf("  %sFrom:%s %s\n", colorGray, colorReset, a.incoming.From)
	fmt.Println()
	fmt.Printf("  [a] %sAnswer%s   [r] %sReject%s\n", colorGreen, colorReset, colorRed, colorReset)
}

func (a *App) formatDuration(s int) string {
	m := s / 60
	sec := s % 60
	return fmt.Sprintf("%02d:%02d", m, sec)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// =========== Main Loop ===========

func (a *App) Run() {
	oldState, _ := term.MakeRaw(int(os.Stdin.Fd()))
	a.oldState = oldState
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Handle resize
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	go func() {
		for range sigChan {
			// terminal resized
		}
	}()

	// Non-blocking stdin reader
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return
			}
			a.handleKey(b)
		}
	}()

	// Auto-connect if configured
	if a.config.NodeID != "" && a.config.NetworkID != "" && a.config.Name != "" {
		a.statusMsg = "Registered as " + a.config.Name
		a.statusTime = time.Now().Add(2 * time.Second)
		go a.fetchNodes()
		go func() {
			ch, _ := a.client.ConnectWebSocket(a.config.NodeID)
			for msg := range ch {
				a.handleWS(msg)
			}
		}()
	}

	// Render loop
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for !a.quit() {
		select {
		case <-ticker.C:
			a.Render()
		case msg := <-a.client.msgChan:
			a.handleWS(msg)
		case <-a.client.closeChan:
			// ws closed
		}
	}

	// Cleanup
	if a.callTimer != nil {
		a.callTimer.Stop()
	}
	clear()
	fmt.Println("Goodbye!")
}

func (a *App) handleKey(b byte) {
	switch b {
	case 'q', 3: // ctrl+c
		a.quitApp()
	case 'r':
		a.fetchNodes()
	case 13: // Enter
		if a.state == StateBrowse {
			a.callSelected()
		} else if a.state == StateInCall {
			a.endCall()
			a.Render()
		}
	case 'a':
		if a.state == StateIncoming && a.incoming != nil {
			a.answerCall(a.incoming.From, a.incoming.CallID)
		}
	case 'R':
		if a.state == StateIncoming && a.incoming != nil {
			a.rejectCall(a.incoming.From, a.incoming.CallID)
		}
	case 0x1B: // ESC sequence start
		// Read next bytes (arrow keys)
		b2 := readByte()
		if b2 == '[' {
			b3 := readByte()
			switch b3 {
			case 'A': // up
				a.selectedIdx = (a.selectedIdx - 1 + len(a.filterNodes())) % len(a.filterNodes())
			case 'B': // down
				a.selectedIdx = (a.selectedIdx + 1) % len(a.filterNodes())
			}
		}
	}
}

func readByte() byte {
	var b [1]byte
	os.Stdin.Read(b[:])
	return b[0]
}

func (a *App) handleWS(msg Message) {
	switch msg.Type {
	case "CALL_REQUEST":
		a.setIncoming(msg.FromID, msg.CallID)
	case "CALL_ACCEPT":
		if a.state == StateCalling && a.activeCall != nil {
			a.state = StateInCall
			a.callSecs = 0
			a.callTimer = time.NewTicker(1 * time.Second)
			go func() {
				for range a.callTimer.C {
					if a.state != StateInCall {
						return
					}
					a.callSecs++
				}
			}()
		}
	case "CALL_REJECT":
		if a.state == StateCalling {
			a.errMsg = "Call rejected"
			a.state = StateBrowse
			a.activeCall = nil
			a.statusTime = time.Now().Add(3 * time.Second)
		}
	case "CALL_END":
		if a.state == StateInCall || a.state == StateCalling {
			a.state = StateBrowse
			a.activeCall = nil
			if a.callTimer != nil {
				a.callTimer.Stop()
			}
			a.statusMsg = "Call ended"
			a.statusTime = time.Now().Add(2 * time.Second)
		}
	}
}

func (a *App) quit() bool {
	// Implement quit logic if needed
	return false
}

func (a *App) quitApp() {
	os.Exit(0)
}

// =========== Entry ===========

func main() {
	// Load config
	cfg := NewConfig()
	_ = cfg.Load()

	// Create client
	client := NewClient(cfg.ServerAddr)

	// Start app
	app := NewApp(client, cfg)
	app.Run()
}
