package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/fstest"

	"oscar/internal/config"
	"oscar/internal/engine"
	"oscar/internal/store"
)

type chatState struct {
	Chat     []store.ChatMessage `json:"chat"`
	ChatMore bool                `json:"chat_more"`
}

func getState(t *testing.T, url string) chatState {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var s chatState
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The Captain box gets only the last page of chat at first; "Load more" asks
// for older messages with chat_from, and they stay loaded on later refreshes.
func TestStateChatPaging(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir, cfg.TypesDir = t.TempDir(), t.TempDir()
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	const total = 120
	for i := 1; i <= total; i++ {
		if n, err := st.AppendChat(store.ChatMessage{From: "user", Text: "m" + strconv.Itoa(i)}); err != nil || n != i {
			t.Fatalf("append %d: got seq %d, err %v", i, n, err)
		}
	}
	e, err := engine.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(e, fstest.MapFS{}).Handler())
	defer srv.Close()

	check := func(q string, first, n int, more bool) {
		t.Helper()
		s := getState(t, srv.URL+"/api/state"+q)
		if len(s.Chat) != n || s.ChatMore != more {
			t.Fatalf("%q: got %d messages, more=%v; want %d, more=%v", q, len(s.Chat), s.ChatMore, n, more)
		}
		for i, m := range s.Chat {
			if want := first + i; m.Seq != want || m.Text != "m"+strconv.Itoa(want) {
				t.Fatalf("%q: message %d is seq %d %q, want seq %d", q, i, m.Seq, m.Text, want)
			}
		}
	}
	check("", total-engine.ChatPage+1, engine.ChatPage, true)
	check("?chat_from=21", 21, 100, true)
	check("?chat_from=1", 1, total, false)

	// A new message gets the next number, and a page start stays put.
	if n, _ := st.AppendChat(store.ChatMessage{From: "captain", Text: "m121"}); n != 121 {
		t.Fatalf("new message seq %d, want 121", n)
	}
	check("?chat_from=21", 21, 101, true)
}
