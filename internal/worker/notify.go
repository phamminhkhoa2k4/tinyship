package worker

// Notification dispatch: a periodic tick claims due queue rows, sends
// each to its configured sinks and records the outcome on the row.
// Producers (deploy, stats, backup) only ever insert; everything here
// is best-effort — a dead endpoint delays a notification, never a
// deploy. Network I/O runs off the job loop in its own goroutine with
// a busy flag, like the stats sampler, so a stuck receiver cannot
// stall job claims.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/tinyship/tinyship/internal/notify"
	"github.com/tinyship/tinyship/internal/store"
)

// DefaultNotifyInterval is how often the worker looks for due
// notification deliveries and host checks (disk floor edge, cert
// expiry). Override with TINYSHIP_NOTIFY_TICK.
const DefaultNotifyInterval = 15 * time.Second

// NotifyHourlyCap is the global delivery ceiling per rolling hour: the
// dispatcher defers (does not drop) once this many events were
// delivered in the window, so a flapping host cannot spam a receiver.
const NotifyHourlyCap = 30

// CertWarnDays is the lead time before certificate expiry that starts
// emitting cert_expiring warnings; CertReFire is how often the same
// hostname may warn again while still inside the window.
const (
	CertWarnDays = 21
	CertReFire   = 7 * 24 * time.Hour
)

const (
	// DiskSampleInterval is how often the trend sampler records the
	// Docker data filesystem; hourly keeps a month of forecast input at
	// a few hundred rows.
	DiskSampleInterval = time.Hour
	// ForecastWindow is how far back the disk forecast fits its trend.
	ForecastWindow = 30 * 24 * time.Hour
	// DiskSampleRetain is how long trend samples are kept.
	DiskSampleRetain = 90 * 24 * time.Hour
)

// dueBatch bounds how many events one tick processes.
const dueBatch = 10

// NotifyDispatcher consumes the notify_events queue.
type NotifyDispatcher struct {
	Store  *store.Store
	Logger *slog.Logger

	// DiskFree and CertExpiry are the host probes; nil means the
	// real readers (statfs of the Docker data path, SNI dial of the
	// local proxy). Tests pin them to a scenario.
	DiskFree   func(path string) (int, error)
	CertExpiry func(host string) (time.Time, error)
	// DiskUsage reads free/total for the hourly trend sample; nil
	// means the real statfs of the Docker data path.
	DiskUsage func(path string) (int64, int64, error)

	client *http.Client

	// Edge state for the disk checks and the cert re-fire pacing;
	// only touched inside the busy-guarded tick goroutine.
	diskLow            bool
	diskForecastWarned bool
	lastCertCheck      time.Time
	lastDiskSample     time.Time
}

// NewNotifyDispatcher wires a dispatcher over the store.
func NewNotifyDispatcher(s *store.Store, log *slog.Logger) *NotifyDispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &NotifyDispatcher{
		Store:  s,
		Logger: log,
		client: &http.Client{
			// One request must never hold the tick: a receiver that
			// accepts TCP and stalls fails here and is retried.
			Timeout: 10 * time.Second,
		},
	}
}

// RunOnce performs one dispatch tick: host checks first (they only
// enqueue), then delivery of due rows.
func (d *NotifyDispatcher) RunOnce(ctx context.Context) {
	d.checkDisk(ctx)
	d.sampleDisk(ctx)
	d.checkDiskForecast(ctx)
	d.checkCerts(ctx)

	capFrom := time.Now().UTC().Add(-time.Hour)
	n, err := d.Store.CountNotifyDeliveredSince(ctx, capFrom)
	if err != nil {
		d.Logger.Warn("worker: notify cap check failed", "err", err)
		return
	}
	if n >= NotifyHourlyCap {
		// Defer, not drop: rows stay pending and are claimed on a
		// later tick once the window slides.
		d.Logger.Debug("worker: notify hourly cap reached, deferring",
			"delivered", n, "cap", NotifyHourlyCap)
		return
	}

	due, err := d.Store.DueNotifyEvents(ctx, time.Now().UTC(), dueBatch)
	if err != nil {
		d.Logger.Warn("worker: notify claim failed", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	sinks, err := d.loadSinks(ctx)
	if err != nil {
		d.Logger.Warn("worker: notify sinks unreadable", "err", err)
		return
	}
	for _, ev := range due {
		d.deliver(ctx, ev, sinks)
	}
}

func (d *NotifyDispatcher) loadSinks(ctx context.Context) ([]notify.Sink, error) {
	raw, err := d.Store.GetSetting(ctx, notify.SettingKey)
	if err != nil {
		return nil, err
	}
	return notify.ParseSinks(raw)
}

// deliver sends one event to the sinks that have not accepted it yet.
func (d *NotifyDispatcher) deliver(ctx context.Context, ev *store.NotifyEvent, sinks []notify.Sink) {
	var matching int
	for _, s := range sinks {
		if matchesTarget(ev, s) {
			matching++
		}
	}
	target := targetsFor(ev, sinks)
	if len(target) == 0 {
		if matching > 0 {
			// Every matching sink already accepted the event on an
			// earlier attempt (partial progress), so the row is done.
			if err := d.Store.MarkNotifyDelivered(ctx, ev.ID, ev.SentTo); err != nil {
				d.Logger.Warn("worker: notify mark delivered failed", "id", ev.ID, "err", err)
			}
			return
		}
		// Nothing can ever take this event: no sinks at all, or the
		// event's target_kind names a kind nobody configured. Record
		// the cause so notify list shows why it is not arriving.
		cause := "no notification sinks configured"
		if len(sinks) > 0 {
			cause = "no sink configured for the event's target kind"
		}
		if err := d.Store.NotifyRetry(ctx, ev.ID, ev.SentTo, cause); err != nil {
			d.Logger.Warn("worker: notify fail failed", "id", ev.ID, "err", err)
		}
		return
	}

	nde, err := toNotifyEvent(ev)
	if err != nil {
		d.Logger.Warn("worker: notify detail unreadable", "id", ev.ID, "err", err)
		return
	}
	sent := append([]string{}, ev.SentTo...)
	var firstErr error
	for _, sink := range target {
		if err := notify.Send(ctx, d.client, sink, nde, sink.Secret); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", sink.Kind, err)
			}
			continue
		}
		sent = append(sent, sink.Kind)
	}
	if firstErr == nil {
		if err := d.Store.MarkNotifyDelivered(ctx, ev.ID, sent); err != nil {
			d.Logger.Warn("worker: notify mark delivered failed", "id", ev.ID, "err", err)
		}
		return
	}
	if err := d.Store.NotifyRetry(ctx, ev.ID, sent, firstErr.Error()); err != nil {
		d.Logger.Warn("worker: notify retry record failed", "id", ev.ID, "err", err)
	}
	d.Logger.Info("worker: notify delivery failed, will retry",
		"id", ev.ID, "type", ev.Type, "attempts", ev.Attempts+1, "err", firstErr)
}

// targetsFor returns the sinks this attempt should hit: matching the
// event's optional target_kind and not already recorded in sent_to.
func targetsFor(ev *store.NotifyEvent, sinks []notify.Sink) []notify.Sink {
	var out []notify.Sink
	for _, s := range sinks {
		if matchesTarget(ev, s) && !containsKind(ev.SentTo, s.Kind) {
			out = append(out, s)
		}
	}
	return out
}

// matchesTarget honors a test event's detail.target_kind filter: an
// empty filter targets every sink.
func matchesTarget(ev *store.NotifyEvent, s notify.Sink) bool {
	var detail struct {
		TargetKind string `json:"target_kind"`
	}
	_ = json.Unmarshal([]byte(ev.Detail), &detail)
	if detail.TargetKind == "" {
		return true
	}
	return detail.TargetKind == s.Kind
}

func containsKind(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func toNotifyEvent(ev *store.NotifyEvent) (notify.Event, error) {
	var detail map[string]any
	if err := json.Unmarshal([]byte(ev.Detail), &detail); err != nil {
		return notify.Event{}, err
	}
	return notify.Event{
		ID:       ev.ID,
		Type:     ev.Type,
		AppName:  ev.AppName,
		Title:    ev.Title,
		Detail:   detail,
		DupCount: ev.DupCount,
	}, nil
}

// checkDisk is the edge-triggered low-disk warning: it fires once when
// free space crosses below the admission floor and re-arms only after
// the floor is met again, so a persistently low disk warns once
// instead of every tick.
func (d *NotifyDispatcher) checkDisk(ctx context.Context) {
	freeFn := d.DiskFree
	if freeFn == nil {
		freeFn = statfsFreeMB
	}
	free, err := freeFn(DockerDataPath)
	if err != nil {
		d.Logger.Debug("worker: disk probe failed", "err", err)
		return
	}
	settings, err := d.Store.GetBudgetSettings(ctx)
	if err != nil {
		d.Logger.Warn("worker: budget settings unreadable", "err", err)
		return
	}
	low := free < settings.MinFreeDiskMB
	if low && !d.diskLow {
		if _, err := d.Store.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
			Type:  "disk_low",
			Title: "Disk space low",
			Detail: map[string]any{
				"free_mb": free, "floor_mb": settings.MinFreeDiskMB,
			},
			DedupKey: "disk_low",
		}); err != nil {
			d.Logger.Warn("worker: enqueue disk_low failed", "err", err)
		}
	}
	d.diskLow = low
}

// sampleDisk records the hourly free/total observation the forecast
// fits through, and prunes samples past the retention window. Paced by
// the dispatcher's own clock; a failed read retries next tick.
func (d *NotifyDispatcher) sampleDisk(ctx context.Context) {
	if time.Since(d.lastDiskSample) < DiskSampleInterval {
		return
	}
	usage := d.DiskUsage
	if usage == nil {
		usage = statfsUsageMB
	}
	free, total, err := usage(DockerDataPath)
	if err != nil {
		d.Logger.Debug("worker: disk sample probe failed", "err", err)
		return
	}
	now := time.Now().UTC()
	if err := d.Store.AppendDiskSample(ctx, store.DiskSample{
		At: now, FreeMB: free, TotalMB: total,
	}); err != nil {
		d.Logger.Warn("worker: disk sample append failed", "err", err)
		return
	}
	d.lastDiskSample = now
	if err := d.Store.PruneDiskSamples(ctx, now.Add(-DiskSampleRetain)); err != nil {
		d.Logger.Warn("worker: disk sample prune failed", "err", err)
	}
}

// checkDiskForecast is the edge-triggered fill-up warning: it fires
// once when the fitted days-until-full drops under the configured lead
// time and re-arms only when the estimate climbs back, so a disk that
// hovers near the threshold warns once instead of every tick. The
// estimate is a range from two fits (whole window and recent half);
// the pessimistic bound crossing the threshold is what arms it.
func (d *NotifyDispatcher) checkDiskForecast(ctx context.Context) {
	settings, err := d.Store.GetBudgetSettings(ctx)
	if err != nil {
		d.Logger.Warn("worker: budget settings unreadable", "err", err)
		return
	}
	samples, err := d.Store.ListDiskSamples(ctx, time.Now().UTC().Add(-ForecastWindow))
	if err != nil {
		d.Logger.Warn("worker: list disk samples failed", "err", err)
		return
	}
	if len(samples) == 0 {
		return
	}
	fc := ForecastDisk(samples, samples[len(samples)-1].FreeMB)
	if !fc.Enough {
		return
	}
	warn := fc.Filling && fc.DaysLow < float64(settings.DiskForecastDays)
	if warn && !d.diskForecastWarned {
		if _, err := d.Store.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
			Type:  "disk_forecast",
			Title: "Disk projected to fill soon",
			Detail: map[string]any{
				"days_low":       round1(fc.DaysLow),
				"days_high":      round1(fc.DaysHigh),
				"threshold_days": settings.DiskForecastDays,
				"free_mb":        samples[len(samples)-1].FreeMB,
				"total_mb":       samples[len(samples)-1].TotalMB,
			},
			DedupKey: "disk_forecast",
		}); err != nil {
			d.Logger.Warn("worker: enqueue disk_forecast failed", "err", err)
		}
	}
	d.diskForecastWarned = warn
}

// checkCerts probes each active hostname's served certificate over a
// local SNI dial (the proxy's certificate directory is not readable by
// this user; the handshake answers the same question without new
// permissions) and warns inside the expiry window. Paced to twice a
// day; re-fire suppression lives in the queue's dedup window.
func (d *NotifyDispatcher) checkCerts(ctx context.Context) {
	if time.Since(d.lastCertCheck) < 12*time.Hour {
		return
	}
	d.lastCertCheck = time.Now()
	expiryFn := d.CertExpiry
	if expiryFn == nil {
		expiryFn = SNIExpiry
	}
	hosts, err := d.Store.ListActiveHostnames(ctx)
	if err != nil {
		d.Logger.Warn("worker: list hostnames for cert check failed", "err", err)
		return
	}
	for _, host := range hosts {
		notAfter, err := expiryFn(host)
		if err != nil {
			// No certificate yet (pending DNS, on-demand issuance) is
			// not a warning condition.
			d.Logger.Debug("worker: cert probe failed", "host", host, "err", err)
			continue
		}
		days := int(time.Until(notAfter).Hours() / 24)
		if days >= CertWarnDays {
			continue
		}
		if _, err := d.Store.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
			Type:  "cert_expiring",
			Title: "Certificate expiring",
			Detail: map[string]any{
				"hostname": host, "expires_in_days": days,
				"expires_at": notAfter.UTC().Format(time.RFC3339),
			},
			DedupKey:    "cert_expiring:" + host,
			DedupWindow: CertReFire,
		}); err != nil {
			d.Logger.Warn("worker: enqueue cert_expiring failed", "err", err)
		}
	}
}

// SNIExpiry reads the certificate the local proxy serves for host by
// dialing loopback with that host as SNI. Verification is skipped on
// purpose: the question is "what expiry does the served chain carry",
// and an operator-internal loopback handshake proves nothing about
// public trust either way.
func SNIExpiry(host string) (time.Time, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", "127.0.0.1:443",
		&tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, errors.New("no peer certificate")
	}
	return certs[0].NotAfter, nil
}
