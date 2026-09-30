package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// 骑手提前到店等待责任拆分与争议复核
//
// 等待按责任方拆为三段（互不重叠、不重复计时）：
//   - rider_arrive_early 骑手提前到店：骑手早于「ETA-早到宽限」到店的部分
//   - platform_dispatch_early 平台派单过早：派单后、制作目标时长内骑手已在等的部分
//   - store_make_timeout 门店制作超时：超过「实际派单时间 + 可配置制作目标时长」仍未出杯的部分
//   - waived 免计段：复核改判豁免（不计门店绩效，骑手补贴由平台承担）
//
// 订单发生改配、重做、骑手离店再到店时，按事件时间线整单重新归因，
// 每次重算生成一个版本（wait_attribution_runs + wait_segments），原始归因永不覆盖。

const (
	segRiderEarly       = "rider_arrive_early"
	segPlatformEarly    = "platform_dispatch_early"
	segStoreMakeTimeout = "store_make_timeout"
	segWaived           = "waived"
	partyRider          = "rider"
	partyPlatform       = "platform"
	partyStore          = "store"
	partyWaived         = "waived"
	financeRiderWaitSub = "rider_wait_subsidy"
)

// ---------- 纯计算部分（无 IO，便于测试） ----------

type waitThresholds struct {
	MakeTargetSeconds int     `json:"make_target_seconds"`
	EarlyGraceSeconds int     `json:"early_grace_seconds"`
	SubsidyPerMinute  float64 `json:"subsidy_per_minute"`
}

type arrivalSpan struct {
	ArrivalID     int64
	ArrivedAt     time.Time
	LeftAt        *time.Time
	QueueCups     int `json:"queue_cups"`
	QueuePosition int `json:"queue_position"`
	Surge         map[string]any
}

type dispatchPoint struct {
	At   time.Time `json:"at"`
	Type string    `json:"type"` // dispatched / redispatched
}

type readyToggle struct {
	At    time.Time `json:"at"`
	Ready bool      `json:"ready"`
}

type waitSegment struct {
	ArrivalID   *int64    `json:"arrival_id,omitempty"`
	SegmentType string    `json:"segment_type"`
	Party       string    `json:"party"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	Seconds     int       `json:"seconds"`
	Note        string    `json:"note,omitempty"`
}

type appealOverride struct {
	AppealID   int64
	WindowFrom *time.Time
	WindowTo   *time.Time
	MoveTo     string // platform / rider / waived
}

type waitInput struct {
	OrderedAt    time.Time
	DispatchedAt *time.Time
	RiderETA     *time.Time
	Arrivals     []arrivalSpan
	Dispatches   []dispatchPoint
	ReadyToggles []readyToggle
	WaitEnd      time.Time // 等待观察终点（交接/取消/当前）
	Thresholds   waitThresholds
	Remade       bool
	Override     *appealOverride
}

type timeSpan struct{ start, end time.Time }

// intersect 返回两个区间的交集，无交返回 ok=false。
func intersect(a, b timeSpan) (timeSpan, bool) {
	s := a.start
	if b.start.After(s) {
		s = b.start
	}
	e := a.end
	if b.end.Before(e) {
		e = b.end
	}
	if !e.After(s) {
		return timeSpan{}, false
	}
	return timeSpan{s, e}, true
}

// unreadyIntervals 返回窗口内「订单尚未出杯（ready=false）」的子区间。
// 初始状态由窗口之前的最后一次切换决定（order_ready→true，cup_remade→false）。
func unreadyIntervals(windowStart, windowEnd time.Time, toggles []readyToggle) []timeSpan {
	type pt struct {
		at    time.Time
		ready bool
	}
	state := false
	var pts []pt
	for _, t := range toggles {
		if !t.At.After(windowStart) {
			state = t.Ready
			continue
		}
		if t.At.Before(windowEnd) {
			pts = append(pts, pt{t.At, t.Ready})
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].at.Before(pts[j].at) })

	var out []timeSpan
	cur := windowStart
	for _, p := range pts {
		if !state && p.at.After(cur) {
			out = append(out, timeSpan{cur, p.at})
		}
		state = p.ready
		cur = p.at
	}
	if !state && windowEnd.After(cur) {
		out = append(out, timeSpan{cur, windowEnd})
	}
	return out
}

// dispatchForArrival 返回该次到店对应的派单时间（到店前最近一次派单；缺省用下单时间）。
func dispatchForArrival(in *waitInput, arrived time.Time) time.Time {
	d := in.OrderedAt
	for _, dp := range in.Dispatches {
		if !dp.At.After(arrived) && dp.At.After(d) {
			d = dp.At
		}
	}
	if in.DispatchedAt != nil && in.DispatchedAt.After(d) && !in.DispatchedAt.After(arrived) {
		d = *in.DispatchedAt
	}
	return d
}

// attributeWait 是责任拆分的纯函数实现：到店区间 ∩ 未出杯区间，
// 再按 ETA 宽限线与「派单+制作目标」线切成三段。
func attributeWait(in *waitInput) []waitSegment {
	var segs []waitSegment
	grace := (*time.Time)(nil)
	if in.RiderETA != nil {
		g := in.RiderETA.Add(-time.Duration(in.Thresholds.EarlyGraceSeconds) * time.Second)
		grace = &g
	}

	for _, ar := range in.Arrivals {
		end := in.WaitEnd
		if ar.LeftAt != nil && ar.LeftAt.Before(end) {
			end = *ar.LeftAt
		}
		span := timeSpan{ar.ArrivedAt, end}
		if !span.end.After(span.start) {
			continue
		}
		dispatchAt := dispatchForArrival(in, ar.ArrivedAt)
		makeDeadline := dispatchAt.Add(time.Duration(in.Thresholds.MakeTargetSeconds) * time.Second)
		surgeActive := false
		if v, ok := ar.Surge["active"].(bool); ok {
			surgeActive = v
		}

		for _, ur := range unreadyIntervals(span.start, span.end, in.ReadyToggles) {
			piece, ok := intersect(span, ur)
			if !ok {
				continue
			}
			// 用 grace / makeDeadline 两个边界切分
			bounds := []time.Time{}
			if grace != nil && grace.After(piece.start) && grace.Before(piece.end) {
				bounds = append(bounds, *grace)
			}
			if makeDeadline.After(piece.start) && makeDeadline.Before(piece.end) {
				bounds = append(bounds, makeDeadline)
			}
			sort.Slice(bounds, func(i, j int) bool { return bounds[i].Before(bounds[j]) })
			cuts := append([]time.Time{piece.start}, bounds...)
			cuts = append(cuts, piece.end)
			for i := 0; i+1 < len(cuts); i++ {
				a, b := cuts[i], cuts[i+1]
				if !b.After(a) {
					continue
				}
				// 三段优先级：门店制作超时（已超过 派单+制作目标，门店违反制作 SLA，
				//   与骑手是否早到无关）
				//   > 骑手提前到店（仍在制作目标窗口内且早于 ETA 宽限线）
				//   > 平台派单过早（制作目标窗口内、骑手已到 ETA 前后的等待）
				typ, party := segPlatformEarly, partyPlatform
				switch {
				case !a.Before(makeDeadline):
					typ, party = segStoreMakeTimeout, partyStore
				case grace != nil && a.Before(*grace):
					typ, party = segRiderEarly, partyRider
				default:
					typ, party = segPlatformEarly, partyPlatform
				}
				note := ""
				if party == partyStore && surgeActive {
					note = "等待期间门店处于爆单保护状态"
				}
				id := ar.ArrivalID
				segs = append(segs, waitSegment{
					ArrivalID: &id, SegmentType: typ, Party: party,
					StartedAt: a, EndedAt: b, Seconds: int(b.Sub(a).Seconds()), Note: note,
				})
			}
		}
	}

	// 相邻同类段合并
	sort.Slice(segs, func(i, j int) bool {
		return segs[i].StartedAt.Before(segs[j].StartedAt)
	})
	merged := segs[:0]
	for _, s := range segs {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.Party == s.Party && last.SegmentType == s.SegmentType &&
				(last.ArrivalID == nil || s.ArrivalID == nil || *last.ArrivalID == *s.ArrivalID) &&
				!last.EndedAt.Before(s.StartedAt) {
				last.EndedAt = s.EndedAt
				last.Seconds += s.Seconds
				if s.Note != "" {
					last.Note = s.Note
				}
				continue
			}
		}
		merged = append(merged, s)
	}

	// 复核改判：把申诉窗口内的门店制作超时段改判给 platform / rider / waived
	if in.Override != nil {
		merged = applyOverride(merged, *in.Override)
	}
	return merged
}

// applyOverride 在不改动原段时间覆盖的前提下，把窗口内 store 段重定向给改判责任方。
func applyOverride(segs []waitSegment, ov appealOverride) []waitSegment {
	newType, newParty := segWaived, partyWaived
	switch ov.MoveTo {
	case partyPlatform:
		newType, newParty = segPlatformEarly, partyPlatform
	case partyRider:
		newType, newParty = segRiderEarly, partyRider
	default:
		newType, newParty = segWaived, partyWaived
	}
	win := timeSpan{
		start: time.Time{}, // 零值=不限
		end:   time.Time{},
	}
	if ov.WindowFrom != nil {
		win.start = *ov.WindowFrom
	}
	if ov.WindowTo != nil {
		win.end = *ov.WindowTo
	}
	var out []waitSegment
	for _, s := range segs {
		// 与申诉窗口无交集（或非门店段）则原样保留
		if s.Party != partyStore ||
			(!win.start.IsZero() && !win.start.Before(s.EndedAt)) ||
			(!win.end.IsZero() && !win.end.After(s.StartedAt)) {
			out = append(out, s)
			continue
		}
		// 求窗口有效交集（缺省端用段端点补足）
		clipStart, clipEnd := s.StartedAt, s.EndedAt
		if !win.start.IsZero() && win.start.After(clipStart) {
			clipStart = win.start
		}
		if !win.end.IsZero() && win.end.Before(clipEnd) {
			clipEnd = win.end
		}
		emit := func(start, end time.Time, moved bool) {
			if !end.After(start) {
				return
			}
			ns := s
			ns.StartedAt, ns.EndedAt, ns.Seconds = start, end, int(end.Sub(start).Seconds())
			if moved {
				ns.SegmentType, ns.Party, ns.Note = newType, newParty,
					fmt.Sprintf("区域经理复核改判（申诉#%d）", ov.AppealID)
			} else {
				ns.Note = s.Note
			}
			out = append(out, ns)
		}
		emit(s.StartedAt, clipStart, false) // 窗口前：保留门店责任
		emit(clipStart, clipEnd, true)      // 窗口内：改判
		emit(clipEnd, s.EndedAt, false)     // 窗口后：保留门店责任
	}
	return out
}

// summarizeSegments 汇总各责任方秒数。
func summarizeSegments(segs []waitSegment) map[string]int {
	sum := map[string]int{
		segRiderEarly:       0,
		segPlatformEarly:    0,
		segStoreMakeTimeout: 0,
		segWaived:           0,
	}
	for _, s := range segs {
		sum[s.SegmentType] += s.Seconds
	}
	return sum
}

// subsidyAmount 非骑手责任等待按秒折算补贴（元，保留两位）。
func subsidyAmount(seconds int, rate float64) float64 {
	v := float64(seconds) / 60.0 * rate
	return math.Round(v*100) / 100
}

// ---------- 数据装载与持久化（事务内） ----------

type recomputeResult struct {
	RunID    int64              `json:"run_id"`
	Version  int                `json:"version"`
	Reason   string             `json:"reason"`
	Summary  map[string]int     `json:"summary_seconds"`
	Subsidy  map[string]float64 `json:"subsidy_by_party"`
	Segments []waitSegment      `json:"segments"`
	Basis    map[string]any     `json:"basis"`
}

func (a *App) recomputeWaitAttribution(ctx context.Context, tx pgx.Tx, orderID int64,
	reason string, ov *appealOverride, createdBy int64) (*recomputeResult, error) {

	var (
		storeID      int64
		status       string
		orderedAt    time.Time
		dispatchedAt *time.Time
		riderETA     *time.Time
		allSealedAt  *time.Time
		handedAt     *time.Time
		canceledAt   *time.Time
	)
	if err := tx.QueryRow(ctx, `
		SELECT store_id,status,ordered_at,dispatched_at,rider_eta,all_sealed_at,handed_at,canceled_at
		FROM orders WHERE id=$1 FOR UPDATE`, orderID).Scan(
		&storeID, &status, &orderedAt, &dispatchedAt, &riderETA, &allSealedAt, &handedAt, &canceledAt); err != nil {
		return nil, err
	}

	th, err := loadThresholds(ctx, tx, storeID)
	if err != nil {
		return nil, err
	}

	// 到店/离店区间
	rows, err := tx.Query(ctx, `
		SELECT id,arrived_at,left_at,queue_cups,queue_position,surge_snapshot
		FROM rider_arrivals WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	var arrivals []arrivalSpan
	for rows.Next() {
		var ar arrivalSpan
		var raw []byte
		if err := rows.Scan(&ar.ArrivalID, &ar.ArrivedAt, &ar.LeftAt,
			&ar.QueueCups, &ar.QueuePosition, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(raw, &ar.Surge)
		arrivals = append(arrivals, ar)
	}
	rows.Close()

	// 派单/改派与出杯状态切换时间线
	evRows, err := tx.Query(ctx, `
		SELECT event_type,created_at FROM order_events
		WHERE order_id=$1 AND event_type IN
		  ('order_received','order_dispatched','order_redispatched','order_ready','cup_remade','item_oos')
		ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	var dispatches []dispatchPoint
	var toggles []readyToggle
	remade := false
	for evRows.Next() {
		var et string
		var at time.Time
		if err := evRows.Scan(&et, &at); err != nil {
			evRows.Close()
			return nil, err
		}
		switch et {
		case "order_dispatched":
			dispatches = append(dispatches, dispatchPoint{At: at, Type: "dispatched"})
		case "order_redispatched":
			dispatches = append(dispatches, dispatchPoint{At: at, Type: "redispatched"})
		case "order_ready":
			toggles = append(toggles, readyToggle{At: at, Ready: true})
		case "cup_remade":
			remade = true
			toggles = append(toggles, readyToggle{At: at, Ready: false})
		case "item_oos":
			toggles = append(toggles, readyToggle{At: at, Ready: false})
		}
	}
	evRows.Close()
	if len(dispatches) == 0 {
		dispatches = append(dispatches, dispatchPoint{At: firstNonNil(dispatchedAt, orderedAt), Type: "dispatched"})
	}

	// 等待观察终点：交接 > 取消 > 当前
	waitEnd := time.Now()
	endReason := "in_progress"
	switch {
	case handedAt != nil:
		waitEnd, endReason = *handedAt, "handoff"
	case canceledAt != nil:
		waitEnd, endReason = *canceledAt, "canceled"
	case reason == "rider_left":
		endReason = "rider_left"
	}

	in := &waitInput{
		OrderedAt: orderedAt, DispatchedAt: dispatchedAt, RiderETA: riderETA,
		Arrivals: arrivals, Dispatches: dispatches, ReadyToggles: toggles,
		WaitEnd: waitEnd, Thresholds: th, Remade: remade, Override: ov,
	}
	segs := attributeWait(in)
	sum := summarizeSegments(segs)

	// 版本号（同时取上一版本 run_id，用于补贴链式冲回）
	var version int
	var prevRunID *int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(max(version),0)+1 FROM wait_attribution_runs WHERE order_id=$1`,
		orderID).Scan(&version); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx,
		`SELECT id FROM wait_attribution_runs WHERE order_id=$1 ORDER BY version DESC LIMIT 1`,
		orderID).Scan(&prevRunID); err != nil && err != pgx.ErrNoRows {
		return nil, err
	}

	arrivalJSON := []map[string]any{}
	for _, ar := range arrivals {
		arrivalJSON = append(arrivalJSON, map[string]any{
			"arrival_id": ar.ArrivalID, "arrived_at": ar.ArrivedAt, "left_at": ar.LeftAt,
			"queue_cups": ar.QueueCups, "queue_position": ar.QueuePosition, "surge": ar.Surge,
		})
	}
	finalReady := allSealedAt
	basis := map[string]any{
		"order_status": status, "ordered_at": orderedAt, "dispatched_at": dispatchedAt,
		"rider_eta": riderETA, "all_sealed_at": finalReady, "wait_end": waitEnd,
		"end_reason": endReason, "remade": remade, "thresholds": th,
		"dispatch_history": dispatches, "ready_toggles": toggles, "arrivals": arrivalJSON,
		"override": ovSummary(ov), "previous_run_id": prevRunID, "settlement_applied": isTerminalWaitReason(reason),
	}
	var appealID *int64
	if ov != nil {
		appealID = &ov.AppealID
	}
	var runID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO wait_attribution_runs(order_id,version,reason,basis,appeal_id,created_by)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id`,
		orderID, version, reason, basis, appealID, createdBy).Scan(&runID); err != nil {
		return nil, err
	}
	for i, s := range segs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO wait_segments(run_id,order_id,arrival_id,segment_no,segment_type,party,
				started_at,ended_at,seconds,note)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			runID, orderID, s.ArrivalID, i+1, s.SegmentType, s.Party,
			s.StartedAt, s.EndedAt, s.Seconds, s.Note); err != nil {
			return nil, err
		}
	}

	// 到店记录投影：每段秒数按 arrival 聚合，供门店时效报表/到店屏使用
	perArrival := map[int64]map[string]int{}
	totalByArrival := map[int64]int{}
	for _, s := range segs {
		if s.ArrivalID == nil {
			continue
		}
		id := *s.ArrivalID
		if perArrival[id] == nil {
			perArrival[id] = map[string]int{}
		}
		perArrival[id][s.SegmentType] += s.Seconds
		totalByArrival[id] += s.Seconds
	}
	for id, split := range perArrival {
		if _, err := tx.Exec(ctx, `
			UPDATE rider_arrivals
			SET wait_split=$2, wait_seconds=$3,
			    store_fault_wait_seconds=$4
			WHERE id=$1`,
			id, split, totalByArrival[id], split[segStoreMakeTimeout]); err != nil {
			return nil, err
		}
	}

	// 补贴结算：中间态（到店/出杯/重做/改配）只算时段不落财务账；
	// 终态（离店/交接/取消/复核改判）先冲回「最近一个原始结算版本」，再写新版本。
	// 冲回行带固定标记且不再被冲回，望远镜式抵消保证累计净额只等于最新版本。
	settle := isTerminalWaitReason(reason)
	if settle {
		var lastSettledRunID *int64
		if err := tx.QueryRow(ctx, `
			SELECT max(run_id) FROM finance_records
			WHERE order_id=$1 AND kind=$2 AND run_id IS NOT NULL AND note NOT LIKE '%[冲回]%'`,
			orderID, financeRiderWaitSub).Scan(&lastSettledRunID); err != nil {
			return nil, err
		}
		if lastSettledRunID != nil && *lastSettledRunID != runID {
			if err := reverseRunSubsidy(ctx, tx, orderID, *lastSettledRunID, runID, createdBy); err != nil {
				return nil, err
			}
		}
	}
	subsidy := map[string]float64{}
	if settle {
		type key struct {
			party string
			day   time.Time
		}
		agg := map[key]int{}
		for _, s := range segs {
			if s.Party == partyRider {
				continue // 骑手自身责任段不补贴
			}
			k := key{party: s.Party, day: truncDate(s.EndedAt)}
			agg[k] += s.Seconds
		}
		for k, sec := range agg {
			amount := subsidyAmount(sec, th.SubsidyPerMinute)
			if amount <= 0 {
				continue
			}
			bearer := k.party
			if bearer == partyWaived {
				bearer = partyPlatform // 豁免段骑手照补，由平台承担
			}
			note := "骑手等待补贴（按最新责任归因自动结算）"
			if reason == "review_overturn" {
				note = "区域经理复核改判后重新结算骑手等待补贴"
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO finance_records(store_id,order_id,kind,amount,bearer,period_date,note,created_by,run_id)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				storeID, orderID, financeRiderWaitSub, -amount, bearer, k.day,
				note, createdBy, runID); err != nil {
				return nil, err
			}
			subsidy[bearer] = subsidy[bearer] + amount
		}
		for k := range subsidy {
			subsidy[k] = math.Round(subsidy[k]*100) / 100
		}
	}

	if err := a.logEvent(ctx, tx, orderID, "wait_reattributed", &eventActor{ID: createdBy}, map[string]any{
		"run_id": runID, "version": version, "reason": reason,
		"summary_seconds": sum, "subsidy_by_party": subsidy,
	}); err != nil {
		return nil, err
	}

	return &recomputeResult{
		RunID: runID, Version: version, Reason: reason,
		Summary: sum, Subsidy: subsidy, Segments: segs, Basis: basis,
	}, nil
}

func ovSummary(ov *appealOverride) any {
	if ov == nil {
		return nil
	}
	return map[string]any{
		"appeal_id": ov.AppealID, "window_from": ov.WindowFrom,
		"window_to": ov.WindowTo, "move_to": ov.MoveTo,
	}
}

func firstNonNil(t *time.Time, fallback time.Time) time.Time {
	if t != nil {
		return *t
	}
	return fallback
}

// isTerminalWaitReason 判断该归因触发点是否为等待终态（需要落补贴结算）。
func isTerminalWaitReason(reason string) bool {
	switch reason {
	case "rider_left", "handoff", "canceled", "review_overturn":
		return true
	default:
		// arrived / ready / remade / redispatched 为中间态，只更新时段不落账
		return false
	}
}

func loadThresholds(ctx context.Context, tx pgx.Tx, storeID int64) (waitThresholds, error) {
	var th waitThresholds
	err := tx.QueryRow(ctx, `
		SELECT make_target_seconds,early_grace_seconds,subsidy_per_minute::float8
		FROM wait_thresholds WHERE store_id=$1`, storeID).Scan(
		&th.MakeTargetSeconds, &th.EarlyGraceSeconds, &th.SubsidyPerMinute)
	if err == pgx.ErrNoRows {
		// 兜底默认值
		return waitThresholds{MakeTargetSeconds: 900, EarlyGraceSeconds: 0, SubsidyPerMinute: 0.5}, nil
	}
	return th, err
}

// reverseRunSubsidy 把指定版本的全部骑手等待补贴结算按承担方反向冲回（原记录保留作审计）。
// 链式进行：新版本只反转紧邻的上一版本；多次重算时净额望远镜式抵消，不会重复结算。
func reverseRunSubsidy(ctx context.Context, tx pgx.Tx, orderID, prevRunID, newRunID, createdBy int64) error {
	note := fmt.Sprintf("[冲回] 责任归因重算：冲回原版本骑手等待补贴（原run#%d）", prevRunID)
	_, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,kind,amount,bearer,period_date,note,created_by,run_id)
		SELECT store_id,order_id,$2,-sum(amount),bearer,period_date,$3,$4,$5
		FROM finance_records
		WHERE order_id=$1 AND kind=$2 AND run_id=$6 AND note NOT LIKE '%[冲回]%'
		GROUP BY store_id,order_id,bearer,period_date
		HAVING abs(sum(amount)) >= 0.005`,
		orderID, financeRiderWaitSub, note, createdBy, newRunID, prevRunID)
	return err
}

func truncDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// ---------- HTTP：阈值配置 ----------

type thresholdsResp struct {
	StoreID int64 `json:"store_id"`
	waitThresholds
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

func (a *App) getWaitThresholds(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	var v thresholdsResp
	err := a.Pool.QueryRow(r.Context(), `
		SELECT store_id,make_target_seconds,early_grace_seconds,subsidy_per_minute::float8,updated_at
		FROM wait_thresholds WHERE store_id=$1`, storeID).Scan(
		&v.StoreID, &v.MakeTargetSeconds, &v.EarlyGraceSeconds, &v.SubsidyPerMinute, &v.UpdatedAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

type thresholdsReq struct {
	MakeTargetSeconds *int     `json:"make_target_seconds"`
	EarlyGraceSeconds *int     `json:"early_grace_seconds"`
	SubsidyPerMinute  *float64 `json:"subsidy_per_minute"`
}

func (a *App) updateWaitThresholds(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	var req thresholdsReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.MakeTargetSeconds != nil && *req.MakeTargetSeconds < 0 {
		writeErr(w, errf(400, "bad_request", "make_target_seconds 不能为负"))
		return
	}
	if req.EarlyGraceSeconds != nil && *req.EarlyGraceSeconds < 0 {
		writeErr(w, errf(400, "bad_request", "early_grace_seconds 不能为负"))
		return
	}
	if req.SubsidyPerMinute != nil && *req.SubsidyPerMinute < 0 {
		writeErr(w, errf(400, "bad_request", "subsidy_per_minute 不能为负"))
		return
	}
	ac := actor(r)
	if _, err := a.Pool.Exec(r.Context(), `
		UPDATE wait_thresholds
		SET make_target_seconds=COALESCE($2,make_target_seconds),
		    early_grace_seconds=COALESCE($3,early_grace_seconds),
		    subsidy_per_minute=COALESCE($4,subsidy_per_minute),
		    updated_by=$5,updated_at=now()
		WHERE store_id=$1`,
		storeID, req.MakeTargetSeconds, req.EarlyGraceSeconds, req.SubsidyPerMinute, ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	// 阈值变更后，在途订单下次交接/事件时按新阈值重算；已结算订单保持原归因，可经申诉复核调整
	var v thresholdsResp
	if err := a.Pool.QueryRow(r.Context(), `
		SELECT store_id,make_target_seconds,early_grace_seconds,subsidy_per_minute::float8,updated_at
		FROM wait_thresholds WHERE store_id=$1`, storeID).Scan(
		&v.StoreID, &v.MakeTargetSeconds, &v.EarlyGraceSeconds, &v.SubsidyPerMinute, &v.UpdatedAt); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

// ---------- HTTP：改派（平台实际派单时间线） ----------

type redispatchReq struct {
	RiderName string `json:"rider_name"`
	Reason    string `json:"reason"`
}

func (a *App) redispatchOrder(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	var req redispatchReq
	_ = decode(w, r, &req)
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var storeID int64
	var status string
	var oldDispatch *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT store_id,status,dispatched_at FROM orders WHERE id=$1 FOR UPDATE`,
		orderID).Scan(&storeID, &status, &oldDispatch); err != nil {
		writeErr(w, err)
		return
	}
	if status == "handed" || status == "canceled" {
		writeErr(w, errf(409, "conflict", "订单已 %s，不能改派", status))
		return
	}
	now := time.Now()
	if _, err := tx.Exec(ctx, `UPDATE orders SET dispatched_at=$2 WHERE id=$1`, orderID, now); err != nil {
		writeErr(w, err)
		return
	}
	// 原在店骑手的到店区间在改派时刻收口；新骑手到店时另开 arrival
	if _, err := tx.Exec(ctx,
		`UPDATE rider_arrivals SET left_at=COALESCE(left_at,$2)
		 WHERE order_id=$1 AND handed_at IS NULL AND (left_at IS NULL OR left_at>$2)`,
		orderID, now); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "order_redispatched", actorOf(ac), map[string]any{
		"previous_dispatched_at": oldDispatch, "redispatched_at": now,
		"rider_name": req.RiderName, "reason": req.Reason,
	}); err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.recomputeWaitAttribution(ctx, tx, orderID, "redispatched", nil, ac.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"order_id": orderID, "dispatched_at": now,
		"previous_dispatched_at": oldDispatch, "attribution": res,
		"message": "已记录改配并按事件时间线重新归因，原骑手等待区间已收口",
	})
}

// ---------- HTTP：骑手离店 / 回店 ----------

func (a *App) riderLeave(w http.ResponseWriter, r *http.Request) {
	arrivalID := toInt64(chi.URLParam(r, "id"))
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var orderID int64
	var riderAccount *int64
	var leftAt, handedAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT order_id,rider_account_id,left_at,handed_at FROM rider_arrivals WHERE id=$1 FOR UPDATE`,
		arrivalID).Scan(&orderID, &riderAccount, &leftAt, &handedAt); err != nil {
		writeErr(w, err)
		return
	}
	if ac.Role == "rider" && riderAccount != nil && *riderAccount != ac.ID {
		writeErr(w, errForbidden)
		return
	}
	if handedAt != nil {
		writeErr(w, errf(409, "conflict", "该到店记录已交接，不能登记离店"))
		return
	}
	now := time.Now()
	if leftAt != nil {
		now = *leftAt // 幂等
	} else {
		if _, err := tx.Exec(ctx, `UPDATE rider_arrivals SET left_at=$2 WHERE id=$1`, arrivalID, now); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := a.logEvent(ctx, tx, orderID, "rider_left_store", actorOf(ac), map[string]any{
		"arrival_id": arrivalID, "left_at": now,
	}); err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.recomputeWaitAttribution(ctx, tx, orderID, "rider_left", nil, ac.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"arrival_id": arrivalID, "left_at": now, "attribution": res,
	})
}

// ---------- HTTP：查询归因结果与历史版本 ----------

func (a *App) getWaitAttribution(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()
	versionFilter := r.URL.Query().Get("version")

	var runID, version int64
	var reason string
	var basisRaw []byte
	var createdAt time.Time
	q := `SELECT id,version,reason,basis,created_at FROM wait_attribution_runs WHERE order_id=$1`
	args := []any{orderID}
	if versionFilter != "" {
		q += " AND version=$2 ORDER BY version DESC LIMIT 1"
		args = append(args, toInt64(versionFilter))
	} else {
		q += " ORDER BY version DESC LIMIT 1"
	}
	err := a.Pool.QueryRow(ctx, q, args...).Scan(&runID, &version, &reason, &basisRaw, &createdAt)
	if err == pgx.ErrNoRows {
		writeJSON(w, 200, map[string]any{
			"order_id": orderID, "version": 0,
			"message":  "订单尚未产生等待归因（骑手到店并交接/离店后生成）",
			"segments": []waitSegment{},
		})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	var basis map[string]any
	_ = json.Unmarshal(basisRaw, &basis)
	rows, err := a.Pool.Query(ctx, `
		SELECT arrival_id,segment_type,party,started_at,ended_at,seconds,COALESCE(note,'')
		FROM wait_segments WHERE run_id=$1 ORDER BY segment_no`, runID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var segs []waitSegment
	for rows.Next() {
		var s waitSegment
		if err := rows.Scan(&s.ArrivalID, &s.SegmentType, &s.Party,
			&s.StartedAt, &s.EndedAt, &s.Seconds, &s.Note); err != nil {
			writeErr(w, err)
			return
		}
		segs = append(segs, s)
	}
	var runHistory []map[string]any
	hrows, err := a.Pool.Query(ctx, `
		SELECT r.version,r.reason,r.created_at,
		       COALESCE(ap.status,''),COALESCE(ap.reason_code,'')
		FROM wait_attribution_runs r
		LEFT JOIN wait_appeals ap ON ap.id=r.appeal_id
		WHERE r.order_id=$1 ORDER BY r.version DESC`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer hrows.Close()
	for hrows.Next() {
		var v int
		var rs, appealStatus, appealReason string
		var at time.Time
		if err := hrows.Scan(&v, &rs, &at, &appealStatus, &appealReason); err != nil {
			writeErr(w, err)
			return
		}
		runHistory = append(runHistory, map[string]any{
			"version": v, "reason": rs, "created_at": at,
			"appeal_status": appealStatus, "appeal_reason_code": appealReason,
		})
	}
	writeJSON(w, 200, map[string]any{
		"order_id": orderID, "run_id": runID, "version": version,
		"reason": reason, "created_at": createdAt,
		"summary_seconds": summarizeSegments(segs),
		"segments":        segs, "basis": basis, "run_history": runHistory,
	})
}

// ---------- HTTP：店长申诉 ----------

type waitAppealReq struct {
	ReasonCode   string         `json:"reason_code"`
	ReasonDetail string         `json:"reason_detail"`
	Evidence     map[string]any `json:"evidence"`
	WindowFrom   *time.Time     `json:"window_from"`
	WindowTo     *time.Time     `json:"window_to"`
	ArrivalID    *int64         `json:"arrival_id"`
}

var validAppealReasons = map[string]bool{
	"machine_fault": true, "temp_shortage": true, "staff_shortage": true,
	"surge_knockon": true, "other": true,
}

func (a *App) createWaitAppeal(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	var req waitAppealReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if !validAppealReasons[req.ReasonCode] {
		writeErr(w, errf(400, "bad_request",
			"reason_code 必须是 machine_fault/temp_shortage/staff_shortage/surge_knockon/other"))
		return
	}
	if req.ReasonDetail == "" {
		writeErr(w, errf(400, "bad_request", "reason_detail 必填（说明故障/缺货情况与影响时段）"))
		return
	}
	if req.WindowFrom != nil && req.WindowTo != nil && req.WindowTo.Before(*req.WindowFrom) {
		writeErr(w, errf(400, "bad_request", "window_to 不能早于 window_from"))
		return
	}
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var storeID int64
	var mgrStore *int64
	var status string
	if err := tx.QueryRow(ctx, `SELECT store_id,status FROM orders WHERE id=$1 FOR UPDATE`,
		orderID).Scan(&storeID, &status); err != nil {
		writeErr(w, err)
		return
	}
	if ac.Role == "store_manager" {
		if err := tx.QueryRow(ctx, `SELECT store_id FROM accounts WHERE id=$1`, ac.ID).Scan(&mgrStore); err != nil {
			writeErr(w, err)
			return
		}
		if mgrStore == nil || *mgrStore != storeID {
			writeErr(w, errForbidden)
			return
		}
	}
	// 必须已产生归因版本（否则暂无责任可申诉）
	var originalRunID int64
	err = tx.QueryRow(ctx,
		`SELECT id FROM wait_attribution_runs WHERE order_id=$1 ORDER BY version DESC LIMIT 1`,
		orderID).Scan(&originalRunID)
	if err == pgx.ErrNoRows {
		writeErr(w, errf(409, "no_attribution", "该订单尚未产生等待归因，暂不能申诉"))
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	// 同一原因已有待复核申诉，不重复提交
	var dup int
	_ = tx.QueryRow(ctx,
		`SELECT count(*) FROM wait_appeals WHERE order_id=$1 AND reason_code=$2 AND status='pending'`,
		orderID, req.ReasonCode).Scan(&dup)
	if dup > 0 {
		writeErr(w, errf(409, "appeal_duplicate", "同一原因已有待复核申诉，请勿重复提交"))
		return
	}
	if req.Evidence == nil {
		req.Evidence = map[string]any{}
	}
	var appealID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO wait_appeals(order_id,store_id,arrival_id,reason_code,reason_detail,evidence,
			window_from,window_to,submitted_by,original_run_id,settlement_only)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		orderID, storeID, req.ArrivalID, req.ReasonCode, req.ReasonDetail, req.Evidence,
		req.WindowFrom, req.WindowTo, ac.ID, originalRunID,
		status == "handed" || status == "canceled").Scan(&appealID); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "wait_appeal_submitted", actorOf(ac), map[string]any{
		"appeal_id": appealID, "reason_code": req.ReasonCode,
		"reason_detail": req.ReasonDetail, "window_from": req.WindowFrom, "window_to": req.WindowTo,
		"settlement_only": status == "handed" || status == "canceled",
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"appeal_id": appealID, "order_id": orderID, "status": "pending",
		"settlement_only": status == "handed" || status == "canceled",
		"message":         "申诉已提交，等待区域经理复核；复核通过后自动回算时效、补贴、平台指标与班组评分",
	})
}

func (a *App) listWaitAppeals(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	st := r.URL.Query().Get("status")
	q := `
		SELECT ap.id,ap.order_id,ap.store_id,COALESCE(ap.arrival_id,0),ap.reason_code,ap.reason_detail,
		       ap.evidence,ap.window_from,ap.window_to,ap.status,ap.submitted_by,ap.submitted_at,
		       COALESCE(ap.reviewed_by,0),ap.reviewed_at,COALESCE(ap.review_note,''),
		       COALESCE(ap.move_to_party,''),ap.settlement_only,
		       COALESCE(ap.original_run_id,0),COALESCE(ap.effective_run_id,0)
		FROM wait_appeals ap WHERE ap.store_id=$1`
	args := []any{storeID}
	if st != "" {
		q += " AND ap.status=$2"
		args = append(args, st)
	}
	q += " ORDER BY ap.id DESC LIMIT 200"
	rows, err := a.Pool.Query(r.Context(), q, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var (
			id, orderID, sid, arrivalID, submittedBy, reviewedBy, origRun, effRun int64
			reasonCode, reasonDetail, status, reviewNote, moveTo                  string
			evidence                                                              []byte
			windowFrom, windowTo, submittedAt                                     *time.Time
			reviewedAt                                                            *time.Time
			settlementOnly                                                        bool
		)
		if err := rows.Scan(&id, &orderID, &sid, &arrivalID, &reasonCode, &reasonDetail,
			&evidence, &windowFrom, &windowTo, &status, &submittedBy, &submittedAt,
			&reviewedBy, &reviewedAt, &reviewNote, &moveTo, &settlementOnly, &origRun, &effRun); err != nil {
			writeErr(w, err)
			return
		}
		var ev map[string]any
		_ = json.Unmarshal(evidence, &ev)
		out = append(out, map[string]any{
			"id": id, "order_id": orderID, "store_id": sid, "arrival_id": arrivalID,
			"reason_code": reasonCode, "reason_detail": reasonDetail, "evidence": ev,
			"window_from": windowFrom, "window_to": windowTo, "status": status,
			"submitted_by": submittedBy, "submitted_at": submittedAt,
			"reviewed_by": reviewedBy, "reviewed_at": reviewedAt, "review_note": reviewNote,
			"move_to_party": moveTo, "settlement_only": settlementOnly,
			"original_run_id": origRun, "effective_run_id": effRun,
		})
	}
	writeJSON(w, 200, out)
}

// ---------- HTTP：区域经理复核 ----------

type appealReviewReq struct {
	Decision    string `json:"decision"` // approved / rejected
	MoveToParty string `json:"move_to_party"`
	Note        string `json:"note"`
}

func (a *App) reviewWaitAppeal(w http.ResponseWriter, r *http.Request) {
	appealID := toInt64(chi.URLParam(r, "id"))
	var req appealReviewReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Decision != "approved" && req.Decision != "rejected" {
		writeErr(w, errf(400, "bad_request", "decision 必须是 approved/rejected"))
		return
	}
	if req.Decision == "approved" {
		switch req.MoveToParty {
		case partyPlatform, partyRider, partyWaived:
		default:
			writeErr(w, errf(400, "bad_request",
				"approved 时 move_to_party 必须是 platform/rider/waived"))
			return
		}
	}
	if req.Note == "" {
		writeErr(w, errf(400, "bad_request", "note 必填（复核改判依据）"))
		return
	}
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var orderID, storeID, submittedBy, origRun int64
	var reasonCode, reasonDetail, curStatus string
	var windowFrom, windowTo *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT order_id,store_id,reason_code,reason_detail,window_from,window_to,
		       status,submitted_by,COALESCE(original_run_id,0)
		FROM wait_appeals WHERE id=$1 FOR UPDATE`,
		appealID).Scan(&orderID, &storeID, &reasonCode, &reasonDetail, &windowFrom, &windowTo,
		&curStatus, &submittedBy, &origRun); err != nil {
		writeErr(w, err)
		return
	}
	if curStatus != "pending" {
		writeErr(w, errf(409, "appeal_closed", "申诉已复核：%s", curStatus))
		return
	}
	// 区域经理只能复核本区域门店（店长不能复核本店申诉）
	var region string
	var mgrRegion *string
	if err := tx.QueryRow(ctx, `SELECT region FROM stores WHERE id=$1`, storeID).Scan(&region); err != nil {
		writeErr(w, err)
		return
	}
	if ac.Role == "area_manager" {
		if err := tx.QueryRow(ctx, `SELECT region FROM accounts WHERE id=$1`, ac.ID).Scan(&mgrRegion); err != nil {
			writeErr(w, err)
			return
		}
		if mgrRegion == nil || *mgrRegion != region {
			writeErr(w, errForbidden)
			return
		}
	}

	if req.Decision == "rejected" {
		if _, err := tx.Exec(ctx, `
			UPDATE wait_appeals SET status='rejected',reviewed_by=$2,reviewed_at=now(),review_note=$3
			WHERE id=$1`, appealID, ac.ID, req.Note); err != nil {
			writeErr(w, err)
			return
		}
		if err := a.logEvent(ctx, tx, orderID, "wait_appeal_reviewed", actorOf(ac), map[string]any{
			"appeal_id": appealID, "decision": "rejected", "note": req.Note,
		}); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"appeal_id": appealID, "status": "rejected"})
		return
	}

	// approved：保留原始归因版本，生成改判新版本
	ov := &appealOverride{
		AppealID: appealID, WindowFrom: windowFrom, WindowTo: windowTo,
		MoveTo: req.MoveToParty,
	}
	res, err := a.recomputeWaitAttribution(ctx, tx, orderID, "review_overturn", ov, ac.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	var orderStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, orderID).Scan(&orderStatus); err != nil {
		writeErr(w, err)
		return
	}
	settlementOnly := orderStatus == "handed" || orderStatus == "canceled"
	if _, err := tx.Exec(ctx, `
		UPDATE wait_appeals SET status='approved',reviewed_by=$2,reviewed_at=now(),
		  review_note=$3,move_to_party=$4,effective_run_id=$5,settlement_only=$6
		WHERE id=$1`,
		appealID, ac.ID, req.Note, req.MoveToParty, res.RunID, settlementOnly); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "wait_appeal_reviewed", actorOf(ac), map[string]any{
		"appeal_id": appealID, "decision": "approved", "move_to_party": req.MoveToParty,
		"note": req.Note, "effective_run_id": res.RunID,
		"original_run_id": origRun, "settlement_only": settlementOnly,
		"reason_code": reasonCode, "reason_detail": reasonDetail,
	}); err != nil {
		writeErr(w, err)
		return
	}

	// 自动回算等待发生日的班组评分（门店时效口径随最新归因变化；跨日则回算相关每一天）
	days := []string{}
	if err := func() error {
		rows, qerr := tx.Query(ctx, `
			SELECT DISTINCT ended_at::date FROM wait_segments WHERE run_id=$1 ORDER BY 1`, res.RunID)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var d time.Time
			if err := rows.Scan(&d); err != nil {
				return err
			}
			days = append(days, d.Format("2006-01-02"))
		}
		return rows.Err()
	}(); err != nil {
		writeErr(w, err)
		return
	}
	if len(days) == 0 {
		days = []string{time.Now().Format("2006-01-02")}
	}
	for _, d := range days {
		if _, _, _, _, err := refreshScoreTx(ctx, tx, storeID, d); err != nil {
			writeErr(w, err)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"appeal_id": appealID, "status": "approved", "move_to_party": req.MoveToParty,
		"original_run_id": origRun, "effective_run_id": res.RunID,
		"settlement_only": settlementOnly,
		"attribution":     res,
		"message":         messageBySettlement(settlementOnly),
	})
}

func messageBySettlement(settlementOnly bool) string {
	if settlementOnly {
		return "复核改判生效：已回算门店时效、骑手等待补贴、平台服务指标与班组评分；" +
			"订单已交接/取消，仅调整绩效结算，订单交接事实未改写"
	}
	return "复核改判生效：已回算门店时效、骑手等待补贴、平台服务指标与班组评分；原始归因版本已保留"
}
