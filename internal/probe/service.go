package probe

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// The probe service.
//
// The gateway keeps one of these for its lifetime. It decides when a probe is
// worth sending — after a restart, after a line has been idle, when a line comes
// back from a cooldown — sends it through the same transport and proxy the
// gateway uses, and keeps the answer.
//
// Three rules shape the schedule:
//
//   - A line that is held out of rotation is not probed for idleness. It is out
//     for a reason, and probing it would spend the account's quota to learn what
//     the circuit already says. It is probed once when it comes back, which is
//     what tells an operator whether the cooldown was long enough.
//   - A line that has carried no traffic for the idle interval is probed, because
//     a credential that only fails is usually one nobody has used lately.
//   - A probe never changes a circuit. It records what it found; bringing a line
//     back is an operator's decision or its cooldown expiring.

// Settings are the probe policy values in force. They come from the runtime
// policy, so a console write applies to the next scan without a restart.
type Settings struct {
	// Startup probes every eligible line once after the gateway starts.
	Startup bool
	// Idle is how long a line may carry no traffic before it is probed. Zero
	// switches idle probing off.
	Idle time.Duration
	// Concurrency bounds how many probes are in flight at once. A startup sweep
	// can hit many lines at the same moment, and an unbounded sweep would spend
	// the operator's quota in one burst.
	Concurrency int
	// Keep is how many probe records the log holds.
	Keep int
	// Interval is how often the scheduler looks for work.
	Interval time.Duration
	// BreakerMode is the process-wide circuit mode, which decides whether a
	// line's circuit is filed by line, by credential, or by credential and model.
	BreakerMode breaker.Mode
}

// Normalized fixes the values a caller may leave out.
func (s Settings) Normalized() Settings {
	if s.Concurrency <= 0 {
		s.Concurrency = 1
	}
	if s.Concurrency > 64 {
		s.Concurrency = 64
	}
	if s.Interval <= 0 {
		s.Interval = defaultScanInterval
	}
	if s.Keep <= 0 {
		s.Keep = defaultProbeKeep
	}
	if s.BreakerMode == "" {
		s.BreakerMode = breaker.ModeCooldown
	}
	return s
}

const (
	defaultScanInterval = 30 * time.Second
	defaultProbeKeep    = 2000
)

// startupDelay is how long after Start the startup sweep goes out. It lets the
// gateway finish coming up before the first probe competes with real traffic; it
// is a variable so a test does not have to wait for it.
var startupDelay = 2 * time.Second

// RunStore keeps what the probes found. It is satisfied by the SQLite store.
type RunStore interface {
	AppendProbeRun(ctx context.Context, result Result, keep int) error
	ListProbeRuns(ctx context.Context, limit int) ([]Result, int64, error)
}

// LatestResultStore is the optional part of a run store that can answer the newest
// result recorded for each line. The console's target list shows it beside the line
// it belongs to, and a store that cannot answer simply leaves that column empty.
type LatestResultStore interface {
	LatestProbeResults(ctx context.Context) (map[string]Result, error)
}

// Target is one line the schedule can probe: where a question would go, the model
// it would ask about, and the newest result recorded for that line. The console's
// 探测 view lists these, so an operator sees what the gateway can check and how each
// line last answered, and not only the checks that already happened.
type Target struct {
	// SiteID is the upstream the line belongs to, so a view can group the lines it
	// can check by the upstream they serve.
	SiteID      int64  `json:"site_id,omitempty"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Model       string `json:"model"`
	// Held reports that the line is out of rotation right now: its circuit is
	// cooling down or disabled. The schedule does not probe such a line for
	// idleness, and a manual probe is how an operator asks anyway.
	Held bool `json:"held,omitempty"`
	// Last is the newest result recorded for this line, absent until it is probed.
	Last *Result `json:"last,omitempty"`
}

// Service schedules and runs probes for the gateway.
type Service struct {
	// Prober sends one probe. It is required.
	Prober *Prober
	// Lines reports the lines that could be probed right now: an enabled line
	// with a model it can be asked about. It is called on every scan, so it reads
	// the live configuration.
	Lines func() []Line
	// Circuits reports the recorded circuits, which is how a line that is held out
	// of rotation is recognized.
	Circuits func() map[breaker.Scope]breaker.State
	// Store keeps the results. When nil, results are logged and not recorded.
	Store  RunStore
	Logger *slog.Logger
	Now    func() time.Time
	// RecordRun is told about every finished probe, which is how a probe reaches the
	// request log: a probe is a request the gateway made on its own, and an operator
	// hunting a broken account should find it beside the traffic it stands in for.
	// It is called from the probe workers, so it must be safe to call concurrently.
	RecordRun func(Result)

	settings atomic.Pointer[Settings]
	// activity is when each line last carried traffic, which is the clock the idle
	// probe measures.
	activity *activity
	// held remembers the lines that were held out of rotation on the previous
	// scan, so a line that comes back is probed once.
	heldMu sync.Mutex
	held   map[string]bool

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startOnce sync.Once
	// async bounds how many background probe runs (a "probe everything" from the
	// console) may be in flight, so repeated clicks do not stack.
	asyncBusy atomic.Bool
}

// NewService returns a service with default settings.
func NewService() *Service {
	service := &Service{
		activity: newActivity(),
		held:     make(map[string]bool),
	}
	defaults := Settings{}.Normalized()
	service.settings.Store(&defaults)
	return service
}

// SetSettings replaces the probe policy.
func (s *Service) SetSettings(settings Settings) {
	if s == nil {
		return
	}
	normalized := settings.Normalized()
	s.settings.Store(&normalized)
}

// currentSettings returns the policy in force.
func (s *Service) currentSettings() Settings {
	if s == nil {
		return Settings{}.Normalized()
	}
	if stored := s.settings.Load(); stored != nil {
		return *stored
	}
	return Settings{}.Normalized()
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// NoteCall records that a line carried a real request, which restarts its idle
// clock. A probe counts as a call as well: it is traffic to that account.
func (s *Service) NoteCall(channelID, model string) {
	if s == nil {
		return
	}
	s.activity.note(channelID, model, s.now())
}

// Start launches the background schedule: the startup sweep and the periodic
// scan. It is safe to call more than once; only the first call starts anything.
func (s *Service) Start(parent context.Context) {
	if s == nil || s.Prober == nil {
		return
	}
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
		s.cancel = cancel
		settings := s.currentSettings()

		if settings.Startup {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				// The sweep runs after Start returns, so the gateway is already
				// serving before the first probe goes out.
				time.Sleep(startupDelay)
				lines := s.eligibleLines(s.lines(), settings)
				if len(lines) == 0 {
					return
				}
				s.logger().Info("probe_startup_sweep", "lines", len(lines))
				s.runLines(ctx, lines, KindStartup, settings)
			}()
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(settings.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.scan(ctx)
				}
			}
		}()
	})
}

// Stop ends the background schedule and waits for the probes in flight.
func (s *Service) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
	s.cancel = nil
}

// ProbeAll runs one probe for every eligible line, now, and returns what it
// found. An operator asks for this; the startup sweep and the scan use the same
// path.
func (s *Service) ProbeAll(ctx context.Context) []Result {
	if s == nil || s.Prober == nil {
		return nil
	}
	settings := s.currentSettings()
	return s.runLines(ctx, s.eligibleLines(s.lines(), settings), KindManual, settings)
}

// ProbeAllAsync starts a background "probe everything" and reports whether it
// started. A second request while one is running is refused rather than queued:
// the operator can see the results of the first.
func (s *Service) ProbeAllAsync(ctx context.Context) bool {
	if s == nil || s.Prober == nil {
		return false
	}
	if !s.asyncBusy.CompareAndSwap(false, true) {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.asyncBusy.Store(false)
		s.ProbeAll(ctx)
	}()
	return true
}

// ProbeChannel probes every line of one upstream, now. A channel id names one
// line; the answer is empty when no such line is enabled.
func (s *Service) ProbeChannel(ctx context.Context, channelID string) []Result {
	if s == nil || s.Prober == nil {
		return nil
	}
	settings := s.currentSettings()
	selected := make([]Line, 0, 1)
	for _, line := range s.lines() {
		if line.ChannelID == strings.TrimSpace(channelID) {
			selected = append(selected, line)
		}
	}
	if len(selected) == 0 {
		return nil
	}
	return s.runLines(ctx, selected, KindManual, settings)
}

// ProbeSite checks every probeable line of one upstream, now. It is the batch a
// console row asks for: one small request per key of that upstream, sharing the
// concurrency limit a sweep would use, and answered while the operator waits. A
// line whose circuit is cooling down is included, because confirming that the
// upstream serves again is what a manual check is for; a disabled line, or one
// whose routes name no concrete model, cannot be asked and is left out.
func (s *Service) ProbeSite(ctx context.Context, siteID int64) []Result {
	if s == nil || s.Prober == nil || siteID <= 0 {
		return nil
	}
	selected := make([]Line, 0)
	for _, line := range s.lines() {
		if line.SiteID == siteID {
			selected = append(selected, line)
		}
	}
	return s.runLines(ctx, selected, KindManual, s.currentSettings())
}

// ListRuns answers the console's probe view.
func (s *Service) ListRuns(ctx context.Context, limit int) ([]Result, int64, error) {
	if s == nil || s.Store == nil {
		return nil, 0, ErrUnavailable
	}
	return s.Store.ListProbeRuns(ctx, limit)
}

// ClearRuns empties the probe record.
func (s *Service) ClearRuns(ctx context.Context) (int64, error) {
	if s == nil || s.Store == nil {
		return 0, ErrUnavailable
	}
	clearer, ok := s.Store.(interface {
		ClearProbeRuns(context.Context) (int64, error)
	})
	if !ok {
		return 0, ErrUnavailable
	}
	return clearer.ClearProbeRuns(ctx)
}

// ErrUnavailable reports a gateway that keeps no probe records.
var ErrUnavailable = errUnavailable{}

type errUnavailable struct{}

func (errUnavailable) Error() string { return "this gateway keeps no probe records" }

// lines reports the current lines, tolerating a service without a source.
func (s *Service) lines() []Line {
	if s.Lines == nil {
		return nil
	}
	return s.Lines()
}

// eligibleLines keeps the lines a probe may use: an enabled line on an upstream
// that is not held out of rotation, with a model it can be asked about.
func (s *Service) eligibleLines(lines []Line, settings Settings) []Line {
	if len(lines) == 0 {
		return nil
	}
	now := s.now()
	held := s.heldLines(lines, now, settings)
	eligible := make([]Line, 0, len(lines))
	for _, line := range lines {
		if held[line.ChannelID] {
			continue
		}
		eligible = append(eligible, line)
	}
	return eligible
}

// heldLines reports which lines are held out of rotation right now: the circuit
// that the line's own mode files it under is cooling down or disabled.
func (s *Service) heldLines(lines []Line, now time.Time, settings Settings) map[string]bool {
	held := make(map[string]bool)
	if s.Circuits == nil {
		return held
	}
	circuits := s.Circuits()
	for _, line := range lines {
		scope := breaker.ScopeForMode(line.BreakerMode, settings.BreakerMode, line.SiteID, line.ChannelID, line.APIKey, line.Model)
		state, known := circuits[scope]
		if !known {
			continue
		}
		if state.Disabled || state.BlockedUntil.After(now) {
			held[line.ChannelID] = true
		}
	}
	return held
}

// Targets reports the lines the schedule could probe right now, each with the
// newest result recorded for it. The list is a live view of the configuration, so a
// line added from the console appears before it is ever probed — with no result yet.
func (s *Service) Targets(ctx context.Context) []Target {
	if s == nil {
		return nil
	}
	lines := s.lines()
	if len(lines) == 0 {
		return nil
	}
	latest := s.latestResults(ctx)
	held := s.heldLines(lines, s.now(), s.currentSettings())
	targets := make([]Target, 0, len(lines))
	for _, line := range lines {
		target := Target{
			SiteID:      line.SiteID,
			ChannelID:   line.ChannelID,
			ChannelName: line.ChannelName,
			Model:       line.Model,
			Held:        held[line.ChannelID],
		}
		if result, known := latest[line.ChannelID]; known {
			// The stored result is copied: the map belongs to the reader, and the
			// answer outlives the call that produced it.
			stored := result
			target.Last = &stored
		}
		targets = append(targets, target)
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].ChannelName != targets[j].ChannelName {
			return targets[i].ChannelName < targets[j].ChannelName
		}
		return targets[i].Model < targets[j].Model
	})
	return targets
}

// latestResults reads the newest result recorded for each line. A store that cannot
// answer it is not an error: the target list still says which lines can be asked.
func (s *Service) latestResults(ctx context.Context) map[string]Result {
	reader, ok := s.Store.(LatestResultStore)
	if !ok || reader == nil {
		return map[string]Result{}
	}
	latest, err := reader.LatestProbeResults(ctx)
	if err != nil {
		s.logger().Warn("probe_latest_unreadable", "error", err.Error())
		return map[string]Result{}
	}
	if latest == nil {
		return map[string]Result{}
	}
	return latest
}

// DiscoverPin asks one upstream model which internal providers it has.
func (s *Service) DiscoverPin(ctx context.Context, line Line) PinDiscovery {
	if s == nil || s.Prober == nil {
		return PinDiscovery{Detail: "this gateway runs no prober"}
	}
	return s.Prober.DiscoverPin(ctx, line)
}

// ValidatePins asks each named provider whether it answers.
func (s *Service) ValidatePins(ctx context.Context, line Line, providers []string, pipeline string) []PinValidation {
	if s == nil || s.Prober == nil {
		return nil
	}
	return s.Prober.ValidatePins(ctx, line, providers, pipeline)
}

// Scan runs one scheduling round: it looks for lines that just came back from a
// cooldown and lines that have been idle long enough, and probes them. The
// background ticker calls it; exposing it lets a caller (and a test) drive one
// round without waiting for the interval.
func (s *Service) Scan(ctx context.Context) {
	if s == nil || s.Prober == nil {
		return
	}
	s.scan(ctx)
}

// scan runs one scheduling round.
func (s *Service) scan(ctx context.Context) {
	settings := s.currentSettings()
	lines := s.lines()
	if len(lines) == 0 {
		return
	}
	now := s.now()
	current := s.heldLines(lines, now, settings)

	s.heldMu.Lock()
	previous := s.held
	s.held = current
	s.heldMu.Unlock()

	// One probe per line is enough for a scan; a note for each line's model would
	// ask the same account several questions at the same moment.
	byChannel := make(map[string]Line, len(lines))
	order := make([]string, 0, len(lines))
	for _, line := range lines {
		if _, seen := byChannel[line.ChannelID]; !seen {
			order = append(order, line.ChannelID)
		}
		byChannel[line.ChannelID] = line
	}

	tasks := make([]Line, 0)
	kinds := make([]Kind, 0)
	for _, channelID := range order {
		line := byChannel[channelID]
		if current[channelID] {
			// Held out: its recovery is what triggers a probe, not its idle clock.
			continue
		}
		if previous[channelID] {
			// It was held out on the previous scan and is not any more: the account
			// claims to be back, and a probe is what verifies it.
			tasks = append(tasks, line)
			kinds = append(kinds, KindRecovery)
			continue
		}
		if settings.Idle <= 0 {
			continue
		}
		last, known := s.activity.last(channelID, line.Model)
		if !known {
			// First sighting: the idle clock starts now rather than at the epoch, so
			// a freshly started gateway does not probe everything a second time.
			s.activity.note(channelID, line.Model, now)
			continue
		}
		if now.Sub(last) >= settings.Idle {
			tasks = append(tasks, line)
			kinds = append(kinds, KindIdle)
		}
	}
	if len(tasks) == 0 {
		return
	}
	s.runLinesMixed(ctx, tasks, kinds, settings)
}

// runLines probes the same set of lines with one kind.
func (s *Service) runLines(ctx context.Context, lines []Line, kind Kind, settings Settings) []Result {
	kinds := make([]Kind, len(lines))
	for index := range kinds {
		kinds[index] = kind
	}
	return s.runLinesMixed(ctx, lines, kinds, settings)
}

// runLinesMixed probes lines with their own kind apiece, at most
// settings.Concurrency at a time. The answer is ordered by line so a view reads
// one line's result where it expects it.
func (s *Service) runLinesMixed(ctx context.Context, lines []Line, kinds []Kind, settings Settings) []Result {
	if len(lines) == 0 {
		return nil
	}
	concurrency := settings.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(lines) {
		concurrency = len(lines)
	}

	type job struct {
		line Line
		kind Kind
	}
	jobs := make(chan job)
	results := make([]Result, 0, len(lines))
	var resultsMu sync.Mutex
	var workers sync.WaitGroup

	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range jobs {
				result := s.Prober.Probe(ctx, item.line, item.kind)
				s.record(ctx, result, settings)
				resultsMu.Lock()
				results = append(results, result)
				resultsMu.Unlock()
			}
		}()
	}

	for index, line := range lines {
		kind := KindManual
		if index < len(kinds) {
			kind = kinds[index]
		}
		select {
		case jobs <- job{line: line, kind: kind}:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return results
		}
	}
	close(jobs)
	workers.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].ChannelName != results[j].ChannelName {
			return results[i].ChannelName < results[j].ChannelName
		}
		return results[i].Model < results[j].Model
	})
	return results
}

// record keeps one result and reports it. A probe that fails is a warning: it
// means an account the operator is paying for is not serving.
func (s *Service) record(ctx context.Context, result Result, settings Settings) {
	// A probe is traffic to that account, so it restarts the idle clock.
	s.activity.note(result.ChannelID, result.Model, result.At)
	level := slog.LevelInfo
	if !result.OK {
		level = slog.LevelWarn
	}
	s.logger().Log(ctx, level, "probe_completed",
		"kind", string(result.Kind),
		"channel_id", result.ChannelID,
		"channel", result.ChannelName,
		"model", result.Model,
		"ok", result.OK,
		"reason", result.Reason,
		"status", result.Status,
		"latency_ms", result.LatencyMS,
	)
	// The request log is told about every probe, whether or not this gateway keeps
	// its own probe records: it is the one place that shows what the gateway did
	// and when, and a check it ran on its own belongs there beside client traffic.
	if s.RecordRun != nil {
		s.RecordRun(result)
	}
	if s.Store == nil {
		return
	}
	// The schedule outlives the request that may have triggered it, so the write
	// is not tied to a caller's context.
	writeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.AppendProbeRun(writeContext, result, settings.Keep); err != nil {
		s.logger().Warn("probe_record_failed", "error", err.Error())
	}
}

// activity remembers when each line last carried traffic.
type activity struct {
	mu    sync.Mutex
	last_ map[string]time.Time
}

func newActivity() *activity {
	return &activity{last_: make(map[string]time.Time)}
}

// activityKey identifies one line's idle clock. A line is one upstream key, and
// the model is part of the key because a key that is broken for one model may
// serve the others.
func activityKey(channelID, model string) string {
	return channelID + "\x00" + model
}

func (a *activity) note(channelID, model string, at time.Time) {
	if channelID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.last_[activityKey(channelID, model)] = at
}

func (a *activity) last(channelID, model string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	value, ok := a.last_[activityKey(channelID, model)]
	return value, ok
}
