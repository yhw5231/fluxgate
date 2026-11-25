// Package proxypool talks to an IPv6 proxy pool: a service that hands out leases,
// each a dedicated IPv6 exit reachable over SOCKS5.
//
// A pool buys one thing the gateway cannot make for itself: a different outbound
// address per upstream account. An upstream that rate-limits by address then sees
// each account from its own IP, and an account whose exit was blocked can be given
// a new one without touching any other account.
//
// The gateway keeps a lease per upstream account and remembers which account holds
// which lease, so a restart does not shuffle addresses. Two accounts of the same
// upstream never share a lease — that is the whole point — while accounts of
// different upstreams may, because each upstream only sees one address per
// account.
package proxypool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pool is one configured proxy pool.
type Pool struct {
	ID int64
	// Name is the label the console shows.
	Name string
	// AdminURL is the pool's management address, for example http://10.0.0.5:8080.
	AdminURL string
	// Token authorizes the management calls, when the pool requires one.
	Token string
	// SocksHost overrides where the SOCKS5 service is reached. Empty means it is on
	// the same host as the management address.
	SocksHost string
	// Mode is the SOCKS5 layout: "auto" (decided from the pool's status),
	// "per_ipv6" (a port per lease), or "multiplex" (one port, the lease in the
	// username).
	Mode string
	// Enabled pools are used; a disabled pool is left alone.
	Enabled bool
}

// Lease is one IPv6 exit as the pool describes it.
type Lease struct {
	ID string `json:"id"`
	// IPv6 is the address the lease exits from.
	IPv6 string `json:"ipv6"`
	// Port is the lease's own SOCKS5 port. Zero means the pool multiplexes leases
	// over one port, with the lease id as the username.
	Port       int  `json:"port"`
	Persistent bool `json:"persistent"`
}

// Status is what a pool reports about itself.
type Status struct {
	Status             string `json:"status"`
	LeaseCount         int    `json:"lease_count"`
	SocksMode          string `json:"socks_mode"`
	SocksListenAddress string `json:"socks_listen_address"`
}

// Client is the pool management API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// apiError is a non-2xx answer from the pool, with its status kept so a caller can
// tell "the lease is gone" from "the pool is down".
type apiError struct {
	Status int
	Msg    string
}

func (e apiError) Error() string { return e.Msg }

// IsNotFound reports whether the pool answered that a lease does not exist.
func IsNotFound(err error) bool {
	var failed apiError
	return errors.As(err, &failed) && failed.Status == http.StatusNotFound
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	base := strings.TrimSpace(c.BaseURL)
	if base != "" && !strings.Contains(base, "://") {
		base = "http://" + base
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return nil, errors.New("the pool has no management address")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token := strings.TrimSpace(c.Token); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.client().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := fmt.Sprintf("the pool answered %s %s with HTTP %d", method, path, response.StatusCode)
		var described struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(answer, &described) == nil && described.Error != "" {
			message = described.Error
		}
		return nil, apiError{Status: response.StatusCode, Msg: message}
	}
	return answer, nil
}

// Status reads the pool's own state, which is where the SOCKS5 layout comes from.
func (c *Client) Status(ctx context.Context) (Status, error) {
	answer, err := c.call(ctx, http.MethodGet, "/v1/status", nil)
	if err != nil {
		return Status{}, err
	}
	var status Status
	if err := json.Unmarshal(answer, &status); err != nil {
		return Status{}, fmt.Errorf("read the pool status: %w", err)
	}
	return status, nil
}

// Acquire takes a lease, or returns the one already carrying that id. The pool
// makes the call idempotent, so a retry after a network error is safe.
func (c *Client) Acquire(ctx context.Context, id string) (Lease, error) {
	answer, err := c.call(ctx, http.MethodPost, "/v1/leases", map[string]any{
		"id":         id,
		"persistent": true,
	})
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	if err := json.Unmarshal(answer, &lease); err != nil {
		return Lease{}, fmt.Errorf("read the lease: %w", err)
	}
	if strings.TrimSpace(lease.ID) == "" {
		lease.ID = id
	}
	return lease, nil
}

// Rotate asks the pool for a new exit address on the same lease.
func (c *Client) Rotate(ctx context.Context, id string) (Lease, error) {
	answer, err := c.call(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+"/rotate", nil)
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	if err := json.Unmarshal(answer, &lease); err != nil {
		return Lease{}, fmt.Errorf("read the rotated lease: %w", err)
	}
	lease.ID = id
	return lease, nil
}

// Release gives a lease back to the pool.
func (c *Client) Release(ctx context.Context, id string) error {
	_, err := c.call(ctx, http.MethodDelete, "/v1/leases/"+url.PathEscape(id), nil)
	return err
}

// Assignment is one account's lease, as the gateway remembers it.
type Assignment struct {
	PoolID  int64  `json:"pool_id"`
	LeaseID string `json:"lease_id"`
	IPv6    string `json:"ipv6,omitempty"`
	Port    int    `json:"port,omitempty"`
	// SiteID is the upstream the account belongs to. It is part of the
	// assignment because accounts of one upstream must not share an address.
	SiteID int64 `json:"site_id"`
	// OwnerKey identifies the account: a credential or a row id.
	OwnerKey string `json:"owner_key"`
	// SocksURL is the proxy address the gateway dials for this assignment.
	SocksURL string `json:"socks_url"`
	// UpdatedAt is when the assignment was last changed.
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists assignments so a restart keeps addresses stable. Saving the
// whole set is enough: there are as many assignments as accounts that use a pool.
type Store interface {
	LoadPoolAssignments(ctx context.Context) ([]Assignment, error)
	SavePoolAssignments(ctx context.Context, assignments []Assignment) error
}

// Manager decides which lease each account uses and renders the SOCKS5 address.
type Manager struct {
	Store Store
	// SocksTimeout bounds one management call.
	SocksTimeout time.Duration

	mu          sync.Mutex
	assignments map[string]Assignment
	// ownerLocks serializes the work for one account, so two requests arriving
	// together do not each acquire a lease.
	ownerLocks map[string]*sync.Mutex
	loaded     bool
}

// NewManager returns a manager with no loaded state.
func NewManager(store Store) *Manager {
	return &Manager{
		Store:       store,
		assignments: make(map[string]Assignment),
		ownerLocks:  make(map[string]*sync.Mutex),
	}
}

// assignmentKey identifies one account's assignment.
func assignmentKey(poolID int64, ownerKey string) string {
	return strconv.FormatInt(poolID, 10) + "|" + ownerKey
}

// Acquire returns the proxy URL one account should dial through, acquiring a
// lease when the account has none yet. The URL is what the transport wants: a
// SOCKS5 address, optionally with the lease as its username.
func (m *Manager) Acquire(ctx context.Context, pool Pool, siteID int64, ownerKey string) (string, error) {
	if !pool.Enabled {
		return "", fmt.Errorf("proxy pool %q is disabled", pool.Name)
	}
	owner := assignmentKey(pool.ID, ownerKey)

	m.mu.Lock()
	lock := m.ownerLock(owner)
	m.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	if err := m.load(ctx); err != nil {
		return "", err
	}
	client := &Client{BaseURL: pool.AdminURL, Token: pool.Token}

	m.mu.Lock()
	existing, known := m.assignments[owner]
	m.mu.Unlock()
	if known && existing.SocksURL != "" {
		// The assignment is sticky: re-asking the pool on every request would make
		// the address depend on the pool's uptime rather than on the lease.
		return existing.SocksURL, nil
	}

	status, statusErr := client.Status(ctx)
	leaseID := m.chooseLease(pool, siteID, ownerKey)
	lease, err := client.Acquire(ctx, leaseID)
	if err != nil {
		return "", fmt.Errorf("acquire a lease from pool %q: %w", pool.Name, err)
	}
	socksURL, err := socksURL(pool, status, statusErr, lease)
	if err != nil {
		return "", err
	}
	assignment := Assignment{
		PoolID:    pool.ID,
		LeaseID:   lease.ID,
		IPv6:      lease.IPv6,
		Port:      lease.Port,
		SiteID:    siteID,
		OwnerKey:  ownerKey,
		SocksURL:  socksURL,
		UpdatedAt: time.Now().UTC(),
	}
	m.mu.Lock()
	m.assignments[owner] = assignment
	m.mu.Unlock()
	if err := m.save(ctx); err != nil {
		return "", err
	}
	return socksURL, nil
}

// Rotate asks the pool for a new address on the account's lease and returns the
// proxy URL to dial from now on.
func (m *Manager) Rotate(ctx context.Context, pool Pool, ownerKey string) (Assignment, error) {
	if err := m.load(ctx); err != nil {
		return Assignment{}, err
	}
	owner := assignmentKey(pool.ID, ownerKey)
	m.mu.Lock()
	existing, known := m.assignments[owner]
	m.mu.Unlock()
	if !known {
		return Assignment{}, fmt.Errorf("no lease is assigned to that account")
	}
	client := &Client{BaseURL: pool.AdminURL, Token: pool.Token}
	status, statusErr := client.Status(ctx)
	lease, err := client.Rotate(ctx, existing.LeaseID)
	if err != nil {
		return Assignment{}, fmt.Errorf("rotate the lease: %w", err)
	}
	if lease.IPv6 == "" {
		lease.IPv6 = existing.IPv6
	}
	if lease.Port == 0 {
		lease.Port = existing.Port
	}
	socksURL, err := socksURL(pool, status, statusErr, lease)
	if err != nil {
		return Assignment{}, err
	}
	updated := existing
	updated.IPv6 = lease.IPv6
	updated.Port = lease.Port
	updated.SocksURL = socksURL
	updated.UpdatedAt = time.Now().UTC()
	m.mu.Lock()
	m.assignments[owner] = updated
	m.mu.Unlock()
	if err := m.save(ctx); err != nil {
		return Assignment{}, err
	}
	return updated, nil
}

// Release gives the account's lease back to the pool and forgets it.
func (m *Manager) Release(ctx context.Context, pool Pool, ownerKey string) error {
	if err := m.load(ctx); err != nil {
		return err
	}
	owner := assignmentKey(pool.ID, ownerKey)
	m.mu.Lock()
	existing, known := m.assignments[owner]
	m.mu.Unlock()
	if !known {
		return nil
	}
	client := &Client{BaseURL: pool.AdminURL, Token: pool.Token}
	if err := client.Release(ctx, existing.LeaseID); err != nil && !IsNotFound(err) {
		return fmt.Errorf("release the lease: %w", err)
	}
	m.mu.Lock()
	delete(m.assignments, owner)
	m.mu.Unlock()
	return m.save(ctx)
}

// Assignments returns a copy of every assignment the manager holds.
func (m *Manager) Assignments(ctx context.Context) ([]Assignment, error) {
	if err := m.load(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	assignments := make([]Assignment, 0, len(m.assignments))
	for _, assignment := range m.assignments {
		assignments = append(assignments, assignment)
	}
	return assignments, nil
}

// chooseLease picks the lease id an account should ask for.
//
// An account never shares a lease with another account of the same upstream, so a
// lease already carrying that site is out. Of the leases that are allowed,
// the most loaded one is preferred — one IP covering as many upstreams as
// possible — and when none qualifies a new id is minted from the account.
func (m *Manager) chooseLease(pool Pool, siteID int64, ownerKey string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	sitesByLease := make(map[string]map[int64]bool)
	for _, assignment := range m.assignments {
		if assignment.PoolID != pool.ID {
			continue
		}
		if sitesByLease[assignment.LeaseID] == nil {
			sitesByLease[assignment.LeaseID] = make(map[int64]bool)
		}
		sitesByLease[assignment.LeaseID][assignment.SiteID] = true
	}
	best := ""
	bestSites := -1
	for leaseID, sites := range sitesByLease {
		if sites[siteID] {
			continue
		}
		if len(sites) > bestSites || (len(sites) == bestSites && leaseID < best) {
			best = leaseID
			bestSites = len(sites)
		}
	}
	if best != "" {
		return best
	}
	// A new lease id has to be stable and unique: the account is what this lease is
	// for, so its key is part of the name.
	return "gw-" + strconv.FormatInt(pool.ID, 10) + "-" + sanitizeLeasePart(ownerKey)
}

// sanitizeLeasePart renders an account key into something a pool accepts as a
// lease id.
func sanitizeLeasePart(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			builder.WriteRune(character)
		case character == '-', character == '_':
			builder.WriteRune(character)
		default:
			builder.WriteRune('-')
		}
	}
	name := strings.Trim(builder.String(), "-_")
	if name == "" {
		name = "account"
	}
	if len(name) > 48 {
		name = name[:48]
	}
	return name
}

// socksURL renders the SOCKS5 address for one lease. In per-lease mode the lease
// has its own port; in multiplex mode every lease shares the base port and the
// lease id is the username.
func socksURL(pool Pool, status Status, statusErr error, lease Lease) (string, error) {
	host := strings.TrimSpace(pool.SocksHost)
	if host == "" {
		host = hostOf(pool.AdminURL)
	}
	if host == "" {
		return "", errors.New("the pool has no SOCKS5 host")
	}
	mode := strings.ToLower(strings.TrimSpace(pool.Mode))
	if mode == "" || mode == "auto" {
		if statusErr == nil {
			mode = strings.ToLower(strings.TrimSpace(status.SocksMode))
		}
	}
	multiplex := mode == "multiplex" || (lease.Port == 0 && mode != "per_ipv6")
	if multiplex {
		port := basePort(status)
		if port == 0 {
			if statusErr != nil {
				return "", fmt.Errorf("read the pool's SOCKS5 address: %w", statusErr)
			}
			return "", errors.New("the pool did not report a SOCKS5 port")
		}
		return fmt.Sprintf("socks5h://user:%s:x@%s", lease.ID, joinHostPort(host, port)), nil
	}
	if lease.Port == 0 {
		return "", errors.New("the pool returned a lease without a SOCKS5 port")
	}
	return "socks5h://" + joinHostPort(host, lease.Port), nil
}

// hostOf is the host part of a management address.
func hostOf(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// basePort reads the multiplex SOCKS5 port out of the pool's listen address.
func basePort(status Status) int {
	address := strings.TrimSpace(status.SocksListenAddress)
	if address == "" {
		return 0
	}
	_, port, err := splitHostPort(address)
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return value
}

// splitHostPort splits an address that may be written as ":8080", "host:8080", or
// "[::1]:8080".
func splitHostPort(address string) (string, string, error) {
	if strings.HasPrefix(address, ":") {
		return "", address[1:], nil
	}
	index := strings.LastIndex(address, ":")
	if index < 0 {
		return "", "", fmt.Errorf("%q carries no port", address)
	}
	return address[:index], address[index+1:], nil
}

// joinHostPort renders host and port, bracketing an IPv6 literal.
func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port)
}

// ownerLock returns the mutex serializing one assignment.
func (m *Manager) ownerLock(owner string) *sync.Mutex {
	if lock, ok := m.ownerLocks[owner]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	m.ownerLocks[owner] = lock
	return lock
}

// load reads the persisted assignments once.
func (m *Manager) load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded || m.Store == nil {
		m.loaded = true
		return nil
	}
	assignments, err := m.Store.LoadPoolAssignments(ctx)
	if err != nil {
		return err
	}
	for _, assignment := range assignments {
		m.assignments[assignmentKey(assignment.PoolID, assignment.OwnerKey)] = assignment
	}
	m.loaded = true
	return nil
}

// save writes the whole assignment set.
func (m *Manager) save(ctx context.Context) error {
	if m.Store == nil {
		return nil
	}
	m.mu.Lock()
	assignments := make([]Assignment, 0, len(m.assignments))
	for _, assignment := range m.assignments {
		assignments = append(assignments, assignment)
	}
	m.mu.Unlock()
	return m.Store.SavePoolAssignments(ctx, assignments)
}
