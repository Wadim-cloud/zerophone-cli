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
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// =========== Domain Types ===========

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

// =========== Configuration ===========

type Config struct {
	NetworkID  string `json:"network_id"`
	NodeID     string `json:"node_id"`
	Name       string `json:"name"`
	ServerAddr string `json:"server_addr"`
	configPath string
}

func NewConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		ServerAddr: "http://localhost:8080",
		configPath: filepath.Join(home, ".zerophone-cli.json"),
	}
}

func (c *Config) Path() string { return c.configPath }

func (c *Config) Load() error {
	data, err := os.ReadFile(c.configPath)
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
	return os.WriteFile(c.configPath, data, 0600)
}

// =========== HTTP Client ===========

type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
	nodeID     string
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *HTTPClient) GetNodes(networkID string) ([]Node, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/nodes?network_id=" + networkID)
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

func (c *HTTPClient) DeleteNode(nodeID string) error {
	req, _ := http.NewRequest("DELETE", c.baseURL+"/nodes/"+nodeID, nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *HTTPClient) Register(req RegisterRequest) (*Node, error) {
	body, _ := json.Marshal(req)
	resp, err := c.httpClient.Post(c.baseURL+"/register", "application/json", bytes.NewReader(body))
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
	return &node, nil
}

func (c *HTTPClient) InitiateCall(fromID, toID string) error {
	callID := fmt.Sprintf("call-%d", time.Now().UnixNano())
	req := SignalRequest{
		Type:   "CALL_REQUEST",
		FromID: fromID,
		ToID:   toID,
		CallID: callID,
	}
	return c.sendSignal(req)
}

func (c *HTTPClient) AnswerCall(fromID, toID, callID string) error {
	req := SignalRequest{
		Type:   "CALL_ACCEPT",
		FromID: fromID,
		ToID:   toID,
		CallID: callID,
	}
	return c.sendSignal(req)
}

func (c *HTTPClient) RejectCall(fromID, toID, callID string) error {
	req := SignalRequest{
		Type:   "CALL_REJECT",
		FromID: fromID,
		ToID:   toID,
		CallID: callID,
	}
	return c.sendSignal(req)
}

func (c *HTTPClient) EndCall(fromID, toID, callID string) error {
	req := SignalRequest{
		Type:   "CALL_END",
		FromID: fromID,
		ToID:   toID,
		CallID: callID,
	}
	return c.sendSignal(req)
}

func (c *HTTPClient) sendSignal(req SignalRequest) error {
	body, _ := json.Marshal(req)
	resp, err := c.httpClient.Post(c.baseURL+"/signal", "application/json", bytes.NewReader(body))
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

// =========== WebSocket Client ===========

type WSClient struct {
	conn      *websocket.Conn
	msgChan   chan Message
	closeChan chan struct{}
}

func NewWSClient(baseURL string) *WSClient {
	return &WSClient{
		msgChan:   make(chan Message, 100),
		closeChan: make(chan struct{}),
	}
}

func (c *WSClient) Connect(baseURL, nodeID string) error {
	url := baseURL + "/ws/" + nodeID
	url = "ws" + url[4:]

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return err
	}
	c.conn = conn
	go c.readPump()
	return nil
}

func (c *WSClient) readPump() {
	for {
		var msg Message
		if err := c.conn.ReadJSON(&msg); err != nil {
			if io.EOF == err {
				close(c.closeChan)
			}
			return
		}
		select {
		case c.msgChan <- msg:
		case <-c.closeChan:
			return
		}
	}
}

func (c *WSClient) Messages() <-chan Message { return c.msgChan }
func (c *WSClient) Closed() <-chan struct{}  { return c.closeChan }
func (c *WSClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// =========== Application State ===========

type AppState int

const (
	StateBrowse AppState = iota
	StateCalling
	StateInCall
	StateIncoming
	StateError
)

type CallInfo struct {
	CallID string
	From   string
	To     string
}

type App struct {
	cfg   *Config
	http  *HTTPClient
	ws    *WSClient
	state AppState

	nodes       []Node
	selectedIdx int

	activeCall *CallInfo
	incoming   *CallInfo
	callTimer  *time.Ticker
	callSecs   int

	statusMsg  string
	statusTime time.Time
	errMsg     string
	errTime    time.Time

	shouldQuit chan struct{}
	stdinChan  chan byte
	renderTick <-chan time.Time
}

func NewApp(cfg *Config) *App {
	client := NewHTTPClient(cfg.ServerAddr)
	ws := NewWSClient(cfg.ServerAddr)

	return &App{
		cfg:        cfg,
		http:       client,
		ws:         ws,
		state:      StateBrowse,
		nodes:      []Node{},
		shouldQuit: make(chan struct{}),
		stdinChan:  make(chan byte, 100),
		renderTick: time.NewTicker(33 * time.Millisecond).C,
	}
}

// =========== Registration ===========

func (a *App) isRegistered() bool {
	return a.cfg.NodeID != "" && a.cfg.NetworkID != "" && a.cfg.Name != ""
}

func (a *App) Register(nodeID, name, networkID string) {
	a.cfg.NodeID = nodeID
	a.cfg.Name = name
	a.cfg.NetworkID = networkID
	_ = a.cfg.Save()

	req := RegisterRequest{
		ID:           nodeID,
		Name:         name,
		NetworkID:    networkID,
		Capabilities: []string{"audio"},
	}

	registered, err := a.http.Register(req)
	if err != nil {
		a.setError("Registration failed: " + err.Error())
	} else {
		a.http.nodeID = registered.ID
		a.setStatus("Registered as " + registered.Name)
		go a.fetchNodes()
		go a.connectWS()
	}
}

func (a *App) fetchNodes() {
	if a.cfg.NetworkID == "" {
		return
	}
	nodes, err := a.http.GetNodes(a.cfg.NetworkID)
	if err != nil {
		a.setError("Fetch error: " + err.Error())
	} else {
		a.nodes = nodes
		online := 0
		for _, n := range nodes {
			if n.Status == "online" && n.ID != a.cfg.NodeID {
				online++
			}
		}
		if online == 0 {
			a.setStatus("No other nodes online")
		} else {
			a.setStatus(fmt.Sprintf("%d node(s) online", online))
		}
	}
}

func (a *App) DeleteSelectedNode() {
	node := a.getSelected()
	if node == nil {
		return
	}
	if node.Status == "online" {
		a.setError("Cannot delete online nodes")
		return
	}
	err := a.http.DeleteNode(node.ID)
	if err != nil {
		a.setError("Delete failed: " + err.Error())
	} else {
		a.setStatus("Node deleted")
		go a.fetchNodes()
	}
}

func (a *App) getSelected() *Node {
	filtered := a.filterNodes()
	if a.selectedIdx >= 0 && a.selectedIdx < len(filtered) {
		return &filtered[a.selectedIdx]
	}
	return nil
}

func (a *App) filterNodes() []Node {
	out := []Node{}
	for _, n := range a.nodes {
		if n.ID != a.cfg.NodeID {
			out = append(out, n)
		}
	}
	return out
}

// =========== Calling ===========

func (a *App) callSelected() {
	if !a.isRegistered() {
		a.setError("Register first (edit ~/.zerophone-cli.json)")
		return
	}
	node := a.getSelected()
	if node == nil {
		return
	}
	if node.Status != "online" {
		a.setError("Node is offline")
		return
	}
	a.state = StateCalling
	a.activeCall = &CallInfo{From: a.cfg.NodeID, To: node.ID}
	err := a.http.InitiateCall(a.cfg.NodeID, node.ID)
	if err != nil {
		a.setError("Call error: " + err.Error())
		a.state = StateBrowse
		a.activeCall = nil
	} else {
		a.setStatus("Calling " + node.Name + "...")
	}
}

func (a *App) answerCall(from, callID string) {
	a.state = StateInCall
	a.incoming = nil
	a.activeCall = &CallInfo{CallID: callID, From: from, To: a.cfg.NodeID}
	err := a.http.AnswerCall(a.cfg.NodeID, from, callID)
	if err != nil {
		a.setError("Answer failed: " + err.Error())
		a.state = StateBrowse
		a.activeCall = nil
		return
	}
	a.startCallTimer()
}

func (a *App) rejectCall(from, callID string) {
	_ = a.http.RejectCall(a.cfg.NodeID, from, callID)
	a.incoming = nil
	a.state = StateBrowse
}

func (a *App) endCall() {
	if a.activeCall == nil {
		return
	}
	_ = a.http.EndCall(a.cfg.NodeID, a.activeCall.To, a.activeCall.CallID)
	a.activeCall = nil
	if a.callTimer != nil {
		a.callTimer.Stop()
	}
	a.state = StateBrowse
	a.callSecs = 0
}

func (a *App) startCallTimer() {
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

func (a *App) setIncoming(from, callID string) {
	a.incoming = &CallInfo{From: from, CallID: callID}
	a.state = StateIncoming
}

// =========== WebSocket Handling ===========

func (a *App) connectWS() {
	if !a.isRegistered() {
		return
	}
	_ = a.ws.Connect(a.cfg.ServerAddr, a.cfg.NodeID)
	go func() {
		for msg := range a.ws.Messages() {
			a.handleWS(msg)
		}
	}()
}

func (a *App) handleWS(msg Message) {
	switch msg.Type {
	case "CALL_REQUEST":
		a.setIncoming(msg.FromID, msg.CallID)
	case "CALL_ACCEPT":
		if a.state == StateCalling && a.activeCall != nil {
			a.state = StateInCall
			a.startCallTimer()
		}
	case "CALL_REJECT":
		if a.state == StateCalling {
			a.setError("Call rejected")
			a.state = StateBrowse
			a.activeCall = nil
		}
	case "CALL_END":
		if a.state == StateInCall || a.state == StateCalling {
			a.state = StateBrowse
			a.activeCall = nil
			if a.callTimer != nil {
				a.callTimer.Stop()
			}
			a.setStatus("Call ended")
		}
	}
}

// =========== Status Management ===========

func (a *App) setStatus(msg string) {
	a.statusMsg = msg
	a.statusTime = time.Now().Add(3 * time.Second)
}

func (a *App) setError(msg string) {
	a.errMsg = msg
	a.errTime = time.Now().Add(5 * time.Second)
}

func (a *App) hasStatus() bool {
	return time.Now().Before(a.statusTime) && a.statusMsg != ""
}

func (a *App) hasError() bool {
	return time.Now().Before(a.errTime) && a.errMsg != ""
}

// =========== Rendering ===========

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
	colorBold   = "\033[1m"
)

var (
	styleTitle   = colorCyan + colorBold
	styleHeader  = colorPurple
	styleOnline  = colorGreen
	styleOffline = colorGray
	styleSelect  = colorYellow + colorBold
	styleError   = colorRed
	styleSuccess = colorGreen
	styleMuted   = colorGray
)

func clear() {
	fmt.Print("\033[2J\033[H")
}

func (a *App) Render() {
	clear()
	a.renderHeader()
	fmt.Println()
	if a.incoming != nil {
		a.renderIncoming()
		fmt.Println()
	}
	if a.activeCall != nil && a.state == StateInCall {
		a.renderActiveCall()
		fmt.Println()
	}
	if !a.isRegistered() {
		a.renderRegister()
	} else {
		a.renderNodeList()
	}
	fmt.Println()
	a.renderFooter()
}

func (a *App) renderHeader() {
	title := styleTitle + " ZeroPhone CLI " + colorReset
	status := styleMuted + "[Disconnected]" + colorReset
	if a.isRegistered() {
		status = styleSuccess + "[Online]" + colorReset
	}
	fmt.Println(" " + title + " " + status)
}

func (a *App) renderRegister() {
	fmt.Println(colorYellow + " ╔═══════════════════════════════════════╗ " + colorReset)
	fmt.Println(colorYellow + " ║          Not Registered               ║ " + colorReset)
	fmt.Println(colorYellow + " ╚═══════════════════════════════════════╝ " + colorReset)
	fmt.Println()
	fmt.Println("  Identity file: " + styleMuted + a.cfg.configPath + colorReset)
	fmt.Println()
	fmt.Printf("  %sNetwork ID:%s  %s\n", styleMuted, colorReset, a.cfg.NetworkID)
	fmt.Printf("  %sNode ID:%s     %s\n", styleMuted, colorReset, a.cfg.NodeID)
	fmt.Printf("  %sName:%s        %s\n", styleMuted, colorReset, a.cfg.Name)
	fmt.Println()
	fmt.Println("  Edit the file or create it with:")
	fmt.Println()
	fmt.Println("  Example:")
	example := `  {
    "network_id": "a84ac5c123456789",
    "node_id": "your-zerotier-node-id",
    "name": "Your Name"
  }`
	fmt.Println(styleMuted + example + colorReset)
}

func (a *App) renderNodeList() {
	filtered := a.filterNodes()
	online := 0
	for _, n := range filtered {
		if n.Status == "online" {
			online++
		}
	}

	header := fmt.Sprintf(" Nodes on %s ", a.cfg.NetworkID)
	fmt.Println(" " + styleHeader + header + colorReset)
	fmt.Println()

	if len(filtered) == 0 {
		fmt.Println("  " + styleMuted + "No other nodes discovered" + colorReset)
		return
	}

	fmt.Printf("  %s  %-20s  %-20s  %s\n", styleSelect+"▶"+colorReset, "Name", "Node ID", "Status")
	fmt.Println("  " + strings.Repeat("─", 73))

	for i, node := range filtered {
		sel := "  "
		if i == a.selectedIdx {
			sel = styleSelect + "▶" + colorReset
		}

		name := truncate(node.Name, 20)
		id := truncate(node.ID, 20)

		var status string
		if node.Status == "online" {
			status = styleOnline + "● online" + colorReset
		} else {
			status = styleOffline + "○ offline" + colorReset
		}

		fmt.Printf("%s  %-20s  %-20s  %s\n", sel, name, id, status)
	}

	fmt.Println()
	summary := fmt.Sprintf("%d total  %s%d online%s  %s%d offline%s",
		len(filtered),
		styleSuccess, online, colorReset,
		styleMuted, len(filtered)-online, colorReset,
	)
	fmt.Println("  " + summary)
}

func (a *App) renderActiveCall() {
	fmt.Print(" " + styleSuccess)
	fmt.Printf(" ╔═ Active Call with %s  %s ╗ ", a.activeCall.To, a.formatDuration(a.callSecs))
	fmt.Println(colorReset)
	fmt.Println()
	fmt.Println("  Press [Enter] to end the call")
}

func (a *App) renderIncoming() {
	fmt.Print(" " + colorYellow)
	fmt.Printf(" ╔═ Incoming Call from %s ╗ ", a.incoming.From)
	fmt.Println(colorReset)
	fmt.Println()
	fmt.Printf("  [a] %sAnswer%s    [%sR%s] %sReject%s\n",
		styleSuccess, colorReset,
		styleSelect, colorReset,
		styleError, colorReset,
	)
	fmt.Println()
}

func (a *App) renderFooter() {
	now := time.Now().Format("15:04:05")

	if a.hasError() {
		fmt.Println(" " + styleError + a.errMsg + colorReset + " ")
	} else if a.hasStatus() {
		fmt.Println(" " + styleSuccess + a.statusMsg + colorReset + " ")
	}

	help := styleMuted + " [↑↓]Select  [Enter]Call/End  [a]Ans  [R]Rej  [d]Del  [r]Ref  [q]Quit " + colorReset
	fmt.Println(" " + help + "  " + styleMuted + now + colorReset)
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
	return s[:n-2] + ".."
}

// =========== Input Handling ===========

func (a *App) Run() {
	oldState, _ := term.MakeRaw(int(os.Stdin.Fd()))
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)
	go func() {
		<-sigChan
		close(a.shouldQuit)
	}()

	// Stdin reader
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			b, err := reader.ReadByte()
			if err != nil {
				close(a.stdinChan)
				return
			}
			select {
			case a.stdinChan <- b:
			case <-a.shouldQuit:
				return
			}
		}
	}()

	// Auto-connect
	if a.isRegistered() {
		a.setStatus("Registered as " + a.cfg.Name)
		go a.fetchNodes()
		go a.connectWS()
	}

	// Main loop
	for {
		select {
		case <-a.renderTick:
			a.Render()
		case msg := <-a.ws.Messages():
			a.handleWS(msg)
		case <-a.ws.Closed():
			// WS closed — could reconnect
		case b := <-a.stdinChan:
			a.handleInput(b)
		case <-a.shouldQuit:
			clear()
			os.Exit(0)
		}
	}
}

func (a *App) handleInput(b byte) {
	switch b {
	case 'q':
		close(a.shouldQuit)
	case 3: // Ctrl+C
		close(a.shouldQuit)
	case 'r':
		a.fetchNodes()
	case 13: // Enter
		switch a.state {
		case StateBrowse:
			a.callSelected()
		case StateInCall:
			a.endCall()
		}
	case 'a':
		if a.state == StateIncoming && a.incoming != nil {
			a.answerCall(a.incoming.From, a.incoming.CallID)
		}
	case 'R':
		if a.state == StateIncoming && a.incoming != nil {
			a.rejectCall(a.incoming.From, a.incoming.CallID)
		}
	case 'd':
		if a.state == StateBrowse {
			a.DeleteSelectedNode()
		}
	case 0x1B: // ESC
		go func() {
			b2 := a.readByte()
			if b2 != '[' {
				return
			}
			b3 := a.readByte()
			filtered := a.filterNodes()
			if len(filtered) == 0 {
				return
			}
			switch b3 {
			case 'A':
				a.selectedIdx = (a.selectedIdx - 1 + len(filtered)) % len(filtered)
			case 'B':
				a.selectedIdx = (a.selectedIdx + 1) % len(filtered)
			}
		}()
	}
}

func (a *App) readByte() byte {
	var b [1]byte
	os.Stdin.Read(b[:])
	return b[0]
}

// =========== Entry ===========

func main() {
	cfg := NewConfig()
	_ = cfg.Load()

	app := NewApp(cfg)
	app.Run()
}
