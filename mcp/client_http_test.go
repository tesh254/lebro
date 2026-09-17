package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro/mcp"
)

func newHTTPClient() *mcp.Client {
	return mcp.NewClient(mcp.ClientConfig{
		Implementation: &mcpsdk.Implementation{Name: "lebro-http-client", Version: "test"},
		ServerName:     "remote",
	})
}

func TestConnectStreamableHTTP_ReportsModernStatelessMode(t *testing.T) {
	server := newRemoteServer(t, addRemoteEcho)
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return server
	}, &mcpsdk.StreamableHTTPOptions{Stateless: true}))
	defer httpServer.Close()

	client := newHTTPClient()
	if err := client.ConnectStreamableHTTP(context.Background(), mcp.StreamableHTTPConfig{Endpoint: httpServer.URL}); err != nil {
		t.Fatalf("ConnectStreamableHTTP: %v", err)
	}
	defer func() { _ = client.Close() }()

	health := client.ConnectionHealth()
	if health.Mode != mcp.ConnectionModeStateless {
		t.Errorf("Mode = %q, want stateless", health.Mode)
	}
	if health.ProtocolVersion != "2026-07-28" {
		t.Errorf("ProtocolVersion = %q, want 2026-07-28", health.ProtocolVersion)
	}
	if health.HasSession {
		t.Error("HasSession = true, want false for stateless server")
	}
	if _, err := client.DiscoverTools(context.Background()); err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
}

func TestConnectStreamableHTTP_LegacySessionContinuesAcrossCalls(t *testing.T) {
	fixture := newLegacyHTTPFixture(t)
	defer fixture.Close()

	client := newHTTPClient()
	if err := client.ConnectStreamableHTTP(context.Background(), mcp.StreamableHTTPConfig{
		Endpoint: fixture.URL,
		// The fixture intentionally tests request/response continuity only.
		DisableStandaloneSSE: true,
	}); err != nil {
		t.Fatalf("ConnectStreamableHTTP: %v", err)
	}
	defer func() { _ = client.Close() }()

	health := client.ConnectionHealth()
	if health.Mode != mcp.ConnectionModeStateful || !health.HasSession {
		t.Errorf("health = %+v, want stateful session", health)
	}
	for range 2 {
		if _, err := client.DiscoverTools(context.Background()); err != nil {
			t.Fatalf("DiscoverTools: %v", err)
		}
	}
	if got := fixture.sessionIDs(); len(got) != 2 || got[0] != got[1] || got[0] == "" {
		t.Errorf("tools/list session IDs = %q, want same non-empty ID twice", got)
	}
}

func TestConnectStreamableHTTP_SessionLossIsRecoverable(t *testing.T) {
	fixture := newLegacyHTTPFixture(t)
	defer fixture.Close()
	client := newHTTPClient()
	if err := client.ConnectStreamableHTTP(context.Background(), mcp.StreamableHTTPConfig{Endpoint: fixture.URL, DisableStandaloneSSE: true}); err != nil {
		t.Fatalf("ConnectStreamableHTTP: %v", err)
	}
	defer func() { _ = client.Close() }()

	fixture.loseSession()
	_, err := client.DiscoverTools(context.Background())
	if !errors.Is(err, mcp.ErrRemoteSessionLost) {
		t.Fatalf("DiscoverTools error = %v, want ErrRemoteSessionLost", err)
	}
	health := client.ConnectionHealth()
	if health.Connected || !health.SessionLost {
		t.Errorf("health after 404 = %+v, want disconnected lost session", health)
	}
	if err := client.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if health := client.ConnectionHealth(); !health.Connected || health.SessionLost {
		t.Errorf("health after reconnect = %+v", health)
	}
	if _, err := client.DiscoverTools(context.Background()); err != nil {
		t.Fatalf("DiscoverTools after reconnect: %v", err)
	}
}

func TestConnectStreamableHTTP_ExpiresIdleSession(t *testing.T) {
	fixture := newLegacyHTTPFixture(t)
	defer fixture.Close()
	client := newHTTPClient()
	if err := client.ConnectStreamableHTTP(context.Background(), mcp.StreamableHTTPConfig{
		Endpoint: fixture.URL, DisableStandaloneSSE: true, IdleTimeout: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("ConnectStreamableHTTP: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	deadline := time.Now().Add(time.Second)
	for !client.ConnectionHealth().Closed && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if health := client.ConnectionHealth(); !health.Closed || health.Connected {
		t.Errorf("health after idle expiry = %+v", health)
	}
}

func TestConnectStreamableHTTP_ConcurrentClientsDoNotShareLegacySessions(t *testing.T) {
	fixture := newLegacyHTTPFixture(t)
	defer fixture.Close()

	clients := []*mcp.Client{newHTTPClient(), newHTTPClient()}
	for _, client := range clients {
		if err := client.ConnectStreamableHTTP(context.Background(), mcp.StreamableHTTPConfig{Endpoint: fixture.URL, DisableStandaloneSSE: true}); err != nil {
			t.Fatalf("ConnectStreamableHTTP: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
	}
	var group sync.WaitGroup
	for _, client := range clients {
		group.Add(1)
		go func(client *mcp.Client) {
			defer group.Done()
			for range 4 {
				if _, err := client.DiscoverTools(context.Background()); err != nil {
					t.Errorf("DiscoverTools: %v", err)
				}
			}
		}(client)
	}
	group.Wait()

	seen := fixture.sessionIDs()
	if len(seen) != 8 {
		t.Fatalf("tools/list calls = %d, want 8", len(seen))
	}
	ids := map[string]bool{}
	for _, id := range seen {
		ids[id] = true
	}
	if len(ids) != 2 {
		t.Errorf("session IDs = %v, want two isolated clients", ids)
	}
}

type legacyHTTPFixture struct {
	*httptest.Server
	mu       sync.Mutex
	nextID   int
	sessions map[string]bool
	seen     []string
}

func newLegacyHTTPFixture(t *testing.T) *legacyHTTPFixture {
	t.Helper()
	fixture := &legacyHTTPFixture{sessions: make(map[string]bool)}
	fixture.Server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	return fixture
}

func (f *legacyHTTPFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch request.Method {
	case "server/discover":
		writeRPC(w, request.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
	case "initialize":
		f.mu.Lock()
		f.nextID++
		id := "legacy-session-" + strconv.Itoa(f.nextID)
		f.sessions[id] = true
		f.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", id)
		writeRPC(w, request.ID, map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "legacy", "version": "test"},
		}, nil)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		id := r.Header.Get("Mcp-Session-Id")
		f.mu.Lock()
		lost := !f.sessions[id]
		if !lost {
			f.seen = append(f.seen, id)
		}
		f.mu.Unlock()
		if lost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRPC(w, request.ID, map[string]any{"tools": []any{}}, nil)
	default:
		writeRPC(w, request.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
	}
}

func (f *legacyHTTPFixture) sessionIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *legacyHTTPFixture) loseSession() {
	f.mu.Lock()
	// Remove only sessions that exist now. A later initialize from Reconnect
	// receives a fresh valid session so this fixture verifies recovery, not
	// merely that the reconnect handshake completed.
	for id := range f.sessions {
		delete(f.sessions, id)
	}
	f.mu.Unlock()
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcError any) {
	payload := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if rpcError != nil {
		payload["error"] = rpcError
	} else {
		payload["result"] = result
	}
	_ = json.NewEncoder(w).Encode(payload)
}
