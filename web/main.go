//go:build js && wasm

// Command web is the MateFlow chat page. It is compiled to WebAssembly and
// drives the markup in index.html through syscall/js, so the page logic is Go:
//
//	GOOS=js GOARCH=wasm go build -o main.wasm ./web
package main

import (
	"errors"
	"strconv"
	"strings"
	"syscall/js"
)

const storageKey = "mfchat_key"

var (
	apiKey     string
	meName     = "guest"
	meRole     = "seller"
	activeRoom = "main"
	roomState  = js.Null()
	switchSeq  int // bumped on every room switch so a slower, older one stops

	authBackdrop js.Value
)

func main() {
	try("theme", initTheme)
	classes(doc.Get("documentElement")).Call("remove", "mf-loading")
	try("messages", wireStream)
	try("room", wireRoom)
	try("audio", wireAudio)
	try("auth", wireAuth)
	every(30000, func() {
		if apiKey != "" {
			spawn(loadBots)
		}
	})
	if apiKey = storageGet(storageKey); apiKey == "" {
		showAuth()
	} else {
		spawn(boot)
	}
	select {} // stay alive for the event handlers
}

// api makes an authenticated request and returns the decoded JSON reply (null
// for an empty one). body is nil, a FormData value, or a map to send as JSON.
// It waits on the network, so it is only called from spawned work.
func api(method, url string, body any) (js.Value, error) {
	headers := map[string]any{"Authorization": "Bearer " + apiKey}
	opts := map[string]any{"method": method, "headers": headers}
	switch b := body.(type) {
	case nil:
	case js.Value:
		opts["body"] = b
	default:
		headers["Content-Type"] = "application/json"
		opts["body"] = toJSON(b)
	}
	res, err := await(win.Call("fetch", url, opts))
	if err != nil {
		return js.Null(), err
	}
	code := int(num(res.Get("status")))
	if code == 401 {
		storageDel(storageKey)
		setHidden(byID("authError"), false)
		showAuth()
		return js.Null(), errors.New("unauthorized")
	}
	if !res.Get("ok").Truthy() {
		msg := "HTTP " + strconv.Itoa(code)
		if text, err := await(res.Call("text")); err == nil && strings.TrimSpace(str(text)) != "" {
			msg = strings.TrimSpace(str(text))
		}
		return js.Null(), errors.New(msg)
	}
	if code == 204 {
		return js.Null(), nil
	}
	return await(res.Call("json"))
}

// post sends an action whose reply is not needed. A failure shows in the
// status line under the message box.
func post(url string, body any) bool {
	if _, err := api("POST", url, body); err != nil {
		status("FAILED: " + err.Error())
		return false
	}
	return true
}

func roomURL(path string) string { return path + "?room=" + enc(activeRoom) }

func roleClass(role string) string {
	if role == "admin" {
		return "badge-role-admin"
	}
	return "badge-role-seller"
}

// boot signs in with the stored key and opens the current room.
func boot() {
	me, err := api("GET", "/api/me", nil)
	if err != nil {
		if err.Error() != "unauthorized" {
			setText(byID("connStatus"), "OFFLINE")
			status("CANNOT REACH SERVER: " + err.Error())
		}
		return
	}
	meName, meRole = str(get(me, "name")), str(get(me, "role"))
	setText(byID("selfName"), meName)
	badge := byID("selfRoleBadge")
	setText(badge, meRole)
	badge.Set("className", "badge font-mono "+roleClass(meRole))
	spawn(loadBots)
	switchRoom(activeRoom)
}

// The access-key dialog uses Bootstrap's modal styles, shown and hidden here.
func showAuth() {
	modal := byID("authModal")
	setStyle(modal, "display", "block")
	classes(modal).Call("add", "show")
	modal.Call("setAttribute", "role", "dialog")
	modal.Call("setAttribute", "aria-modal", "true")
	if !authBackdrop.Truthy() {
		authBackdrop = mk("div", "modal-backdrop fade show", "")
		add(doc.Get("body"), authBackdrop)
	}
	classes(doc.Get("body")).Call("add", "modal-open")
	byID("inputKey").Call("focus")
}

func hideAuth() {
	modal := byID("authModal")
	classes(modal).Call("remove", "show")
	setStyle(modal, "display", "none")
	modal.Call("removeAttribute", "role")
	modal.Call("removeAttribute", "aria-modal")
	if authBackdrop.Truthy() {
		authBackdrop.Call("remove")
		authBackdrop = js.Undefined()
	}
	classes(doc.Get("body")).Call("remove", "modal-open")
}

func wireAuth() {
	on(byID("btnSettings"), "click", func(js.Value) {
		byID("inputKey").Set("value", apiKey)
		showAuth()
	})
	on(byID("authForm"), "submit", func(ev js.Value) {
		ev.Call("preventDefault")
		apiKey = field("inputKey")
		storageSet(storageKey, apiKey)
		setHidden(byID("authError"), true)
		hideAuth()
		spawn(boot)
	})
	on(byID("authModal"), "keydown", func(ev js.Value) {
		if str(ev.Get("key")) == "Escape" {
			hideAuth()
		}
	})
}
