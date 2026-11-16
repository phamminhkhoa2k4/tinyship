package worker

// Right-sizing math and the disk-fill forecast: pure functions over
// observed data, shared by the API (suggestion view, budget view) and
// the worker's alert dispatch. No I/O — callers bring the samples.

import (
	"math"
	"time"

	"github.com/tinyship/tinyship/internal/store"
)

const (
	// RightsizeWindowDays is the observation window a suggestion is
	// computed over, and the span a window must cover before its data
	// counts as mature.
	RightsizeWindowDays = 7

	// RightsizeMarginNum/Den express the safety margin: suggested =
	// peak × 5/4 (peak plus 25 percent).
	RightsizeMarginNum = 5
	RightsizeMarginDen = 4

	// RightsizeRoundMB is the granularity of a suggestion; limits are
	// round numbers an operator can reason about.
	RightsizeRoundMB = 16

	// RightsizeMinMB is the floor for an app's memory limit; a tiny
	// peak still suggests at least this.
	RightsizeMinMB = 32

	// forecastMinSamples and forecastMinSpan bound when a trend may be
	// stated at all, and forecastMinRecent is how many samples the
	// recent-half fit needs to give the estimate a real uncertainty
	// range instead of a false-precision point.
	forecastMinSamples = 3
	forecastMinSpan    = 48 * time.Hour
)

// SuggestMemoryMB turns an observed peak (bytes) into a suggested
// memory limit in MiB: the peak plus 25 percent, rounded up to the
// next 16 MiB, floored at the documented minimum. By construction the
// result is never below the observed peak — the invariant
// right-sizing must not break.
func SuggestMemoryMB(peakBytes int64) int {
	peakMB := int((peakBytes + (1 << 20) - 1) >> 20)              // ceil bytes → MiB
	suggested := peakMB * RightsizeMarginNum / RightsizeMarginDen // ×1.25
	suggested = (suggested + RightsizeRoundMB - 1) / RightsizeRoundMB * RightsizeRoundMB
	if suggested < RightsizeMinMB {
		suggested = RightsizeMinMB
	}
	return suggested
}

// DiskForecast estimates when the disk holding Docker's data fills up,
// as a range rather than a point: one least-squares fit over the whole
// window and one over the recent half of it. The two estimates only
// agree on a stable trajectory, so spread between them is honest
// uncertainty, not decoration.
type DiskForecast struct {
	// Enough reports whether the trend data meets the bar (sample
	// count and span) to state an estimate at all.
	Enough bool
	// Filling is true when both fits show free space decreasing; a
	// flat or growing disk has no estimate to give.
	Filling bool
	// DaysLow and DaysHigh bound the estimate; both are meaningful
	// only when Filling is true.
	DaysLow  float64
	DaysHigh float64
	// Samples and Span describe the evidence behind the estimate.
	Samples int
	Span    time.Duration
}

// ForecastDisk fits free space against time over samples and returns
// the days-until-full range using freeNowMB as the current position.
func ForecastDisk(samples []store.DiskSample, freeNowMB int64) DiskForecast {
	out := DiskForecast{Samples: len(samples)}
	if len(samples) < forecastMinSamples {
		return out
	}
	span := samples[len(samples)-1].At.Sub(samples[0].At)
	out.Span = span
	if span < forecastMinSpan {
		return out
	}
	// The recent-half fit needs at least two points in the second half
	// of the window; without it there is no second opinion to offer.
	mid := samples[0].At.Add(span / 2)
	recent := make([]store.DiskSample, 0, len(samples))
	for _, s := range samples {
		if !s.At.Before(mid) {
			recent = append(recent, s)
		}
	}
	if len(recent) < 2 {
		return out
	}
	out.Enough = true

	fullPerDay := slopePerDay(samples)
	recentPerDay := slopePerDay(recent)
	// Both fits must agree the disk is filling; a plateau means no
	// warning, and re-arming when one fit turns flat keeps a recovered
	// disk from spamming.
	if fullPerDay >= 0 || recentPerDay >= 0 {
		return out
	}
	out.Filling = true
	if freeNowMB < 0 {
		freeNowMB = 0
	}
	daysFull := float64(freeNowMB) / -fullPerDay
	daysRecent := float64(freeNowMB) / -recentPerDay
	out.DaysLow = math.Min(daysFull, daysRecent)
	out.DaysHigh = math.Max(daysFull, daysRecent)
	return out
}

// round1 rounds a day estimate to one decimal for alert and view
// display: enough precision to compare against a threshold, no false
// digits beyond it.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// slopePerDay is the least-squares slope of free space (MiB) against
// time, in MiB per day. Samples are ordered oldest first.
func slopePerDay(samples []store.DiskSample) float64 {
	n := float64(len(samples))
	if n < 2 {
		return 0
	}
	// x in days since the first sample keeps the arithmetic small.
	x0 := samples[0].At
	var sumX, sumY, sumXY, sumXX float64
	for _, s := range samples {
		x := s.At.Sub(x0).Hours() / 24
		y := float64(s.FreeMB)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	den := n*sumXX - sumX*sumX
	if den == 0 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / den
}
