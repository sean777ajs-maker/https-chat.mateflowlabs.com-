package main

// AI chat bots. Each one answers through a local Ollama model when a message
// mentions it by name, and an admin can switch any of them off.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const botReplyTimeout = 90 * time.Second // leaves room for a cold model load

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
	botOff    = map[string]bool{} // bots an admin switched off; guarded by mu

	ollamaMu     sync.Mutex
	ollamaModels map[string]bool // installed models; nil while Ollama is unreachable
	ollamaAt     time.Time
)

// loadBots restores which bots were switched off before the last restart.
func loadBots() {
	b, err := os.ReadFile(botsFile)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, &botOff); err != nil {
		log.Printf("load bots: %v", err)
	}
	if botOff == nil {
		botOff = map[string]bool{}
	}
}

func knownBot(name string) bool {
	for _, b := range aiBots {
		if b.Name == name {
			return true
		}
	}
	return false
}

// ollamaInstalled lists the models Ollama has, cached for 15s so the status
// panel cannot hammer it. nil means Ollama did not answer.
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
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
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
	Enabled bool   `json:"enabled"`          // switched on by an admin
	Active  bool   `json:"active"`           // switched on and able to answer
	Reason  string `json:"reason,omitempty"` // why it is not active
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

// apiBots lists the bots (GET) or switches one on or off (POST, admins only).
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
		if !knownBot(in.Name) {
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
		// Every room shows the bot panel, so the change goes to all clients.
		event, _ := json.Marshal(map[string]any{"type": "bots", "data": list})
		mu.Lock()
		for c := range clients {
			select {
			case c.send <- event:
			default:
			}
		}
		mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// mentions reports whether text addresses a bot. The @name must stand alone,
// so "@museum" does not wake @muse and "bob@grok.com" does not wake @grok.
func mentions(text, mention string) bool {
	low := strings.ToLower(text)
	for from := 0; ; {
		i := strings.Index(low[from:], mention)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(mention)
		if (start == 0 || !isWordByte(low[start-1])) && (end == len(low) || !isWordByte(low[end])) {
			return true
		}
		from = end
	}
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')
}

// stripThinking drops the <think>...</think> reasoning some models put in
// front of their answer.
func stripThinking(s string) string {
	for {
		open := strings.Index(s, "<think>")
		if open < 0 {
			return strings.TrimSpace(s)
		}
		end := strings.Index(s[open:], "</think>")
		if end < 0 {
			return strings.TrimSpace(s[:open])
		}
		s = s[:open] + s[open+end+len("</think>"):]
	}
}

// askBot gets the bot's reply to one message from Ollama. ok is false when no
// usable answer came back.
func askBot(bot aiBot, fromUser, text string) (reply string, ok bool) {
	payload, _ := json.Marshal(map[string]any{
		"model":  bot.Model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "system", "content": bot.Persona},
			{"role": "user", "content": fromUser + " says: " + text},
		},
	})
	client := &http.Client{Timeout: botReplyTimeout}
	resp, err := client.Post(ollamaURL+"/api/chat", "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("bot %s (%s): %v", bot.Name, bot.Model, err)
		return "", false
	}
	defer resp.Body.Close()
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		log.Printf("bot %s (%s): unreadable reply: %v", bot.Name, bot.Model, err)
		return "", false
	}
	if reply = stripThinking(out.Message.Content); reply == "" {
		log.Printf("bot %s (%s): no reply (HTTP %d %s)", bot.Name, bot.Model, resp.StatusCode, out.Error)
		return "", false
	}
	return reply, true
}

// triggerAIMentions starts a reply from every switched-on bot the text
// mentions. A bot that cannot answer says so in the room rather than staying
// silent, so the failure is visible.
func triggerAIMentions(room, fromUser, text string) {
	for _, bot := range aiBots {
		mu.Lock()
		off := botOff[bot.Name]
		mu.Unlock()
		if off || !mentions(text, bot.Mention) {
			continue
		}
		go func(bot aiBot) {
			reply, ok := askBot(bot, fromUser, text)
			if !ok {
				reply = "[" + bot.Name + " could not reply: model " + bot.Model + " did not answer]"
			}
			mu.Lock()
			defer mu.Unlock()
			if _, err := appendMsgLocked(Msg{
				Room: room,
				From: bot.Name,
				Role: "bot",
				Text: reply,
				At:   time.Now().UTC().Format(time.RFC3339),
			}); err != nil {
				log.Printf("bot %s: store reply: %v", bot.Name, err)
			}
		}(bot)
	}
}
