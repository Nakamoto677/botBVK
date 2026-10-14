package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

type blockingInboxStore struct {
	domain.Store
	mu             sync.Mutex
	updates        map[int64]domain.StoredUpdate
	finished       map[int64]bool
	saved          chan int64
	firstFinishing chan struct{}
	releaseFirst   chan struct{}
}

func (s *blockingInboxStore) LastUpdateID(context.Context) (int64, error) { return 0, nil }
func (s *blockingInboxStore) SaveUpdate(_ context.Context, id int64, payload json.RawMessage) (bool, error) {
	s.mu.Lock()
	s.updates[id] = domain.StoredUpdate{ID: id, Payload: payload}
	s.mu.Unlock()
	s.saved <- id
	return true, nil
}
func (s *blockingInboxStore) PendingUpdates(context.Context, int) ([]domain.StoredUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range []int64{1, 2} {
		if update, ok := s.updates[id]; ok && !s.finished[id] {
			return []domain.StoredUpdate{update}, nil
		}
	}
	return nil, nil
}
func (s *blockingInboxStore) FinishUpdate(_ context.Context, id int64) error {
	if id == 1 {
		close(s.firstFinishing)
		<-s.releaseFirst
	}
	s.mu.Lock()
	s.finished[id] = true
	s.mu.Unlock()
	return nil
}

func TestPollingStoresNewUpdateWhilePreviousUpdateIsSlow(t *testing.T) {
	store := &blockingInboxStore{updates: map[int64]domain.StoredUpdate{}, finished: map[int64]bool{}, saved: make(chan int64, 2), firstFinishing: make(chan struct{}), releaseFirst: make(chan struct{})}
	feed := make(chan Update, 2)
	shutdown := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case u := <-feed:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []Update{u}})
		case <-shutdown:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(shutdown)
		server.CloseClientConnections()
		server.Close()
	}()
	client := NewClient("test")
	client.BaseURL = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewAdapter(client, store, nil, Config{}).Run(ctx) }()
	feed <- Update{ID: 1, Message: &Message{From: Person{ID: 1, IsBot: true}}}
	select {
	case <-store.firstFinishing:
	case <-time.After(time.Second):
		close(store.releaseFirst)
		cancel()
		t.Fatal("first update did not reach processing")
	}
	feed <- Update{ID: 2, Message: &Message{From: Person{ID: 2, IsBot: true}}}
	storedSecond := false
	deadline := time.After(500 * time.Millisecond)
	for !storedSecond {
		select {
		case id := <-store.saved:
			storedSecond = id == 2
		case <-deadline:
			close(store.releaseFirst)
			cancel()
			t.Fatal("polling waited for previous update to finish")
		}
	}
	close(store.releaseFirst)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("adapter did not stop")
	}
}
