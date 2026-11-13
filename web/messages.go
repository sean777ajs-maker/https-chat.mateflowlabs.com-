//go:build js && wasm

package main

import (
	"strconv"
	"strings"
	"syscall/js"
	"unicode/utf16"
)

const deletedText = "This message was deleted"

// card is one message on screen plus the state of its inline edit box.
type card struct {
	el        js.Value
	msg       js.Value
	editing   bool
	saving    bool
	savedBody js.Value // the .msg-body node the edit box replaced
	pending   js.Value // newest update that arrived while editing or saving
}

var (
	stream js.Value
	cards  = map[int]*card{}
)

func msgID(m js.Value) int      { return int(num(get(m, "id"))) }
func msgRev(m js.Value) int     { return int(num(get(m, "rev"))) } // absent on unedited messages: 0
func isDeleted(m js.Value) bool { return get(m, "deleted").Truthy() }

func canModify(m js.Value) bool {
	return msgID(m) != 0 && !isDeleted(m) && (str(get(m, "from")) == meName || meRole == "admin")
}

func buildCard(m js.Value) js.Value {
	from, text := str(get(m, "from")), str(get(m, "text"))
	class := "msg-card"
	if from == meName {
		class += " self"
	}
	if isDeleted(m) {
		class += " deleted"
	}
	c := mk("div", class, "")
	c.Get("dataset").Set("id", strconv.Itoa(msgID(m)))

	head := mk("div", "d-flex align-items-center gap-2 mb-1", "")
	add(head, mk("span", "fw-bold text-info small", from))
	if role := str(get(m, "role")); role != "" {
		badge := mk("span", "badge font-mono "+roleClass(role), role)
		setStyle(badge, "fontSize", "0.65rem")
		add(head, badge)
	}
	if get(m, "stt").Truthy() {
		badge := mk("span", "badge bg-secondary font-mono", "STT")
		setStyle(badge, "fontSize", "0.62rem")
		add(head, badge)
	}
	when := clock(str(get(m, "at")))
	if get(m, "edited").Truthy() && !isDeleted(m) {
		when += " \u00B7 edited"
	}
	stamp := mk("span", "text-secondary font-mono ms-auto", when)
	setStyle(stamp, "fontSize", "0.68rem")
	add(head, stamp)
	if canModify(m) {
		if text != "" {
			add(head, actionButton("edit", "Edit", "bi-pencil"))
		}
		add(head, actionButton("delete", "Delete", "bi-trash"))
	}
	add(c, head)

	if text != "" {
		body := mk("div", "small msg-body", text)
		setStyle(body, "whiteSpace", "pre-wrap")
		add(c, body)
	}
	if u, ok := safeMediaURL(str(get(m, "media_url"))); ok {
		img := mk("img", "msg-img d-block", "")
		img.Set("src", u)
		img.Set("alt", "")
		img.Set("loading", "lazy")
		add(c, add(link(mk("a", "open-image d-inline-block", ""), u), img))
	}
	if item := get(m, "pinned_item"); item.Type() == js.TypeObject {
		add(c, pinnedBox(item))
	}
	return c
}

// actionButton is found by the stream's single click handler through its
// data-action, so cards carry no handlers of their own.
func actionButton(action, title, iconName string) js.Value {
	b := mk("button", "msg-act", "")
	b.Set("type", "button")
	b.Set("title", title)
	b.Get("dataset").Set("action", action)
	return add(b, icon(iconName))
}

func pinnedBox(item js.Value) js.Value {
	box := mk("div", "mt-2 p-2 rounded border border-info-subtle bg-dark small", "")
	title := add(mk("div", "fw-bold text-info", ""), icon("bi-pin-angle-fill me-1"), doc.Call("createTextNode", str(get(item, "title"))))
	price := mk("div", "font-mono text-success", "$"+money(get(item, "price"))+" \u00B7 Qty: "+str(get(item, "quantity")))
	add(box, title, price)
	if u := str(get(item, "url")); isHTTP(u) {
		buy := link(mk("a", "btn btn-xs btn-success py-0 px-2 mt-1", "View / Buy "), u)
		add(box, add(buy, icon("bi-box-arrow-up-right ms-1")))
	}
	return box
}

// addMessage shows a message that is not on screen yet.
func addMessage(m js.Value) {
	id := msgID(m)
	if _, shown := cards[id]; shown && id != 0 {
		return
	}
	c := &card{el: buildCard(m), msg: m}
	if id != 0 {
		cards[id] = c
	}
	add(stream, c.el)
}

func scrollToEnd() { stream.Set("scrollTop", stream.Get("scrollHeight")) }

// render swaps in a fresh card for the message's new state and ends any edit.
func (c *card) render(m js.Value) {
	fresh := buildCard(m)
	c.el.Call("replaceWith", fresh)
	c.el, c.msg = fresh, m
	c.editing, c.savedBody, c.pending = false, js.Undefined(), js.Undefined()
}

// hold keeps an update for later instead of drawing over an open edit box or
// a save that is still in flight. A delete always goes through.
func (c *card) hold(m js.Value) bool {
	if c.saving || (c.editing && !isDeleted(m)) {
		c.pending = m
		return true
	}
	return false
}

// applyUpdate handles a live edit or delete. An event older than what is
// shown is dropped: without that, a slow event carrying old text could
// overwrite newer text and nothing would correct it.
func applyUpdate(m js.Value) {
	c, ok := cards[msgID(m)]
	if !ok || msgRev(m) < msgRev(c.msg) {
		return
	}
	if c.pending.Truthy() && msgRev(m) < msgRev(c.pending) {
		return
	}
	if !c.hold(m) {
		c.render(m)
	}
}

// replaceWith applies the server's copy without comparing revisions. It is
// used after a reconnect, when events were missed and the server is the only
// truth (its revisions may even have restarted).
func (c *card) replaceWith(m js.Value) {
	same := msgRev(m) == msgRev(c.msg) && isDeleted(m) == isDeleted(c.msg) &&
		str(get(m, "text")) == str(get(c.msg, "text"))
	if !same && !c.hold(m) {
		c.render(m)
	}
}

// markDeleted mirrors on screen what the server does to a deleted message.
// The card stays as a tombstone, so a late edit event cannot revive the text.
func markDeleted(id, rev int) {
	c, ok := cards[id]
	if !ok || isDeleted(c.msg) {
		return
	}
	m := win.Get("Object").Call("assign", win.Get("Object").New(), c.msg)
	m.Set("deleted", true)
	m.Set("text", deletedText)
	m.Set("media_url", "")
	m.Set("pinned_item", js.Null())
	if rev <= msgRev(c.msg) {
		rev = msgRev(c.msg) + 1
	}
	m.Set("rev", rev)
	applyUpdate(m)
}

func startEdit(c *card) {
	if c.editing {
		return
	}
	body := c.el.Call("querySelector", ".msg-body")
	if !body.Truthy() {
		return
	}
	text := str(get(c.msg, "text"))
	box := mk("input", "form-control form-control-sm bg-dark text-light border-secondary msg-edit", "")
	box.Set("type", "text")
	box.Set("value", text)
	box.Set("title", "Enter to save, Esc to cancel")
	c.editing, c.savedBody = true, body
	body.Call("replaceWith", box)
	box.Call("focus")
	end := len(utf16.Encode([]rune(text))) // the browser counts UTF-16 units
	box.Call("setSelectionRange", end, end)
}

// cancelEdit puts the saved body node back. Nothing else in the card is
// rebuilt, so a button under the pointer survives a click in progress.
func cancelEdit(c *card) {
	if !c.editing || c.saving {
		return
	}
	c.editing = false
	if box := c.el.Call("querySelector", ".msg-edit"); box.Truthy() && c.savedBody.Truthy() {
		box.Call("replaceWith", c.savedBody)
	}
	c.savedBody = js.Undefined()
	if held := c.pending; held.Truthy() {
		c.render(held)
	}
}

func saveEdit(c *card, box js.Value) {
	if c.saving {
		return
	}
	text := strings.TrimSpace(str(box.Get("value")))
	if text == "" || text == str(get(c.msg, "text")) {
		cancelEdit(c)
		return
	}
	c.saving = true
	box.Set("readOnly", true)
	spawn(func() {
		saved, err := api("PUT", "/api/messages", map[string]any{"id": msgID(c.msg), "content": text})
		c.saving = false
		held := c.pending
		switch {
		case err == nil:
			if held.Truthy() && msgRev(held) > msgRev(saved) {
				saved = held // changed again while this save was in flight
			}
			c.render(saved)
		case held.Truthy() && isDeleted(held):
			c.render(held)
		default:
			// The box stays open so the text is not lost and can be retried.
			box.Set("readOnly", false)
			status("EDIT FAILED: " + err.Error())
		}
	})
}

func deleteMessage(c *card) {
	id := msgID(c.msg)
	spawn(func() {
		if _, err := api("DELETE", "/api/messages?id="+strconv.Itoa(id), nil); err != nil {
			status("DELETE FAILED: " + err.Error())
			return
		}
		markDeleted(id, 0)
	})
}

func cardOf(inside js.Value) *card {
	el := inside.Call("closest", ".msg-card")
	if !el.Truthy() {
		return nil
	}
	id, _ := strconv.Atoi(dataOf(el, "id"))
	return cards[id]
}

// wireStream gives the message list one click and one key handler. Cards are
// rebuilt often, and per-card handlers would pile up for the life of the page.
func wireStream() {
	stream = byID("messageStream")
	on(stream, "click", func(ev js.Value) {
		btn := closest(ev, "[data-action]")
		if !btn.Truthy() {
			return
		}
		c := cardOf(btn)
		if c == nil {
			return
		}
		switch dataOf(btn, "action") {
		case "edit":
			startEdit(c)
		case "delete":
			if win.Call("confirm", "Delete this message?").Truthy() {
				deleteMessage(c)
			}
		}
	})
	on(stream, "keydown", func(ev js.Value) {
		box := closest(ev, ".msg-edit")
		if !box.Truthy() || get(ev, "isComposing").Truthy() {
			return
		}
		c := cardOf(box)
		if c == nil {
			return
		}
		switch str(ev.Get("key")) {
		case "Enter":
			ev.Call("preventDefault")
			saveEdit(c, box)
		case "Escape":
			cancelEdit(c)
		}
	})
}
