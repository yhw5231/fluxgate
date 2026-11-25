package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/proxypool"
	"github.com/yhw5231/fluxgate/internal/store"
)

// poolStub is a pool management API for the lease endpoints.
func poolStub(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	leases := map[string]int{}
	nextPort := 21000
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":               "ok",
			"lease_count":          len(leases),
			"socks_mode":           "per_ipv6",
			"socks_listen_address": "",
		})
	})
	mux.HandleFunc("/v1/leases", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		defer mu.Unlock()
		port, known := leases[request.ID]
		if !known {
			nextPort++
			port = nextPort
			leases[request.ID] = port
		}
		_ = json.NewEncoder(w).Encode(proxypool.Lease{ID: request.ID, IPv6: "2001:db8::1", Port: port})
	})
	mux.HandleFunc("/v1/leases/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/leases/")
		parts := strings.Split(path, "/")
		id := parts[0]
		mu.Lock()
		defer mu.Unlock()
		port, known := leases[id]
		if !known {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			delete(leases, id)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		nextPort++
		leases[id] = nextPort
		_ = json.NewEncoder(w).Encode(proxypool.Lease{ID: id, IPv6: "2001:db8::new", Port: nextPort})
		_ = port
	})
	return httptest.NewServer(mux)
}

// memoryAssignments is the manager's store for one test.
type memoryAssignments struct {
	mu    sync.Mutex
	items []proxypool.Assignment
}

func (m *memoryAssignments) LoadPoolAssignments(context.Context) ([]proxypool.Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]proxypool.Assignment(nil), m.items...), nil
}

func (m *memoryAssignments) SavePoolAssignments(_ context.Context, assignments []proxypool.Assignment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = append([]proxypool.Assignment(nil), assignments...)
	return nil
}

// poolServer assembles a gateway with one configured pool and a manager.
func poolServer(t *testing.T) (*Server, *proxypool.Manager, proxypool.Pool) {
	t.Helper()
	upstream := poolStub(t)
	t.Cleanup(upstream.Close)
	manager := proxypool.NewManager(&memoryAssignments{})
	pool := proxypool.Pool{
		ID: 1, Name: "IPv6 pool", AdminURL: upstream.URL, Mode: "auto", Enabled: true,
	}
	server := &Server{
		ManagementToken: "management-secret",
		Pools:           manager,
		PolicyDefaults:  policy.Default(),
	}
	server.SetConfiguration(store.Configuration{Pools: []proxypool.Pool{pool}})
	return server, manager, pool
}

func poolHTTP(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer management-secret")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestPoolLeasesEndpointListsWhatTheGatewayHolds(t *testing.T) {
	server, manager, pool := poolServer(t)
	if _, err := manager.Acquire(context.Background(), pool, 7, "t12"); err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, poolHTTP(http.MethodGet, "/management/pools/leases", ""))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var answer struct {
		Assignments []proxypool.Assignment `json:"assignments"`
		Pools       []map[string]any       `json:"pools"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if len(answer.Assignments) != 1 || answer.Assignments[0].OwnerKey != "t12" {
		t.Fatalf("assignments = %#v", answer.Assignments)
	}
	if len(answer.Pools) != 1 || answer.Pools[0]["name"] != "IPv6 pool" {
		t.Fatalf("pools = %#v", answer.Pools)
	}
}

func TestPoolRotateEndpointChangesTheAddress(t *testing.T) {
	server, manager, pool := poolServer(t)
	before, err := manager.Acquire(context.Background(), pool, 7, "t12")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, poolHTTP(http.MethodPost, "/management/pools/rotate",
		`{"pool_id":1,"owner_key":"t12"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var answer struct {
		Assignment proxypool.Assignment `json:"assignment"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer.Assignment.SocksURL == before {
		t.Fatalf("address = %q, want a new one", answer.Assignment.SocksURL)
	}
}

func TestPoolReleaseEndpointGivesTheLeaseBack(t *testing.T) {
	server, manager, pool := poolServer(t)
	if _, err := manager.Acquire(context.Background(), pool, 7, "t12"); err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, poolHTTP(http.MethodPost, "/management/pools/release",
		`{"pool_id":1,"owner_key":"t12"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	assignments, err := manager.Assignments(context.Background())
	if err != nil {
		t.Fatalf("Assignments() error = %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments = %#v, want none", assignments)
	}
}

func TestPoolTestEndpointReportsThePool(t *testing.T) {
	server, _, _ := poolServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, poolHTTP(http.MethodPost, "/management/pools/test", `{"pool_id":1}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer["status"] != "ok" {
		t.Fatalf("answer = %#v", answer)
	}
}

func TestPoolEndpointsRefuseWhatTheyCannotDo(t *testing.T) {
	server, _, _ := poolServer(t)

	// Unknown pool.
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, poolHTTP(http.MethodPost, "/management/pools/rotate",
		`{"pool_id":99,"owner_key":"t1"}`))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown pool status = %d, want 404", response.Code)
	}

	// Missing owner key.
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, poolHTTP(http.MethodPost, "/management/pools/rotate", `{"pool_id":1}`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing owner status = %d, want 400", response.Code)
	}

	// Unauthenticated.
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/management/pools/leases", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", response.Code)
	}

	// A gateway started without pools.
	bare := &Server{ManagementToken: "management-secret"}
	response = httptest.NewRecorder()
	bare.Handler().ServeHTTP(response, poolHTTP(http.MethodGet, "/management/pools/leases", ""))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-pools status = %d, want 503", response.Code)
	}
}
