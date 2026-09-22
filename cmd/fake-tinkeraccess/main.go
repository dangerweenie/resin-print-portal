// Command fake-tinkeraccess is a throwaway stand-in for TinkerAccess's Rust
// server, for local dev and hardware bring-up when the real one isn't
// reachable. It serves the same get_users contract internal/tinkeraccess
// expects (POST, any path under /api/get_users, JSON array response) so the
// portal's roster-sync worker can point straight at it — nothing in the
// worker or the store needs to know the difference.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
)

// user mirrors internal/tinkeraccess.User's wire shape (kept independent on
// purpose -- this binary has no reason to import the real portal packages).
type user struct {
	ID     int64   `json:"id"`
	Name   *string `json:"name"`
	Code   *string `json:"code"`
	Status string  `json:"status"`
}

func main() {
	addr := flag.String("addr", ":3000", "listen address")
	code := flag.String("code", "", "fob code for a single fake active member (quickest path: whatever your probe/journal decoded)")
	name := flag.String("name", "Test Member", "that member's name")
	status := flag.String("status", "A", "that member's status: A (active), I (inactive), or S (active, 24h)")
	rosterPath := flag.String("roster", "", "path to a JSON file of [{\"id\":1,\"name\":\"...\",\"code\":\"...\",\"status\":\"A\"}, ...] -- overrides -code/-name/-status for multiple members")
	flag.Parse()

	var roster []user
	switch {
	case *rosterPath != "":
		b, err := os.ReadFile(*rosterPath)
		if err != nil {
			log.Fatalf("read -roster file: %v", err)
		}
		if err := json.Unmarshal(b, &roster); err != nil {
			log.Fatalf("parse -roster file: %v", err)
		}
	case *code != "":
		n, c := *name, *code
		roster = []user{{ID: 1, Name: &n, Code: &c, Status: *status}}
	default:
		log.Fatal("need -code (quick single-member path) or -roster (a JSON file of members) -- see -h")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The real endpoint's path carries a Leptos build hash suffix that
		// changes on every TinkerAccess deploy (get_users<hash>); match any
		// suffix here so whatever TINKERACCESS_GET_USERS_PATH you already
		// have configured just works, unchanged.
		if !strings.HasPrefix(r.URL.Path, "/api/get_users") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(roster); err != nil {
			log.Printf("encode roster: %v", err)
		}
	})

	log.Printf("fake-tinkeraccess listening on %s -- serving %d member(s):", *addr, len(roster))
	for _, u := range roster {
		var n, c string
		if u.Name != nil {
			n = *u.Name
		}
		if u.Code != nil {
			c = *u.Code
		}
		log.Printf("  id=%d name=%q code=%q status=%s", u.ID, n, c, u.Status)
	}
	log.Fatal(http.ListenAndServe(*addr, mux))
}
