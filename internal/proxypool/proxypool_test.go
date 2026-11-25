package proxypool

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakePool is a stand-in pool API: it hands out leases, rotates them, and records
// what it was asked.
type fakePool struct {
	mu       sync.Mutex
	leases   map[string]Lease
	nextPort int
	acquires int
	rotates  int
	releases int
	mode     string
	listen   string
	token    string
}

func newFakePool() *fakePool {
	return &fakePool{
		leases:   make(map[string]Lease),
		nextPort: 20000,
		mode:     "per_ipv6",
		listen:   "",
	}
}

func (f *fakePool) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":               "ok",
			"lease_count":          len(f.leases),
			"socks_mode":           f.mode,
			"socks_listen_address": f.listen,
		})
	})
	mux.HandleFunc("/v1/leases", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.acquires++
		lease, known := f.leases[request.ID]
		if !known {
			f.nextPort++
			lease = Lease{ID: request.ID, IPv6: "2001:db8::" + request.ID, Port: f.nextPort}
			f.leases[request.ID] = lease
		}
		_ = json.NewEncoder(w).Encode(lease)
	})
	mux.HandleFunc("/v1/leases/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/leases/"), "/")
		id := parts[0]
		f.mu.Lock()
		defer f.mu.Unlock()
		lease, known := f.leases[id]
		if !known {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such lease"}`))
			return
		}
		switch {
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "rotate":
			f.rotates++
			f.nextPort++
			lease.IPv6 = "2001:db8::rotated-" + id
			lease.Port = f.nextPort
			f.leases[id] = lease
			_ = json.NewEncoder(w).Encode(lease)
		case r.Method == http.MethodDelete:
			f.releases++
			delete(f.leases, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return httptest.NewServer(mux)
}

// memoryStore keeps assignments for one test.
type memoryStore struct {
	mu          sync.Mutex
	assignments []Assignment
	saves       int
}

func (m *memoryStore) LoadPoolAssignments(context.Context) ([]Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Assignment(nil), m.assignments...), nil
}

func (m *memoryStore) SavePoolAssignments(_ context.Context, assignments []Assignment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	m.assignments = append([]Assignment(nil), assignments...)
	return nil
}

func testPool(server *httptest.Server) Pool {
	return Pool{
		ID:       1,
		Name:     "pool-a",
		AdminURL: server.URL,
		Mode:     "auto",
		Enabled:  true,
	}
}

func TestAcquireRendersTheLeaseSocksAddress(t *testing.T) {
	pool := newFakePool()
	server := pool.server(t)
	defer server.Close()
	store := &memoryStore{}
	manager := NewManager(store)

	address, err := manager.Acquire(context.Background(), testPool(server), 7, "t12")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !strings.HasPrefix(address, "socks5h://") || !strings.Contains(address, ":20001") {
		t.Fatalf("address = %q, want the lease's own SOCKS5 port", address)
	}
	assignments, err := manager.Assignments(context.Background())
	if err != nil || len(assignments) != 1 {
		t.Fatalf("assignments = %#v, %v", assignments, err)
	}
	if assignments[0].SiteID != 7 || assignments[0].OwnerKey != "t12" || assignments[0].IPv6 == "" {
		t.Fatalf("assignment = %#v", assignments[0])
	}
	if store.saves == 0 {
		t.Fatal("the assignment was not persisted")
	}

	// The second request reuses the assignment instead of asking the pool again.
	if _, err := manager.Acquire(context.Background(), testPool(server), 7, "t12"); err != nil {
		t.Fatalf("Acquire() second call error = %v", err)
	}
	if pool.acquires != 1 {
		t.Fatalf("acquires = %d, want the lease kept (sticky)", pool.acquires)
	}
}

func TestAcquireKeepsAccountsOfOneUpstreamApart(t *testing.T) {
	pool := newFakePool()
	server := pool.server(t)
	defer server.Close()
	manager := NewManager(&memoryStore{})

	first, err := manager.Acquire(context.Background(), testPool(server), 7, "t1")
	if err != nil {
		t.Fatalf("Acquire(t1) error = %v", err)
	}
	second, err := manager.Acquire(context.Background(), testPool(server), 7, "t2")
	if err != nil {
		t.Fatalf("Acquire(t2) error = %v", err)
	}
	if first == second {
		t.Fatalf("two accounts of one upstream share %q, which is what a pool exists to avoid", first)
	}
	assignments, _ := manager.Assignments(context.Background())
	if len(assignments) != 2 {
		t.Fatalf("assignments = %#v", assignments)
	}
}

func TestAcquireSharesALeaseAcrossUpstreams(t *testing.T) {
	pool := newFakePool()
	server := pool.server(t)
	defer server.Close()
	manager := NewManager(&memoryStore{})

	// The first upstream takes a lease; a different upstream may be carried by the
	// same address, because each upstream only ever sees one account per address.
	first, err := manager.Acquire(context.Background(), testPool(server), 7, "t1")
	if err != nil {
		t.Fatalf("Acquire(site 7) error = %v", err)
	}
	second, err := manager.Acquire(context.Background(), testPool(server), 8, "t9")
	if err != nil {
		t.Fatalf("Acquire(site 8) error = %v", err)
	}
	if first != second {
		t.Fatalf("addresses %q and %q differ; a lease should be packed across upstreams", first, second)
	}
	// The second upstream asked for the same lease by id, so the pool issued one
	// lease even though it was asked twice.
	if len(pool.leases) != 1 {
		t.Fatalf("the pool issued %d leases, want one carrying both upstreams", len(pool.leases))
	}
}

func TestAcquireInMultiplexModeUsesTheSharedPortAndLeaseName(t *testing.T) {
	pool := newFakePool()
	pool.mode = "multiplex"
	pool.listen = "0.0.0.0:1080"
	server := pool.server(t)
	defer server.Close()
	manager := NewManager(&memoryStore{})

	poolConfig := testPool(server)
	poolConfig.Mode = "auto"
	address, err := manager.Acquire(context.Background(), poolConfig, 7, "t12")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !strings.HasPrefix(address, "socks5h://user:") || !strings.HasSuffix(address, ":1080") {
		t.Fatalf("address = %q, want the multiplex form with the lease as username", address)
	}
}

func TestRotateAndReleaseGoThroughThePool(t *testing.T) {
	pool := newFakePool()
	server := pool.server(t)
	defer server.Close()
	manager := NewManager(&memoryStore{})

	address, err := manager.Acquire(context.Background(), testPool(server), 7, "t12")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	rotated, err := manager.Rotate(context.Background(), testPool(server), "t12")
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotated.SocksURL == address {
		t.Fatalf("rotated address = %q, want a new port", rotated.SocksURL)
	}
	if !strings.Contains(rotated.IPv6, "rotated") {
		t.Fatalf("rotated lease = %#v", rotated)
	}
	if err := manager.Release(context.Background(), testPool(server), "t12"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	assignments, _ := manager.Assignments(context.Background())
	if len(assignments) != 0 {
		t.Fatalf("assignments = %#v, want none after a release", assignments)
	}
	if pool.rotates != 1 || pool.releases != 1 {
		t.Fatalf("pool saw %d rotations and %d releases", pool.rotates, pool.releases)
	}
}

func TestDisabledPoolIsRefused(t *testing.T) {
	manager := NewManager(&memoryStore{})
	poolConfig := Pool{ID: 1, Name: "off", Enabled: false}
	if _, err := manager.Acquire(context.Background(), poolConfig, 7, "t1"); err == nil {
		t.Fatal("a disabled pool handed out a lease")
	}
}

func TestPoolClientSendsItsToken(t *testing.T) {
	pool := newFakePool()
	pool.token = "secret-token"
	server := pool.server(t)
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "secret-token"}
	if _, err := client.Status(context.Background()); err != nil {
		t.Fatalf("Status() with the token error = %v", err)
	}
	unauthorized := &Client{BaseURL: server.URL, Token: "wrong"}
	if _, err := unauthorized.Status(context.Background()); err == nil {
		t.Fatal("the pool accepted a wrong token")
	}
}

func TestSocksAddressHelpers(t *testing.T) {
	if host := hostOf("http://10.0.0.5:8080/api"); host != "10.0.0.5" {
		t.Fatalf("hostOf() = %q", host)
	}
	if host := hostOf("pool.example.com"); host != "pool.example.com" {
		t.Fatalf("hostOf(bare host) = %q", host)
	}
	if port := basePort(Status{SocksListenAddress: "0.0.0.0:1080"}); port != 1080 {
		t.Fatalf("basePort() = %d", port)
	}
	if port := basePort(Status{SocksListenAddress: ":1081"}); port != 1081 {
		t.Fatalf("basePort(short form) = %d", port)
	}
	if got := joinHostPort("2001:db8::1", 1080); got != "[2001:db8::1]:1080" {
		t.Fatalf("joinHostPort(ipv6) = %q", got)
	}
	if got := sanitizeLeasePart("Key/With Spaces"); got != "key-with-spaces" {
		t.Fatalf("sanitizeLeasePart() = %q", got)
	}

	// A test server that is not listening is reported as an error, not a lease.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	manager := NewManager(&memoryStore{})
	_, acquireErr := manager.Acquire(context.Background(), Pool{ID: 2, Name: "down", AdminURL: "http://" + address, Enabled: true}, 1, "a1")
	if acquireErr == nil {
		t.Fatal("a pool that is not listening handed out a lease")
	}
	if !strings.Contains(acquireErr.Error(), "acquire") && !strings.Contains(acquireErr.Error(), "refused") {
		t.Fatalf("error = %v, want the connection failure named", acquireErr)
	}
	_ = io.Discard
}
