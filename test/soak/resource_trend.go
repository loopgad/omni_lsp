package soak

import "sort"

// This file is deliberately untagged: the resource-trend form checks are
// pure functions and must run in the default suite, not only behind the
// soak build tag (2026-10-25 ruling 3). The heavy real-stdio soak that feeds
// these functions stays tagged.

const (
	resourceTrendWindows       = 10
	resourcePrivateGrowthLimit = uint64(512 << 20)
	resourceHandleGrowthLimit  = uint64(1024)
)

type resourceWindow struct {
	Minute                           int64   `json:"minute"`
	SampleCount                      int     `json:"sample_count"`
	FirstPrivate                     uint64  `json:"first_private_bytes"`
	LastPrivate                      uint64  `json:"last_private_bytes"`
	MedianPrivate                    uint64  `json:"median_private_bytes"`
	MinPrivate                       uint64  `json:"min_private_bytes"`
	MaxPrivate                       uint64  `json:"max_private_bytes"`
	FirstHandles                     uint64  `json:"first_handles"`
	LastHandles                      uint64  `json:"last_handles"`
	MedianHandles                    uint64  `json:"median_handles"`
	MinHandles                       uint64  `json:"min_handles"`
	MaxHandles                       uint64  `json:"max_handles"`
	TrendWindowCount                 int     `json:"trend_window_count,omitempty"`
	TrendFirstPrivate                uint64  `json:"trend_first_private_bytes,omitempty"`
	TrendLastPrivate                 uint64  `json:"trend_last_private_bytes,omitempty"`
	TrendPrivateDelta                int64   `json:"trend_private_delta_bytes,omitempty"`
	TrendFirstHandles                uint64  `json:"trend_first_handles,omitempty"`
	TrendLastHandles                 uint64  `json:"trend_last_handles,omitempty"`
	TrendHandlesDelta                int64   `json:"trend_handles_delta"`
	TrendPrivateSlopeBytesPerMinute  float64 `json:"trend_private_slope_bytes_per_minute"`
	TrendHandlesSlopePerMinute       float64 `json:"trend_handles_slope_per_minute"`
	TrendPrivateEstimatedGrowthBytes float64 `json:"trend_private_estimated_growth_bytes"`
	TrendHandlesEstimatedGrowth      float64 `json:"trend_handles_estimated_growth"`
	PrivateTrendPositive             bool    `json:"private_trend_positive"`
	HandlesTrendPositive             bool    `json:"handles_trend_positive"`
}

type resourceTrendSummary struct {
	firstPrivate, lastPrivate  uint64
	firstHandles, lastHandles  uint64
	privateSlopeBytesPerMinute float64
	handlesSlopePerMinute      float64
	privateEstimatedGrowth     float64
	handlesEstimatedGrowth     float64
}

func resourceTrend(windows []resourceWindow) resourceTrendSummary {
	trend := resourceTrendSummary{}
	if len(windows) == 0 {
		return trend
	}
	trend.firstPrivate, trend.lastPrivate = windows[0].MedianPrivate, windows[len(windows)-1].MedianPrivate
	trend.firstHandles, trend.lastHandles = windows[0].MedianHandles, windows[len(windows)-1].MedianHandles
	trend.privateSlopeBytesPerMinute = theilSenSlope(windows, func(window resourceWindow) uint64 { return window.MedianPrivate })
	trend.handlesSlopePerMinute = theilSenSlope(windows, func(window resourceWindow) uint64 { return window.MedianHandles })
	spanMinutes := float64(windows[len(windows)-1].Minute - windows[0].Minute)
	if spanMinutes > 0 {
		trend.privateEstimatedGrowth = trend.privateSlopeBytesPerMinute * spanMinutes
		trend.handlesEstimatedGrowth = trend.handlesSlopePerMinute * spanMinutes
	}
	return trend
}

func theilSenSlope(windows []resourceWindow, value func(resourceWindow) uint64) float64 {
	slopes := make([]float64, 0, len(windows)*(len(windows)-1)/2)
	for i := 0; i < len(windows); i++ {
		for j := i + 1; j < len(windows); j++ {
			elapsed := windows[j].Minute - windows[i].Minute
			if elapsed <= 0 {
				continue
			}
			delta := float64(value(windows[j])) - float64(value(windows[i]))
			slopes = append(slopes, delta/float64(elapsed))
		}
	}
	if len(slopes) == 0 {
		return 0
	}
	sort.Float64s(slopes)
	middle := len(slopes) / 2
	if len(slopes)%2 == 1 {
		return slopes[middle]
	}
	return (slopes[middle-1] + slopes[middle]) / 2
}

func resourceTrendExceeded(trend resourceTrendSummary) bool {
	return trend.privateEstimatedGrowth >= float64(resourcePrivateGrowthLimit) ||
		trend.handlesEstimatedGrowth >= float64(resourceHandleGrowthLimit)
}

func signedDelta(last, first uint64) int64 {
	if last >= first {
		return int64(last - first)
	}
	return -int64(first - last)
}
