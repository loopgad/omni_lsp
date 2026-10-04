//go:build soak

package soak

import "testing"

func TestResourceTrendMonotonicGrowthTripsExistingLimits(t *testing.T) {
	windows := make([]resourceWindow, resourceTrendWindows)
	for i := range windows {
		windows[i] = resourceWindow{
			Minute:        int64(i),
			MedianPrivate: uint64(2<<30) + uint64(i)*(64<<20),
			MedianHandles: uint64(1000 + i*120),
		}
	}

	trend := resourceTrend(windows)
	if trend.privateSlopeBytesPerMinute <= 0 || trend.handlesSlopePerMinute <= 0 {
		t.Fatalf("monotonic increases produced non-positive slopes: %+v", trend)
	}
	if trend.privateEstimatedGrowth < float64(resourcePrivateGrowthLimit) {
		t.Fatalf("private estimated growth = %.0f, want at least %d", trend.privateEstimatedGrowth, resourcePrivateGrowthLimit)
	}
	if trend.handlesEstimatedGrowth < float64(resourceHandleGrowthLimit) {
		t.Fatalf("handle estimated growth = %.1f, want at least %d", trend.handlesEstimatedGrowth, resourceHandleGrowthLimit)
	}
	if !resourceTrendExceeded(trend) {
		t.Fatal("monotonic growth above either existing limit did not trip the trend gate")
	}
}

func TestResourceTrendNoisyGrowthTripsDespiteLocalDips(t *testing.T) {
	privateNoiseMiB := []int64{0, 120, -100, 100, -20, 10, -25, 20, -15, 5}
	handleNoise := []int64{0, 200, -100, 100, -35, 45, -30, 20, -15, 5}
	windows := make([]resourceWindow, resourceTrendWindows)
	for i := range windows {
		privateMiB := int64(2<<10) + int64(i*80) + privateNoiseMiB[i]
		windows[i] = resourceWindow{
			Minute:        int64(i),
			MedianPrivate: uint64(privateMiB) << 20,
			MedianHandles: uint64(int64(10000) + int64(i*150) + handleNoise[i]),
		}
	}
	if windows[2].MedianPrivate >= windows[1].MedianPrivate || windows[2].MedianHandles >= windows[1].MedianHandles {
		t.Fatal("fixture must contain local dips to distinguish robust trend detection from strict monotonicity")
	}

	trend := resourceTrend(windows)
	if !resourceTrendExceeded(trend) {
		t.Fatalf("noisy sustained growth did not trip the trend gate: %+v", trend)
	}
}

func TestResourceTrendFlatNoiseDoesNotTrip(t *testing.T) {
	privateNoiseMiB := []int64{0, 2, -1, 1, -2, 1, 0, -1, 2, 0}
	handleNoise := []int64{0, 2, -1, 1, -2, 1, 0, -1, 2, 0}
	windows := make([]resourceWindow, resourceTrendWindows)
	for i := range windows {
		windows[i] = resourceWindow{
			Minute:        int64(i),
			MedianPrivate: uint64(int64(2<<10)+privateNoiseMiB[i]) << 20,
			MedianHandles: uint64(int64(10000) + handleNoise[i]),
		}
	}

	trend := resourceTrend(windows)
	if resourceTrendExceeded(trend) {
		t.Fatalf("flat resource levels with small median noise tripped the trend gate: %+v", trend)
	}
}

func TestResourceTrendSingleSpikeDoesNotTrip(t *testing.T) {
	windows := make([]resourceWindow, resourceTrendWindows)
	for i := range windows {
		windows[i] = resourceWindow{Minute: int64(i), MedianPrivate: 2 << 30, MedianHandles: 10000}
	}
	windows[len(windows)-1].MedianPrivate += 1 << 30
	windows[len(windows)-1].MedianHandles += 4096

	trend := resourceTrend(windows)
	if trend.privateSlopeBytesPerMinute != 0 || trend.handlesSlopePerMinute != 0 {
		t.Fatalf("single endpoint spikes skewed the Theil-Sen slope: %+v", trend)
	}
	if resourceTrendExceeded(trend) {
		t.Fatalf("single endpoint spikes tripped the sustained-growth gate: %+v", trend)
	}
}
