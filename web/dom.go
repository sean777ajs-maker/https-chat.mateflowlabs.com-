//go:build js && wasm

package main

import (
	"errors"
	"strconv"
	"strings"
	"syscall/js"
)

var (
	win     = js.Global()
	doc     = win.Get("document")
	console = win.Get("console")
)

func byID(id string) js.Value { return doc.Call("getElementById", id) }

// mk creates an element. Text only ever goes in through textContent, never
// innerHTML, so nothing a user typed can turn into markup.
func mk(tag, class, text string) js.Value {
	e := doc.Call("createElement", tag)
	if class != "" {
		e.Set("className", class)
	}
	if text != "" {
		e.Set("textContent", text)
	}
	return e
}

func add(parent js.Value, children ...js.Value) js.Value {
	for _, c := range children {
		parent.Call("appendChild", c)
	}
	return parent
}

func icon(name string) js.Value               { return mk("i", "bi "+name, "") }
func setText(e js.Value, s string)            { e.Set("textContent", s) }
func empty(e js.Value)                        { e.Set("textContent", "") }
func classes(e js.Value) js.Value             { return e.Get("classList") }
func setHidden(e js.Value, hidden bool)       { classes(e).Call("toggle", "d-none", hidden) }
func setStyle(e js.Value, prop, value string) { e.Get("style").Set(prop, value) }
func dataOf(e js.Value, key string) string    { return str(get(e, "dataset", key)) }
func field(id string) string                  { return strings.TrimSpace(str(byID(id).Get("value"))) }
func status(text string)                      { setText(byID("telemetryLeft"), text) }
func link(e js.Value, href string) js.Value {
	e.Set("href", href)
	e.Set("target", "_blank")
	e.Set("rel", "noopener noreferrer")
	return e
}

func first(args []js.Value) js.Value {
	if len(args) == 0 {
		return js.Undefined()
	}
	return args[0]
}

// handler wraps fn for the browser to call. It runs inside event dispatch, so
// it must never wait on the network: the reply can only arrive after the
// handler has returned, and waiting would freeze the page. Slow work goes
// through spawn.
func handler(name string, fn func(ev js.Value)) js.Func {
	return js.FuncOf(func(_ js.Value, args []js.Value) any {
		defer guard(name)
		fn(first(args))
		return nil
	})
}

func on(target js.Value, event string, fn func(ev js.Value)) {
	target.Call("addEventListener", event, handler(event, fn))
}

// act runs fn in the background when the element is clicked.
func act(id string, fn func()) {
	on(byID(id), "click", func(js.Value) { spawn(fn) })
}

// closest finds the nearest ancestor of the event target matching selector.
func closest(ev js.Value, selector string) js.Value {
	target := get(ev, "target")
	if get(target, "closest").Type() != js.TypeFunction {
		return js.Null()
	}
	return target.Call("closest", selector)
}

// guard stops a failure in one handler from ending the whole program: an
// unrecovered panic exits the Go runtime and every handler dies with it.
func guard(where string) {
	r := recover()
	if r == nil {
		return
	}
	msg := "unexpected failure"
	switch v := r.(type) {
	case error:
		msg = v.Error()
	case string:
		msg = v
	}
	console.Call("error", "mfchat: "+where+": "+msg)
}

func try(where string, fn func()) {
	defer guard(where)
	fn()
}

func spawn(fn func()) { go try("background task", fn) }

// await blocks the calling goroutine until the promise settles.
func await(promise js.Value) (js.Value, error) {
	if get(promise, "then").Type() != js.TypeFunction {
		return promise, nil
	}
	type outcome struct {
		value js.Value
		err   error
	}
	done := make(chan outcome, 1)
	resolved := js.FuncOf(func(_ js.Value, args []js.Value) any {
		done <- outcome{first(args), nil}
		return nil
	})
	rejected := js.FuncOf(func(_ js.Value, args []js.Value) any {
		done <- outcome{js.Undefined(), errors.New(errText(first(args)))}
		return nil
	})
	defer resolved.Release()
	defer rejected.Release()
	promise.Call("then", resolved, rejected)
	out := <-done
	return out.value, out.err
}

func errText(v js.Value) string {
	if s := str(v); s != "" {
		return s
	}
	if s := str(get(v, "message")); s != "" {
		return s
	}
	return "request failed"
}

func after(ms int, fn func()) {
	var cb js.Func
	cb = js.FuncOf(func(js.Value, []js.Value) any {
		defer guard("timer")
		cb.Release()
		fn()
		return nil
	})
	win.Call("setTimeout", cb, ms)
}

func every(ms int, fn func()) {
	win.Call("setInterval", handler("interval", func(js.Value) { fn() }), ms)
}

// get walks object properties and yields undefined when a step is missing.
// js.Value.Get itself panics on null, undefined and primitive values.
func get(v js.Value, path ...string) js.Value {
	for _, key := range path {
		if t := v.Type(); t != js.TypeObject && t != js.TypeFunction {
			return js.Undefined()
		}
		v = v.Get(key)
	}
	return v
}

// str reads a string (or number) and yields "" for anything else, where
// js.Value.String would give text like "<undefined>".
func str(v js.Value) string {
	switch v.Type() {
	case js.TypeString:
		return v.String()
	case js.TypeNumber:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	}
	return ""
}

func num(v js.Value) float64 {
	if v.Type() == js.TypeNumber {
		return v.Float()
	}
	return 0
}

func each(list js.Value, fn func(item js.Value)) {
	if list.Type() != js.TypeObject || !win.Get("Array").Call("isArray", list).Bool() {
		return
	}
	for i, n := 0, list.Length(); i < n; i++ {
		fn(list.Index(i))
	}
}

func parseJSON(s string) (v js.Value, ok bool) {
	defer func() {
		if recover() != nil {
			v, ok = js.Undefined(), false
		}
	}()
	return win.Get("JSON").Call("parse", s), true
}

// toJSON encodes maps, slices and plain values (map[string]any, []any).
func toJSON(v any) string {
	return win.Get("JSON").Call("stringify", js.ValueOf(v)).String()
}

// Storage can be blocked by the browser, in which case the page still works
// but forgets the key and theme.
func storageGet(key string) (s string) {
	defer func() {
		if recover() != nil {
			s = ""
		}
	}()
	return str(win.Get("localStorage").Call("getItem", key))
}

func storageSet(key, value string) {
	try("storage", func() { win.Get("localStorage").Call("setItem", key, value) })
}

func storageDel(key string) {
	try("storage", func() { win.Get("localStorage").Call("removeItem", key) })
}

func enc(s string) string { return win.Call("encodeURIComponent", s).String() }

func isHTTP(u string) bool {
	u = strings.ToLower(u)
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// safeMediaURL accepts only same-origin /media/ paths, the only place uploads
// are served from, so a message can never make the page load anything else.
func safeMediaURL(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	var u js.Value
	try("media url", func() { u = win.Get("URL").New(raw, win.Get("location").Get("href")) })
	path := str(get(u, "pathname"))
	if str(get(u, "origin")) != str(win.Get("location").Get("origin")) || !strings.HasPrefix(path, "/media/") {
		return "", false
	}
	return path + str(get(u, "search")), true
}

func money(v js.Value) string { return strconv.FormatFloat(num(v), 'f', 2, 64) }

// clock shows a timestamp as local 24-hour time.
func clock(at string) string {
	if at == "" {
		return ""
	}
	d := win.Get("Date").New(at)
	if d.Call("getTime").IsNaN() {
		return at
	}
	return d.Call("toLocaleTimeString", []any{}, map[string]any{"hour12": false}).String()
}
