package main

// Player slots and connection status.
//
// Each WebSocket connection is a "player" with the lowest free slot number
// (1, 2, ...). The slot is a label for the phone and the logs; the virtual
// device itself is identical for every slot, so games assign players in the
// order their devices appear.

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

type player struct {
	Slot        int       `json:"slot"`
	IP          string    `json:"ip"`
	ConnectedAt time.Time `json:"connected_at"`
}

var (
	playersMu sync.Mutex
	players   = map[int]*player{}
)

// addPlayer assigns the lowest free slot to a new connection from remote.
func addPlayer(remote string) *player {
	playersMu.Lock()
	defer playersMu.Unlock()
	slot := 1
	for players[slot] != nil {
		slot++
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	p := &player{Slot: slot, IP: host, ConnectedAt: time.Now()}
	players[slot] = p
	log.Printf("player %d connected from %s (%d connected)", slot, host, len(players))
	return p
}

func removePlayer(p *player) {
	playersMu.Lock()
	defer playersMu.Unlock()
	delete(players, p.Slot)
	log.Printf("player %d disconnected (%d connected)", p.Slot, len(players))
}

// statusHandler reports connected players, sorted by slot.
func statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, 405)
		return
	}
	playersMu.Lock()
	list := make([]*player, 0, len(players))
	for _, p := range players {
		list = append(list, p)
	}
	playersMu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Slot < list[j].Slot })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"count":   len(list),
		"players": list,
	})
}
