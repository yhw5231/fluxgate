package probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// memoryRuns keeps probe results the way the SQLite store does, for tests that
// care about what was recorded rather than how.
type memoryRuns struct {
	mu   sync.Mutex
	runs []Result
}

func (m *memoryRuns) AppendProbeRun(_ context.Context, result Result, keep int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, result)
	if keep > 0 && len(m.runs) > keep {
		m.runs = m.runs[len(m.runs)-keep:]
	}
	return nil
}

// LatestProbeResults answers the newest result per line, the way the store does.
func (m *memoryRuns) LatestProbeResults(context.Context) (map[string]Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	latest := make(map[string]Result, len(m.runs))
	for _, result := range m.runs {
		latest[result.ChannelID] = result
	}
	return latest, nil
}

func (m *memoryRuns) ListProbeRuns(_ context.Context, limit int) ([]Result, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs := append([]Result(nil), m.runs...)
	if limit > 0 && len(runs) > limit {
		runs = runs[len(runs)-limit:]
	}
	return runs, int64(len(m.runs)), nil
}

func (m *memoryRuns) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runs)
}

func (m *memoryRuns) kinds() []Kind {
	m.mu.Lock()
	defer m.mu.Unlock()
	kinds := make([]Kind, len(m.runs))
	for index := range m.runs {
		kinds[index] = m.runs[index].Kind
	}
	return kinds
}

// serviceHarness is a service wired to a working stub upstream, a shared clock,
// and an in-memory record.
type serviceHarness struct {
	service *Service
	runs    *memoryRuns
	calls   *int32
	now     *time.Time
}

func newServiceHarness(t *testing.T, lines []Line) *serviceHarness {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		answer, _ := questionAnswer(promptOfBody(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatAnswer("The answer is " + itoa(answer) + ".")))
	}))
	t.Cleanup(server.Close)

	for index := range lines {
		lines[index].BaseURL = server.URL + "/v1"
	}
	linesCopy := append([]Line(nil), lines...)

	runs := &memoryRuns{}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	service := NewService()
	service.Prober = &Prober{Client: server.Client()}
	service.Lines = func() []Line { return linesCopy }
	service.Store = runs
	service.Now = func() time.Time { return now }
	service.SetSettings(Settings{Startup: false, Idle: 0, Concurrency: 2, Keep: 100})
	return &serviceHarness{service: service, runs: runs, calls: &calls, now: &now}
}

func itoa(value int64) string {
	// strconv would drag an import in for one call; the values are small.
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func harnessLine(channelID, model string) Line {
	return Line{
		ChannelID:   channelID,
		ChannelName: channelID,
		BaseURL:     "http://127.0.0.1:1/v1",
		APIKey:      "sk-" + channelID,
		Model:       model,
	}
}

func (h *serviceHarness) probeCalls() int {
	return int(atomic.LoadInt32(h.calls))
}

func TestServiceProbesALineThatHasBeenIdle(t *testing.T) {
	harness := newServiceHarness(t, []Line{harnessLine("line-1", "gpt-4.1")})
	harness.service.SetSettings(Settings{Idle: time.Hour, Concurrency: 2, Keep: 100})
	started := *harness.now
	harness.service.NoteCall("line-1", "gpt-4.1")

	// Half an hour of silence is not idle.
	*harness.now = started.Add(30 * time.Minute)
	harness.service.Scan(context.Background())
	if calls := harness.probeCalls(); calls != 0 {
		t.Fatalf("probes = %d, want none before the idle interval", calls)
	}

	// Past the interval, the line is asked one question.
	*harness.now = started.Add(61 * time.Minute)
	harness.service.Scan(context.Background())
	if calls := harness.probeCalls(); calls != 1 {
		t.Fatalf("probes = %d, want one after the idle interval", calls)
	}
	if harness.runs.count() != 1 {
		t.Fatalf("recorded %d results, want one", harness.runs.count())
	}
	if kinds := harness.runs.kinds(); kinds[0] != KindIdle {
		t.Fatalf("kind = %q, want %q", kinds[0], KindIdle)
	}
	// The probe itself is traffic: the line's clock restarts, so the next scan does
	// not ask again.
	harness.service.Scan(context.Background())
	if calls := harness.probeCalls(); calls != 1 {
		t.Fatalf("probes = %d, want the idle clock restarted by the probe", calls)
	}
}

func TestServiceEstablishesTheIdleClockOnFirstSight(t *testing.T) {
	harness := newServiceHarness(t, []Line{harnessLine("line-1", "gpt-4.1")})
	harness.service.SetSettings(Settings{Idle: time.Hour, Concurrency: 2, Keep: 100})
	started := *harness.now

	// A freshly started gateway has never seen this line, which is not the same as
	// knowing it has been idle since the epoch.
	harness.service.Scan(context.Background())
	if calls := harness.probeCalls(); calls != 0 {
		t.Fatalf("probes = %d, want none on the first sighting", calls)
	}
	*harness.now = started.Add(61 * time.Minute)
	harness.service.Scan(context.Background())
	if calls := harness.probeCalls(); calls != 1 {
		t.Fatalf("probes = %d, want one once the idle interval passed", calls)
	}
}

func TestServiceSkipsAHeldLineAndProbesItsRecovery(t *testing.T) {
	harness := newServiceHarness(t, []Line{harnessLine("line-1", "gpt-4.1")})
	held := map[breaker.Scope]breaker.State{
		{ChannelID: "line-1"}: {BlockedUntil: harness.now.Add(time.Hour)},
	}
	var circuitsMu sync.Mutex
	harness.service.Circuits = func() map[breaker.Scope]breaker.State {
		circuitsMu.Lock()
		defer circuitsMu.Unlock()
		copied := make(map[breaker.Scope]breaker.State, len(held))
		for scope, state := range held {
			copied[scope] = state
		}
		return copied
	}

	harness.service.Scan(context.Background())
	if calls := harness.probeCalls(); calls != 0 {
		t.Fatalf("probes = %d, want none while the line is held out", calls)
	}

	circuitsMu.Lock()
	held = map[breaker.Scope]breaker.State{}
	circuitsMu.Unlock()
	harness.service.Scan(context.Background())
	if calls := harness.probeCalls(); calls != 1 {
		t.Fatalf("probes = %d, want one when the line comes back", calls)
	}
	if kinds := harness.runs.kinds(); kinds[0] != KindRecovery {
		t.Fatalf("kind = %q, want %q", kinds[0], KindRecovery)
	}
}

func TestServiceProbesEveryLineOnDemand(t *testing.T) {
	harness := newServiceHarness(t, []Line{
		harnessLine("line-1", "gpt-4.1"),
		harnessLine("line-2", "gpt-4.1"),
	})
	results := harness.service.ProbeAll(context.Background())
	if len(results) != 2 {
		t.Fatalf("results = %#v, want one per line", results)
	}
	if harness.runs.count() != 2 {
		t.Fatalf("recorded %d results, want two", harness.runs.count())
	}
	for _, result := range results {
		if !result.OK {
			t.Fatalf("result = %#v, want a healthy stub upstream", result)
		}
		if result.Kind != KindManual {
			t.Fatalf("kind = %q, want %q", result.Kind, KindManual)
		}
	}
}

func TestServiceProbesOneChannelOnDemand(t *testing.T) {
	harness := newServiceHarness(t, []Line{
		harnessLine("line-1", "gpt-4.1"),
		harnessLine("line-2", "gpt-4.1"),
	})
	results := harness.service.ProbeChannel(context.Background(), "line-2")
	if len(results) != 1 || results[0].ChannelID != "line-2" {
		t.Fatalf("results = %#v, want only line-2", results)
	}
	if results := harness.service.ProbeChannel(context.Background(), "no-such-line"); len(results) != 0 {
		t.Fatalf("results = %#v, want no line for an unknown id", results)
	}
}

func TestServiceRunsTheStartupSweep(t *testing.T) {
	previousDelay := startupDelay
	startupDelay = 10 * time.Millisecond
	defer func() { startupDelay = previousDelay }()

	harness := newServiceHarness(t, []Line{harnessLine("line-1", "gpt-4.1")})
	harness.service.SetSettings(Settings{Startup: true, Interval: time.Hour, Concurrency: 1, Keep: 100})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness.service.Start(ctx)
	defer harness.service.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if harness.probeCalls() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls := harness.probeCalls(); calls != 1 {
		t.Fatalf("probes = %d, want the startup sweep to have asked once", calls)
	}
	if kinds := harness.runs.kinds(); kinds[0] != KindStartup {
		t.Fatalf("kind = %q, want %q", kinds[0], KindStartup)
	}
}

func TestServiceAsyncSweepRefusesASecondRun(t *testing.T) {
	harness := newServiceHarness(t, []Line{harnessLine("line-1", "gpt-4.1")})
	// A sweep that is already running is what a second request meets.
	harness.service.asyncBusy.Store(true)
	if harness.service.ProbeAllAsync(context.Background()) {
		t.Fatal("a second async sweep started while one was in flight")
	}
	harness.service.asyncBusy.Store(false)
	if !harness.service.ProbeAllAsync(context.Background()) {
		t.Fatal("the async sweep did not start once the previous one finished")
	}
	// Wait for it, so its store write cannot race the test's exit.
	harness.service.wg.Wait()
}

// The target list is what the console shows above the probe records: every line the
// schedule could ask, the model it would ask about, whether the line is held out of
// rotation right now, and how it last answered.
func TestServiceReportsWhatItCouldProbe(t *testing.T) {
	harness := newServiceHarness(t, []Line{
		harnessLine("line-1", "gpt-4.1"),
		harnessLine("line-2", "claude-3"),
	})
	harness.service.Circuits = func() map[breaker.Scope]breaker.State {
		return map[breaker.Scope]breaker.State{
			{ChannelID: "line-2"}: {BlockedUntil: harness.now.Add(time.Hour)},
		}
	}

	// A line that has never been probed is listed with no result yet.
	targets := harness.service.Targets(context.Background())
	if len(targets) != 2 {
		t.Fatalf("targets = %#v, want one per line", targets)
	}
	if targets[0].ChannelID != "line-1" || targets[0].Model != "gpt-4.1" || targets[0].Last != nil {
		t.Fatalf("first target = %#v, want line-1 with no result yet", targets[0])
	}
	if !targets[1].Held || targets[1].ChannelID != "line-2" {
		t.Fatalf("second target = %#v, want the held line", targets[1])
	}

	// A probe fills in the newest result of the line it asked.
	harness.service.ProbeChannel(context.Background(), "line-1")
	targets = harness.service.Targets(context.Background())
	if targets[0].Last == nil {
		t.Fatalf("target = %#v, want the result of the probe that just ran", targets[0])
	}
	if !targets[0].Last.OK || targets[0].Last.Reason != ReasonOK || targets[0].Last.Kind != KindManual {
		t.Fatalf("last result = %#v, want the manual probe that just ran", targets[0].Last)
	}
}

// Every finished probe is told to whoever keeps the request log: a probe is a
// request the gateway made on its own, and the log is where its effects show up.
func TestServiceTellsTheRequestLogAboutEveryProbe(t *testing.T) {
	harness := newServiceHarness(t, []Line{harnessLine("line-1", "gpt-4.1")})
	var recorded []Result
	var mu sync.Mutex
	harness.service.RecordRun = func(result Result) {
		mu.Lock()
		recorded = append(recorded, result)
		mu.Unlock()
	}

	harness.service.ProbeAll(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("recorded = %#v, want the one probe", recorded)
	}
	if recorded[0].ChannelID != "line-1" || !recorded[0].OK || recorded[0].Kind != KindManual {
		t.Fatalf("recorded = %#v, want what the probe found", recorded[0])
	}
}
