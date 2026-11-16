package worker

// Right-size math: the suggestion must never fall under an observed
// peak, and the disk forecast must refuse to state an estimate before
// the evidence supports one.

import (
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/store"
)

func TestSuggestMemoryMBNeverBelowObservedPeak(t *testing.T) {
	// A spread of peaks in bytes, including awkward non-aligned ones.
	peaks := []int64{
		0, 1, 1 << 20, 10 << 20, 31 << 20, 32 << 20, 100 << 20,
		255 << 20, 256 << 20, 262144000, 123456789, 1<<30 + 12345,
	}
	for _, peak := range peaks {
		got := SuggestMemoryMB(peak)
		peakMB := int((peak + (1 << 20) - 1) >> 20)
		if got < peakMB {
			t.Errorf("SuggestMemoryMB(%d) = %d, below observed peak %d MiB", peak, got, peakMB)
		}
		if got < RightsizeMinMB {
			t.Errorf("SuggestMemoryMB(%d) = %d, below the floor %d", peak, got, RightsizeMinMB)
		}
		if got%RightsizeRoundMB != 0 {
			t.Errorf("SuggestMemoryMB(%d) = %d, not a multiple of %d", peak, got, RightsizeRoundMB)
		}
	}
}

func TestSuggestMemoryMBAddsMargin(t *testing.T) {
	cases := map[int64]int{
		0:                  32,  // floor
		10 << 20:           32,  // 12.5 → 16, then the floor wins
		100 << 20:          128, // 125 → 128
		256 << 20:          320, // 320 exactly
		(int64(300) << 20): 384, // 375 → 384
	}
	for peak, want := range cases {
		if got := SuggestMemoryMB(peak); got != want {
			t.Errorf("SuggestMemoryMB(%d) = %d, want %d", peak, got, want)
		}
	}
}

// trend builds hourly samples where free space was rateMBPerDay higher
// per day in the past (positive rate = the disk is filling).
func trend(startMB int64, rateMBPerDay float64, hours int) []store.DiskSample {
	now := time.Now().UTC().Truncate(time.Hour)
	out := make([]store.DiskSample, 0, hours+1)
	for i := hours; i >= 0; i-- {
		at := now.Add(-time.Duration(i) * time.Hour)
		daysAgo := float64(i) / 24
		out = append(out, store.DiskSample{
			At: at, FreeMB: startMB + int64(rateMBPerDay*daysAgo), TotalMB: 40000,
		})
	}
	return out
}

func TestForecastDiskNeedsMatureData(t *testing.T) {
	if fc := ForecastDisk(nil, 1000); fc.Enough {
		t.Error("no samples: forecast must not be enough")
	}
	// Right count, wrong span: 3 samples over 2 hours.
	short := []store.DiskSample{
		{At: time.Now().Add(-2 * time.Hour), FreeMB: 3000},
		{At: time.Now().Add(-1 * time.Hour), FreeMB: 2500},
		{At: time.Now(), FreeMB: 2000},
	}
	if fc := ForecastDisk(short, 2000); fc.Enough {
		t.Error("2 h span: forecast must not be enough")
	}
	// Mature span but only two points: no second opinion to offer.
	two := []store.DiskSample{
		{At: time.Now().Add(-5 * 24 * time.Hour), FreeMB: 9000},
		{At: time.Now(), FreeMB: 4500},
	}
	if fc := ForecastDisk(two, 4500); fc.Enough {
		t.Error("two points: forecast must not be enough")
	}
}

func TestForecastDiskLinearDecline(t *testing.T) {
	samples := trend(9000, 900, 5*24) // 900 MB/day, free(now) = 9000
	fc := ForecastDisk(samples, 9000)
	if !fc.Enough {
		t.Fatal("mature linear decline must be enough")
	}
	if !fc.Filling {
		t.Fatal("declining disk must be filling")
	}
	// Perfect line: both fits agree, days = 9000/900 = 10.
	if fc.DaysLow < 9.5 || fc.DaysLow > 10.5 {
		t.Errorf("DaysLow = %v, want ≈10", fc.DaysLow)
	}
	if fc.DaysHigh < fc.DaysLow {
		t.Errorf("range inverted: low %v > high %v", fc.DaysLow, fc.DaysHigh)
	}
}

func TestForecastDiskFlatOrGrowingIsNotFilling(t *testing.T) {
	// Flat: enough data, no estimate.
	flat := trend(5000, 0, 5*24)
	if fc := ForecastDisk(flat, 5000); fc.Enough && fc.Filling {
		t.Error("flat disk must not be filling")
	}
	// Growing: same.
	grow := trend(5000, -500, 5*24) // free increases 500 MB/day
	if fc := ForecastDisk(grow, 9500); fc.Filling {
		t.Error("growing disk must not be filling")
	}
	// Declining early, plateaued across the recent half: the recent
	// fit decides, so a recovered disk re-arms instead of warning.
	stale := trend(9000, 900, 5*24)
	for i := len(stale) / 2; i < len(stale); i++ {
		stale[i].FreeMB = stale[len(stale)-1].FreeMB
	}
	if fc := ForecastDisk(stale, stale[len(stale)-1].FreeMB); fc.Filling {
		t.Error("a recent plateau must re-arm the forecast to not-filling")
	}
}
