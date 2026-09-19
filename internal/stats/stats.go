// Package stats keeps in-memory token counters and fans out live updates.
//
// Design notes:
//   - All counters live behind a single mutex; contention is irrelevant at the
//     request rates a personal gateway sees, and it keeps the invariants easy
//     to reason about.
//   - Subscribers receive on a buffered channel and are dropped rather than
//     blocked when slow. A lagging dashboard must never stall a proxied LLM
//     request, which is the one thing this gateway must not do.
package stats

import (
	"sort"
	"sync"
	"time"

	"llmtools/internal/usage"
)

// RequestEvent is one completed proxied request, as shown in the live feed.
type RequestEvent struct {
	ID               int64  `json:"id"`
	Time             string `json:"time"`
	Model            string `json:"model"`
	StatusCode       int    `json:"status_code"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	ReasoningTokens  int    `json:"reasoning_tokens"`
	Stream           bool   `json:"stream"`
	DurationMS       int64  `json:"duration_ms"`
	WaitMS           int64  `json:"wait_ms"`
	UsageReported    bool   `json:"usage_reported"`

	// Client and Upstream attribute usage to a workbench and a provider. Both
	// matter once one gateway serves several people: "who spent this" is a
	// different question from "which provider served it".
	Client   string `json:"client"`
	Upstream string `json:"upstream"`
}

// ClientTotals aggregates usage for one workbench key.
type ClientTotals struct {
	Client           string `json:"client"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	Errors           int64  `json:"errors"`
	AvgDurationMS    int64  `json:"avg_duration_ms"`
}

// ModelTotals aggregates usage for one model.
type ModelTotals struct {
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	Errors           int64  `json:"errors"`
	// AvgDurationMS is the mean wall-clock time per completed request.
	AvgDurationMS int64 `json:"avg_duration_ms"`
}

// Snapshot is the full state pushed to dashboards.
type Snapshot struct {
	StartedAt        string          `json:"started_at"`
	UptimeSeconds    int64           `json:"uptime_seconds"`
	Requests         int64           `json:"requests"`
	Errors           int64           `json:"errors"`
	InFlight         int64           `json:"in_flight"`
	PromptTokens     int64           `json:"prompt_tokens"`
	CompletionTokens int64           `json:"completion_tokens"`
	TotalTokens      int64           `json:"total_tokens"`
	CachedTokens     int64           `json:"cached_tokens"`
	ReasoningTokens  int64           `json:"reasoning_tokens"`
	TokensPerMinute  float64         `json:"tokens_per_minute"`
	RequestsPerMin   float64         `json:"requests_per_minute"`
	Models           []ModelTotals   `json:"models"`
	Clients          []ClientTotals  `json:"clients"`
	Upstreams        []UpstreamTotal `json:"upstreams"`
	Recent           []RequestEvent  `json:"recent"`
}

// UpstreamTotal aggregates usage for one upstream provider.
type UpstreamTotal struct {
	Upstream         string `json:"upstream"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	Errors           int64  `json:"errors"`
}

// maxRecent bounds the rolling request feed. Old entries are discarded so a
// long-running gateway has a flat memory profile.
const maxRecent = 200

// subBuffer is how many snapshots a subscriber may fall behind before it is
// considered dead and dropped.
const subBuffer = 16

// Store is the concurrency-safe token ledger.
type Store struct {
	mu       sync.Mutex
	started  time.Time
	nextID   int64
	totals   ModelTotals
	byModel  map[string]*ModelTotals
	byClient map[string]*ClientTotals
	byUp     map[string]*UpstreamTotal
	recent   []RequestEvent
	inFlight int64

	subs map[chan Snapshot]struct{}
}

// New creates an empty Store.
func New() *Store {
	return &Store{
		started:  time.Now(),
		byModel:  make(map[string]*ModelTotals),
		byClient: make(map[string]*ClientTotals),
		byUp:     make(map[string]*UpstreamTotal),
		recent:   make([]RequestEvent, 0, maxRecent),
		subs:     make(map[chan Snapshot]struct{}),
	}
}

// Attribution carries the dimensions a finished request is recorded against.
type Attribution struct {
	Client   string
	Upstream string
}

// Begin records that a request entered the gateway and returns it for later
// completion via Finish.
func (s *Store) Begin() {
	s.mu.Lock()
	s.inFlight++
	s.mu.Unlock()
}

// Finish folds a completed request into the ledger and notifies subscribers.
func (s *Store) Finish(rec usage.Record, statusCode int, dur time.Duration, wait time.Duration, attr Attribution) RequestEvent {
	s.mu.Lock()

	if s.inFlight > 0 {
		s.inFlight--
	}
	s.nextID++

	model := rec.Model
	if model == "" {
		model = "unknown"
	}
	client := attr.Client
	if client == "" {
		client = "anonymous"
	}
	// A rejected request never reached an upstream, so it must not be recorded
	// against one. Attributing it to the default upstream would make the
	// per-upstream token totals read as if that provider had served it, and
	// would inflate that provider's request count with failures.
	upstream := attr.Upstream
	noUpstream := upstream == ""
	if noUpstream {
		upstream = "(none)"
	}

	ev := RequestEvent{
		ID:               s.nextID,
		Time:             time.Now().Format(time.RFC3339),
		Model:            model,
		StatusCode:       statusCode,
		PromptTokens:     rec.PromptTokens,
		CompletionTokens: rec.CompletionTokens,
		TotalTokens:      rec.TotalTokens,
		CachedTokens:     rec.CachedTokens,
		ReasoningTokens:  rec.ReasoningTokens,
		Stream:           rec.Stream,
		DurationMS:       dur.Milliseconds(),
		WaitMS:           wait.Milliseconds(),
		UsageReported:    rec.Known(),
		Client:           client,
		Upstream:         upstream,
	}

	s.totals.Requests++
	s.totals.PromptTokens += int64(rec.PromptTokens)
	s.totals.CompletionTokens += int64(rec.CompletionTokens)
	s.totals.TotalTokens += int64(rec.TotalTokens)
	s.totals.CachedTokens += int64(rec.CachedTokens)
	s.totals.ReasoningTokens += int64(rec.ReasoningTokens)

	mt := s.byModel[model]
	if mt == nil {
		mt = &ModelTotals{Model: model}
		s.byModel[model] = mt
	}
	mt.Requests++
	mt.PromptTokens += int64(rec.PromptTokens)
	mt.CompletionTokens += int64(rec.CompletionTokens)
	mt.TotalTokens += int64(rec.TotalTokens)
	mt.CachedTokens += int64(rec.CachedTokens)
	mt.ReasoningTokens += int64(rec.ReasoningTokens)

	ct := s.byClient[client]
	if ct == nil {
		ct = &ClientTotals{Client: client}
		s.byClient[client] = ct
	}
	ct.Requests++
	ct.PromptTokens += int64(rec.PromptTokens)
	ct.CompletionTokens += int64(rec.CompletionTokens)
	ct.TotalTokens += int64(rec.TotalTokens)
	ct.CachedTokens += int64(rec.CachedTokens)
	ct.ReasoningTokens += int64(rec.ReasoningTokens)

	ut := s.byUp[upstream]
	if ut == nil {
		ut = &UpstreamTotal{Upstream: upstream}
		s.byUp[upstream] = ut
	}
	ut.Requests++
	ut.PromptTokens += int64(rec.PromptTokens)
	ut.CompletionTokens += int64(rec.CompletionTokens)
	ut.TotalTokens += int64(rec.TotalTokens)

	// 4xx/5xx responses still consume tokens in most providers, so they are
	// counted in the token totals but tracked separately as errors.
	if statusCode >= 400 {
		s.totals.Errors++
		mt.Errors++
		ct.Errors++
		ut.Errors++
	}
	mt.AvgDurationMS = mt.AvgDurationMS + (ev.DurationMS-mt.AvgDurationMS)/mt.Requests
	ct.AvgDurationMS = ct.AvgDurationMS + (ev.DurationMS-ct.AvgDurationMS)/ct.Requests

	s.recent = append([]RequestEvent{ev}, s.recent...)
	if len(s.recent) > maxRecent {
		s.recent = s.recent[:maxRecent]
	}

	snap := s.snapshotLocked()
	s.broadcastLocked(snap)

	s.mu.Unlock()
	return ev
}

// Snapshot returns the current ledger state.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() Snapshot {
	uptime := time.Since(s.started)
	snap := Snapshot{
		StartedAt:        s.started.Format(time.RFC3339),
		UptimeSeconds:    int64(uptime.Seconds()),
		Requests:         s.totals.Requests,
		Errors:           s.totals.Errors,
		InFlight:         s.inFlight,
		PromptTokens:     s.totals.PromptTokens,
		CompletionTokens: s.totals.CompletionTokens,
		TotalTokens:      s.totals.TotalTokens,
		CachedTokens:     s.totals.CachedTokens,
		ReasoningTokens:  s.totals.ReasoningTokens,
	}

	// Rates are averaged over the process lifetime rather than a sliding
	// window. For a dashboard the lifetime average is both cheaper and less
	// jittery; it converges to the true rate as uptime grows.
	if mins := uptime.Minutes(); mins > 0 {
		snap.TokensPerMinute = float64(snap.TotalTokens) / mins
		snap.RequestsPerMin = float64(snap.Requests) / mins
	}

	snap.Models = make([]ModelTotals, 0, len(s.byModel))
	for _, mt := range s.byModel {
		snap.Models = append(snap.Models, *mt)
	}
	sort.Slice(snap.Models, func(i, j int) bool {
		if snap.Models[i].TotalTokens != snap.Models[j].TotalTokens {
			return snap.Models[i].TotalTokens > snap.Models[j].TotalTokens
		}
		return snap.Models[i].Model < snap.Models[j].Model
	})

	snap.Clients = make([]ClientTotals, 0, len(s.byClient))
	for _, ct := range s.byClient {
		snap.Clients = append(snap.Clients, *ct)
	}
	sort.Slice(snap.Clients, func(i, j int) bool {
		if snap.Clients[i].TotalTokens != snap.Clients[j].TotalTokens {
			return snap.Clients[i].TotalTokens > snap.Clients[j].TotalTokens
		}
		return snap.Clients[i].Client < snap.Clients[j].Client
	})

	snap.Upstreams = make([]UpstreamTotal, 0, len(s.byUp))
	for _, ut := range s.byUp {
		snap.Upstreams = append(snap.Upstreams, *ut)
	}
	sort.Slice(snap.Upstreams, func(i, j int) bool {
		if snap.Upstreams[i].TotalTokens != snap.Upstreams[j].TotalTokens {
			return snap.Upstreams[i].TotalTokens > snap.Upstreams[j].TotalTokens
		}
		return snap.Upstreams[i].Upstream < snap.Upstreams[j].Upstream
	})

	snap.Recent = append([]RequestEvent(nil), s.recent...)
	return snap
}

func (s *Store) broadcastLocked(snap Snapshot) {
	for ch := range s.subs {
		select {
		case ch <- snap:
		default:
			// Subscriber is too slow to keep up (a browser tab on a stalled
			// connection). Drop it instead of blocking the request path.
			close(ch)
			delete(s.subs, ch)
		}
	}
}

// Subscribe registers a listener for live snapshots. The returned cancel
// function must be called to release the subscription.
func (s *Store) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, subBuffer)

	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if _, ok := s.subs[ch]; ok {
				delete(s.subs, ch)
				close(ch)
			}
			s.mu.Unlock()
		})
	}
	return ch, cancel
}
