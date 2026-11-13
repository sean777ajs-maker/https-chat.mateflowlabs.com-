package main

// The message log. A stored message only ever changes through the functions
// in this file, which keep three things together: the revision counter, the
// file on disk and the event sent to the room. A handler that changed a
// message any other way would leave clients showing stale text.

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
)

const deletedText = "This message was deleted"

func msgRoom(room string) string {
	if room == "" {
		return "main"
	}
	return room
}

// findMsgLocked returns the index of the message with the given ID, or -1.
func findMsgLocked(id int) int {
	for i := range msgs {
		if msgs[i].ID == id {
			return i
		}
	}
	return -1
}

// appendMsgLocked gives the message the next ID, persists it and tells the room.
func appendMsgLocked(m Msg) (Msg, error) {
	m.ID = lastID + 1
	b, err := json.Marshal(m)
	if err != nil {
		return Msg{}, err
	}
	if _, err := store.Write(append(b, '\n')); err != nil {
		return Msg{}, err
	}
	msgs = append(msgs, m)
	lastID = m.ID
	broadcastRoomLocked(msgRoom(m.Room), "chat", m)
	return m, nil
}

// editMsgLocked replaces a message's text. The bumped Rev lets a client drop
// an event that is older than what it already shows.
func editMsgLocked(i int, text string) Msg {
	m := &msgs[i]
	m.Text, m.Edited = text, true
	m.Rev++
	saveAllMsgsLocked()
	broadcastRoomLocked(msgRoom(m.Room), "msg_edit", *m)
	return *m
}

// deleteMsgLocked blanks a message but keeps it as a tombstone with a higher
// Rev, so a late edit event cannot bring the old text back on a client.
func deleteMsgLocked(i int, tombstone string) Msg {
	m := &msgs[i]
	if m.Deleted {
		return *m
	}
	m.Deleted, m.Text = true, tombstone
	m.MediaURL, m.MediaType, m.PinnedItem = "", "", nil
	m.Rev++
	saveAllMsgsLocked()
	broadcastRoomLocked(msgRoom(m.Room), "msg_delete", map[string]any{"id": m.ID, "room": m.Room, "rev": m.Rev})
	return *m
}

// saveAllMsgsLocked rewrites the log that is loaded at startup. It writes a
// temporary file and renames it over the old one, so a crash cannot leave half
// a log, then reopens the append handle so later posts land in the new file.
func saveAllMsgsLocked() {
	if store == nil {
		return
	}
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
	err = bw.Flush()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, msgsFile)
	}
	if err != nil {
		os.Remove(tmp)
		log.Printf("save messages: %v", err)
		return
	}
	nf, err := os.OpenFile(msgsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("reopen messages: %v", err)
		return
	}
	store.Close()
	store = nf
}
