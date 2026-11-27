package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/yhw5231/fluxgate/internal/probe"
)

// siteProbeFake records the upstream a probe was asked about, and can end the
// caller's context while it answers, which is how a check that runs past its
// deadline is exercised without waiting for one.
type siteProbeFake struct {
	*fakeProbes
	site   int64
	cancel context.CancelFunc
}

func (f *siteProbeFake) ProbeSite(_ context.Context, siteID int64) []probe.Result {
	f.site = siteID
	if f.cancel != nil {
		f.cancel()
	}
	return f.results
}

func postSiteProbe(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/management/probes/run", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer management-secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	return w
}

func probeResultChannelIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	if w.Code != http.StatusOK {
		return nil
	}
	var answer struct {
		Probes []probe.Result `json:"probes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(answer.Probes))
	for _, result := range answer.Probes {
		ids = append(ids, result.ChannelID)
	}
	return ids
}

// A 检测 click names the upstream, and the gateway checks every key it can ask
// about in one batch instead of the browser walking the lines itself.
func TestProbeRunChecksEveryKeyOfOneUpstream(t *testing.T) {
	f := &fakeProbes{
		targets: []probe.Target{
			{SiteID: 1, ChannelID: "a"},
			{SiteID: 2, ChannelID: "other"},
			// The same key on a second route of the same upstream is one account.
			{SiteID: 1, ChannelID: "a"},
			{SiteID: 1, ChannelID: "b", Held: true},
		},
		results: []probe.Result{
			{ChannelID: "a", OK: true},
			{ChannelID: "b"},
			{ChannelID: "other", OK: true},
		},
	}
	w := postSiteProbe(t, probeServer(t, f), `{"site_id":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	if ids := probeResultChannelIDs(t, w); !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("ids=%v, want the two keys of the named upstream", ids)
	}
	if f.site != 1 || f.channel != "" || f.started {
		t.Fatalf("site=%d channel=%q sweep=%v, want one batch for site 1", f.site, f.channel, f.started)
	}
}

// A check an operator waits for names its upstream one way: a line, an upstream,
// or nothing at all. Anything else is refused before a probe goes out.
func TestProbeRunRejectsMalformedSiteBeforeAnyProbe(t *testing.T) {
	for _, body := range []string{
		`{"site_id":0}`,
		`{"site_id":-1}`,
		`{"site_id":null}`,
		`{"site_id":"1"}`,
		`{"site_id":1.5}`,
		`{"site_id":true}`,
		`{"site_id":9223372036854775808}`,
		`{"site_id":1,"channel_id":"a"}`,
		`{"site_id":1}{}`,
	} {
		t.Run(body, func(t *testing.T) {
			f := &fakeProbes{}
			w := postSiteProbe(t, probeServer(t, f), body)
			var answer struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
				t.Fatalf("status=%d, unreadable body: %v", w.Code, err)
			}
			if w.Code != http.StatusBadRequest || answer.Error.Code != "invalid_probe" {
				t.Fatalf("status=%d code=%q, want 400 invalid_probe", w.Code, answer.Error.Code)
			}
			if f.site != 0 || f.channel != "" || f.started {
				t.Fatalf("site=%d channel=%q sweep=%v, want nothing probed", f.site, f.channel, f.started)
			}
		})
	}
}

// An upstream with no probeable line, and an upstream id that no longer exists,
// are answered as such rather than as a check that found nothing wrong.
func TestProbeRunWithoutAProbeableLineIsNotASweep(t *testing.T) {
	f := &fakeProbes{targets: []probe.Target{{SiteID: 2, ChannelID: "other"}}}
	w := postSiteProbe(t, probeServer(t, f), `{"site_id":1}`)
	if w.Code != http.StatusNotFound || f.site != 1 || f.channel != "" || f.started {
		t.Fatalf("status=%d site=%d channel=%q sweep=%v, want 404 for site 1 alone",
			w.Code, f.site, f.channel, f.started)
	}
	if !strings.Contains(w.Body.String(), "unknown_site") {
		t.Fatalf("body=%s, want the stable unknown_site code", w.Body.String())
	}
}

// A check that runs past its deadline answers 504 rather than reporting the
// results it happened to collect, because the console would read those as the
// whole answer.
func TestProbeRunSiteTimeoutIsNotSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &siteProbeFake{
		fakeProbes: &fakeProbes{results: []probe.Result{{ChannelID: "a", OK: true}}},
		cancel:     cancel,
	}
	s := probeServer(t, f.fakeProbes)
	s.Probes = f
	r := httptest.NewRequest(http.MethodPost, "/management/probes/run", strings.NewReader(`{"site_id":1}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer management-secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d, want 504", w.Code)
	}
	if !strings.Contains(w.Body.String(), "probe_timeout") {
		t.Fatalf("body=%s, want the stable probe_timeout code", w.Body.String())
	}
}
