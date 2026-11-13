package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	adminKey = "key-sean-000000000000000000000000000"
	bobKey   = "key-bob-0000000000000000000000000000"
	amyKey   = "key-amy-0000000000000000000000000000"
)

// resetState points every global at a fresh temp dir so tests cannot leak
// into each other.
func resetState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	mu.Lock()
	defer mu.Unlock()
	msgs, lastID = nil, 0
	keys = map[string]UserInfo{
		adminKey: {Name: "sean", Role: "admin"},
		bobKey:   {Name: "bob", Role: "seller"},
		amyKey:   {Name: "amy", Role: "seller"},
	}
	rooms = map[string]*RoomState{}
	clients = map[*wsClient]bool{}
	botOff = map[string]bool{}
	customInv = []InvItem{}
	msgsFile = filepath.Join(dir, "messages.jsonl")
	botsFile = filepath.Join(dir, "bots.json")
	roomsFile = filepath.Join(dir, "rooms.json")
	invFile = filepath.Join(dir, "inventory.json")
	mediaDir = filepath.Join(dir, "media")
	f, err := os.OpenFile(msgsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	store = f
	initRooms()
	t.Cleanup(func() {
		mu.Lock()
		store.Close()
		mu.Unlock()
	})
}

// fakeOllama answers like Ollama for the models it is told are installed and
// records which models were asked for a chat reply.
type fakeOllama struct {
	*httptest.Server
	mu    sync.Mutex
	asked []string
}

func newFakeOllama(t *testing.T, installed ...string) *fakeOllama {
	t.Helper()
	f := &fakeOllama{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		type model struct {
			Name string `json:"name"`
		}
		models := []model{}
		for _, name := range installed {
			models = append(models, model{name})
		}
		json.NewEncoder(w).Encode(map[string]any{"models": models})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.asked = append(f.asked, in.Model)
		f.mu.Unlock()
		for _, name := range installed {
			if name == in.Model {
				json.NewEncoder(w).Encode(map[string]any{
					"message": map[string]string{"content": "<think>pick a greeting</think>hello from " + in.Model},
				})
				return
			}
		}
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	ollamaMu.Lock()
	ollamaURL, ollamaAt, ollamaModels = f.URL, time.Time{}, nil
	ollamaMu.Unlock()
	return f
}

func (f *fakeOllama) askedModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func call(t *testing.T, h http.HandlerFunc, method, target, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body, err)
	}
}

func postMsg(t *testing.T, key string, body map[string]any) Msg {
	t.Helper()
	rec := call(t, apiMessages, "POST", "/api/messages", key, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST message: status %d: %s", rec.Code, rec.Body)
	}
	var m Msg
	decode(t, rec, &m)
	return m
}

func waitForMsg(t *testing.T, match func(Msg) bool) Msg {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		for _, m := range msgs {
			if match(m) {
				mu.Unlock()
				return m
			}
		}
		mu.Unlock()
	}
	t.Fatal("timed out waiting for a message")
	return Msg{}
}

// storedMsgs reads the log back the way a restart would.
func storedMsgs(t *testing.T) []Msg {
	t.Helper()
	b, err := os.ReadFile(msgsFile)
	if err != nil {
		t.Fatal(err)
	}
	var out []Msg
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var m Msg
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func botStatusByName(t *testing.T) map[string]botStatus {
	t.Helper()
	rec := call(t, apiBots, "GET", "/api/bots", bobKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/bots: status %d", rec.Code)
	}
	var list []botStatus
	decode(t, rec, &list)
	out := map[string]botStatus{}
	for _, b := range list {
		out[b.Name] = b
	}
	if len(out) != len(aiBots) {
		t.Fatalf("got %d bots, want %d", len(out), len(aiBots))
	}
	return out
}

func TestMentionGetsBotReplyThatSurvivesRestart(t *testing.T) {
	resetState(t)
	newFakeOllama(t, "qwen2.5:7b")

	postMsg(t, bobKey, map[string]any{"room": "main", "text": "hi @Cyra, are you there?"})
	reply := waitForMsg(t, func(m Msg) bool { return m.From == "cyra" })

	if reply.Role != "bot" || reply.Room != "main" || reply.Text != "hello from qwen2.5:7b" {
		t.Fatalf("unexpected bot reply: %+v", reply)
	}
	saved := storedMsgs(t)
	if len(saved) != 2 || saved[1].From != "cyra" || saved[1].Text != reply.Text {
		t.Fatalf("bot reply was not persisted: %+v", saved)
	}
}

func TestBotThatCannotAnswerSaysSo(t *testing.T) {
	resetState(t)
	newFakeOllama(t) // no models installed

	postMsg(t, bobKey, map[string]any{"room": "main", "text": "@grok hello"})
	reply := waitForMsg(t, func(m Msg) bool { return m.From == "grok" })

	if !strings.Contains(reply.Text, "could not reply") {
		t.Fatalf("want a visible failure, got %q", reply.Text)
	}
}

func TestSwitchedOffBotStaysSilent(t *testing.T) {
	resetState(t)
	ollama := newFakeOllama(t, "qwen2.5:7b", "qwen3:30b")
	off := map[string]any{"name": "cyra", "enabled": false}

	if rec := call(t, apiBots, "POST", "/api/bots", bobKey, off); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin switch: status %d, want 403", rec.Code)
	}
	if rec := call(t, apiBots, "POST", "/api/bots", adminKey, off); rec.Code != http.StatusOK {
		t.Fatalf("admin switch: status %d: %s", rec.Code, rec.Body)
	}
	bots := botStatusByName(t)
	if bots["cyra"].Enabled || bots["cyra"].Active || !bots["muse"].Enabled || !bots["muse"].Active {
		t.Fatalf("unexpected status: cyra %+v, muse %+v", bots["cyra"], bots["muse"])
	}

	postMsg(t, bobKey, map[string]any{"room": "main", "text": "@cyra @muse hello"})
	waitForMsg(t, func(m Msg) bool { return m.From == "muse" })
	if asked := ollama.askedModels(); len(asked) != 1 || asked[0] != "qwen3:30b" {
		t.Fatalf("models asked = %v, want only muse's qwen3:30b", asked)
	}

	// The switch survives a restart.
	mu.Lock()
	botOff = map[string]bool{}
	loadBots()
	stillOff := botOff["cyra"]
	mu.Unlock()
	if !stillOff {
		t.Fatal("cyra came back on after reloading bots.json")
	}
}

func TestBotStatusSaysWhyABotIsInactive(t *testing.T) {
	resetState(t)
	ollama := newFakeOllama(t, "qwen2.5:7b")

	bots := botStatusByName(t)
	if !bots["cyra"].Active {
		t.Fatalf("cyra should be active: %+v", bots["cyra"])
	}
	if bots["grok"].Active || !strings.Contains(bots["grok"].Reason, "not installed") {
		t.Fatalf("grok should be inactive, model not installed: %+v", bots["grok"])
	}

	ollama.Close()
	ollamaMu.Lock()
	ollamaAt = time.Time{}
	ollamaMu.Unlock()
	if cyra := botStatusByName(t)["cyra"]; cyra.Active || cyra.Reason != "Ollama unreachable" {
		t.Fatalf("with Ollama down: %+v", cyra)
	}
}

func TestMentionsMatchWholeNamesOnly(t *testing.T) {
	cases := []struct {
		text, mention string
		want          bool
	}{
		{"hey @Cyra!", "@cyra", true},
		{"ask @muse, please", "@muse", true},
		{"visit the @museum", "@muse", false},
		{"the @museum, then @muse?", "@muse", true},
		{"mail bob@grok.com", "@grok", false},
	}
	for _, c := range cases {
		if got := mentions(c.text, c.mention); got != c.want {
			t.Errorf("mentions(%q, %q) = %v, want %v", c.text, c.mention, got, c.want)
		}
	}
}

func TestEditAndDelete(t *testing.T) {
	resetState(t)
	newFakeOllama(t)
	m := postMsg(t, bobKey, map[string]any{
		"room": "main", "text": "first draft", "media_url": "/media/a.png",
		"pinned_item": map[string]any{"id": "i1", "title": "Slab", "price": 5, "quantity": 1},
	})
	edit := func(method, key, content string) *httptest.ResponseRecorder {
		return call(t, apiMessages, method, "/api/messages", key, map[string]any{"id": m.ID, "content": content})
	}
	del := func(key string, id int) *httptest.ResponseRecorder {
		return call(t, apiMessages, "DELETE", "/api/messages?id="+strconv.Itoa(id), key, nil)
	}

	if rec := edit("PATCH", amyKey, "hijack"); rec.Code != http.StatusForbidden {
		t.Fatalf("edit by another user: status %d, want 403", rec.Code)
	}
	if rec := edit("PUT", bobKey, "   "); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty edit: status %d, want 400", rec.Code)
	}
	var got Msg
	rec := edit("PATCH", bobKey, "second draft")
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.Text != "second draft" || !got.Edited || got.Rev != 1 {
		t.Fatalf("owner edit: status %d, %+v", rec.Code, got)
	}
	rec = edit("PUT", adminKey, "admin fix")
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.Text != "admin fix" || got.Rev != 2 {
		t.Fatalf("admin edit: status %d, %+v", rec.Code, got)
	}
	if saved := storedMsgs(t); len(saved) != 1 || saved[0].Text != "admin fix" || !saved[0].Edited {
		t.Fatalf("edit would be lost on restart: %+v", saved)
	}

	if rec := del(amyKey, m.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("delete by another user: status %d, want 403", rec.Code)
	}
	if rec := del(bobKey, m.ID+99); rec.Code != http.StatusNotFound {
		t.Fatalf("delete of unknown id: status %d, want 404", rec.Code)
	}
	if rec := del(bobKey, m.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", rec.Code)
	}
	saved := storedMsgs(t)[0]
	if !saved.Deleted || saved.Text != deletedText || saved.MediaURL != "" || saved.PinnedItem != nil || saved.Rev != 3 {
		t.Fatalf("delete would be lost on restart, or left content behind: %+v", saved)
	}
	if rec := edit("PUT", bobKey, "back from the dead"); rec.Code != http.StatusConflict {
		t.Fatalf("edit of a deleted message: status %d, want 409", rec.Code)
	}
}

// The older edit and delete routes under /api/rooms keep their own request and
// response shapes but must share the revision counter with /api/messages.
func TestLegacyRoomRoutesShareTheRevCounter(t *testing.T) {
	resetState(t)
	newFakeOllama(t)
	m := postMsg(t, bobKey, map[string]any{"room": "main", "text": "v0"})

	var got Msg
	rec := call(t, apiRooms, "PATCH", "/api/rooms", bobKey, map[string]any{"id": m.ID, "text": "v1"})
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.Text != "v1" || got.Rev != 1 {
		t.Fatalf("legacy edit: status %d, %+v", rec.Code, got)
	}
	rec = call(t, apiMessages, "PUT", "/api/messages", bobKey, map[string]any{"id": m.ID, "content": "v2"})
	decode(t, rec, &got)
	if got.Rev != 2 {
		t.Fatalf("rev after an edit on each route = %d, want 2", got.Rev)
	}

	var body map[string]any
	rec = call(t, apiRooms, "DELETE", "/api/rooms?id="+strconv.Itoa(m.ID), bobKey, nil)
	decode(t, rec, &body)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("legacy delete: status %d, body %v", rec.Code, body)
	}
	if saved := storedMsgs(t)[0]; !saved.Deleted || saved.Text != "[message recalled]" || saved.Rev != 3 {
		t.Fatalf("after legacy delete: %+v", saved)
	}
}

// testWS is a bare client for the server's WebSocket: masked frames out,
// unmasked frames in.
type testWS struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

type wsEvent struct {
	Type string          `json:"type"`
	From string          `json:"from"`
	Data json.RawMessage `json:"data"`
}

func dialWS(t *testing.T, serverURL, key string) *testWS {
	t.Helper()
	addr := strings.TrimPrefix(serverURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Key and expected accept value are the worked example in RFC 6455.
	_, err = io.WriteString(conn, "GET /ws?room=main&token="+key+" HTTP/1.1\r\n"+
		"Host: "+addr+"\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: keep-alive, Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept := resp.Header.Get("Sec-WebSocket-Accept"); resp.StatusCode != http.StatusSwitchingProtocols || accept != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("handshake: status %d, accept %q", resp.StatusCode, accept)
	}
	return &testWS{t, conn, br}
}

func (w *testWS) read() (opcode int, payload []byte) {
	w.t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(w.br, head[:]); err != nil {
		w.t.Fatalf("reading frame: %v", err)
	}
	if head[1]&0x80 != 0 {
		w.t.Fatal("server sent a masked frame")
	}
	size := int(head[1] & 0x7f)
	if size == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			w.t.Fatal(err)
		}
		size = int(ext[0])<<8 | int(ext[1])
	} else if size == 127 {
		w.t.Fatal("unexpectedly large frame")
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(w.br, payload); err != nil {
		w.t.Fatal(err)
	}
	return int(head[0] & 0x0f), payload
}

func (w *testWS) write(opcode int, payload []byte, fin bool) {
	w.t.Helper()
	first := byte(opcode)
	if fin {
		first |= 0x80
	}
	var mask [4]byte
	rand.Read(mask[:])
	frame := append([]byte{first, 0x80 | byte(len(payload))}, mask[:]...)
	for i, c := range payload {
		frame = append(frame, c^mask[i&3])
	}
	if _, err := w.conn.Write(frame); err != nil {
		w.t.Fatal(err)
	}
}

// event returns the next event of the wanted type, skipping any others.
func (w *testWS) event(want string) wsEvent {
	w.t.Helper()
	for {
		opcode, payload := w.read()
		if opcode != wsText {
			continue
		}
		var ev wsEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			w.t.Fatalf("bad event %q: %v", payload, err)
		}
		if ev.Type == want {
			return ev
		}
	}
}

func TestWebSocketCarriesLiveEvents(t *testing.T) {
	resetState(t)
	newFakeOllama(t)
	srv := httptest.NewServer(http.HandlerFunc(apiWS))
	defer srv.Close()

	bob := dialWS(t, srv.URL, bobKey)
	var state RoomState
	if err := json.Unmarshal(bob.event("room_state").Data, &state); err != nil || len(state.Online) != 1 || state.Online[0] != "bob" {
		t.Fatalf("room state on join: %+v (%v)", state, err)
	}

	m := postMsg(t, amyKey, map[string]any{"room": "main", "text": "hello room"})
	var got Msg
	json.Unmarshal(bob.event("chat").Data, &got)
	if got.ID != m.ID || got.Text != "hello room" {
		t.Fatalf("chat event: %+v", got)
	}

	call(t, apiMessages, "PATCH", "/api/messages", amyKey, map[string]any{"id": m.ID, "content": "hello again"})
	json.Unmarshal(bob.event("msg_edit").Data, &got)
	if got.Text != "hello again" || got.Rev != 1 {
		t.Fatalf("edit event: %+v", got)
	}

	call(t, apiMessages, "DELETE", "/api/messages?id="+strconv.Itoa(m.ID), amyKey, nil)
	var tombstone struct{ ID, Rev int }
	json.Unmarshal(bob.event("msg_delete").Data, &tombstone)
	if tombstone.ID != m.ID || tombstone.Rev != 2 {
		t.Fatalf("delete event: %+v", tombstone)
	}

	bob.write(wsPing, []byte("still there?"), true)
	if opcode, payload := bob.read(); opcode != wsPong || string(payload) != "still there?" {
		t.Fatalf("ping answered with opcode %d %q", opcode, payload)
	}
}

func TestWebSocketRelaysSignalSentInFragments(t *testing.T) {
	resetState(t)
	mu.Lock()
	rooms["main"].AudioMode = "live"
	mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(apiWS))
	defer srv.Close()

	bob, amy := dialWS(t, srv.URL, bobKey), dialWS(t, srv.URL, amyKey)
	amy.event("room_state") // amy is registered once her own join is announced

	signal := `{"type":"rtc_signal","to":"amy","data":{"candidate":"c1"}}`
	bob.write(wsText, []byte(signal[:20]), false)
	bob.write(wsContinuation, []byte(signal[20:]), true)

	ev := amy.event("rtc_signal")
	if ev.From != "bob" || string(ev.Data) != `{"candidate":"c1"}` {
		t.Fatalf("relayed signal: from %q, data %s", ev.From, ev.Data)
	}
}
