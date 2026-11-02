package scheduler

// PriorityAgingMs 单位契约钉住（round20/bugC）。字段文档（scheduler.go
// Config.PriorityAgingMs）写的是「after this many ms」，两个消费点必须按毫秒
// 解释。修复前两处都用 time.Duration(x) 把 int 当纳秒读：生产默认 5000
// （意图 5 秒）实际生效为 5µs 的过期阈值——每个 aging tick 都把任何排队请求
// 提拔一级，P5 维护请求 ~800ms 即涌入 P0 交互车道（§F7 反噬 §F3）。
//
// boostStaleRequests 的扫描循环（pq 递减重扫）会让一个 stale 请求在单个
// pass 内级联到扫描地板，因此断言以「离开 P5 / 落入更低车道」为准，不钉
// 单一目的车道。

import (
	"context"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// TestPriorityAgingThresholdIsMilliseconds pins both sides of the unit
// contract: a request queued far less than the configured threshold must NOT
// be boosted (the false-aging that flooded P0), and one queued beyond it must
// be (aging must stay able to relieve real starvation). No workers are
// started: the passes are driven manually, so every assertion is
// deterministic — no tick race.
func TestPriorityAgingThresholdIsMilliseconds(t *testing.T) {
	s := New(Config{
		MaxConcurrent:   1,
		MaxQueueSize:    16,
		MaxInFlight:     16,
		PriorityAgingMs: 60_000, // intended: 60 seconds
	})

	park := func(name string, age time.Duration) *Request {
		req := &Request{
			RequestID:  identity.RequestID(name),
			Priority:   PriorityMaintenance,
			CostClass:  CostTiny,
			EnqueuedAt: time.Now().Add(-age),
			Result:     make(chan Result, 1),
			Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
				return nil, nil
			},
		}
		if !s.trySend(CostTiny, int(PriorityMaintenance), req) {
			t.Fatalf("setup: could not park %s", name)
		}
		return req
	}

	// 100ms 旧：远小于文档化的 60s 阈值，一个 pass 后必须仍在 P5 车道。
	young := park("aging-young", 100*time.Millisecond)
	// 61s 旧：超过 60s 阈值，pass 2 必须把它提拔出 P5。
	old := park("aging-old", 61*time.Second)

	p5 := s.queues[CostTiny][int(PriorityMaintenance)]

	// Pass 1: the young request is at the lane head (FIFO). It must be put
	// back, not promoted.
	s.boostStaleRequests(context.Background())
	if n := len(p5); n != 2 {
		t.Fatalf("PriorityAgingMs=%d acted as %v, not %dms: a %v-old request left the P5 lane after one pass (P5 holds %d, want 2)",
			60_000, time.Duration(60_000), 60_000, 100*time.Millisecond, n)
	}
	for lane := 0; lane < int(PriorityMaintenance); lane++ {
		if n := len(s.queues[CostTiny][lane]); n != 0 {
			t.Fatalf("false aging: %d request(s) sit below P5 after the young one aged only %v against a %dms threshold",
				n, 100*time.Millisecond, 60_000)
		}
	}

	// Pass 2: the old request is now at the lane head and must be boosted
	// out of P5 — aging itself must keep working for genuinely stale
	// requests. The young one stays parked.
	s.boostStaleRequests(context.Background())
	if n := len(p5); n != 1 {
		t.Fatalf("after pass 2 the P5 lane holds %d requests, want 1 (only the 61s-old request may leave)", n)
	}
	if got := <-p5; got != young {
		t.Fatalf("P5 holds %s, want the young request (the wrongly aged one must be the one that stays)", got.RequestID)
	}
	boosted := 0
	for lane := 0; lane < int(PriorityMaintenance); lane++ {
		for len(s.queues[CostTiny][lane]) > 0 {
			req := <-s.queues[CostTiny][lane]
			if req == young {
				t.Fatalf("the %v-old request was boosted to lane %d despite the %dms threshold", 100*time.Millisecond, lane, 60_000)
			}
			if req == old {
				boosted++
			}
		}
	}
	if boosted != 1 {
		t.Fatalf("the 61s-old request was boosted %d time(s) out of P5, want exactly 1 (aging must still fire past the threshold)", boosted)
	}
}
