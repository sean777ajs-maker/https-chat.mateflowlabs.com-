//go:build js && wasm

package main

import (
	"sort"
	"strconv"
	"strings"
	"syscall/js"
)

var emojis = []string{
	"\U0001F525", "\U0001F680", "\U0001F4B0", "\U0001F389", "\U0001F44D", "\u2764\uFE0F", "\U0001F602",
	"\U0001F60E", "\U0001F4AF", "\u26A1", "\U0001F3AC", "\U0001F3A4", "\U0001F6D2", "\U0001F3F7\uFE0F",
	"\U0001F4E6", "\u2705", "\U0001F440", "\U0001F64C", "\U0001F91D", "\U0001F48E", "\U0001F3C6",
}

var (
	inventory []js.Value
	sock      js.Value // the current live socket; events from any other are ignored
	resyncDue bool     // the socket dropped, so the next one to open reloads state
	noop      = js.FuncOf(func(js.Value, []js.Value) any { return nil })

	wsOpened, wsClosed, wsMessage js.Func
)

func canHost() bool {
	return meRole == "admin" || str(get(roomState, "owner")) == meName
}

// applyRoomState redraws everything that depends on the room: title, audio
// policy, host controls, who is online, the shared video and the giveaway.
func applyRoomState(rs js.Value) {
	if rs.Type() != js.TypeObject {
		return
	}
	roomState = rs
	title := str(get(rs, "title"))
	if title == "" {
		title = str(get(rs, "id"))
	}
	setText(byID("activeRoomTitle"), title)
	setText(byID("playerRoomLabel"), title)
	setText(byID("followerBadge"), strconv.Itoa(int(num(get(rs, "follower_count"))))+" followers")

	live := str(get(rs, "audio_mode")) == "live"
	badge := byID("audioPolicyBadge")
	if live {
		setText(badge, "AUDIO: LIVE MIC")
		badge.Set("className", "badge font-mono bg-success")
	} else {
		setText(badge, "AUDIO: STT ONLY")
		badge.Set("className", "badge font-mono bg-dark border border-info-subtle")
	}

	follow := byID("btnFollowRoom")
	empty(follow)
	if get(rs, "followed_by_me").Truthy() {
		add(follow, icon("bi-heart-fill text-danger me-1"), doc.Call("createTextNode", "Followed (Entered)"))
	} else {
		add(follow, icon("bi-heart me-1"), doc.Call("createTextNode", "Follow (1 Entry)"))
	}

	host := canHost()
	audioToggle := byID("btnHostAudioToggle")
	setHidden(audioToggle, !host)
	if live {
		setText(audioToggle.Call("querySelector", "span"), "Switch to STT-Only")
	} else {
		setText(audioToggle.Call("querySelector", "span"), "Enable Live Audio")
	}
	setHidden(byID("hostVideoControls"), !host)
	setHidden(byID("hostGiveawayControls"), !host)
	setHidden(byID("addInvBox"), !(host || meRole == "seller"))
	setHidden(byID("hostStoreBox"), !host)

	storeURL := str(get(rs, "store_url"))
	if in := byID("storeUrlInput"); !doc.Get("activeElement").Equal(in) {
		in.Set("value", storeURL)
	}
	storeBtn := byID("roomStoreBtn")
	if isHTTP(storeURL) {
		storeBtn.Set("href", storeURL)
	}
	setHidden(storeBtn, !isHTTP(storeURL))

	online := byID("onlineList")
	empty(online)
	each(get(rs, "online"), func(u js.Value) {
		add(online, mk("span", "badge bg-dark border border-info-subtle font-mono", str(u)))
	})

	syncVideo(get(rs, "video"))
	renderGiveaway(get(rs, "giveaway"))
}

// syncVideo follows the host's shared video: same source, same position
// (allowing for time passed since the host's last update), same play state.
func syncVideo(v js.Value) {
	src := str(get(v, "url"))
	if src == "" {
		return
	}
	video := byID("syncVideo")
	if dataOf(video, "src") != src {
		video.Get("dataset").Set("src", src)
		video.Set("src", src)
		byID("videoUrlInput").Set("value", src)
	}
	playing := get(v, "playing").Truthy()
	target := num(get(v, "position"))
	if playing {
		target += (num(win.Get("Date").Call("now")) - num(get(v, "updated_at"))) / 1000
	}
	if target < 0 {
		target = 0
	}
	if off := num(video.Get("currentTime")) - target; off > 1.8 || off < -1.8 {
		video.Set("currentTime", target)
	}
	paused := video.Get("paused").Truthy()
	if playing && paused {
		// The browser may refuse to autoplay; that is not an error worth reporting.
		if started := video.Call("play"); get(started, "catch").Type() == js.TypeFunction {
			started.Call("catch", noop)
		}
	}
	if !playing && !paused {
		video.Call("pause")
	}
}

func renderGiveaway(gw js.Value) {
	title := str(get(gw, "title"))
	var entries []string
	each(get(gw, "entries"), func(e js.Value) { entries = append(entries, str(e)) })
	count := strconv.Itoa(len(entries)) + " entries"

	banner := byID("giveawayBanner")
	active := get(gw, "active").Truthy()
	classes(banner).Call("toggle", "d-none", !active)
	classes(banner).Call("toggle", "d-flex", active)
	if active {
		setText(byID("giveawayBannerText"), "GIVEAWAY: "+title+" \u00B7 Follow seller for 1 entry!")
		setText(byID("giveawayEntryCount"), count)
		setText(byID("giveawayStatusText"), "Active: "+title+" ("+count+")")
	} else {
		setText(byID("giveawayStatusText"), "No active giveaway right now.")
	}

	winner := str(get(gw, "winner"))
	winnerBox := byID("giveawayWinnerBox")
	setHidden(winnerBox, winner == "")
	if winner != "" {
		setText(winnerBox, "Winner: "+winner+"!")
	}
	list := ""
	if len(entries) > 0 {
		list = "Entries: " + strings.Join(entries, ", ")
	}
	setText(byID("giveawayEntriesList"), list)
}

func loadRooms() error {
	list, err := api("GET", "/api/rooms", nil)
	if err != nil {
		return err
	}
	var rooms []js.Value
	each(list, func(r js.Value) { rooms = append(rooms, r) })
	sort.Slice(rooms, func(i, j int) bool { return str(get(rooms[i], "id")) < str(get(rooms[j], "id")) })

	box := byID("roomList")
	empty(box)
	for _, r := range rooms {
		id := str(get(r, "id"))
		title := str(get(r, "title"))
		if title == "" {
			title = id
		}
		class := "channel-item"
		if id == activeRoom {
			class += " active"
		}
		online := 0
		each(get(r, "online"), func(js.Value) { online++ })
		btn := mk("button", class, "")
		btn.Set("type", "button")
		btn.Get("dataset").Set("room", id)
		add(box, add(btn, mk("span", "", title), mk("span", "badge bg-dark font-mono", strconv.Itoa(online))))
	}
	return nil
}

func loadInventory() error {
	items, err := api("GET", roomURL("/api/inventory"), nil)
	if err != nil {
		return err
	}
	inventory = inventory[:0]
	each(items, func(it js.Value) { inventory = append(inventory, it) })

	box := byID("inventoryList")
	empty(box)
	if len(inventory) == 0 {
		add(box, mk("div", "small text-secondary", "No items in this room yet."))
		return nil
	}
	for i, it := range inventory {
		head := add(mk("div", "d-flex justify-content-between align-items-start", ""),
			mk("div", "fw-semibold small text-light", str(get(it, "title"))),
			mk("span", "badge bg-success font-mono", "$"+money(get(it, "price"))))
		condition := str(get(it, "condition"))
		if condition == "" {
			condition = "NEW"
		}
		meta := mk("div", "text-secondary font-mono mt-1", "Qty: "+str(get(it, "quantity"))+" \u00B7 "+condition)
		setStyle(meta, "fontSize", "0.72rem")
		pin := mk("button", "btn btn-xs btn-outline-info flex-grow-1 py-0 pin-btn", "Pin to Chat")
		pin.Set("type", "button")
		pin.Get("dataset").Set("idx", strconv.Itoa(i))
		row := add(mk("div", "d-flex gap-1 mt-2", ""), pin)
		if u := str(get(it, "url")); isHTTP(u) {
			add(row, link(mk("a", "btn btn-xs btn-success py-0 px-2", "Buy / Link"), u))
		}
		add(box, add(mk("div", "p-2 rounded glass", ""), head, meta, row))
	}
	return nil
}

// renderBots draws the bot panel: a status dot, the name, active or inactive,
// an on/off switch, and how many are switched on.
func renderBots(list js.Value) {
	box := byID("botList")
	empty(box)
	total, enabled := 0, 0
	each(list, func(b js.Value) {
		total++
		isOn, active := get(b, "enabled").Truthy(), get(b, "active").Truthy()
		if isOn {
			enabled++
		}
		row := mk("div", "bot-row", "")
		dot := mk("span", "bot-dot", "")
		state := mk("span", "text-secondary font-mono ms-auto", "inactive")
		setStyle(state, "fontSize", "0.68rem")
		if active {
			row.Set("title", "Active \u00B7 "+str(get(b, "model")))
			classes(dot).Call("add", "on")
			setText(state, "active")
		} else {
			row.Set("title", "Inactive \u00B7 "+str(get(b, "reason")))
		}
		toggle := mk("input", "form-check-input", "")
		toggle.Set("type", "checkbox")
		toggle.Set("checked", isOn)
		toggle.Get("dataset").Set("bot", str(get(b, "name")))
		switch {
		case meRole != "admin":
			toggle.Set("disabled", true)
			toggle.Set("title", "Only admins can switch bots")
		case isOn:
			toggle.Set("title", "Switch off")
		default:
			toggle.Set("title", "Switch on")
		}
		add(box, add(row, dot, mk("span", "fw-semibold", str(get(b, "mention"))), state,
			add(mk("span", "form-check form-switch m-0 p-0 d-flex", ""), toggle)))
	})
	setText(byID("botCount"), strconv.Itoa(enabled)+"/"+strconv.Itoa(total))
}

func loadBots() {
	if list, err := api("GET", "/api/bots", nil); err == nil {
		renderBots(list)
	}
}

// connectWS opens the room's live socket. One set of handlers serves every
// socket and checks which socket fired, so closing the old socket on a room
// switch can never trigger a reconnect on top of the new one.
func connectWS() {
	if sock.Truthy() {
		sock.Call("close")
	}
	loc := win.Get("location")
	scheme := "ws://"
	if str(loc.Get("protocol")) == "https:" {
		scheme = "wss://"
	}
	sock = win.Get("WebSocket").New(scheme + str(loc.Get("host")) + "/ws?room=" + enc(activeRoom) + "&token=" + enc(apiKey))
	sock.Call("addEventListener", "open", wsOpened)
	sock.Call("addEventListener", "close", wsClosed)
	sock.Call("addEventListener", "message", wsMessage)
}

func isCurrentSocket(ev js.Value) bool {
	return sock.Truthy() && get(ev, "target").Equal(sock)
}

func onSocketMessage(raw string) {
	msg, ok := parseJSON(raw)
	if !ok {
		return
	}
	payload := get(msg, "data")
	switch str(get(msg, "type")) {
	case "chat":
		addMessage(payload)
		scrollToEnd()
	case "msg_edit":
		applyUpdate(payload)
	case "msg_delete":
		markDeleted(msgID(payload), msgRev(payload))
	case "bots":
		renderBots(payload)
	case "room_state":
		applyRoomState(payload)
	case "rtc_signal":
		from := str(get(msg, "from"))
		spawn(func() { handleRTCSignal(from, payload) })
	}
}

// resync runs once the socket is back after a drop. Events were missed, so
// the server's copy replaces what is on screen.
func resync() {
	room := activeRoom
	list, err := api("GET", "/api/messages?room="+enc(room)+"&since=0", nil)
	if err != nil || room != activeRoom {
		return
	}
	added := false
	each(list, func(m js.Value) {
		if c, ok := cards[msgID(m)]; ok {
			c.replaceWith(m)
			return
		}
		addMessage(m)
		added = true
	})
	if added {
		scrollToEnd()
	}
	if state, err := api("GET", "/api/rooms/state?room="+enc(room), nil); err == nil && room == activeRoom {
		applyRoomState(state)
	}
	loadBots()
}

// switchRoom loads a room and connects its live socket.
func switchRoom(id string) {
	switchSeq++
	seq := switchSeq
	stale := func() bool { return seq != switchSeq }
	activeRoom = id
	cards = map[int]*card{}
	empty(stream)
	stopMic()
	if loadRooms() != nil || stale() {
		return
	}
	list, err := api("GET", "/api/messages?room="+enc(id)+"&since=0", nil)
	if err != nil || stale() {
		return
	}
	each(list, addMessage)
	scrollToEnd()
	state, err := api("GET", "/api/rooms/state?room="+enc(id), nil)
	if err != nil || stale() {
		return
	}
	applyRoomState(state)
	if loadInventory() != nil || stale() {
		return
	}
	connectWS()
}

// uploadFile sends an image through the server's scan gateway and posts it.
func uploadFile(file js.Value) {
	if !file.Truthy() {
		return
	}
	status("SCANNING VIA CLAMAV GATEWAY...")
	form := win.Get("FormData").New()
	form.Call("append", "file", file)
	res, err := api("POST", "/api/upload", form)
	if err == nil {
		status("CLEAN (" + str(get(res, "scan")) + ") // POSTING MEDIA")
		_, err = api("POST", "/api/messages", map[string]any{
			"room": activeRoom, "text": "", "media_url": str(get(res, "url")), "media_type": str(get(res, "mime")),
		})
	}
	if err != nil {
		status("UPLOAD BLOCKED: " + err.Error())
		return
	}
	status("READY")
}

func saveStore() {
	note, in := byID("storeSaveStatus"), byID("storeUrlInput")
	storeURL := field("storeUrlInput")
	if storeURL != "" && !isHTTP(storeURL) {
		storeURL = "https://" + storeURL
		in.Set("value", storeURL)
	}
	_, err := api("POST", roomURL("/api/rooms/store"), map[string]any{"store_name": "", "store_url": storeURL})
	if err != nil {
		setText(note, "Save failed: "+err.Error())
		note.Set("className", "small mt-1 text-danger")
		return
	}
	setText(note, "Saved!")
	note.Set("className", "small mt-1 text-success")
	after(2500, func() { setHidden(note, true) })
}

func addInventoryItem() {
	title := field("invTitle")
	if title == "" {
		return
	}
	price, _ := strconv.ParseFloat(field("invPrice"), 64)
	quantity, err := strconv.Atoi(field("invQty"))
	if err != nil {
		quantity = 1
	}
	if !post(roomURL("/api/inventory"), map[string]any{
		"room": activeRoom, "title": title, "price": price, "quantity": quantity, "condition": "NEW", "url": field("invUrl"),
	}) {
		return
	}
	byID("invTitle").Set("value", "")
	byID("invUrl").Set("value", "")
	loadInventory()
}

// wirePlayerDrag lets the floating player be dragged by its header. The move
// and release handlers are attached only while a drag is in progress.
func wirePlayerDrag(box, header js.Value) {
	var dx, dy float64
	var move, release js.Func
	px := func(v float64) string {
		if v < 0 {
			v = 0
		}
		return strconv.FormatFloat(v, 'f', -1, 64) + "px"
	}
	move = handler("drag", func(ev js.Value) {
		setStyle(box, "left", px(num(ev.Get("clientX"))-dx))
		setStyle(box, "top", px(num(ev.Get("clientY"))-dy))
		setStyle(box, "right", "auto")
	})
	release = handler("drag end", func(js.Value) {
		win.Call("removeEventListener", "mousemove", move)
		win.Call("removeEventListener", "mouseup", release)
	})
	on(header, "mousedown", func(ev js.Value) {
		if closest(ev, "button").Truthy() {
			return
		}
		dx = num(ev.Get("clientX")) - num(box.Get("offsetLeft"))
		dy = num(ev.Get("clientY")) - num(box.Get("offsetTop"))
		win.Call("addEventListener", "mousemove", move)
		win.Call("addEventListener", "mouseup", release)
	})
}

// wireRoom registers every handler outside the message list, once. Lists that
// are redrawn (rooms, inventory, bots) are handled by one listener on their
// container instead of one per item.
func wireRoom() {
	connStatus := byID("connStatus")
	wsOpened = handler("socket open", func(ev js.Value) {
		if !isCurrentSocket(ev) {
			return
		}
		setText(connStatus, "WS // ONLINE")
		if resyncDue {
			resyncDue = false
			spawn(resync)
		}
	})
	wsClosed = handler("socket close", func(ev js.Value) {
		if !isCurrentSocket(ev) {
			return
		}
		setText(connStatus, "WS // RECONNECTING")
		dropped := sock
		after(2500, func() {
			if sock.Equal(dropped) { // nothing has replaced it meanwhile
				resyncDue = true
				connectWS()
			}
		})
	})
	wsMessage = handler("socket message", func(ev js.Value) {
		if isCurrentSocket(ev) {
			onSocketMessage(str(ev.Get("data")))
		}
	})

	// Emoji picker
	picker := byID("emojiPopover")
	for _, e := range emojis {
		b := mk("button", "emoji-btn", e)
		b.Set("type", "button")
		add(picker, b)
	}
	on(picker, "click", func(ev js.Value) {
		b := closest(ev, ".emoji-btn")
		if !b.Truthy() {
			return
		}
		in := byID("messageInput")
		in.Set("value", str(in.Get("value"))+str(b.Get("textContent")))
		in.Call("focus")
		setHidden(picker, true)
	})
	on(byID("btnEmoji"), "click", func(js.Value) { classes(picker).Call("toggle", "d-none") })

	// Floating player
	player := byID("floatingPlayer")
	on(byID("btnMinPlayer"), "click", func(js.Value) { classes(player).Call("toggle", "minimized") })
	on(byID("btnClosePlayer"), "click", func(js.Value) { setHidden(player, true) })
	on(byID("btnTogglePlayer"), "click", func(js.Value) { classes(player).Call("toggle", "d-none") })
	wirePlayerDrag(player, byID("floatingHeader"))

	// Right drawer and its two tabs
	toggleDrawer := func(js.Value) { classes(byID("rightDrawer")).Call("toggle", "collapsed") }
	on(byID("btnToggleDrawer"), "click", toggleDrawer)
	on(byID("btnDrawerHeaderToggle"), "click", toggleDrawer)
	showTab := func(name string) {
		classes(byID("tabInvBtn")).Call("toggle", "active", name == "inv")
		classes(byID("tabGiveBtn")).Call("toggle", "active", name == "give")
		setHidden(byID("tabInv"), name != "inv")
		setHidden(byID("tabGive"), name != "give")
	}
	on(byID("tabInvBtn"), "click", func(js.Value) { showTab("inv") })
	on(byID("tabGiveBtn"), "click", func(js.Value) { showTab("give") })

	// Redrawn lists
	on(byID("roomList"), "click", func(ev js.Value) {
		if b := closest(ev, "[data-room]"); b.Truthy() {
			id := dataOf(b, "room")
			spawn(func() { switchRoom(id) })
		}
	})
	on(byID("inventoryList"), "click", func(ev js.Value) {
		b := closest(ev, ".pin-btn")
		if !b.Truthy() {
			return
		}
		i, err := strconv.Atoi(dataOf(b, "idx"))
		if err != nil || i < 0 || i >= len(inventory) {
			return
		}
		item := inventory[i]
		spawn(func() {
			post("/api/messages", map[string]any{
				"room": activeRoom, "text": "Pinned item: " + str(get(item, "title")), "pinned_item": item,
			})
		})
	})
	on(byID("botList"), "change", func(ev js.Value) {
		toggle := get(ev, "target")
		name := dataOf(toggle, "bot")
		if name == "" {
			return
		}
		want := toggle.Get("checked").Truthy()
		toggle.Set("disabled", true)
		spawn(func() {
			list, err := api("POST", "/api/bots", map[string]any{"name": name, "enabled": want})
			if err != nil {
				win.Call("alert", "Bot switch failed: "+err.Error())
				loadBots()
				return
			}
			renderBots(list)
		})
	})

	// Sending: text, image upload, pasted image
	on(byID("messageForm"), "submit", func(ev js.Value) {
		ev.Call("preventDefault")
		in := byID("messageInput")
		text := strings.TrimSpace(str(in.Get("value")))
		if text == "" {
			return
		}
		in.Set("value", "")
		room := activeRoom
		spawn(func() {
			if _, err := api("POST", "/api/messages", map[string]any{"room": room, "text": text}); err != nil {
				status("SEND FAILED: " + err.Error())
				if str(in.Get("value")) == "" {
					in.Set("value", text) // give the unsent text back
				}
			}
		})
	})
	fileInput := byID("fileInput")
	on(byID("btnUpload"), "click", func(js.Value) { fileInput.Call("click") })
	on(fileInput, "change", func(js.Value) {
		file := get(fileInput, "files", "0")
		fileInput.Set("value", "") // so the same file can be picked again
		spawn(func() { uploadFile(file) })
	})
	on(byID("messageInput"), "paste", func(ev js.Value) {
		items := get(ev, "clipboardData", "items")
		if items.Type() != js.TypeObject {
			return
		}
		for i, n := 0, items.Length(); i < n; i++ {
			if it := items.Index(i); strings.HasPrefix(str(get(it, "type")), "image/") {
				file := it.Call("getAsFile") // only readable during the paste event
				spawn(func() { uploadFile(file) })
				return
			}
		}
	})

	// Room actions
	act("btnHostAudioToggle", func() {
		next := "live"
		if str(get(roomState, "audio_mode")) == "live" {
			next = "stt_only"
		}
		post(roomURL("/api/rooms/audio"), map[string]any{"audio_mode": next})
	})
	follow := func() { post(roomURL("/api/rooms/follow"), nil) }
	act("btnFollowRoom", follow)
	act("btnDrawerFollow", follow)
	act("btnStartGiveaway", func() {
		post(roomURL("/api/rooms/giveaway"), map[string]any{"action": "start", "title": field("giveawayTitleInput")})
	})
	act("btnDrawGiveaway", func() { post(roomURL("/api/rooms/giveaway"), map[string]any{"action": "draw"}) })
	video := byID("syncVideo")
	act("btnLoadVideo", func() {
		post(roomURL("/api/rooms/video"), map[string]any{"action": "load", "url": field("videoUrlInput")})
	})
	act("btnHostPlay", func() {
		post(roomURL("/api/rooms/video"), map[string]any{
			"action": "play", "url": field("videoUrlInput"), "position": num(video.Get("currentTime")),
		})
	})
	act("btnHostPause", func() {
		post(roomURL("/api/rooms/video"), map[string]any{"action": "pause", "position": num(video.Get("currentTime"))})
	})
	act("btnRefreshInv", func() { loadInventory() })
	act("btnSaveStoreUrl", saveStore)
	act("btnAddInv", addInventoryItem)
	act("btnMySellerRoom", func() {
		id := "seller:" + meName
		if post("/api/rooms", map[string]any{"id": id, "title": "Seller // " + meName}) {
			switchRoom(id)
		}
	})
}
