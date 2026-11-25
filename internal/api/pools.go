package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/proxypool"
)

// The leases the gateway holds from its proxy pools.
//
//	GET  /management/pools/leases
//	POST /management/pools/rotate   {"pool_id": 1, "owner_key": "t12"}
//	POST /management/pools/release  {"pool_id": 1, "owner_key": "t12"}
//	POST /management/pools/test     {"pool_id": 1}
//
// A lease is one exit address. The gateway acquires one per upstream account on
// the first request through that account and keeps it, so an upstream sees a
// stable address per account. These endpoints are how an operator sees which
// account holds which address, asks for a new one, gives one back, or checks that
// a pool is reachable at all.

// handlePoolLeases lists the assignments the gateway remembers.
func (s *Server) handlePoolLeases(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if s.Pools == nil {
		writeError(w, http.StatusServiceUnavailable, "pool_not_configured",
			"this gateway was started without proxy pools")
		return
	}
	assignments, err := s.Pools.Assignments(r.Context())
	if err != nil {
		s.logger().Error("pool_leases_read_failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "pool_unreadable", "failed to read the pool leases")
		return
	}
	if assignments == nil {
		assignments = []proxypool.Assignment{}
	}
	// The names travel beside the assignments rather than joined into them: the
	// console has the pool and upstream lists already, and these are the same ids.
	writeJSON(w, http.StatusOK, map[string]any{
		"assignments": assignments,
		"pools":       s.poolSummaries(),
		"sites":       s.siteNames(),
	})
}

// poolRequest is the body the rotate, release, and test endpoints share.
type poolRequest struct {
	PoolID   int64  `json:"pool_id"`
	OwnerKey string `json:"owner_key"`
}

func (s *Server) decodePoolRequest(w http.ResponseWriter, r *http.Request) (poolRequest, bool) {
	var request poolRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_pool_request", "the request must be a JSON object")
		return poolRequest{}, false
	}
	if request.PoolID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_pool_request", "pool_id must be a row id")
		return poolRequest{}, false
	}
	return request, true
}

// poolByID resolves one configured pool.
func (s *Server) poolByID(id int64) (proxypool.Pool, bool) {
	for _, pool := range s.currentConfiguration().Pools {
		if pool.ID == id {
			return pool, true
		}
	}
	return proxypool.Pool{}, false
}

func (s *Server) poolSummaries() []map[string]any {
	summaries := make([]map[string]any, 0)
	for _, pool := range s.currentConfiguration().Pools {
		summaries = append(summaries, map[string]any{
			"id":      pool.ID,
			"name":    pool.Name,
			"mode":    pool.Mode,
			"enabled": pool.Enabled,
		})
	}
	return summaries
}

// siteNames renders upstream ids to names.
func (s *Server) siteNames() map[int64]string {
	names := make(map[int64]string)
	for _, route := range s.currentRoutes() {
		for _, channel := range route.Channels {
			if channel.Name != "" {
				names[channel.SiteID] = channel.Name
			}
		}
	}
	return names
}

// handlePoolRotate asks the pool for a new address on one account's lease.
func (s *Server) handlePoolRotate(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"rotating a lease must come from the console's own origin")
		return
	}
	if s.Pools == nil {
		writeError(w, http.StatusServiceUnavailable, "pool_not_configured",
			"this gateway was started without proxy pools")
		return
	}
	request, ok := s.decodePoolRequest(w, r)
	if !ok {
		return
	}
	pool, known := s.poolByID(request.PoolID)
	if !known {
		writeError(w, http.StatusNotFound, "unknown_pool", "no pool has that id")
		return
	}
	if strings.TrimSpace(request.OwnerKey) == "" {
		writeError(w, http.StatusBadRequest, "invalid_pool_request", "owner_key is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	assignment, err := s.Pools.Rotate(ctx, pool, strings.TrimSpace(request.OwnerKey))
	if err != nil {
		writeErrorDetails(w, http.StatusBadGateway, "pool_rotate_failed", err.Error(), nil)
		return
	}
	s.logConfigurationChange(r, "console_pool_lease_rotated", "pools", pool.ID)
	writeJSON(w, http.StatusOK, map[string]any{"assignment": assignment})
}

// handlePoolRelease gives one account's lease back to the pool.
func (s *Server) handlePoolRelease(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"releasing a lease must come from the console's own origin")
		return
	}
	if s.Pools == nil {
		writeError(w, http.StatusServiceUnavailable, "pool_not_configured",
			"this gateway was started without proxy pools")
		return
	}
	request, ok := s.decodePoolRequest(w, r)
	if !ok {
		return
	}
	pool, known := s.poolByID(request.PoolID)
	if !known {
		writeError(w, http.StatusNotFound, "unknown_pool", "no pool has that id")
		return
	}
	if strings.TrimSpace(request.OwnerKey) == "" {
		writeError(w, http.StatusBadRequest, "invalid_pool_request", "owner_key is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Pools.Release(ctx, pool, strings.TrimSpace(request.OwnerKey)); err != nil {
		writeErrorDetails(w, http.StatusBadGateway, "pool_release_failed", err.Error(), nil)
		return
	}
	s.logConfigurationChange(r, "console_pool_lease_released", "pools", pool.ID)
	writeJSON(w, http.StatusOK, map[string]any{"released": true})
}

// handlePoolTest checks that a pool's management API answers.
func (s *Server) handlePoolTest(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"a pool test must come from the console's own origin")
		return
	}
	request, ok := s.decodePoolRequest(w, r)
	if !ok {
		return
	}
	pool, known := s.poolByID(request.PoolID)
	if !known {
		writeError(w, http.StatusNotFound, "unknown_pool", "no pool has that id")
		return
	}
	client := &proxypool.Client{BaseURL: pool.AdminURL, Token: pool.Token}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	status, err := client.Status(ctx)
	if err != nil {
		writeErrorDetails(w, http.StatusBadGateway, "pool_test_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       status.Status,
		"lease_count":  status.LeaseCount,
		"socks_mode":   status.SocksMode,
		"socks_listen": status.SocksListenAddress,
	})
}
