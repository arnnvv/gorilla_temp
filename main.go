package main

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Client struct {
	conn     *websocket.Conn
	user     string
	mu       sync.Mutex
	channels map[string]bool
}

type Message struct {
	Type    string `json:"type"`           // message, status
	Channel string `json:"channel"`        // Format: "dm:user1:user2"
	Content any    `json:"content"`        // Can be string or structured data
	From    string `json:"from,omitempty"` // Add this line
	To      string `json:"to,omitempty"`
}

var (
	clients   = make(map[string]*Client)
	clientsMu sync.RWMutex

	channels   = make(map[string]map[string]bool) // channel -> participants
	channelsMu sync.RWMutex
)

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	user := r.URL.Query().Get("user")
	if user == "" {
		conn.WriteJSON(Message{Type: "error", Content: "Missing user parameter"})
		return
	}

	// Register client
	clientsMu.Lock()
	if old, exists := clients[user]; exists {
		old.conn.Close()
	}
	client := &Client{
		conn:     conn,
		user:     user,
		channels: make(map[string]bool),
	}
	clients[user] = client
	clientsMu.Unlock()

	defer func() {
		cleanupClient(user)
		log.Printf("%s disconnected", user)
	}()

	// Send confirmation
	conn.WriteJSON(Message{Type: "connect", Content: "Authenticated as " + user})

	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				log.Printf("Read error: %v", err)
			}
			break
		}

		switch msg.Type {
		case "message":
			handleMessage(user, msg)
		case "status":
			handleStatusRequest(user)
		default:
			conn.WriteJSON(Message{Type: "error", Content: "Unknown message type"})
		}
	}
}

func handleMessage(sender string, msg Message) {
	clientsMu.RLock()
	client, exists := clients[sender]
	clientsMu.RUnlock()

	if !exists {
		return
	}

	if msg.To != "" {
		createDMChannel(sender, msg.To, client)
		return
	}

	if msg.Channel == "" {
		client.conn.WriteJSON(Message{Type: "error", Content: "Missing channel"})
		return
	}

	if !validateChannelAccess(sender, msg.Channel) {
		client.conn.WriteJSON(Message{Type: "error", Content: "Access denied"})
		return
	}

	// Check DM requirements
	if strings.HasPrefix(msg.Channel, "dm:") {
		parts := strings.Split(msg.Channel, ":")
		if len(parts) != 3 {
			client.conn.WriteJSON(Message{Type: "error", Content: "Invalid DM channel"})
			return
		}

		channelsMu.RLock()
		participants, exists := channels[msg.Channel]
		channelsMu.RUnlock()

		if !exists {
			client.conn.WriteJSON(Message{Type: "error", Content: "Channel not found"})
			return
		}

		// Verify both participants are connected
		if !bothParticipantsConnected(participants) {
			client.conn.WriteJSON(Message{Type: "error", Content: "Both users must be connected"})
			return
		}
	}

	broadcastMessage(msg.Channel, Message{
		Type:    "message",
		Channel: msg.Channel,
		Content: msg.Content,
		From:    sender,
	})
}

func bothParticipantsConnected(participants map[string]bool) bool {
	clientsMu.RLock()
	defer clientsMu.RUnlock()

	for user := range participants {
		if _, exists := clients[user]; !exists {
			return false
		}
	}
	return true
}

func createDMChannel(user1, user2 string, client *Client) {
	if user1 == user2 {
		client.conn.WriteJSON(Message{Type: "error", Content: "Cannot message yourself"})
		return
	}

	clientsMu.RLock()
	_, exists := clients[user2]
	clientsMu.RUnlock()

	if !exists {
		client.conn.WriteJSON(Message{Type: "error", Content: "User not found"})
		return
	}

	// Generate sorted channel name
	participants := []string{user1, user2}
	sort.Strings(participants)
	channelName := "dm:" + strings.Join(participants, ":")

	// Add both users to the channel
	addToChannel(user1, channelName)
	addToChannel(user2, channelName)

	// Notify both users
	msg := Message{
		Type:    "system",
		Channel: channelName,
		Content: "Direct message channel established",
	}
	broadcastMessage(channelName, msg)
}

func addToChannel(user, channel string) {
	channelsMu.Lock()
	defer channelsMu.Unlock()

	// Add to client's channel list
	clientsMu.Lock()
	clients[user].mu.Lock()
	clients[user].channels[channel] = true
	clients[user].mu.Unlock()
	clientsMu.Unlock()

	// Create channel if not exists
	if _, exists := channels[channel]; !exists {
		channels[channel] = make(map[string]bool)
	}
	channels[channel][user] = true
}

func validateChannelAccess(user, channel string) bool {
	if strings.HasPrefix(channel, "dm:") {
		parts := strings.Split(channel, ":")
		if len(parts) != 3 {
			return false
		}
		return parts[1] == user || parts[2] == user
	}
	return false
}

func broadcastMessage(channel string, msg Message) {
	channelsMu.RLock()
	defer channelsMu.RUnlock()

	participants, exists := channels[channel]
	if !exists {
		return
	}

	clientsMu.RLock()
	defer clientsMu.RUnlock()

	for user := range participants {
		if client, ok := clients[user]; ok {
			client.conn.WriteJSON(msg)
		}
	}
}

func cleanupClient(user string) {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	client, exists := clients[user]
	if !exists {
		return
	}

	// Remove from all channels
	client.mu.Lock()
	defer client.mu.Unlock()

	channelsMu.Lock()
	defer channelsMu.Unlock()

	for channel := range client.channels {
		if participants, ok := channels[channel]; ok {
			delete(participants, user)
			if len(participants) == 0 {
				delete(channels, channel)
			}
		}
	}

	delete(clients, user)
}

func handleStatusRequest(user string) {
	clientsMu.RLock()
	client, exists := clients[user]
	clientsMu.RUnlock()

	if !exists {
		return
	}

	status := struct {
		OnlineUsers  []string `json:"onlineUsers"`
		OpenChannels []string `json:"openChannels"`
	}{
		OnlineUsers:  getOnlineUsers(),
		OpenChannels: getClientChannels(client),
	}

	client.conn.WriteJSON(Message{
		Type:    "status",
		Content: status,
	})
}

func getOnlineUsers() []string {
	clientsMu.RLock()
	defer clientsMu.RUnlock()

	users := make([]string, 0, len(clients))
	for user := range clients {
		users = append(users, user)
	}
	return users
}

func getClientChannels(client *Client) []string {
	client.mu.Lock()
	defer client.mu.Unlock()

	chans := make([]string, 0, len(client.channels))
	for ch := range client.channels {
		chans = append(chans, ch)
	}
	return chans
}

func main() {
	http.HandleFunc("/", wsHandler)
	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
