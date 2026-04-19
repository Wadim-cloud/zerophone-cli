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
	refreshInt int `json:"refresh_interval"` // seconds
}

func NewConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		ServerAddr: "http://localhost:8080",
		configPath: filepath.Join(home, ".zerophone-cli.json"),
		refreshInt: 5,
	}
}

func (c *Config) Path() string { return c.configPath }

func (c *Config) Load() error {
	data, err := os.ReadFile(c.configPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, c); err != nil {
		// Try loading without refreshInt for backwards compat
		var partial struct {
			NetworkID  string `json:"network_id"`
			NodeID     string `json:"node_id"`
			Name       string `json:"name"`
			ServerAddr string `json:"server_addr"`
		}
		if err2 := json.Unmarshal(data, &partial); err2 != nil {
			return err
		}
		c.NetworkID = partial.NetworkID
		c.NodeID = partial.NodeID
		c.Name = partial.Name
		c.ServerAddr = partial.ServerAddr
	}
	return nil
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
	scrollIdx   int

	activeCall *CallInfo
	incoming   *CallInfo
	callTimer  *time.Ticker
	callSecs   int

	statusMsg  string
	statusTime time.Time
	errMsg     string
	errTime    time.Time

	screen      tcell.Screen
	quit        chan struct{}
	eventChan   chan tcell.Event
	lastRefresh time.Time
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
		cfg:         cfg,
		http:        client,
		ws:          ws,
		state:       StateBrowse,
		nodes:       []Node{},
		statusTime:  time.Now(),
		errTime:     time.Now(),
		screen:      screen,
		quit:        make(chan struct{}),
		eventChan:   make(chan tcell.Event, 100),
		lastRefresh: time.Now(),
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
		a.cfg.NodeID = registered.ID
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
		a.selectedIdx = 0
		a.scrollIdx = 0
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
	a.lastRefresh = time.Now()
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

	width, height := screen.Size()

	// Colors
	titleStyle := tcell.StyleDefault.Foreground(tcell.NewRGBColor(0, 255, 255)).Bold(true) // Cyan
	headerStyle := tcell.StyleDefault.Foreground(tcell.NewRGBColor(128, 0, 128))           // Purple
	onlineStyle := tcell.StyleDefault.Foreground(tcell.ColorGreen)
	offlineStyle := tcell.StyleDefault.Foreground(tcell.ColorGray)
	selectStyle := tcell.StyleDefault.Foreground(tcell.NewRGBColor(255, 255, 0)).Bold(true) // Yellow
	errorStyle := tcell.StyleDefault.Foreground(tcell.ColorRed)
	successStyle := tcell.StyleDefault.Foreground(tcell.ColorGreen)
	mutedStyle := tcell.StyleDefault.Foreground(tcell.ColorGray)
	normalStyle := tcell.StyleDefault

	// Header bar
	a.renderHeader(screen, width, titleStyle, headerStyle, mutedStyle)

	// Incoming call overlay
	if a.incoming != nil {
		a.renderIncoming(screen, width, height, errorStyle, selectStyle, successStyle, mutedStyle)
	}

	// Active call bar
	if a.activeCall != nil && a.state == StateInCall {
		a.renderActiveCall(screen, width, height, successStyle, normalStyle)
	}

	// Main content
	if !a.isRegistered() {
		a.renderRegister(screen, width, titleStyle, headerStyle, mutedStyle, normalStyle, errorStyle)
	} else {
		a.renderNodeList(screen, width, height, normalStyle, selectStyle, onlineStyle, offlineStyle, headerStyle, mutedStyle)
	}

	// Footer
	a.renderFooter(screen, width, height, errorStyle, successStyle, mutedStyle)
}

func (a *App) renderHeader(screen tcell.Screen, width int, titleStyle, headerStyle, mutedStyle tcell.Style) {
	title := " ZeroPhone CLI "
	status := "[Disconnected]"
	if a.isRegistered() {
		status = "[Online]"
	}
	headerText := title + " " + status
	a.drawText(screen, 2, 0, headerText, titleStyle)
	separator := strings.Repeat("─", width-2)
	a.drawText(screen, 2, 1, separator, headerStyle)

	// Show network ID on header line 2
	if a.isRegistered() {
		netInfo := " Network: " + a.cfg.NetworkID + " "
		a.drawText(screen, width-len(netInfo)-2, 0, netInfo, mutedStyle)
	}
}

func (a *App) renderRegister(screen tcell.Screen, width int, titleStyle, headerStyle, mutedStyle, normalStyle, errorStyle tcell.Style) {
	y := 3

	boxText := " Not Registered "
	boxWidth := len(boxText) + 4
	boxX := (width - boxWidth) / 2
	if boxX < 2 {
		boxX = 2
	}

	// Draw box
	top := "╔" + strings.Repeat("═", boxWidth-2) + "╗"
	mid := "║" + strings.Repeat(" ", boxWidth-2) + "║"
	bot := "╚" + strings.Repeat("═", boxWidth-2) + "╝"

	a.drawText(screen, boxX, y, top, headerStyle)
	y++
	textX := boxX + 2
	a.drawText(screen, textX, y, boxText, errorStyle)
	y++
	a.drawText(screen, boxX, y, mid, headerStyle)
	textX = boxX + 2
	a.drawText(screen, textX, y, "║ "+boxText+" ║", errorStyle)
	y++
	a.drawText(screen, boxX, y, bot, headerStyle)
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

	// Help
	y += 1
	a.drawText(screen, 2, y, "Press [r] to refresh nodes after registration", normalStyle)
}

func (a *App) renderNodeList(screen tcell.Screen, width, height int, normalStyle, selectStyle, onlineStyle, offlineStyle, headerStyle, mutedStyle tcell.Style) int {
	y := 3
	filtered := a.filterNodes()
	online := 0
	for _, n := range filtered {
		if n.Status == "online" {
			online++
		}
	}

	// Header with full info
	header := " Nodes on " + a.cfg.NetworkID + " "
	a.drawText(screen, 2, y, header, headerStyle)
	y++

	// Stats line
	stats := fmt.Sprintf(" Total: %d  |  Online: %d  |  Offline: %d ", len(filtered), online, len(filtered)-online)
	if a.lastRefresh.After(time.Now().Add(-30 * time.Second)) {
		refreshInfo := fmt.Sprintf(" (ref: %s)", a.lastRefresh.Format("15:04:05"))
		stats += refreshInfo
	}
	a.drawText(screen, 2, y, stats, mutedStyle)
	y += 2

	if len(filtered) == 0 {
		msg := "  No other nodes discovered"
		if a.isRegistered() {
			msg += " (press [r] to refresh)"
		}
		a.drawText(screen, 2, y, msg, normalStyle)
		return y + 1
	}

	// Table headers
	headerRow := fmt.Sprintf(" %s  %-25s  %-20s  %-10s  %s", "▶", "Name", "Node ID", "Status", "Last Seen")
	a.drawText(screen, 2, y, headerRow, selectStyle)
	y++
	separator := strings.Repeat("─", max(width-2, 80))
	a.drawText(screen, 2, y, separator, headerStyle)
	y++

	// Visible nodes with scrolling
	visibleCount := height - y - 5 // leave room for footer
	if visibleCount <= 0 {
		visibleCount = 5
	}
	if a.selectedIdx < a.scrollIdx {
		a.scrollIdx = a.selectedIdx
	}
	if a.selectedIdx >= a.scrollIdx+visibleCount {
		a.scrollIdx = a.selectedIdx - visibleCount + 1
	}

	endIdx := a.scrollIdx + visibleCount
	if endIdx > len(filtered) {
		endIdx = len(filtered)
	}

	for i := a.scrollIdx; i < endIdx; i++ {
		node := filtered[i]

		sel := "  "
		if i == a.selectedIdx {
			sel = "▶"
		}

		name := truncate(node.Name, 25)
		id := truncate(node.ID, 20)
		lastSeen := time.Unix(node.LastSeen, 0).Format("15:04")

		var status string
		if node.Status == "online" {
			status = "● ONLINE"
		} else {
			status = "○ offline"
		}

		line := fmt.Sprintf(" %s  %-25s  %-20s  %-10s  %s", sel, name, id, status, lastSeen)
		style := normalStyle
		if i == a.selectedIdx {
			style = selectStyle
		}
		a.drawText(screen, 2, y, line, style)
		y++

		// Show capabilities if selected
		if i == a.selectedIdx && len(node.Capabilities) > 0 {
			y++
			capLine := "    Capabilities: " + strings.Join(node.Capabilities, ", ")
			a.drawText(screen, 2, y, capLine, mutedStyle)
			y++
		}
	}

	// Scrollbar if needed
	if len(filtered) > visibleCount {
		scrollBarY := y - visibleCount
		barHeight := (visibleCount * visibleCount) / len(filtered)
		if barHeight < 1 {
			barHeight = 1
		}
		barPos := (a.scrollIdx * visibleCount) / len(filtered)
		for i := 0; i < visibleCount; i++ {
			if i >= barPos && i < barPos+barHeight {
				a.drawText(screen, width-2, scrollBarY+i, "█", mutedStyle)
			} else {
				a.drawText(screen, width-2, scrollBarY+i, "│", mutedStyle)
			}
		}
	}

	return y
}

func (a *App) renderActiveCall(screen tcell.Screen, width, height int, successStyle, normalStyle tcell.Style) {
	y := height - 5
	text := fmt.Sprintf(" ╔══ Active Call with %s ─── %s ══╗ ", a.activeCall.To, a.formatDuration(a.callSecs))
	x := (width - len(text)) / 2
	if x < 2 {
		x = 2
	}
	// Draw box edges manually
	a.drawText(screen, x, y, "╔══ Active Call ══╗", successStyle)
	y++
	callInfo := fmt.Sprintf("   With: %s  |  Duration: %s  ", a.activeCall.To, a.formatDuration(a.callSecs))
	a.drawText(screen, x, y, callInfo, successStyle)
	y++
	a.drawText(screen, x, y, "╚══════════════════╝", successStyle)
	y++
	a.drawText(screen, 2, y, "  Press [Enter] to end the call", normalStyle)
}

func (a *App) renderIncoming(screen tcell.Screen, width, height int, errorStyle, selectStyle, successStyle, mutedStyle tcell.Style) {
	y := height/2 - 2
	text := " ╔══ Incoming Call ══ from " + a.incoming.From + " "
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

func (a *App) renderFooter(screen tcell.Screen, width, height int, errorStyle, successStyle, mutedStyle tcell.Style) {
	y := height - 3
	separator := strings.Repeat("─", width-2)
	a.drawText(screen, 2, y, separator, mutedStyle)

	// Status line
	y++
	if a.hasError() {
		a.drawText(screen, 2, y, " "+a.errMsg+" ", errorStyle)
	} else if a.hasStatus() {
		a.drawText(screen, 2, y, " "+a.statusMsg+" ", successStyle)
	}

	// Help + time
	y++
	now := time.Now().Format("15:04:05")
	help := " [↑↓]Move  [Enter]Call  [a]Ans  [R]Rej  [d]Del  [r]Ref  [q]Quit "
	timeStr := " " + now + " "

	space := width - 2 - len(help) - len(timeStr)
	if space < 0 {
		help = help[:max(0, width-2-len(timeStr))]
		a.drawText(screen, 2, y, help, mutedStyle)
		a.drawText(screen, 2+len(help), y, timeStr, mutedStyle)
	} else {
		a.drawText(screen, 2, y, help, mutedStyle)
		a.drawText(screen, width-1-len(timeStr), y, timeStr, mutedStyle)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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

	// Start event reader goroutine
	go func() {
		for {
			event := a.screen.PollEvent()
			select {
			case a.eventChan <- event:
			case <-a.quit:
				return
			}
		}
	}()

	// Auto-connect if configured
	if a.isRegistered() {
		a.setStatus("Registered as " + a.cfg.Name)
		go a.fetchNodes()
		go a.connectWS()
	}

	// Auto-refresh ticker
	refreshTicker := time.NewTicker(time.Duration(a.cfg.refreshInt) * time.Second)
	defer refreshTicker.Stop()

	// Event loop
	for {
		select {
		case <-a.quit:
			return
		case msg := <-a.ws.Messages():
			a.handleWS(msg)
		case <-refreshTicker.C:
			if a.isRegistered() {
				go a.fetchNodes()
			}
		case event := <-a.eventChan:
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
			a.setStatus("Refreshing...")
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
		case '?':
			a.showHelpOverlay()
		}
	}
}

func (a *App) showHelpOverlay() {
	// Simple help dialog — render on top
	screen := a.screen

	help := []string{
		"╔══ ZeroPhone CLI Help ════════════════════════════════╗",
		"║                                                    ║",
		"║  Navigation:                                       ║",
		"║    ↑/↓      Move selection                         ║",
		"║    Enter    Call selected node / End call          ║",
		"║    r        Refresh node list                      ║",
		"║                                                    ║",
		"║  Calls:                                            ║",
		"║    a        Answer incoming call                   ║",
		"║    R        Reject incoming call                   ║",
		"║                                                    ║",
		"║  Node Management:                                  ║",
		"║    d        Delete offline node (when selected)   ║",
		"║                                                    ║",
		"║  Other:                                            ║",
		"║    ?        Show this help                         ║",
		"║    q        Quit                                   ║",
		"║                                                    ║",
		"╚════════════════════════════════════════════════════╝",
	}

	boxW := 60
	boxH := len(help)
	scrWidth, scrHeight := screen.Size()
	startX := (scrWidth - boxW) / 2
	startY := (scrHeight - boxH) / 2

	// Draw dim overlay
	for y := 0; y < scrHeight; y++ {
		for x := 0; x < scrWidth; x++ {
			screen.SetContent(x, y, ' ', nil, tcell.StyleDefault)
		}
	}

	// Draw box
	for i, line := range help {
		a.drawText(screen, startX, startY+i, line, tcell.StyleDefault.Foreground(tcell.ColorYellow))
	}

	screen.Show()
	// Wait for key press
	a.screen.PollEvent()
}

// =========== Entry ===========

func main() {
	cfg := NewConfig()
	_ = cfg.Load()

	app := NewApp(cfg)
	app.Run()
}
