//go:build js && wasm

package main

import (
	"syscall/js"
)

const schemeKey = "mf_chat_scheme"

// initTheme applies the saved colour scheme and wires the scheme picker. A
// scheme is a data-mf-scheme attribute on <html>; mateflow-schemes.css and
// the page styles derive every colour from it. "custom" instead sets the
// background and accent variables from the two colour inputs.
func initTheme() {
	picker, bg, accent := byID("mfSchemeSelect"), byID("mfPickBg"), byID("mfPickAccent")
	root := doc.Get("documentElement")

	apply := func(name, background, accentColour string) {
		for _, v := range []string{"--mf-bg", "--mf-accent", "--mf-accent-bright"} {
			root.Get("style").Call("removeProperty", v)
		}
		if name != "" && name != "custom" {
			root.Call("setAttribute", "data-mf-scheme", name)
		} else {
			root.Call("removeAttribute", "data-mf-scheme")
		}
		if name == "custom" && background != "" && accentColour != "" {
			root.Get("style").Call("setProperty", "--mf-bg", background)
			root.Get("style").Call("setProperty", "--mf-accent", accentColour)
			root.Get("style").Call("setProperty", "--mf-accent-bright", accentColour)
		}
		picker.Set("value", name)
		if background != "" {
			bg.Set("value", background)
		}
		if accentColour != "" {
			accent.Set("value", accentColour)
		}
	}
	custom := func(js.Value) {
		background, accentColour := str(bg.Get("value")), str(accent.Get("value"))
		apply("custom", background, accentColour)
		storageSet(schemeKey, toJSON(map[string]any{"name": "custom", "bg": background, "ac": accentColour}))
	}

	if saved, ok := parseJSON(storageGet(schemeKey)); ok {
		apply(str(get(saved, "name")), str(get(saved, "bg")), str(get(saved, "ac")))
	}
	on(picker, "change", func(ev js.Value) {
		name := str(picker.Get("value"))
		if name == "custom" {
			custom(ev)
			return
		}
		apply(name, "", "")
		storageSet(schemeKey, toJSON(map[string]any{"name": name}))
	})
	on(bg, "input", custom)
	on(accent, "input", custom)
}
