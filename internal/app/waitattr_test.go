package app

import (
	"testing"
	"time"
)

func tAt(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

func sumByParty(segs []waitSegment) map[string]int {
	out := map[string]int{}
	for _, s := range segs {
		out[s.SegmentType] += s.Seconds
	}
	return out
}

// 骑手早于 ETA 到店，制作目标内出杯：全部骑手提前。
func TestAttribute_AllRiderEarly(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	eta := tAt(2026, 9, 30, 12, 10, 0)
	arrival := tAt(2026, 9, 30, 12, 5, 0)
	ready := tAt(2026, 9, 30, 12, 8, 0)
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		RiderETA:     &eta,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 900, SubsidyPerMinute: 0.5},
	}
	segs := attributeWait(in)
	sum := sumByParty(segs)
	if sum[segRiderEarly] != 180 || sum[segPlatformEarly] != 0 || sum[segStoreMakeTimeout] != 0 {
		t.Fatalf("应全部为骑手提前 180s, got %+v segs=%+v", sum, segs)
	}
}

// 派单→ETA→出杯，出杯超过派单+目标：平台派单过早 + 门店制作超时。
func TestAttribute_PlatformEarlyAndStoreTimeout(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	eta := tAt(2026, 9, 30, 12, 10, 0)
	arrival := tAt(2026, 9, 30, 12, 8, 0) // 略早到
	ready := tAt(2026, 9, 30, 12, 20, 0)
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		RiderETA:     &eta,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 600}, // 目标 12:10
	}
	segs := attributeWait(in)
	sum := sumByParty(segs)
	// 12:08-12:10 骑手提前 120s；12:10-12:20 门店超时 600s
	if sum[segRiderEarly] != 120 || sum[segStoreMakeTimeout] != 600 || sum[segPlatformEarly] != 0 {
		t.Fatalf("骑手 120s/门店 600s, got %+v", sum)
	}
}

// 骑手迟到（晚于 ETA 到店），制作在目标内：平台派单过早（骑手已到但在目标窗口内）。
func TestAttribute_RiderOnTimeWithinTarget(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	eta := tAt(2026, 9, 30, 12, 10, 0)
	arrival := tAt(2026, 9, 30, 12, 10, 0) // 准点
	ready := tAt(2026, 9, 30, 12, 12, 0)   // 目标内出杯
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		RiderETA:     &eta,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 900},
	}
	sum := sumByParty(attributeWait(in))
	if sum[segPlatformEarly] != 120 || sum[segRiderEarly] != 0 || sum[segStoreMakeTimeout] != 0 {
		t.Fatalf("准点到店目标内等待应归平台派单过早 120s, got %+v", sum)
	}
}

// 三段齐全：早到宽限线切骑手段，目标线切门店段，中间为平台段。
func TestAttribute_AllThreeSegments(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	eta := tAt(2026, 9, 30, 12, 15, 0)
	arrival := tAt(2026, 9, 30, 12, 10, 0) // 提前 5 分钟
	ready := tAt(2026, 9, 30, 12, 25, 0)   // 超目标（派单+15min=12:15）
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		RiderETA:     &eta,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 900},
	}
	sum := sumByParty(attributeWait(in))
	// 12:10-12:15 骑手 300；12:15-12:25 门店 600；无平台段（ETA=目标线）
	if sum[segRiderEarly] != 300 || sum[segStoreMakeTimeout] != 600 {
		t.Fatalf("骑手 300/门店 600, got %+v", sum)
	}
}

// 早到宽限：早于 ETA 在 grace 内不计骑手责任。
func TestAttribute_EarlyGrace(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	eta := tAt(2026, 9, 30, 12, 10, 0)
	arrival := tAt(2026, 9, 30, 12, 9, 0) // 早 60s
	ready := tAt(2026, 9, 30, 12, 10, 30)
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		RiderETA:     &eta,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 900, EarlyGraceSeconds: 120},
	}
	sum := sumByParty(attributeWait(in))
	// grace 线=12:08；12:09 已过 grace 线，整段归平台派单过早（骑手准点宽限）
	if sum[segRiderEarly] != 0 || sum[segPlatformEarly] != 90 {
		t.Fatalf("宽限内不应计骑手, got %+v", sum)
	}
}

// 离店再到店：两段到店区间分别计时，离店期间不计。
func TestAttribute_LeaveAndReturn(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	eta := tAt(2026, 9, 30, 12, 30, 0)
	a1 := tAt(2026, 9, 30, 12, 10, 0)
	left := tAt(2026, 9, 30, 12, 12, 0)
	a2 := tAt(2026, 9, 30, 12, 20, 0)
	ready := tAt(2026, 9, 30, 12, 25, 0)
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		RiderETA:     &eta,
		Arrivals: []arrivalSpan{
			{ArrivalID: 1, ArrivedAt: a1, LeftAt: &left},
			{ArrivalID: 2, ArrivedAt: a2},
		},
		WaitEnd:    ready,
		Thresholds: waitThresholds{MakeTargetSeconds: 1800},
	}
	sum := sumByParty(attributeWait(in))
	// 段1 12:10-12:12=120s，段2 12:20-12:25=300s，全在 ETA 前 → 骑手 420s，离店 8 分钟不计
	if sum[segRiderEarly] != 420 {
		t.Fatalf("两段骑手等待应 420s（不含离店 8 分钟）, got %+v", sum)
	}
}

// 重做：ready 后又置未 ready，重做新杯期间继续计时且只计一次。
func TestAttribute_RemakeExtendsWaitOnce(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	eta := tAt(2026, 9, 30, 12, 30, 0)
	arrival := tAt(2026, 9, 30, 12, 10, 0)
	ready1 := tAt(2026, 9, 30, 12, 22, 0) // 第一次出杯
	remade := tAt(2026, 9, 30, 12, 23, 0) // 发现过久重做
	ready2 := tAt(2026, 9, 30, 12, 28, 0) // 新杯出杯
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		RiderETA:     &eta,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		ReadyToggles: []readyToggle{
			{At: ready1, Ready: true},
			{At: remade, Ready: false},
			{At: ready2, Ready: true},
		},
		WaitEnd:    ready2,
		Thresholds: waitThresholds{MakeTargetSeconds: 2400},
	}
	segs := attributeWait(in)
	sum := sumByParty(segs)
	// 未出杯区间：12:10-12:22(720) + 重做 12:23-12:28(300) = 1020s 全在 ETA 前 → 骑手；
	// 第一次出杯后的 60s（12:22-12:23）饮品已就绪不计生产等待，重做报损由重做成本另行计入，不重复。
	if sum[segRiderEarly] != 1020 {
		t.Fatalf("重做到最终出杯未出杯等待应 1020s（ready 间隙不重复计）, got %+v segs=%+v", sum, segs)
	}
	if sum[segStoreMakeTimeout] != 0 || sum[segPlatformEarly] != 0 {
		t.Fatalf("ETA 前不应有门店/平台段, got %+v", sum)
	}
}

// 改配：改派重置制作目标；旧骑手区间在改派时收口、新骑手另算，两段不重叠。
func TestAttribute_Redispatch(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	redispatch := tAt(2026, 9, 30, 12, 10, 0)
	eta := tAt(2026, 9, 30, 12, 25, 0)
	a1 := tAt(2026, 9, 30, 12, 5, 0)
	a2 := tAt(2026, 9, 30, 12, 15, 0)
	ready := tAt(2026, 9, 30, 12, 20, 0)
	in := &waitInput{
		OrderedAt: dispatch,
		RiderETA:  &eta,
		Arrivals: []arrivalSpan{
			{ArrivalID: 1, ArrivedAt: a1, LeftAt: &redispatch},
			{ArrivalID: 2, ArrivedAt: a2},
		},
		Dispatches: []dispatchPoint{
			{At: dispatch, Type: "dispatched"},
			{At: redispatch, Type: "redispatched"},
		},
		WaitEnd:    ready,
		Thresholds: waitThresholds{MakeTargetSeconds: 300}, // 原目标 12:05；改派后重置为 12:15
	}
	segs := attributeWait(in)
	sum := sumByParty(segs)
	// 旧骑手 12:05 到店时门店已超原制作目标（12:05）→ 门店 300；
	// 改派重置目标为 12:15，新骑手 12:15-12:20 等待 → 门店超时 300；两段合计 600s 无重叠。
	if total := sum[segRiderEarly] + sum[segPlatformEarly] + sum[segStoreMakeTimeout] + sum[segWaived]; total != 600 {
		t.Fatalf("总等待应 600s（300 旧骑手 + 300 新骑手），无重叠, got %d (%+v)", total, sum)
	}
	arrivals := map[int64]int{}
	for _, s := range segs {
		if s.ArrivalID != nil {
			arrivals[*s.ArrivalID] += s.Seconds
		}
	}
	if arrivals[1] != 300 || arrivals[2] != 300 {
		t.Fatalf("两段到店应各 300s, got %+v", arrivals)
	}
}

// 复核改判：窗口内门店超时段改判豁免，原始段不被物理覆盖（返回含 waived 新段）。
func TestAttribute_OverrideMovesStoreTimeoutToWaived(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	arrival := tAt(2026, 9, 30, 12, 0, 0)
	ready := tAt(2026, 9, 30, 12, 20, 0)
	winFrom := tAt(2026, 9, 30, 12, 10, 0)
	winTo := tAt(2026, 9, 30, 12, 20, 0)
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 600},
		Override: &appealOverride{
			AppealID: 7, WindowFrom: &winFrom, WindowTo: &winTo, MoveTo: partyWaived,
		},
	}
	sum := sumByParty(attributeWait(in))
	// 12:00-12:10 在制作目标窗口内（骑手派单即到店等待）= 平台派单过早 600；
	// 12:10-12:20 门店超时 600，落入申诉窗口 → 豁免 600。
	if sum[segStoreMakeTimeout] != 0 || sum[segWaived] != 600 || sum[segPlatformEarly] != 600 {
		t.Fatalf("窗口内 600s 应豁免、窗口前 600s 归平台, got %+v", sum)
	}
}

// 改判给平台：窗口内门店段变为平台派单过早。
func TestAttribute_OverrideMovesToPlatform(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	arrival := tAt(2026, 9, 30, 12, 0, 0)
	ready := tAt(2026, 9, 30, 12, 20, 0)
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 600},
		Override:     &appealOverride{AppealID: 1, MoveTo: partyPlatform}, // 全窗口
	}
	sum := sumByParty(attributeWait(in))
	if sum[segStoreMakeTimeout] != 0 || sum[segPlatformEarly] != 1200 {
		t.Fatalf("全部 1200s 应改判平台, got %+v", sum)
	}
}

// 门店超时段跨越申诉窗口起点：仅窗口内部分改判，窗口前的门店超时保留。
func TestAttribute_OverrideClipAcrossWindowStart(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	arrival := tAt(2026, 9, 30, 12, 0, 0)
	ready := tAt(2026, 9, 30, 12, 30, 0) // 门店超时 12:10-12:30
	winFrom := tAt(2026, 9, 30, 12, 20, 0)
	winTo := tAt(2026, 9, 30, 12, 30, 0)
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 600},
		Override:     &appealOverride{AppealID: 9, WindowFrom: &winFrom, WindowTo: &winTo, MoveTo: partyWaived},
	}
	sum := sumByParty(attributeWait(in))
	// 平台段 12:00-12:10=600；门店超时 12:10-12:20=600 保留；窗口内 12:20-12:30=600 豁免
	if sum[segPlatformEarly] != 600 || sum[segStoreMakeTimeout] != 600 || sum[segWaived] != 600 {
		t.Fatalf("跨窗口起点应切成 600/600/600, got %+v", sum)
	}
}

// 门店超时段跨越申诉窗口终点：窗口前段改判，窗口后的门店超时保留。
func TestAttribute_OverrideClipAcrossWindowEnd(t *testing.T) {
	dispatch := tAt(2026, 9, 30, 12, 0, 0)
	arrival := tAt(2026, 9, 30, 12, 0, 0)
	ready := tAt(2026, 9, 30, 12, 30, 0)
	winFrom := tAt(2026, 9, 30, 12, 0, 0) // 从派单即开始
	winTo := tAt(2026, 9, 30, 12, 15, 0)
	in := &waitInput{
		OrderedAt:    dispatch,
		DispatchedAt: &dispatch,
		Arrivals:     []arrivalSpan{{ArrivalID: 1, ArrivedAt: arrival}},
		WaitEnd:      ready,
		Thresholds:   waitThresholds{MakeTargetSeconds: 600},
		Override:     &appealOverride{AppealID: 9, WindowFrom: &winFrom, WindowTo: &winTo, MoveTo: partyWaived},
	}
	sum := sumByParty(attributeWait(in))
	// 平台段 12:00-12:10=600 不受门店申诉影响；门店超时仅 12:10-12:15=300 落入窗口豁免；
	// 窗口后门店超时 12:15-12:30=900 保留。
	if sum[segPlatformEarly] != 600 || sum[segWaived] != 300 || sum[segStoreMakeTimeout] != 900 {
		t.Fatalf("跨窗口终点应平台600/豁免300/保留门店900, got %+v", sum)
	}
}

func TestSubsidyAmount(t *testing.T) {
	if got := subsidyAmount(120, 0.5); got != 1.0 {
		t.Fatalf("120s @0.5/min 应 1.0, got %v", got)
	}
	if got := subsidyAmount(90, 0.5); got != 0.75 {
		t.Fatalf("90s 应 0.75, got %v", got)
	}
}
