package worker

// Resource stats: the worker samples managed containers and stores the
// last memory/OOM observation per app. The API and CLI read the sample
// through the worker socket; live CPU comes from the same sample, so a
// request costs one daemon read per running container and nothing when
// no app is running.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// NearLimitPercent is where an app's memory use is called close to its
// configured limit.
const NearLimitPercent = 80

// StatsInterval is how often the worker refreshes stored memory and OOM
// observations while apps run.
const StatsInterval = 30 * time.Second

// AppStats is one app's resource view.
type AppStats struct {
	AppID        string    `json:"app_id"`
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Health       string    `json:"health,omitempty"`
	MemUsageMB   int       `json:"mem_usage_mb"`
	MemLimitMB   int       `json:"mem_limit_mb"`
	CPUPercent   float64   `json:"cpu_percent"`
	NearLimit    bool      `json:"near_limit"`
	OOMKillCount int       `json:"oom_kill_count"`
	SampledAt    time.Time `json:"sampled_at"`

	// sampled marks a fresh daemon stats read behind this view, and
	// memBytes keeps the untruncated reading; both stay unexported so
	// the socket payload is unchanged.
	sampled  bool
	memBytes uint64
}

// StatsStore is the store slice the sampler needs.
type StatsStore interface {
	ListApps(ctx context.Context) ([]*store.App, error)
	ListContainerStates(ctx context.Context) (map[string]*store.ContainerState, error)
	UpsertContainerState(ctx context.Context, in store.ContainerState) error
	AppendMetricSample(ctx context.Context, in store.MetricSample) error
	RollupMetricSamples(ctx context.Context, rawBefore time.Time) error
	PruneMetricSamples(ctx context.Context, appID string, before time.Time) error
	EnqueueNotifyEvent(ctx context.Context, in store.NotifyEventInput) (int64, error)
}

// StatsSampler measures managed containers and persists the observations.
// One instance is shared by the worker loop and the socket handler so
// their read-modify-write cycles do not interleave.
type StatsSampler struct {
	Store  StatsStore
	Docker Docker
	Logger *slog.Logger

	mu sync.Mutex
	// lastSample gates each app's history append to its configured
	// interval; lastSweep paces the hourly rollup/prune pass. Both are
	// in-memory on purpose: a restart re-samples on the next tick,
	// which costs at most one duplicated row.
	lastSample map[string]time.Time
	lastSweep  time.Time
}

// Metrics history policy: raw samples stay raw for the raw window, then
// the sweep folds them into hourly rows; the sweep itself runs hourly on
// the sampling tick.
const (
	metricsRawWindow  = 48 * time.Hour
	metricsSweepEvery = time.Hour
)

// NewStatsSampler wires the sampler over a store and the Docker wrapper.
func NewStatsSampler(s StatsStore, d Docker, log *slog.Logger) *StatsSampler {
	if log == nil {
		log = slog.Default()
	}
	return &StatsSampler{
		Store: s, Docker: d, Logger: log,
		lastSample: map[string]time.Time{},
	}
}

// Sample reads every managed container once and returns the app views in
// app-name order as stored. A Docker read failure for one app leaves its
// last stored observation in place instead of failing the whole view.
func (s *StatsSampler) Sample(ctx context.Context) ([]AppStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	apps, err := s.Store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	previous, err := s.Store.ListContainerStates(ctx)
	if err != nil {
		return nil, err
	}
	infos, err := s.Docker.ListManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	byApp := make(map[string]docker.ContainerInfo, len(infos))
	for _, info := range infos {
		appID := info.Labels[policy.LabelApp]
		if appID == "" {
			continue
		}
		if current, ok := byApp[appID]; ok && current.State == "running" {
			continue // a running container wins over an exited one
		}
		byApp[appID] = info
	}

	now := time.Now().UTC()
	out := make([]AppStats, len(apps))
	var wg sync.WaitGroup
	for i, app := range apps {
		st := AppStats{
			AppID: app.ID, Name: app.Name, State: "missing", SampledAt: now,
			MemLimitMB: appMemLimitMB(app.ConfigJSON),
		}
		prev := previous[app.ID]
		if prev != nil {
			st.OOMKillCount = prev.OOMKillCount
			st.MemUsageMB = prev.MemUsageMB
		}
		out[i] = st
		if info, ok := byApp[app.ID]; ok {
			// The daemon's one-shot read waits for a sampling window, so
			// collect containers concurrently; each goroutine owns one
			// slice element and no store write happens until they join.
			wg.Add(1)
			go func(i int, prev *store.ContainerState, info docker.ContainerInfo) {
				defer wg.Done()
				s.fillContainer(ctx, &out[i], prev, info)
			}(i, prev, info)
		}
	}
	wg.Wait()

	for i, app := range apps {
		st := &out[i]
		if st.MemLimitMB > 0 {
			st.NearLimit = st.MemUsageMB*100 >= st.MemLimitMB*NearLimitPercent
		}
		if err := s.Store.UpsertContainerState(ctx, store.ContainerState{
			AppID:        st.AppID,
			ContainerID:  byApp[app.ID].ID,
			State:        st.State,
			Health:       st.Health,
			MemUsageMB:   st.MemUsageMB,
			OOMKillCount: st.OOMKillCount,
			UpdatedAt:    now,
		}); err != nil {
			s.Logger.Warn("worker: cannot store container stats", "app", app.Name, "err", err)
		}
		if st.sampled {
			s.recordMetric(ctx, app, st, now)
		}
		// A rising OOM counter is the event: one notification per
		// observed kill, never one per sample of a killed container.
		prevKills := 0
		if prev := previous[app.ID]; prev != nil {
			prevKills = prev.OOMKillCount
		}
		if st.OOMKillCount > prevKills {
			if _, err := s.Store.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
				Type:    "oom",
				AppName: app.Name,
				Title:   "App killed out of memory",
				Detail: map[string]any{
					"oom_count": st.OOMKillCount,
					"limit_mb":  st.MemLimitMB,
					"usage_mb":  st.MemUsageMB,
					"container": st.State,
				},
				DedupKey: "oom:" + app.ID,
			}); err != nil {
				s.Logger.Warn("worker: enqueue oom notification failed",
					"app", app.Name, "err", err)
			}
		}
	}
	s.sweepMetrics(ctx, apps, now)
	return out, nil
}

// recordMetric appends one history row when the app has collection
// enabled and its interval has elapsed. Only a fresh daemon reading is
// recorded: a stopped or unreadable container has no usage to store, so
// sleeping apps simply pause their history instead of freezing it.
func (s *StatsSampler) recordMetric(ctx context.Context, app *store.App, st *AppStats, now time.Time) {
	cfg, err := appconfig.Parse([]byte(app.ConfigJSON))
	if err != nil || !cfg.MetricsEnabled() {
		return
	}
	if last, ok := s.lastSample[app.ID]; ok && now.Sub(last) < cfg.MetricsInterval() {
		return
	}
	if err := s.Store.AppendMetricSample(ctx, store.MetricSample{
		AppID:         app.ID,
		At:            now,
		CPUPercent:    st.CPUPercent,
		MemBytes:      int64(st.memBytes),
		MemLimitBytes: int64(st.MemLimitMB) << 20,
	}); err != nil {
		s.Logger.Warn("worker: cannot store metric sample", "app", app.Name, "err", err)
		return
	}
	s.lastSample[app.ID] = now
}

// sweepMetrics folds raw rows past the raw window into hourly rollups
// and prunes each app past its own retention cap. It runs at most once
// per metricsSweepEvery on the sampling tick; a failed sweep is logged
// and retried on the next due tick, so no history is lost by skipping.
func (s *StatsSampler) sweepMetrics(ctx context.Context, apps []*store.App, now time.Time) {
	if now.Sub(s.lastSweep) < metricsSweepEvery {
		return
	}
	s.lastSweep = now
	if err := s.Store.RollupMetricSamples(ctx, now.Add(-metricsRawWindow)); err != nil {
		s.Logger.Warn("worker: metric rollup failed", "err", err)
	}
	for _, app := range apps {
		cfg, err := appconfig.Parse([]byte(app.ConfigJSON))
		if err != nil {
			continue
		}
		if err := s.Store.PruneMetricSamples(ctx, app.ID, now.Add(-cfg.MetricsRetention())); err != nil {
			s.Logger.Warn("worker: metric prune failed", "app", app.Name, "err", err)
		}
	}
}

// fillContainer updates one app view from a managed container. Errors are
// logged and leave the previous memory observation in place: a stopped or
// unreadable container is still reported, just without a fresh sample.
func (s *StatsSampler) fillContainer(ctx context.Context, st *AppStats,
	prev *store.ContainerState, info docker.ContainerInfo) {
	st.State = info.State

	inspected, err := s.Docker.InspectContainer(ctx, info.ID)
	if err != nil {
		st.OOMKillCount = s.oomCount(ctx, prev, info)
		s.Logger.Warn("worker: cannot inspect container for stats",
			"container", info.ID, "err", err)
		return
	}
	st.State = inspected.State
	st.Health = inspected.Health
	st.OOMKillCount = s.oomCount(ctx, prev, *inspected)
	if inspected.State != "running" {
		return
	}
	sample, err := s.Docker.Stats(ctx, info.ID)
	if err != nil {
		s.Logger.Warn("worker: cannot sample container stats",
			"container", info.ID, "err", err)
		return
	}
	st.MemUsageMB = int(sample.MemUsageBytes / (1 << 20))
	st.CPUPercent = sample.CPUPercent
	st.memBytes = sample.MemUsageBytes
	st.sampled = true
}

// oomCount accumulates OOM kills across container replacements. The
// daemon reports only whether the last run was OOM-killed, so an event is
// counted at the transition: once for the old container if it is still
// inspectable, and once when the current container is seen killed.
func (s *StatsSampler) oomCount(ctx context.Context, prev *store.ContainerState,
	current docker.ContainerInfo) int {
	count := 0
	if prev != nil {
		count = prev.OOMKillCount
		if prev.ContainerID != "" && prev.ContainerID != current.ID &&
			prev.State != "exited" && prev.State != "dead" {
			// The previous container was replaced while we last saw it
			// running; if it died of OOM and the event was never sampled,
			// count it now. An already-observed exit was counted then, so
			// re-inspecting it must not add a second kill.
			if old, err := s.Docker.InspectContainer(ctx, prev.ContainerID); err == nil && old.OOMKilled {
				count++
			}
		}
	}
	if !current.OOMKilled {
		return count
	}
	switch {
	case prev == nil:
		return count + 1
	case prev.ContainerID != current.ID:
		return count + 1
	case prev.State != "exited" && prev.State != "dead":
		return count + 1
	}
	return count
}

// appMemLimitMB returns the configured memory limit, or 0 when the stored
// config cannot be read (the app view then omits the near-limit verdict).
func appMemLimitMB(configJSON string) int {
	cfg, err := appconfig.Parse([]byte(configJSON))
	if err != nil || cfg.Resources.MemoryMB <= 0 {
		return 0
	}
	return cfg.Resources.MemoryMB
}
