//go:build js && wasm

package main

import (
	"strings"
	"syscall/js"
)

// Voice follows the room's audio policy. In a "live" room the mic is streamed
// to the others over WebRTC, with the chat socket carrying the signalling.
// Otherwise the browser's speech recognition posts what was said as text.

// peer is one WebRTC connection to another user in the room.
type peer struct {
	pc       js.Value
	handlers []js.Func
}

var (
	micActive   bool
	localStream js.Value
	recognition js.Value
	peers       = map[string]*peer{}

	sttResult, sttEnded, sttFailed js.Func
)

func (p *peer) listen(event string, fn func(ev js.Value)) {
	h := handler(event, fn)
	p.handlers = append(p.handlers, h)
	p.pc.Call("addEventListener", event, h)
}

func (p *peer) close() {
	p.pc.Call("close")
	for _, h := range p.handlers {
		h.Release()
	}
}

func sendSignal(to string, data map[string]any) {
	if sock.Truthy() && int(num(sock.Get("readyState"))) == 1 { // 1 is OPEN
		sock.Call("send", toJSON(map[string]any{"type": "rtc_signal", "to": to, "data": data}))
	}
}

// newPeer connects to one user. receive is set on the listening side, which
// plays whatever audio the other end sends.
func newPeer(name string, receive bool) *peer {
	if old, ok := peers[name]; ok {
		old.close()
	}
	p := &peer{pc: win.Get("RTCPeerConnection").New(map[string]any{
		"iceServers": []any{map[string]any{"urls": "stun:stun.l.google.com:19302"}},
	})}
	p.listen("icecandidate", func(ev js.Value) {
		if candidate := get(ev, "candidate"); candidate.Truthy() {
			sendSignal(name, map[string]any{"candidate": candidate})
		}
	})
	if receive {
		p.listen("track", func(ev js.Value) {
			if media := get(ev, "streams", "0"); media.Truthy() {
				playRemote(name, media)
			}
		})
	}
	if localStream.Truthy() {
		each(localStream.Call("getTracks"), func(track js.Value) { p.pc.Call("addTrack", track, localStream) })
	}
	peers[name] = p
	return p
}

func playRemote(name string, media js.Value) {
	id := "audio-" + name
	audio := byID(id)
	if !audio.Truthy() {
		audio = mk("audio", "", "")
		audio.Set("id", id)
		audio.Set("autoplay", true)
		add(byID("remoteAudioContainer"), audio)
	}
	audio.Set("srcObject", media)
}

// handleRTCSignal answers another user's offer, or takes in their answer or
// network candidate.
func handleRTCSignal(from string, data js.Value) {
	if str(get(roomState, "audio_mode")) != "live" {
		return
	}
	p, ok := peers[from]
	if !ok {
		p = newPeer(from, true)
	}
	if sdp := get(data, "sdp"); sdp.Truthy() {
		if _, err := await(p.pc.Call("setRemoteDescription", win.Get("RTCSessionDescription").New(sdp))); err != nil {
			return
		}
		if str(get(sdp, "type")) != "offer" {
			return
		}
		answer, err := await(p.pc.Call("createAnswer"))
		if err != nil {
			return
		}
		if _, err := await(p.pc.Call("setLocalDescription", answer)); err != nil {
			return
		}
		sendSignal(from, map[string]any{"sdp": p.pc.Get("localDescription")})
	} else if candidate := get(data, "candidate"); candidate.Truthy() {
		// A candidate that cannot be used is simply skipped.
		await(p.pc.Call("addIceCandidate", win.Get("RTCIceCandidate").New(candidate)))
	}
}

func offerTo(name string) {
	p := newPeer(name, false)
	offer, err := await(p.pc.Call("createOffer"))
	if err != nil {
		return
	}
	if _, err := await(p.pc.Call("setLocalDescription", offer)); err != nil {
		return
	}
	sendSignal(name, map[string]any{"sdp": p.pc.Get("localDescription")})
}

func stopTracks(media js.Value) {
	each(media.Call("getTracks"), func(track js.Value) { track.Call("stop") })
}

func stopMic() {
	micActive = false
	classes(byID("btnMic")).Call("remove", "pulse-rec")
	if recognition.Truthy() {
		r := recognition
		recognition = js.Undefined()
		try("speech stop", func() { r.Call("stop") })
	}
	if localStream.Truthy() {
		stopTracks(localStream)
		localStream = js.Undefined()
	}
	for name, p := range peers {
		p.close()
		delete(peers, name)
	}
}

func toggleMic() {
	if micActive {
		stopMic()
		return
	}
	micActive = true
	classes(byID("btnMic")).Call("add", "pulse-rec")

	if str(get(roomState, "audio_mode")) == "live" {
		devices := get(win.Get("navigator"), "mediaDevices")
		if !devices.Truthy() {
			stopMic()
			status("MIC ERROR: this browser gives the page no microphone access")
			return
		}
		media, err := await(devices.Call("getUserMedia", map[string]any{"audio": true}))
		if err != nil {
			stopMic()
			status("MIC ERROR: " + err.Error())
			return
		}
		if !micActive { // switched off while the permission prompt was open
			stopTracks(media)
			return
		}
		localStream = media
		each(get(roomState, "online"), func(u js.Value) {
			if name := str(u); name != meName {
				spawn(func() { offerTo(name) })
			}
		})
		status("LIVE AUDIO STREAMING")
		return
	}

	speech := get(win, "SpeechRecognition")
	if !speech.Truthy() {
		speech = get(win, "webkitSpeechRecognition")
	}
	if !speech.Truthy() {
		stopMic()
		win.Call("alert", "Speech-to-Text not supported in this browser (try Chrome/Edge).")
		return
	}
	recognition = speech.New()
	recognition.Set("continuous", true)
	recognition.Set("interimResults", false)
	recognition.Call("addEventListener", "result", sttResult)
	recognition.Call("addEventListener", "end", sttEnded)
	recognition.Call("addEventListener", "error", sttFailed)
	recognition.Call("start")
	status("STT ACTIVE (HOST DISABLED LIVE AUDIO)")
}

func wireAudio() {
	sttResult = handler("speech result", func(ev js.Value) {
		results := get(ev, "results")
		if !micActive || results.Type() != js.TypeObject || results.Length() == 0 {
			return
		}
		heard := strings.TrimSpace(str(get(results.Index(results.Length()-1), "0", "transcript")))
		if heard == "" {
			return
		}
		room := activeRoom
		spawn(func() { post("/api/messages", map[string]any{"room": room, "text": heard, "stt": true}) })
	})
	sttEnded = handler("speech end", func(ev js.Value) {
		// The browser ends recognition after a pause; keep listening while the mic is on.
		if micActive && recognition.Truthy() && get(ev, "target").Equal(recognition) {
			recognition.Call("start")
		}
	})
	sttFailed = handler("speech error", func(ev js.Value) {
		// Without the microphone or the speech service there is nothing to
		// restart; stopping here keeps the end handler from retrying forever.
		switch reason := str(get(ev, "error")); reason {
		case "not-allowed", "service-not-allowed", "audio-capture":
			stopMic()
			status("MIC ERROR: " + reason)
		}
	})
	act("btnMic", toggleMic)
}
