package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	mrand "math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type UserInfo struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type InvItem struct {
	ID          string  `json:"id"`
	Room        string  `json:"room,omitempty"`
	Title       string  `json:"title"`
	Price       float64 `json:"price"`
	Quantity    int     `json:"quantity"`
	Condition   string  `json:"condition"`
	Description string  `json:"description"`
	Owner       string  `json:"owner,omitempty"`
}

type Msg struct {
	ID         int      `json:"id"`
	Room       string   `json:"room,omitempty"`
	From       string   `json:"from"`
	Role       string   `json:"role,omitempty"`
	Text       string   `json:"text"`
	MediaURL   string   `json:"media_url,omitempty"`
	MediaType  string   `json:"media_type,omitempty"`
	STT        bool     `json:"stt,omitempty"`
	PinnedItem *InvItem `json:"pinned_item,omitempty"`
	Edited     bool     `json:"edited,omitempty"`
	Deleted    bool     `json:"deleted,omitempty"`
	At         string   `json:"at"`
}

type VideoState struct {
	URL       string  `json:"url"`
	Playing   bool    `json:"playing"`
	Position  float64 `json:"position"`
	UpdatedAt int64   `json:"updated_at"`
}

type GiveawayState struct {
	Active  bool     `json:"active"`
	Title   string   `json:"title"`
	Entries []string `json:"entries"`
	Winner  string   `json:"winner"`
}

type AuctionState struct {
Active     bool    `json:"active"`
ItemTitle  string  `json:"item_title"`
StartPrice float64 `json:"start_price"`
HighBid    float64 `json:"high_bid"`
HighBidder string  `json:"high_bidder"`
}

type RoomState struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Owner         string          `json:"owner"`
	StoreName     string          `json:"store_name,omitempty"`
	StoreURL      string          `json:"store_url,omitempty"`
	AudioMode     string          `json:"audio_mode"` // "stt_only" or "live"
	Video         VideoState      `json:"video"`
	Giveaway      GiveawayState   `json:"giveaway"`
	Auction       AuctionState    `json:"auction"`
	Followers     map[string]bool `json:"-"`
	FollowerCount int             `json:"follower_count"`
	FollowedByMe  bool            `json:"followed_by_me,omitempty"`
	Online        []string        `json:"online,omitempty"`
}

type wsClient struct {
	conn *websocket.Conn
	user UserInfo
	room string
	send chan []byte
}

var (
	mu         sync.Mutex
	msgs       []Msg
	lastID     int
	keys       = map[string]UserInfo{}
	store      *os.File
	rooms      = map[string]*RoomState{}
	customInv  = []InvItem{}
	clients    = map[*wsClient]bool{}
	mediaDir   = "/var/lib/mf-chat/media"
	roomsFile  = "/var/lib/mf-chat/rooms.json"
	invFile    = "/var/lib/mf-chat/inventory.json"
	msgsFile   = "/var/lib/mf-chat/messages.json"
	catalogCSV = "/opt/mf-go/catalog.csv"
	upgrader   = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func loadKeys(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) >= 2 && len(parts[1]) >= 32 {
			name := parts[0]
			key := parts[1]
			role := "seller"
			if name == "sean" {
				role = "admin"
			}
			if len(parts) >= 3 && (parts[2] == "admin" || parts[2] == "seller" || parts[2] == "guest") {
				role = parts[2]
			}
			keys[key] = UserInfo{Name: name, Role: role}
		}
	}
	if len(keys) == 0 {
		log.Fatal("no valid keys in ", path)
	}
}

func who(r *http.Request) (UserInfo, bool) {
	k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if k == "" {
		k = r.URL.Query().Get("token")
	}
	for key, u := range keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			return u, true
		}
	}
	return UserInfo{}, false
}

func initRooms() {
	if b, err := os.ReadFile(roomsFile); err == nil {
		_ = json.Unmarshal(b, &rooms)
	}
	defaults := []struct {
		id, title, owner string
	}{
		{"main", "#main-comms", "sean"},
		{"boystown", "BoysTown Live", "sean"},
		{"ops", "#pipeline-ops", "sean"},
	}
	for _, d := range defaults {
		if _, ok := rooms[d.id]; !ok {
			rooms[d.id] = &RoomState{
				ID:        d.id,
				Title:     d.title,
				Owner:     d.owner,
				AudioMode: "stt_only",
				Followers: map[string]bool{},
			}
		}
	}
	for _, rm := range rooms {
		if rm.Followers == nil {
			rm.Followers = map[string]bool{}
		}
		if rm.AudioMode == "" {
			rm.AudioMode = "stt_only"
		}
		rm.FollowerCount = len(rm.Followers)
	}
	if b, err := os.ReadFile(invFile); err == nil {
		_ = json.Unmarshal(b, &customInv)
	}
}

func saveRoomsLocked() {
	b, _ := json.MarshalIndent(rooms, "", "  ")
	_ = os.WriteFile(roomsFile, b, 0600)
}

func getOrCreateRoomLocked(id, owner string) *RoomState {
	if id == "" {
		id = "main"
	}
	if rm, ok := rooms[id]; ok {
		return rm
	}
	title := id
	if strings.HasPrefix(id, "seller:") {
		title = "Seller // " + strings.TrimPrefix(id, "seller:")
	}
	rm := &RoomState{
		ID:        id,
		Title:     title,
		Owner:     owner,
		AudioMode: "stt_only",
		Followers: map[string]bool{},
	}
	rooms[id] = rm
	saveRoomsLocked()
	return rm
}

func canHostRoom(u UserInfo, rm *RoomState) bool {
	return u.Role == "admin" || (rm != nil && rm.Owner == u.Name)
}

func roomSnapshotLocked(rm *RoomState, viewer string) RoomState {
	cp := *rm
	cp.FollowerCount = len(rm.Followers)
	cp.FollowedByMe = rm.Followers[viewer]
	seen := map[string]bool{}
	online := []string{}
	for c := range clients {
		if c.room == rm.ID && !seen[c.user.Name] {
			seen[c.user.Name] = true
			online = append(online, c.user.Name)
		}
	}
	cp.Online = online
	return cp
}

func broadcastRoomLocked(roomID string, eventType string, payload any) {
	env := map[string]any{"type": eventType, "room": roomID, "data": payload}
	b, _ := json.Marshal(env)
	for c := range clients {
		if c.room == roomID {
			select {
			case c.send <- b:
			default:
			}
		}
	}
}

func broadcastRoomStateLocked(rm *RoomState) {
	for c := range clients {
		if c.room == rm.ID {
			snap := roomSnapshotLocked(rm, c.user.Name)
			env := map[string]any{"type": "room_state", "room": rm.ID, "data": snap}
			b, _ := json.Marshal(env)
			select {
			case c.send <- b:
			default:
			}
		}
	}
}

func apiMe(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(u)
}


func saveAllMsgsLocked() {
if store == nil {
return
}
// Rewrite the same file that is loaded at startup, atomically, then reopen
// the append handle so later posts land in the new file.
tmp := msgsFile + ".tmp"
f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
if err != nil {
log.Printf("save messages: %v", err)
return
}
bw := bufio.NewWriter(f)
for _, m := range msgs {
b, _ := json.Marshal(m)
bw.Write(b)
bw.WriteByte('\n')
}
if err := bw.Flush(); err != nil {
f.Close()
os.Remove(tmp)
log.Printf("save messages: %v", err)
return
}
f.Close()
if err := os.Rename(tmp, msgsFile); err != nil {
os.Remove(tmp)
log.Printf("save messages: %v", err)
return
}
if nf, err := os.OpenFile(msgsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err == nil {
store.Close()
store = nf
} else {
log.Printf("reopen messages: %v", err)
}
}

func handleAiMentionsLocked(roomID string, userMsg *Msg) {
    text := strings.ToLower(userMsg.Text)
    bots := map[string]struct{ name, role, reply string }{
        "@mate":     {"@mate", "Co-Pilot", "Acknowledged. MateFlow operational stream active for room: " + roomID + "."},
        "@curator":  {"@curator", "Vault Curator", "Inventory index parsed. Target items cataloged and verified."},
        "@trader":   {"@trader", "Market Analyst", "Liquidity check and pricing spread evaluated for the requested asset."},
        "@sentinel": {"@sentinel", "Security Sentinel", "Integrity confirmed. Zero anomalous state transitions detected."},
        "@oracle":   {"@oracle", "Predictive Oracle", "Trajectory analysis indicates 94.2% positive alignment."},
    }

    for tag, bot := range bots {
        if strings.Contains(text, tag) {
            aiMsg := Msg{
                ID:        int(time.Now().UnixNano()),
                Room:      roomID,
                From:      bot.name,
                Role:      "bot",
                Text:      bot.reply,
                At:        time.Now().Format("15:04:05"),
            }
            msgs = append(msgs, aiMsg)
            saveAllMsgsLocked()
            broadcastRoomLocked(roomID, "chat", aiMsg)
            break
        }
    }
}
func msgRoom(room string) string {
	if room == "" {
		return "main"
	}
	return room
}

func apiMessages(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
case "PUT", "PATCH":
var req struct {
ID      int64  `json:"id"`
Content string `json:"content"`
}
if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil || strings.TrimSpace(req.Content) == "" {
http.Error(w, "bad request", 400)
return
}
mu.Lock()
var updated *Msg
for i := range msgs {
if msgs[i].ID == int(req.ID) {
if msgs[i].From != u.Name && u.Role != "admin" {
mu.Unlock()
http.Error(w, "forbidden", 403)
return
}
if msgs[i].Deleted {
mu.Unlock()
http.Error(w, "message was deleted", 409)
return
}
msgs[i].Text = req.Content
msgs[i].Edited = true
updated = &msgs[i]
break
}
}
if updated != nil {
saveAllMsgsLocked()
broadcastRoomLocked(msgRoom(updated.Room), "msg_edit", updated)
}
mu.Unlock()
if updated == nil {
http.Error(w, "not found", 404)
return
}
json.NewEncoder(w).Encode(updated)
return

case "DELETE":
idStr := r.URL.Query().Get("id")
id, _ := strconv.ParseInt(idStr, 10, 64)
mu.Lock()
var deleted *Msg
for i := range msgs {
if msgs[i].ID == int(id) {
if msgs[i].From != u.Name && u.Role != "admin" {
mu.Unlock()
http.Error(w, "forbidden", 403)
return
}
msgs[i].Deleted = true
msgs[i].Text = "This message was deleted"
msgs[i].MediaURL, msgs[i].MediaType, msgs[i].PinnedItem = "", "", nil
deleted = &msgs[i]
break
}
}
if deleted != nil {
saveAllMsgsLocked()
broadcastRoomLocked(msgRoom(deleted.Room), "msg_delete", map[string]any{"id": deleted.ID, "room": deleted.Room})
}
mu.Unlock()
if deleted == nil {
http.Error(w, "not found", 404)
return
}
w.WriteHeader(204)
return

case "GET":
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		room := r.URL.Query().Get("room")
		if room == "" {
			room = "main"
		}
		mu.Lock()
		out := []Msg{}
		for _, m := range msgs {
			mRoom := m.Room
			if mRoom == "" {
				mRoom = "main"
			}
			if m.ID > since && (room == "all" || mRoom == room) {
				out = append(out, m)
			}
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(out)
	case "POST":
		var in struct {
			Room       string   `json:"room"`
			Text       string   `json:"text"`
			MediaURL   string   `json:"media_url"`
			MediaType  string   `json:"media_type"`
			STT        bool     `json:"stt"`
			PinnedItem *InvItem `json:"pinned_item"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&in); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if strings.TrimSpace(in.Text) == "" && in.MediaURL == "" && in.PinnedItem == nil {
			http.Error(w, "bad request: empty message", 400)
			return
		}
		if in.Room == "" {
			in.Room = "main"
		}
		mu.Lock()
		getOrCreateRoomLocked(in.Room, u.Name)
		m := Msg{
			ID:         lastID + 1,
			Room:       in.Room,
			From:       u.Name,
			Role:       u.Role,
			Text:       in.Text,
			MediaURL:   in.MediaURL,
			MediaType:  in.MediaType,
			STT:        in.STT,
			PinnedItem: in.PinnedItem,
			At:         time.Now().UTC().Format(time.RFC3339),
		}
		b, _ := json.Marshal(m)
		if _, err := store.Write(append(b, '\n')); err != nil {
			mu.Unlock()
			http.Error(w, "store failed", 500)
			return
		}
		msgs = append(msgs, m)
		lastID = m.ID
		broadcastRoomLocked(in.Room, "chat", m)
		mu.Unlock()
		triggerAIMentions(in.Room, u.Name, in.Text)
		json.NewEncoder(w).Encode(m)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func apiRooms(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" {
		mu.Lock()
		out := []RoomState{}
		for _, rm := range rooms {
			out = append(out, roomSnapshotLocked(rm, u.Name))
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(out)
		return
	}
	if r.Method == "POST" {
		var in struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || strings.TrimSpace(in.ID) == "" {
			http.Error(w, "bad request", 400)
			return
		}
		id := strings.ToLower(strings.TrimSpace(in.ID))
		if u.Role != "admin" && id != "seller:"+u.Name {
			http.Error(w, "forbidden: sellers can only create their own seller room", 403)
			return
		}
		mu.Lock()
		rm := getOrCreateRoomLocked(id, u.Name)
		if in.Title != "" {
			rm.Title = in.Title
			saveRoomsLocked()
		}
		snap := roomSnapshotLocked(rm, u.Name)
		mu.Unlock()
		json.NewEncoder(w).Encode(snap)
		return
	}
if r.Method == "PATCH" || r.Method == "PUT" {
var in struct {
ID   int    `json:"id"`
Text string `json:"text"`
}
if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
http.Error(w, "bad request", 400)
return
}
mu.Lock()
for i := range msgs {
if msgs[i].ID == in.ID && !msgs[i].Deleted {
rm := rooms[msgs[i].Room]
if msgs[i].From != u.Name && !canHostRoom(u, rm) {
mu.Unlock()
http.Error(w, "forbidden", 403)
return
}
msgs[i].Text = in.Text
msgs[i].Edited = true
updated := msgs[i]
saveAllMsgsLocked()
broadcastRoomLocked(updated.Room, "msg_edit", updated)
mu.Unlock()
w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(updated)
return
}
}
mu.Unlock()
http.Error(w, "not found", 404)
return
}
if r.Method == "DELETE" {
idStr := r.URL.Query().Get("id")
id, _ := strconv.Atoi(idStr)
mu.Lock()
for i := range msgs {
if msgs[i].ID == int(id) {
rm := rooms[msgs[i].Room]
if msgs[i].From != u.Name && !canHostRoom(u, rm) {
mu.Unlock()
http.Error(w, "forbidden", 403)
return
}
msgs[i].Deleted = true
msgs[i].Text = "[message recalled]"
msgs[i].MediaURL = ""
msgs[i].PinnedItem = nil
targetRoom := msgs[i].Room
saveAllMsgsLocked()
broadcastRoomLocked(targetRoom, "msg_delete", map[string]any{"id": id})
mu.Unlock()
w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
return
}
}
mu.Unlock()
http.Error(w, "not found", 404)
return
}
http.Error(w, "method not allowed", 405)
}

func apiRoomState(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		roomID = "main"
	}
	mu.Lock()
	rm := getOrCreateRoomLocked(roomID, u.Name)
	snap := roomSnapshotLocked(rm, u.Name)
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func apiRoomVideo(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	roomID := r.URL.Query().Get("room")
	var in struct {
		Action   string  `json:"action"` // load, play, pause, seek
		URL      string  `json:"url"`
		Position float64 `json:"position"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	mu.Lock()
	rm := getOrCreateRoomLocked(roomID, u.Name)
	if !canHostRoom(u, rm) {
		mu.Unlock()
		http.Error(w, "forbidden: host only", 403)
		return
	}
	switch in.Action {
	case "load":
		rm.Video.URL = strings.TrimSpace(in.URL)
		rm.Video.Position = 0
		rm.Video.Playing = false
	case "play":
		if in.URL != "" {
			rm.Video.URL = strings.TrimSpace(in.URL)
		}
		rm.Video.Position = in.Position
		rm.Video.Playing = true
	case "pause":
		rm.Video.Position = in.Position
		rm.Video.Playing = false
	case "seek":
		rm.Video.Position = in.Position
	}
	rm.Video.UpdatedAt = time.Now().UnixMilli()
	saveRoomsLocked()
	broadcastRoomStateLocked(rm)
	snap := roomSnapshotLocked(rm, u.Name)
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func apiRoomAudio(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	roomID := r.URL.Query().Get("room")
	var in struct {
		AudioMode string `json:"audio_mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if in.AudioMode != "live" && in.AudioMode != "stt_only" {
		http.Error(w, "invalid audio_mode", 400)
		return
	}
	mu.Lock()
	rm := getOrCreateRoomLocked(roomID, u.Name)
	if !canHostRoom(u, rm) {
		mu.Unlock()
		http.Error(w, "forbidden: host only", 403)
		return
	}
	rm.AudioMode = in.AudioMode
	saveRoomsLocked()
	broadcastRoomStateLocked(rm)
	snap := roomSnapshotLocked(rm, u.Name)
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func safeURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if !strings.Contains(u, "://") && strings.Contains(u, ".") && !strings.ContainsAny(u, " \t\n") {
		u = "https://" + u
	}
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "https://") || strings.HasPrefix(lu, "http://") {
		return u
	}
	return ""
}

func apiRoomStore(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	roomID := r.URL.Query().Get("room")
	var in struct {
		StoreName string `json:"store_name"`
		StoreURL  string `json:"store_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	mu.Lock()
	rm := getOrCreateRoomLocked(roomID, u.Name)
	if !canHostRoom(u, rm) {
		mu.Unlock()
		http.Error(w, "forbidden: host only", 403)
		return
	}
	rm.StoreName = strings.TrimSpace(in.StoreName)
	if len(rm.StoreName) > 80 {
		rm.StoreName = rm.StoreName[:80]
	}
	rm.StoreURL = safeURL(in.StoreURL)
	saveRoomsLocked()
	broadcastRoomStateLocked(rm)
	snap := roomSnapshotLocked(rm, u.Name)
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func apiRoomFollow(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	roomID := r.URL.Query().Get("room")
	mu.Lock()
	rm := getOrCreateRoomLocked(roomID, u.Name)
	rm.Followers[u.Name] = true
	rm.FollowerCount = len(rm.Followers)
	if rm.Giveaway.Active {
		found := false
		for _, e := range rm.Giveaway.Entries {
			if e == u.Name {
				found = true
				break
			}
		}
		if !found {
			rm.Giveaway.Entries = append(rm.Giveaway.Entries, u.Name)
		}
	}
	saveRoomsLocked()
	broadcastRoomStateLocked(rm)
	snap := roomSnapshotLocked(rm, u.Name)
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func apiRoomGiveaway(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	roomID := r.URL.Query().Get("room")
	var in struct {
		Action string `json:"action"` // start, draw, stop
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	mu.Lock()
	rm := getOrCreateRoomLocked(roomID, u.Name)
	if !canHostRoom(u, rm) {
		mu.Unlock()
		http.Error(w, "forbidden: host only", 403)
		return
	}
	switch in.Action {
	case "start":
		entries := []string{}
		for follower := range rm.Followers {
			entries = append(entries, follower)
		}
		title := strings.TrimSpace(in.Title)
		if title == "" {
			title = "Live Room Giveaway"
		}
		rm.Giveaway = GiveawayState{Active: true, Title: title, Entries: entries, Winner: ""}
	case "draw":
		if len(rm.Giveaway.Entries) > 0 {
			winner := rm.Giveaway.Entries[mrand.Intn(len(rm.Giveaway.Entries))]
			rm.Giveaway.Winner = winner
			rm.Giveaway.Active = false
		}
	case "stop":
		rm.Giveaway.Active = false
	}
	saveRoomsLocked()
	broadcastRoomStateLocked(rm)
	snap := roomSnapshotLocked(rm, u.Name)
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}

func apiInventory(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	roomID := r.URL.Query().Get("room")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" {
		items := []InvItem{}
		if f, err := os.Open(catalogCSV); err == nil {
			cr := csv.NewReader(f)
			rows, err := cr.ReadAll()
			f.Close()
			if err == nil && len(rows) > 1 {
				for _, row := range rows[1:] {
					if len(row) >= 7 {
						price, _ := strconv.ParseFloat(row[2], 64)
						qty, _ := strconv.Atoi(row[3])
						items = append(items, InvItem{
							ID:          row[0],
							Room:        "all",
							Title:       row[1],
							Price:       price,
							Quantity:    qty,
							Condition:   row[4],
							Description: row[6],
						})
					}
				}
			}
		}
		mu.Lock()
		for _, it := range customInv {
			if roomID == "" || it.Room == "" || it.Room == roomID || it.Room == "all" {
				items = append(items, it)
			}
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(items)
		return
	}
	if r.Method == "POST" {
		var in InvItem
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil || strings.TrimSpace(in.Title) == "" {
			http.Error(w, "bad request", 400)
			return
		}
		if in.Room == "" {
			in.Room = roomID
		}
		if in.ID == "" {
			buf := make([]byte, 4)
			rand.Read(buf)
			in.ID = "MF-" + strings.ToUpper(hex.EncodeToString(buf))
		}
		in.Owner = u.Name
		mu.Lock()
		customInv = append(customInv, in)
		b, _ := json.MarshalIndent(customInv, "", "  ")
		_ = os.WriteFile(invFile, b, 0600)
		mu.Unlock()
		json.NewEncoder(w).Encode(in)
		return
	}
	http.Error(w, "method not allowed", 405)
}

func scanFile(path string) (bool, string) {
	if _, err := exec.LookPath("clamdscan"); err == nil {
		cmd := exec.Command("clamdscan", "--no-summary", "--fdpass", path)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return true, "clamd-clean"
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, string(out)
		}
	}
	if _, err := exec.LookPath("clamscan"); err == nil {
		cmd := exec.Command("clamscan", "--no-summary", path)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return true, "clamscan-clean"
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, string(out)
		}
	}
	return true, "reencode-only"
}

func apiUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := who(r); !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "file too large (max 10MB)", 400)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", 400)
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 9<<20))
	if err != nil || len(raw) == 0 {
		http.Error(w, "invalid file", 400)
		return
	}

	mime := http.DetectContentType(raw)
	var ext string
	switch mime {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/gif":
		ext = ".gif"
	default:
		http.Error(w, "only JPEG, PNG, and GIF allowed", 415)
		return
	}

	tmp, err := os.CreateTemp("", "mfupload-*"+ext)
	if err != nil {
		http.Error(w, "temp error", 500)
		return
	}
	tmpPath := tmp.Name()
	tmp.Write(raw)
	tmp.Close()
	defer os.Remove(tmpPath)

	clean, scanNote := scanFile(tmpPath)
	if !clean {
		http.Error(w, "rejected by file-scan gateway: "+scanNote, 422)
		return
	}

	var sanitized bytes.Buffer
	switch mime {
	case "image/jpeg":
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			http.Error(w, "corrupt jpeg", 400)
			return
		}
		if err := jpeg.Encode(&sanitized, img, &jpeg.Options{Quality: 88}); err != nil {
			http.Error(w, "encode failed", 500)
			return
		}
	case "image/png":
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			http.Error(w, "corrupt png", 400)
			return
		}
		if err := png.Encode(&sanitized, img); err != nil {
			http.Error(w, "encode failed", 500)
			return
		}
	case "image/gif":
		g, err := gif.DecodeAll(bytes.NewReader(raw))
		if err != nil {
			http.Error(w, "corrupt gif", 400)
			return
		}
		if err := gif.EncodeAll(&sanitized, g); err != nil {
			http.Error(w, "encode failed", 500)
			return
		}
	}

	idBytes := make([]byte, 12)
	rand.Read(idBytes)
	outName := hex.EncodeToString(idBytes) + ext
	if err := os.WriteFile(filepath.Join(mediaDir, outName), sanitized.Bytes(), 0644); err != nil {
		http.Error(w, "write failed", 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"url":  "/media/" + outName,
		"mime": mime,
		"scan": scanNote,
		"size": sanitized.Len(),
	})
}

func apiMedia(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(strings.TrimPrefix(r.URL.Path, "/media/"))
	if name == "" || name == "." || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, filepath.Join(mediaDir, name))
}

func apiWS(w http.ResponseWriter, r *http.Request) {
	u, ok := who(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		roomID = "main"
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsClient{conn: conn, user: u, room: roomID, send: make(chan []byte, 64)}

	mu.Lock()
	rm := getOrCreateRoomLocked(roomID, u.Name)
	clients[c] = true
	broadcastRoomStateLocked(rm)
	mu.Unlock()

	go func() {
		defer conn.Close()
		for b := range c.send {
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				break
			}
		}
	}()

	defer func() {
		mu.Lock()
		delete(clients, c)
		close(c.send)
		if rm, ok := rooms[c.room]; ok {
			broadcastRoomStateLocked(rm)
		}
		mu.Unlock()
		conn.Close()
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var evt struct {
			Type string          `json:"type"`
			To   string          `json:"to"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &evt) != nil {
			continue
		}
		if evt.Type == "rtc_signal" {
			mu.Lock()
			rm := rooms[c.room]
			if rm != nil && rm.AudioMode == "live" {
				out, _ := json.Marshal(map[string]any{
					"type": "rtc_signal",
					"from": c.user.Name,
					"to":   evt.To,
					"data": evt.Data,
				})
				for peer := range clients {
					if peer != c && peer.room == c.room && (evt.To == "" || peer.user.Name == evt.To) {
						select {
						case peer.send <- out:
						default:
						}
					}
				}
			}
			mu.Unlock()
		}
	}
}

const openapi = `{"openapi":"3.1.0","info":{"title":"MateFlow Chat","version":"2"},
"servers":[{"url":"https://chat.mateflowlabs.com"}],
"components":{"securitySchemes":{"key":{"type":"http","scheme":"bearer"}}},"security":[{"key":[]}],
"paths":{"/api/messages":{
"get":{"operationId":"readMessages","summary":"Read messages newer than 'since'","parameters":[{"name":"since","in":"query","schema":{"type":"integer"}},{"name":"room","in":"query","schema":{"type":"string"}}],"responses":{"200":{"description":"messages"}}},
"post":{"operationId":"postMessage","summary":"Post a message","requestBody":{"required":true,"content":{"application/json":{"schema":{"type":"object","required":["text"],"properties":{"text":{"type":"string"},"room":{"type":"string"}}}}}},"responses":{"200":{"description":"posted"}}}}}}`


func apiRoomAuction(w http.ResponseWriter, r *http.Request) {
u, ok := who(r)
if !ok {
http.Error(w, "unauthorized", 401)
return
}
if r.Method != "POST" {
http.Error(w, "method not allowed", 405)
return
}
roomID := r.URL.Query().Get("room")
var in struct {
Action string  `json:"action"` // start, bid, stop
Title  string  `json:"title"`
Price  float64 `json:"price"`
}
if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&in); err != nil {
http.Error(w, "bad request", 400)
return
}
mu.Lock()
rm := getOrCreateRoomLocked(roomID, u.Name)
if in.Action == "bid" {
if !rm.Auction.Active {
mu.Unlock()
http.Error(w, "no active auction", 400)
return
}
if in.Price <= rm.Auction.HighBid || in.Price < rm.Auction.StartPrice {
mu.Unlock()
http.Error(w, "bid must beat current high bid", 400)
return
}
rm.Auction.HighBid = in.Price
rm.Auction.HighBidder = u.Name
saveRoomsLocked()
broadcastRoomStateLocked(rm)
snap := roomSnapshotLocked(rm, u.Name)
mu.Unlock()
w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(snap)
return
}
if !canHostRoom(u, rm) {
mu.Unlock()
http.Error(w, "forbidden: host only", 403)
return
}
switch in.Action {
case "start":
rm.Auction = AuctionState{Active: true, ItemTitle: strings.TrimSpace(in.Title), StartPrice: in.Price, HighBid: in.Price, HighBidder: ""}
case "stop":
rm.Auction.Active = false
}
saveRoomsLocked()
broadcastRoomStateLocked(rm)
snap := roomSnapshotLocked(rm, u.Name)
mu.Unlock()
w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(snap)
}

type aiBot struct {
Mention string
Name    string
Model   string
Persona string
}

var (
aiBots = []aiBot{
{"@cyra", "cyra", "qwen2.5:7b", "You are Cyra, a sharp and friendly MateFlow AI co-host. Keep replies concise (1-3 sentences)."},
{"@muse", "muse", "qwen3:30b", "You are Muse, a creative MateFlow AI stylist & curator. Keep replies concise (1-3 sentences)."},
{"@openclaw", "openclaw", "qwen2.5:7b", "You are OpenClaw, a fast technical MateFlow AI ops agent. Keep replies concise (1-3 sentences)."},
{"@hermes", "hermes", "qwen3:30b-a3b", "You are Hermes, a MateFlow commerce & auction messenger AI. Keep replies concise (1-3 sentences)."},
{"@grok", "grok", "grok-3-latest:latest", "You are Grok, a witty and direct MateFlow AI assistant. Keep replies concise (1-3 sentences)."},
}
ollamaURL = "http://127.0.0.1:11434"
botsFile  = "/var/lib/mf-chat/bots.json"
botOff    = map[string]bool{} // guarded by mu

ollamaMu     sync.Mutex
ollamaModels map[string]bool
ollamaAt     time.Time
)

func loadBots() {
if b, err := os.ReadFile(botsFile); err == nil {
json.Unmarshal(b, &botOff)
}
}

// ollamaInstalled lists the models Ollama has, cached for 15s; nil means Ollama is unreachable.
func ollamaInstalled() map[string]bool {
ollamaMu.Lock()
defer ollamaMu.Unlock()
if time.Since(ollamaAt) < 15*time.Second {
return ollamaModels
}
ollamaAt = time.Now()
ollamaModels = nil
client := &http.Client{Timeout: 3 * time.Second}
resp, err := client.Get(ollamaURL + "/api/tags")
if err != nil {
return nil
}
defer resp.Body.Close()
var out struct {
Models []struct {
Name string `json:"name"`
} `json:"models"`
}
if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil {
return nil
}
ollamaModels = map[string]bool{}
for _, m := range out.Models {
ollamaModels[m.Name] = true
ollamaModels[strings.TrimSuffix(m.Name, ":latest")] = true
}
return ollamaModels
}

type botStatus struct {
Name    string `json:"name"`
Mention string `json:"mention"`
Model   string `json:"model"`
Enabled bool   `json:"enabled"`
Active  bool   `json:"active"`
Reason  string `json:"reason,omitempty"`
}

func botStatuses() []botStatus {
installed := ollamaInstalled()
mu.Lock()
defer mu.Unlock()
out := make([]botStatus, 0, len(aiBots))
for _, b := range aiBots {
s := botStatus{Name: b.Name, Mention: b.Mention, Model: b.Model, Enabled: !botOff[b.Name]}
switch {
case !s.Enabled:
s.Reason = "switched off"
case installed == nil:
s.Reason = "Ollama unreachable"
case !installed[b.Model] && !installed[strings.TrimSuffix(b.Model, ":latest")]:
s.Reason = "model " + b.Model + " not installed"
default:
s.Active = true
}
out = append(out, s)
}
return out
}

func apiBots(w http.ResponseWriter, r *http.Request) {
u, ok := who(r)
if !ok {
http.Error(w, "unauthorized", 401)
return
}
switch r.Method {
case "GET":
case "POST":
if u.Role != "admin" {
http.Error(w, "forbidden: admin only", 403)
return
}
var in struct {
Name    string `json:"name"`
Enabled bool   `json:"enabled"`
}
if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
http.Error(w, "bad request", 400)
return
}
known := false
for _, b := range aiBots {
known = known || b.Name == in.Name
}
if !known {
http.Error(w, "unknown bot", 404)
return
}
mu.Lock()
if in.Enabled {
delete(botOff, in.Name)
} else {
botOff[in.Name] = true
}
b, _ := json.Marshal(botOff)
if err := os.WriteFile(botsFile, b, 0600); err != nil {
log.Printf("save bots: %v", err)
}
mu.Unlock()
default:
http.Error(w, "method not allowed", 405)
return
}
list := botStatuses()
if r.Method == "POST" {
env, _ := json.Marshal(map[string]any{"type": "bots", "data": list})
mu.Lock()
for c := range clients {
select {
case c.send <- env:
default:
}
}
mu.Unlock()
}
w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(list)
}

func triggerAIMentions(room, fromUser, text string) {
low := strings.ToLower(text)
for _, b := range aiBots {
mu.Lock()
off := botOff[b.Name]
mu.Unlock()
if !off && strings.Contains(low, b.Mention) {
go func(bot aiBot) {
payload := map[string]any{
"model":  bot.Model,
"stream": false,
"messages": []map[string]string{
{"role": "system", "content": bot.Persona},
{"role": "user", "content": fromUser + " says: " + text},
},
}
jb, _ := json.Marshal(payload)
client := &http.Client{Timeout: 45 * time.Second}
resp, err := client.Post(ollamaURL+"/api/chat", "application/json", bytes.NewReader(jb))
reply := ""
if err == nil {
defer resp.Body.Close()
var out struct {
Message struct {
Content string `json:"content"`
} `json:"message"`
}
if json.NewDecoder(resp.Body).Decode(&out) == nil {
reply = strings.TrimSpace(out.Message.Content)
}
}
if reply == "" {
log.Printf("bot %s (%s): no reply from Ollama (err=%v)", bot.Name, bot.Model, err)
reply = "[" + bot.Name + " could not reply: model " + bot.Model + " did not answer]"
}
mu.Lock()
m := Msg{
ID:   lastID + 1,
Room: room,
From: bot.Name,
Role: "bot",
Text: reply,
At:   time.Now().UTC().Format(time.RFC3339),
}
lastID++
msgs = append(msgs, m)
saveAllMsgsLocked()
broadcastRoomLocked(room, "chat", m)
mu.Unlock()
}(b)
}
}
}

func main() {
	mrand.Seed(time.Now().UnixNano())
	addr, keyPath, data := os.Getenv("MFCHAT_ADDR"), os.Getenv("MFCHAT_KEYS"), os.Getenv("MFCHAT_DATA")
	msgsFile = data
	botsFile = filepath.Join(filepath.Dir(data), "bots.json")
	if v := os.Getenv("MFCHAT_OLLAMA"); v != "" {
		ollamaURL = strings.TrimRight(v, "/")
	}
	loadBots()
	loadKeys(keyPath)
	initRooms()

	if f, err := os.Open(data); err == nil {
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for s.Scan() {
			var m Msg
			if json.Unmarshal(s.Bytes(), &m) == nil {
				msgs = append(msgs, m)
				lastID = m.ID
			}
		}
		f.Close()
	}
	var err error
	store, err = os.OpenFile(data, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/api/me", apiMe)
	http.HandleFunc("/api/messages", apiMessages)
	http.HandleFunc("/api/bots", apiBots)
	http.HandleFunc("/api/rooms", apiRooms)
	http.HandleFunc("/api/rooms/store", apiRoomStore)
	http.HandleFunc("/api/rooms/state", apiRoomState)
	http.HandleFunc("/api/rooms/video", apiRoomVideo)
	http.HandleFunc("/api/rooms/audio", apiRoomAudio)
	http.HandleFunc("/api/rooms/follow", apiRoomFollow)
	http.HandleFunc("/api/rooms/giveaway", apiRoomGiveaway)
	http.HandleFunc("/api/rooms/auction", apiRoomAuction)
	http.HandleFunc("/api/inventory", apiInventory)
	http.HandleFunc("/api/upload", apiUpload)
	http.HandleFunc("/media/", apiMedia)
	http.HandleFunc("/ws", apiWS)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	http.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(openapi))
	})

	log.Printf("listening on %s, %d keys, %d messages, %d rooms", addr, len(keys), len(msgs), len(rooms))
	log.Fatal(http.ListenAndServe(addr, nil))
}
