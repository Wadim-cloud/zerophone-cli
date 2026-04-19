package main

import (
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

	"github.com/gdamore/tcell/v2"
	"github.com/gorilla/websocket"
)

// =========== Colors/Styles ===========

var (
	styleTitle   = tcell.StyleDefault.Foreground(tcell.NewRGBColor(0, 255, 255)).Bold(true) // Cyan
	styleHeader  = tcell.StyleDefault.Foreground(tcell.NewRGBColor(128, 0, 128))            // Purple
	styleOnline  = tcell.StyleDefault.Foreground(tcell.ColorGreen)
	styleOffline = tcell.StyleDefault.Foreground(tcell.ColorGray)
	styleSelect  = tcell.StyleDefault.Foreground(tcell.NewRGBColor(255, 255, 0)).Bold(true) // Yellow
	styleError   = tcell.StyleDefault.Foreground(tcell.ColorRed)
	styleSuccess = tcell.StyleDefault.Foreground(tcell.ColorGreen)
	styleMuted   = tcell.StyleDefault.Foreground(tcell.ColorGray)
	styleNormal  = tcell.StyleDefault
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

func NewWSClient() *WSClient {
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
	defer close(c.closeChan)
	for {
		var msg Message
		if err := c.conn.ReadJSON(&msg); err != nil {
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
func (c *WSClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// =========== Application ===========

type AppState int

const (
	StateBrowse AppState = iota
	StateCalling
	StateInCall
	StateIncoming
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

	screen tcell.Screen
	quit   chan struct{}
}

func NewApp(cfg *Config) *App {
	screen, err := tcell.NewScreen()
	if err != nil {
		panic(err)
	}
	if err := screen.Init(); err != nil {
		panic(err)
	}
	screen.Clear()

	client := NewHTTPClient(cfg.ServerAddr)
	ws := NewWSClient()

	return &App{
		cfg:        cfg,
		http:       client,
		ws:         ws,
		state:      StateBrowse,
		nodes:      []Node{},
		statusTime: time.Now(),
		errTime:    time.Now(),
		screen:     screen,
		quit:       make(chan struct{}),
	}
}

func (a *App) cleanup() {
	if a.callTimer != nil {
		a.callTimer.Stop()
	}
	if a.ws != nil {
		a.ws.Close()
	}
	a.screen.Fini()
}

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

func (a *App) connectWS() {
	if !a.isRegistered() {
		return
	}
	err := a.ws.Connect(a.cfg.ServerAddr, a.cfg.NodeID)
	if err != nil {
		a.setError("WS connect failed: " + err.Error())
		return
	}
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

// =========== Rendering with tcell ===========

func (a *App) drawText(screen tcell.Screen, x, y int, text string, style tcell.Style) {
	for i, ch := range text {
		screen.SetContent(x+i, y, ch, nil, style)
	}
}

func (a *App) render() {
	screen := a.screen
	screen.Clear()

	_, width := screen.Size()
	height, _ := screen.Size()

	// Styles (define once at package level, recreate per render since tcell.Style is immutable)
	titleStyle := tcell.StyleDefault.Foreground(tcell.NewRGBColor(0, 255, 255)).Bold(true) // Cyan
	headerStyle := tcell.StyleDefault.Foreground(tcell.NewRGBColor(128, 0, 128))           // Purple
	onlineStyle := tcell.StyleDefault.Foreground(tcell.ColorGreen)
	offlineStyle := tcell.StyleDefault.Foreground(tcell.ColorGray)
	selectStyle := tcell.StyleDefault.Foreground(tcell.NewRGBColor(255, 255, 0)).Bold(true) // Yellow
	errorStyle := tcell.StyleDefault.Foreground(tcell.ColorRed)
	successStyle := tcell.StyleDefault.Foreground(tcell.ColorGreen)
	mutedStyle := tcell.StyleDefault.Foreground(tcell.ColorGray)
	normalStyle := tcell.StyleDefault

	// Header
	title := " ZeroPhone CLI "
	status := "[Disconnected]"
	if a.isRegistered() {
		status = "[Online]"
	}
	headerText := title + " " + status
	a.drawText(screen, 2, 0, headerText, titleStyle)
	separator := strings.Repeat("─", width-2)
	a.drawText(screen, 2, 1, separator, headerStyle)

	// Incoming call overlay
	if a.incoming != nil {
		y := height/2 - 2
		text := " Incoming Call from " + a.incoming.From + " "
		x := (width - len(text)) / 2
		if x < 2 {
			x = 2
		}
		a.drawText(screen, x, y, text, errorStyle)
		y += 2
		opt1 := "[a] Answer"
		opt2 := "[R] Reject"
		line := "  " + opt1 + "    " + opt2
		x = (width - len(line)) / 2
		if x < 2 {
			x = 2
		}
		a.drawText(screen, x, y, line, mutedStyle)
	}

	// Active call bar
	if a.activeCall != nil && a.state == StateInCall {
		y := height - 5
		text := fmt.Sprintf(" Active Call with %s  %s ", a.activeCall.To, a.formatDuration(a.callSecs))
		x := (width - len(text)) / 2
		if x < 2 {
			x = 2
		}
		a.drawText(screen, x, y, text, successStyle)
		y++
		a.drawText(screen, 2, y, "  Press [Enter] to end the call", normalStyle)
	}

	// Main content
	y := 3
	if !a.isRegistered() {
		y = a.renderRegister(screen, width, y, titleStyle, headerStyle, mutedStyle, normalStyle, errorStyle)
	} else {
		y = a.renderNodeList(screen, width, y, headerStyle, selectStyle, onlineStyle, offlineStyle, normalStyle)
	}

	// Footer
	separator2 := strings.Repeat("─", width-2)
	a.drawText(screen, 2, height-3, separator2, mutedStyle)

	// Status line
	if a.hasError() {
		a.drawText(screen, 2, height-2, " "+a.errMsg+" ", errorStyle)
	} else if a.hasStatus() {
		a.drawText(screen, 2, height-2, " "+a.statusMsg+" ", successStyle)
	}

	// Help + time
	now := time.Now().Format("15:04:05")
	help := " [↑↓]Select  [Enter]Call/End  [a]Ans  [R]Rej  [d]Del  [r]Ref  [q]Quit "
	timeStr := " " + now + " "
	helpX := 2
	timeX := width - 2 - len(timeStr)
	if timeX < helpX+len(help) {
		timeX = helpX + len(help)
	}
	a.drawText(screen, helpX, height-1, help, mutedStyle)
	a.drawText(screen, timeX, height-1, timeStr, mutedStyle)

	screen.Show()
}

func (a *App) renderRegister(screen tcell.Screen, width, startY int, titleStyle, headerStyle, mutedStyle, normalStyle, errorStyle tcell.Style) int {
	y := startY

	boxText := " Not Registered "
	boxWidth := len(boxText) + 4
	boxX := (width - boxWidth) / 2
	if boxX < 2 {
		boxX = 2
	}

	// Top line
	line := "╔" + strings.Repeat("═", boxWidth-2) + "╗"
	a.drawText(screen, boxX, y, line, headerStyle)
	y++

	// Text line
	inner := "║ " + boxText + " ║"
	a.drawText(screen, boxX, y, inner, errorStyle)
	y++

	// Bottom line
	line = "╚" + strings.Repeat("═", boxWidth-2) + "╝"
	a.drawText(screen, boxX, y, line, headerStyle)
	y += 2

	// Config info
	a.drawText(screen, 2, y, "Identity file: "+a.cfg.configPath, mutedStyle)
	y += 2
	a.drawText(screen, 2, y, "Network ID:  "+a.cfg.NetworkID, normalStyle)
	y++
	a.drawText(screen, 2, y, "Node ID:     "+a.cfg.NodeID, normalStyle)
	y++
	a.drawText(screen, 2, y, "Name:        "+a.cfg.Name, normalStyle)
	y += 2
	a.drawText(screen, 2, y, "Edit the file above or use API to register.", mutedStyle)
	y += 2
	a.drawText(screen, 2, y, "Example:", mutedStyle)
	y++
	example := `  {
    "network_id": "a84ac5c123456789",
    "node_id": "your-zerotier-node-id",
    "name": "Your Name"
  }`
	lines := strings.Split(example, "\n")
	for _, line := range lines {
		a.drawText(screen, 2, y, line, mutedStyle)
		y++
	}

	return y
}

func (a *App) renderNodeList(screen tcell.Screen, width, startY int, headerStyle, selectStyle, onlineStyle, offlineStyle, normalStyle tcell.Style) int {
	y := startY
	filtered := a.filterNodes()
	online := 0
	for _, n := range filtered {
		if n.Status == "online" {
			online++
		}
	}

	// Header
	header := " Nodes on " + a.cfg.NetworkID + " "
	a.drawText(screen, 2, y, header, headerStyle)
	y += 2

	if len(filtered) == 0 {
		a.drawText(screen, 2, y, "  No other nodes discovered", normalStyle)
		return y + 1
	}

	// Table header
	a.drawText(screen, 2, y, "▶  Name                  Node ID                Status", selectStyle)
	y++
	separator := strings.Repeat("─", width-4)
	a.drawText(screen, 2, y, separator, headerStyle)
	y++

	// Nodes
	for i, node := range filtered {
		sel := "  "
		if i == a.selectedIdx {
			sel = "▶"
		}

		name := truncate(node.Name, 20)
		id := truncate(node.ID, 20)

		var status string
		if node.Status == "online" {
			status = "● online"
		} else {
			status = "○ offline"
		}

		line := fmt.Sprintf("%s  %-20s  %-20s  %s", sel, name, id, status)
		style := normalStyle
		if i == a.selectedIdx {
			style = selectStyle
		}
		a.drawText(screen, 2, y, line, style)
		y++
	}

	y++
	summary := fmt.Sprintf("  %d total  %d online  %d offline", len(filtered), online, len(filtered)-online)
	a.drawText(screen, 2, y, summary, normalStyle)

	return y + 2
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

// =========== Main Loop ===========

func (a *App) Run() {
	defer a.cleanup()

	// Signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		close(a.quit)
	}()

	// Auto-connect if configured
	if a.isRegistered() {
		a.setStatus("Registered as " + a.cfg.Name)
		go a.fetchNodes()
		go a.connectWS()
	}

	// Event loop
	for {
		select {
		case <-a.quit:
			return
		case msg := <-a.ws.Messages():
			a.handleWS(msg)
		default:
			// Poll tcell events
			event := a.screen.PollEvent()
			switch ev := event.(type) {
			case *tcell.EventResize:
				a.screen.Sync()
			case *tcell.EventKey:
				if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyEsc {
					return
				}
				a.handleKey(ev)
			}
			a.render()
			// Small sleep to prevent CPU hog
			time.Sleep(33 * time.Millisecond)
		}
	}
}

func (a *App) handleKey(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEnter:
		switch a.state {
		case StateBrowse:
			a.callSelected()
		case StateInCall:
			a.endCall()
		}
	case tcell.KeyUp:
		filtered := a.filterNodes()
		if len(filtered) > 0 {
			a.selectedIdx = (a.selectedIdx - 1 + len(filtered)) % len(filtered)
		}
	case tcell.KeyDown:
		filtered := a.filterNodes()
		if len(filtered) > 0 {
			a.selectedIdx = (a.selectedIdx + 1) % len(filtered)
		}
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'q':
			close(a.quit)
		case 'r':
			a.fetchNodes()
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
		}
	}
}

// =========== Entry ===========

func main() {
	cfg := NewConfig()
	_ = cfg.Load()

	app := NewApp(cfg)
	app.Run()
}
