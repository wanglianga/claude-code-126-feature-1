package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// 骑手提前到店等待责任拆分引擎。
//
// 等待按事件时间线拆成互不重叠的段：
//   rider_early_arrival    骑手早于应到时间（ETA，扣宽限）到店的部分
//   store_make_timeout     超过理论最早出杯时刻门店仍未出杯的部分
//   platform_early_dispatch 平台派单/改配过晚、给门店预留制作前置不足造成的结构性等待
//   store_exempted         复核改判豁免的原门店责任段（机器故障/临时缺货等，转平台承担）
//   grace                  宽限/无责任尾段（不计任何一方绩效与补贴）
//
// 理论最早出杯时刻 P = 最新派单/改配时刻 + 平台应预留制作前置（可配置阈值）。
// 超时等待区间（应到 + 门店宽限 之后仍未出杯）按 P 拆分：
//   - P 早于应到时刻（派单及时）：超时全部归门店；
//   - P 晚于应到宽限（派单/改配过晚）：[应到宽限, P) 归平台，[P, 实际出杯) 归门店。
//
// 每个到店-离店区间独立切分、互不重叠；重做/改配/离店再到店后以最新时间线
// 整单重算并整体换版，从机制上保证同一段等待不会被重复计入门店绩效与骑手补偿。

// ---------- 可配置阈值 ----------

type waitThresholds struct {
	StoreID               int64   `json:"store_id"`
	PlatformDispatchLeadS int     `json:"platform_dispatch_lead_seconds"`
	StoreMakeGraceS       int     `json:"store_make_grace_seconds"`
	RiderEarlyGraceS      int     `json:"rider_early_grace_seconds"`
	RiderSubsidyPerMinute float64 `json:"rider_subsidy_per_minute"`
	StoreWaitScoreSeconds int     `json:"store_wait_score_seconds"`
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func getWaitThresholds(ctx context.Context, q rowQuerier, storeID int64) (waitThresholds, error) {
	var t waitThresholds
	err := q.QueryRow(ctx, `
		SELECT store_id,platform_dispatch_lead_seconds,store_make_grace_seconds,
		       rider_early_grace_seconds,rider_subsidy_per_minute::float8,store_wait_score_seconds
		FROM wait_thresholds WHERE store_id=$1`, storeID).Scan(
		&t.StoreID, &t.PlatformDispatchLeadS, &t.StoreMakeGraceS,
		&t.RiderEarlyGraceS, &t.RiderSubsidyPerMinute, &t.StoreWaitScoreSeconds)
	if err == pgx.ErrNoRows {
		err = q.QueryRow(ctx, `
			SELECT store_id,platform_dispatch_lead_seconds,store_make_grace_seconds,
			       rider_early_grace_seconds,rider_subsidy_per_minute::float8,store_wait_score_seconds
			FROM wait_thresholds WHERE store_id=0`).Scan(
			&t.StoreID, &t.PlatformDispatchLeadS, &t.StoreMakeGraceS,
			&t.RiderEarlyGraceS, &t.RiderSubsidyPerMinute, &t.StoreWaitScoreSeconds)
	}
	if t.StoreWaitScoreSeconds == 0 {
		t.StoreWaitScoreSeconds = 300
	}
	return t, err
}

// ---------- 时间线锚点 ----------

type waitArrivalRow struct {
	ID             int64
	ArrivalNo      int
	RiderName      string
	ArrivedAt      time.Time
	LeftAt         *time.Time
	QueueCups      *int
	QueueAheadCups *int
	SurgeActive    bool
	SurgeSnapshot  string
}

type dispatchAnchor struct {
	ID         int64
	Seq        int
	EventType  string
	RiderName  string
	OccurredAt time.Time
	ETA        *time.Time
}

type waitInput struct {
	OrderID    int64
	StoreID    int64
	Status     string
	OrderedAt  time.Time
	RiderETA   *time.Time
	MakeStart  time.Time
	ReadyAt    *time.Time
	HandedAt   *time.Time
	CanceledAt *time.Time
	Dispatch   *dispatchAnchor
	Arrivals   []waitArrivalRow
	Th         waitThresholds

	EndTime   time.Time
	EndReason string
}

func loadWaitInput(ctx context.Context, q rowQuerier, orderID int64) (*waitInput, error) {
	in := waitInput{OrderID: orderID}
	var eta, ready, handed, canceled *time.Time
	err := q.QueryRow(ctx, `
		SELECT store_id,status,ordered_at,rider_eta,
		       COALESCE((SELECT min(started_at) FROM order_cups c
		         WHERE c.order_id=orders.id AND c.status<>'voided' AND c.started_at IS NOT NULL),ordered_at),
		       all_sealed_at,handed_at,canceled_at
		FROM orders WHERE id=$1`, orderID).Scan(
		&in.StoreID, &in.Status, &in.OrderedAt, &eta, &in.MakeStart, &ready, &handed, &canceled)
	if err != nil {
		return nil, err
	}
	in.RiderETA, in.ReadyAt, in.HandedAt, in.CanceledAt = eta, ready, handed, canceled

	if in.Th, err = getWaitThresholds(ctx, q, in.StoreID); err != nil {
		return nil, err
	}

	// 最新一次派单/改配（归因只认最新平台动作时间；历史派单保留在事件表可审计）
	var d dispatchAnchor
	var dETA *time.Time
	err = q.QueryRow(ctx, `
		SELECT id,seq,event_type,COALESCE(rider_name,''),occurred_at,eta
		FROM dispatch_events WHERE order_id=$1 ORDER BY seq DESC LIMIT 1`, orderID).
		Scan(&d.ID, &d.Seq, &d.EventType, &d.RiderName, &d.OccurredAt, &dETA)
	if err == nil {
		d.ETA = dETA
		in.Dispatch = &d
	} else if err != pgx.ErrNoRows {
		return nil, err
	}
	if in.Dispatch == nil {
		// 历史单无派单事件：以接单时间为派单时间
		in.Dispatch = &dispatchAnchor{EventType: "ordered", RiderName: "平台推单", OccurredAt: in.OrderedAt}
	}

	rows, err := q.Query(ctx, `
		SELECT id,arrival_no,COALESCE(rider_name,''),arrived_at,left_at,queue_cups,queue_ahead_cups,
		       surge_active,COALESCE(surge_snapshot,'')
		FROM rider_arrivals WHERE order_id=$1 ORDER BY arrival_no,id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ar waitArrivalRow
		if err := rows.Scan(&ar.ID, &ar.ArrivalNo, &ar.RiderName, &ar.ArrivedAt, &ar.LeftAt,
			&ar.QueueCups, &ar.QueueAheadCups, &ar.SurgeActive, &ar.SurgeSnapshot); err != nil {
			return nil, err
		}
		in.Arrivals = append(in.Arrivals, ar)
	}
	if in.MakeStart.IsZero() {
		in.MakeStart = in.OrderedAt
	}

	switch {
	case in.HandedAt != nil:
		in.EndTime = *in.HandedAt
		in.EndReason = "handed"
	case in.CanceledAt != nil:
		in.EndTime = *in.CanceledAt
		in.EndReason = "canceled"
	default:
		in.EndTime = time.Now()
		in.EndReason = "provisional"
	}
	return &in, nil
}

// dueAt 骑手“应到时间”：优先平台承诺 ETA；无 ETA 时取派单时间+前置阈值，
// 此时理论出杯时刻与应到时刻重合，平台不可能被判派单过早。
func (in *waitInput) dueAt() time.Time {
	if in.RiderETA != nil {
		return *in.RiderETA
	}
	return in.Dispatch.OccurredAt.Add(time.Duration(in.Th.PlatformDispatchLeadS) * time.Second)
}

// ---------- 计算结果 ----------

type waitSegmentOut struct {
	ArrivalID   int64     `json:"arrival_id"`
	SegmentType string    `json:"segment_type"`
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	Seconds     int       `json:"seconds"`
	QueueCups   *int      `json:"queue_cups,omitempty"`
	SurgeActive bool      `json:"surge_active"`
	RiderName   string    `json:"rider_name,omitempty"`
	Note        string    `json:"note,omitempty"`
}

type waitResult struct {
	Segments        []waitSegmentOut `json:"segments"`
	TotalSeconds    int              `json:"total_seconds"`
	PlatformSec     int              `json:"platform_early_dispatch_seconds"`
	StoreSec        int              `json:"store_make_timeout_seconds"`
	RiderSec        int              `json:"rider_early_arrival_seconds"`
	ExemptedSec     int              `json:"exempted_seconds"`
	GraceSec        int              `json:"grace_seconds"`
	SubsidyStore    float64          `json:"rider_subsidy_store"`
	SubsidyPlatform float64          `json:"rider_subsidy_platform"`
	SubsidyTotal    float64          `json:"rider_subsidy_total"`
}

func segSeconds(a, b time.Time) int {
	if !b.After(a) {
		return 0
	}
	return int(b.Sub(a).Seconds())
}

func minT(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// compute 按时间线把全部到店-离店区间切为互不重叠的段。
func (in *waitInput) compute(exemptStore bool) *waitResult {
	res := &waitResult{}
	due := in.dueAt()
	riderDueEarly := due.Add(-time.Duration(in.Th.RiderEarlyGraceS) * time.Second)
	storeDeadline := due.Add(time.Duration(in.Th.StoreMakeGraceS) * time.Second)
	platformDeadline := in.Dispatch.OccurredAt.Add(
		time.Duration(in.Th.PlatformDispatchLeadS) * time.Second)
	dispatchLate := platformDeadline.After(storeDeadline)
	ready := in.EndTime
	if in.ReadyAt != nil {
		ready = *in.ReadyAt
	}

	// 防区间重叠（离店再到店数据异常时兜底）
	coveredUntil := time.Time{}
	add := func(ar waitArrivalRow, typ string, s, e time.Time, note string) {
		if !e.After(s) {
			return
		}
		if exemptStore && typ == "store_make_timeout" {
			typ = "store_exempted"
		}
		n := segSeconds(s, e)
		res.Segments = append(res.Segments, waitSegmentOut{
			ArrivalID: ar.ID, SegmentType: typ, StartTime: s, EndTime: e, Seconds: n,
			QueueCups: ar.QueueCups, SurgeActive: ar.SurgeActive, RiderName: ar.RiderName, Note: note,
		})
		switch typ {
		case "platform_early_dispatch":
			res.PlatformSec += n
		case "store_make_timeout":
			res.StoreSec += n
		case "rider_early_arrival":
			res.RiderSec += n
		case "store_exempted":
			res.ExemptedSec += n
		default:
			res.GraceSec += n
		}
	}

	for _, ar := range in.Arrivals {
		as := ar.ArrivedAt
		if as.Before(coveredUntil) {
			as = coveredUntil
		}
		ae := in.EndTime
		if ar.LeftAt != nil && ar.LeftAt.Before(ae) {
			ae = *ar.LeftAt
		}
		if !ae.After(as) {
			continue
		}
		coveredUntil = ae
		res.TotalSeconds += segSeconds(as, ae)

		cur := as
		// 1) 骑手提前到店 [到店, 应到-骑手宽限)
		if re := minT(ae, riderDueEarly); re.After(cur) {
			add(ar, "rider_early_arrival", cur, re, "骑手早于预计到店时间到店等待")
			cur = re
		}
		if !ae.After(cur) {
			continue
		}
		// 2) 宽限 [应到-骑手宽限, 应到+门店宽限)，出杯即止
		if ge := minT3(ae, storeDeadline, ready); ge.After(cur) {
			add(ar, "grace", cur, ge, "提前到店宽限/门店出杯宽限")
			cur = ge
		}
		if !ae.After(cur) {
			continue
		}
		// 3) 超时等待 [应到+门店宽限, 出杯)：按理论最早出杯时刻 platformDeadline 拆平台/门店
		timeoutEnd := minT(ae, ready)
		if timeoutEnd.After(cur) {
			if dispatchLate {
				if pe := minT(timeoutEnd, platformDeadline); pe.After(cur) {
					add(ar, "platform_early_dispatch", cur, pe,
						fmt.Sprintf("平台%s给门店预留制作时间不足 %d 秒",
							map[bool]string{true: "改配", false: "派单"}[in.Dispatch.EventType == "redispatch"],
							in.Th.PlatformDispatchLeadS))
					cur = pe
				}
			}
			if se := timeoutEnd; se.After(cur) {
				add(ar, "store_make_timeout", maxT(cur, platformDeadline), se,
					"超过平台预留前置后门店仍未出杯")
				cur = se
			}
		}
		// 4) 出杯后到离店/交接的尾段 → 无责任
		if ae.After(cur) {
			add(ar, "grace", cur, ae, "出杯后等待交接尾段")
		}
	}

	rate := in.Th.RiderSubsidyPerMinute / 60.0
	res.SubsidyStore = round2(float64(res.StoreSec) * rate)
	res.SubsidyPlatform = round2(float64(res.PlatformSec+res.ExemptedSec) * rate)
	res.SubsidyTotal = round2(res.SubsidyStore + res.SubsidyPlatform)
	return res
}

func minT3(a, b, c time.Time) time.Time {
	return minT(minT(a, b), c)
}

// ---------- 持久化（整体换版 + 结算红冲，天然去重） ----------

// persistWaitAttribution 重新计算并落一个新版本；同事务内完成旧补贴红冲、
// 到店记录回算、rider_early 异常归责更新。reviewID 为触发本次换版的复核单。
// 豁免（机器故障/临时缺货等改判）是粘滞属性：该单只要存在 approved 申诉，
// 任何后续重算（交接/取消/手动）都会自动把门店责任段转为豁免。
// 返回新版本 id / 版本号 / 输入 / 结果 / 是否豁免。
func (a *App) persistWaitAttribution(ctx context.Context, tx pgx.Tx, orderID int64,
	reason string, reviewID *int64, by int64) (int64, int, *waitInput, *waitResult, bool, error) {

	if _, err := tx.Exec(ctx, `SELECT id FROM orders WHERE id=$1 FOR UPDATE`, orderID); err != nil {
		return 0, 0, nil, nil, false, err
	}
	in, err := loadWaitInput(ctx, tx, orderID)
	if err != nil {
		return 0, 0, nil, nil, false, err
	}
	var approvedN int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM wait_reviews WHERE order_id=$1 AND status='approved'`, orderID).
		Scan(&approvedN); err != nil {
		return 0, 0, nil, nil, false, err
	}
	exemptStore := approvedN > 0
	res := in.compute(exemptStore)

	var oldIDs []int64
	rows, err := tx.Query(ctx,
		`SELECT id FROM wait_attributions WHERE order_id=$1 AND status='active' ORDER BY id FOR UPDATE`, orderID)
	if err != nil {
		return 0, 0, nil, nil, false, err
	}
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		oldIDs = append(oldIDs, id)
	}
	rows.Close()
	if len(oldIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE wait_attributions SET status='superseded' WHERE order_id=$1 AND status='active'`,
			orderID); err != nil {
			return 0, 0, nil, nil, false, err
		}
	}

	var versionNo int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(max(version_no),0)+1 FROM wait_attributions WHERE order_id=$1`, orderID).
		Scan(&versionNo); err != nil {
		return 0, 0, nil, nil, false, err
	}

	timeline := map[string]any{
		"ordered_at":  in.OrderedAt,
		"dispatch":    nil,
		"rider_eta":   in.RiderETA,
		"make_start":  in.MakeStart,
		"ready_at":    in.ReadyAt,
		"handed_at":   in.HandedAt,
		"canceled_at": in.CanceledAt,
		"due_at":      in.dueAt(),
		"platform_deadline": in.Dispatch.OccurredAt.Add(
			time.Duration(in.Th.PlatformDispatchLeadS) * time.Second),
		"store_deadline": dueStoreDeadline(in),
		"end_time":       in.EndTime,
		"end_reason":     in.EndReason,
		"arrivals":       arrivalSnapshots(in.Arrivals),
	}
	if in.Dispatch != nil {
		timeline["dispatch"] = map[string]any{
			"seq": in.Dispatch.Seq, "event_type": in.Dispatch.EventType,
			"rider": in.Dispatch.RiderName, "occurred_at": in.Dispatch.OccurredAt, "eta": in.Dispatch.ETA,
		}
	}
	params := map[string]any{
		"platform_dispatch_lead_seconds": in.Th.PlatformDispatchLeadS,
		"store_make_grace_seconds":       in.Th.StoreMakeGraceS,
		"rider_early_grace_seconds":      in.Th.RiderEarlyGraceS,
		"rider_subsidy_per_minute":       in.Th.RiderSubsidyPerMinute,
		"store_wait_score_seconds":       in.Th.StoreWaitScoreSeconds,
	}

	var attrID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO wait_attributions(order_id,store_id,version_no,status,reason,end_reason,
		  total_wait_seconds,platform_seconds,store_seconds,rider_seconds,exempted_seconds,grace_seconds,
		  rider_subsidy_total,rider_subsidy_store,rider_subsidy_platform,params,timeline,review_id,created_by)
		VALUES($1,$2,$3,'active',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING id`,
		orderID, in.StoreID, versionNo, reason, in.EndReason,
		res.TotalSeconds, res.PlatformSec, res.StoreSec, res.RiderSec, res.ExemptedSec, res.GraceSec,
		res.SubsidyTotal, res.SubsidyStore, res.SubsidyPlatform, params, timeline, reviewID, byOrNil(by),
	).Scan(&attrID); err != nil {
		return 0, 0, nil, nil, false, err
	}

	for _, s := range res.Segments {
		if _, err := tx.Exec(ctx, `
			INSERT INTO wait_segments(attribution_id,order_id,store_id,arrival_id,segment_type,
			  start_time,end_time,seconds,queue_cups,surge_active,rider_name,note)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			attrID, orderID, in.StoreID, nullableArrival(s.ArrivalID), s.SegmentType,
			s.StartTime, s.EndTime, s.Seconds, s.QueueCups, s.SurgeActive, s.RiderName, s.Note); err != nil {
			return 0, 0, nil, nil, false, err
		}
	}

	// 结算红冲：旧版本补贴流水逐张红冲（reversal_of 指向原流水，审计链完整），再按新版本入账

	// 结算红冲：旧版本补贴流水逐张红冲（reversal_of 指向原流水，审计链完整），再按新版本入账
	if len(oldIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO finance_records(store_id,order_id,attribution_id,review_id,reversal_of,
			  kind,amount,bearer,period_date,note,created_by)
			SELECT store_id,order_id,$2,$3,id,kind,-amount,bearer,period_date,'等待归因换版红冲',$4
			FROM finance_records
			WHERE kind='rider_wait_subsidy' AND reversal_of IS NULL
			  AND attribution_id = ANY($1)`,
			oldIDs, attrID, reviewID, byOrNil(by)); err != nil {
			return 0, 0, nil, nil, false, err
		}
	}
	if res.SubsidyStore > 0 {
		if err := insertSubsidy(ctx, tx, in.StoreID, orderID, attrID, reviewID,
			res.SubsidyStore, "store", res.StoreSec, by); err != nil {
			return 0, 0, nil, nil, false, err
		}
	}
	if res.SubsidyPlatform > 0 {
		if err := insertSubsidy(ctx, tx, in.StoreID, orderID, attrID, reviewID,
			res.SubsidyPlatform, "platform", res.PlatformSec+res.ExemptedSec, by); err != nil {
			return 0, 0, nil, nil, false, err
		}
	}

	// 回到店记录：等待总时长与当前有效门店责任时长（豁免段不再算门店责任）
	perArrival := map[int64][2]int{}
	for _, ar := range in.Arrivals {
		end := in.EndTime
		if ar.LeftAt != nil && ar.LeftAt.Before(end) {
			end = *ar.LeftAt
		}
		if end.After(ar.ArrivedAt) {
			perArrival[ar.ID] = [2]int{segSeconds(ar.ArrivedAt, end), 0}
		}
	}
	for _, s := range res.Segments {
		v := perArrival[s.ArrivalID]
		if s.SegmentType == "store_make_timeout" {
			v[1] += s.Seconds
		}
		perArrival[s.ArrivalID] = v
	}
	for aid, v := range perArrival {
		var readyAt *time.Time
		if in.ReadyAt != nil && !in.ReadyAt.After(in.EndTime) {
			readyAt = in.ReadyAt
		}
		if _, err := tx.Exec(ctx, `
			UPDATE rider_arrivals
			   SET wait_seconds=$2, store_fault_wait_seconds=$3,
			       ready_at=COALESCE(ready_at,$4::timestamptz),
			       handed_at=CASE WHEN $5::timestamptz IS NOT NULL AND left_at IS NULL THEN $5::timestamptz ELSE handed_at END
			 WHERE id=$1`, aid, v[0], v[1], readyAt, in.HandedAt); err != nil {
			return 0, 0, nil, nil, false, err
		}
	}

	// rider_early 异常按当前归因结果归责（改判会再次覆盖，resolution 全程留痕）
	responsibility := "rider"
	switch {
	case res.StoreSec > 0 && (res.PlatformSec+res.ExemptedSec) > 0:
		responsibility = "shared"
	case res.StoreSec > 0:
		responsibility = "store"
	case res.PlatformSec + res.ExemptedSec > 0:
		responsibility = "platform"
	}
	resolution := fmt.Sprintf(
		"等待 %d 秒拆分：平台派单过早 %d / 门店制作超时 %d / 骑手提前到店 %d / 豁免 %d / 宽限 %d；"+
			"骑手等待补贴 %.2f 元（门店 %.2f / 平台 %.2f），归因版本 v%d",
		res.TotalSeconds, res.PlatformSec, res.StoreSec, res.RiderSec, res.ExemptedSec, res.GraceSec,
		res.SubsidyTotal, res.SubsidyStore, res.SubsidyPlatform, versionNo)
	if _, err := tx.Exec(ctx, `
		UPDATE exceptions SET status='resolved',resolved_at=now(),resolved_by=$2,
		  responsibility=$3,resolution=$4,
		  detail=detail||jsonb_build_object('attribution_id',$5::bigint,'attribution_version',$6::int,
		    'platform_seconds',$7::int,'store_seconds',$8::int,'rider_seconds',$9::int,'exempted_seconds',$10::int)
		WHERE id=(SELECT id FROM exceptions WHERE order_id=$1 AND type='rider_early' ORDER BY id DESC LIMIT 1)`,
		orderID, byOrNil(by), responsibility, resolution, attrID, versionNo,
		res.PlatformSec, res.StoreSec, res.RiderSec, res.ExemptedSec); err != nil {
		return 0, 0, nil, nil, false, err
	}

	if err := a.logEvent(ctx, tx, orderID, "wait_attributed", nil, map[string]any{
		"version": versionNo, "reason": reason, "end_reason": in.EndReason,
		"total_seconds":                   res.TotalSeconds,
		"platform_early_dispatch_seconds": res.PlatformSec,
		"store_make_timeout_seconds":      res.StoreSec,
		"rider_early_arrival_seconds":     res.RiderSec,
		"exempted_seconds":                res.ExemptedSec,
		"grace_seconds":                   res.GraceSec,
		"rider_subsidy_total":             res.SubsidyTotal,
		"rider_subsidy_store":             res.SubsidyStore,
		"rider_subsidy_platform":          res.SubsidyPlatform,
		"review_id":                       reviewID,
	}); err != nil {
		return 0, 0, nil, nil, false, err
	}
	return attrID, versionNo, in, res, exemptStore, nil
}

func dueStoreDeadline(in *waitInput) time.Time {
	return in.dueAt().Add(time.Duration(in.Th.StoreMakeGraceS) * time.Second)
}

func insertSubsidy(ctx context.Context, tx pgx.Tx, storeID, orderID, attrID int64,
	reviewID *int64, amount float64, bearer string, seconds int, by int64) error {
	note := fmt.Sprintf("骑手等待补贴：%s承担 %d 秒",
		map[string]string{"store": "门店", "platform": "平台"}[bearer], seconds)
	_, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,attribution_id,review_id,kind,amount,bearer,
		  period_date,note,created_by)
		SELECT $1,$2,$3,$4,'rider_wait_subsidy',$5,$6,(SELECT ordered_at::date FROM orders WHERE id=$2),$7,$8`,
		storeID, orderID, attrID, reviewID, -amount, bearer, note, byOrNil(by))
	return err
}

func byOrNil(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

func nullableArrival(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

func arrivalSnapshots(ars []waitArrivalRow) []map[string]any {
	out := make([]map[string]any, 0, len(ars))
	for _, ar := range ars {
		out = append(out, map[string]any{
			"arrival_id": ar.ID, "arrival_no": ar.ArrivalNo, "rider": ar.RiderName,
			"arrived_at": ar.ArrivedAt, "left_at": ar.LeftAt,
			"queue_cups": ar.QueueCups, "queue_ahead_cups": ar.QueueAheadCups,
			"surge_active": ar.SurgeActive, "surge_snapshot": ar.SurgeSnapshot,
		})
	}
	return out
}

// ---------- 查询 ----------

type storedAttribution struct {
	ID              int64        `json:"id"`
	Version         int          `json:"version_no"`
	Status          string       `json:"status"`
	Reason          string       `json:"reason"`
	EndReason       string       `json:"end_reason"`
	Total           int          `json:"total_seconds"`
	Platform        int          `json:"platform_early_dispatch_seconds"`
	Store           int          `json:"store_make_timeout_seconds"`
	Rider           int          `json:"rider_early_arrival_seconds"`
	Exempted        int          `json:"exempted_seconds"`
	Grace           int          `json:"grace_seconds"`
	SubsidyTotal    float64      `json:"rider_subsidy_total"`
	SubsidyStore    float64      `json:"rider_subsidy_store"`
	SubsidyPlatform float64      `json:"rider_subsidy_platform"`
	CreatedAt       time.Time    `json:"created_at"`
	Params          map[string]any `json:"params"`
	Timeline        map[string]any `json:"timeline"`
}

func loadStoredAttribution(ctx context.Context, q rowQuerier, orderID int64) (*storedAttribution, error) {
	var s storedAttribution
	var paramsRaw, tlRaw []byte
	err := q.QueryRow(ctx, `
		SELECT id,version_no,status,reason,end_reason,total_wait_seconds,platform_seconds,store_seconds,
		       rider_seconds,exempted_seconds,grace_seconds,rider_subsidy_total::float8,
		       rider_subsidy_store::float8,rider_subsidy_platform::float8,created_at,params,timeline
		FROM wait_attributions WHERE order_id=$1 AND status='active' ORDER BY version_no DESC LIMIT 1`,
		orderID).Scan(&s.ID, &s.Version, &s.Status, &s.Reason, &s.EndReason, &s.Total,
		&s.Platform, &s.Store, &s.Rider, &s.Exempted, &s.Grace, &s.SubsidyTotal,
		&s.SubsidyStore, &s.SubsidyPlatform, &s.CreatedAt, &paramsRaw, &tlRaw)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(paramsRaw, &s.Params)
	_ = json.Unmarshal(tlRaw, &s.Timeline)
	return &s, nil
}

func loadSegments(ctx context.Context, q rowQuerier, attrID int64) ([]map[string]any, error) {
	rows, err := q.Query(ctx, `
		SELECT id,COALESCE(arrival_id,0),segment_type,start_time,end_time,seconds,
		       COALESCE(queue_cups,0),surge_active,COALESCE(rider_name,''),COALESCE(note,'')
		FROM wait_segments WHERE attribution_id=$1 ORDER BY start_time,id`, attrID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, aid, sec, queue int64
		var typ, rider, note string
		var st, et time.Time
		var surge bool
		if err := rows.Scan(&id, &aid, &typ, &st, &et, &sec, &queue, &surge, &rider, &note); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "arrival_id": aid, "segment_type": typ,
			"start_time": st, "end_time": et, "seconds": sec,
			"queue_cups": queue, "surge_active": surge, "rider_name": rider, "note": note,
		})
	}
	return out, nil
}

func (a *App) loadOrderWaitReviews(ctx context.Context, orderID int64) ([]map[string]any, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT id,store_id,status,reason_code,reason_detail,evidence,requested_by,requested_at,
		       COALESCE(reviewed_by,0),reviewed_at,COALESCE(decision,''),COALESCE(review_note,''),
		       COALESCE(attribution_before,0),COALESCE(attribution_after,0),
		       before_metrics,after_metrics
		FROM wait_reviews WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, storeID, reqBy, revBy, beforeID, afterID int64
		var status, code, detail, decision, note string
		var evidence, beforeM, afterM []byte
		var reqAt time.Time
		var revAt *time.Time
		if err := rows.Scan(&id, &storeID, &status, &code, &detail, &evidence, &reqBy, &reqAt,
			&revBy, &revAt, &decision, &note, &beforeID, &afterID, &beforeM, &afterM); err != nil {
			return nil, err
		}
		var ev, bm, am map[string]any
		_ = json.Unmarshal(evidence, &ev)
		_ = json.Unmarshal(beforeM, &bm)
		_ = json.Unmarshal(afterM, &am)
		out = append(out, map[string]any{
			"id": id, "store_id": storeID, "status": status, "reason_code": code,
			"reason_detail": detail, "evidence": ev, "requested_by": reqBy, "requested_at": reqAt,
			"reviewed_by": revBy, "reviewed_at": revAt, "decision": decision, "review_note": note,
			"attribution_before": beforeID, "attribution_after": afterID,
			"before_metrics": bm, "after_metrics": am,
		})
	}
	return out, nil
}

// ---------- HTTP: 订单等待归因查询 / 手动重算 ----------

func (a *App) getWaitAttribution(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()
	var orderStore int64
	var status string
	if err := a.Pool.QueryRow(ctx, `SELECT store_id,status FROM orders WHERE id=$1`, orderID).
		Scan(&orderStore, &status); err != nil {
		writeErr(w, err)
		return
	}
	reviews, err := a.loadOrderWaitReviews(ctx, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	stored, err := loadStoredAttribution(ctx, a.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if stored != nil {
		segs, err := loadSegments(ctx, a.Pool, stored.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"order_id": orderID, "store_id": orderStore, "order_status": status,
			"provisional": false, "attribution": stored, "segments": segs, "reviews": reviews,
		})
		return
	}
	// 未结账：实时试算（不落库、不结算）
	in, err := loadWaitInput(ctx, a.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	res := in.compute(false)
	writeJSON(w, 200, map[string]any{
		"order_id": orderID, "store_id": orderStore, "order_status": status,
		"provisional": true,
		"attribution": map[string]any{
			"status": "provisional", "reason": "preview", "end_reason": in.EndReason,
			"total_seconds":                   res.TotalSeconds,
			"platform_early_dispatch_seconds": res.PlatformSec,
			"store_make_timeout_seconds":      res.StoreSec,
			"rider_early_arrival_seconds":     res.RiderSec,
			"exempted_seconds":                res.ExemptedSec,
			"grace_seconds":                   res.GraceSec,
			"rider_subsidy_total":             res.SubsidyTotal,
			"rider_subsidy_store":             res.SubsidyStore,
			"rider_subsidy_platform":          res.SubsidyPlatform,
			"params": map[string]any{
				"platform_dispatch_lead_seconds": in.Th.PlatformDispatchLeadS,
				"store_make_grace_seconds":       in.Th.StoreMakeGraceS,
				"rider_early_grace_seconds":      in.Th.RiderEarlyGraceS,
				"rider_subsidy_per_minute":       in.Th.RiderSubsidyPerMinute,
			},
		},
		"segments": res.Segments, "reviews": reviews,
	})
}

func (a *App) recomputeWait(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	attrID, version, in, res, exempted, err := a.persistWaitAttribution(ctx, tx, orderID, "manual", nil, ac.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"attribution_id": attrID, "version_no": version, "order_id": orderID,
		"end_reason": in.EndReason, "segments": res.Segments,
		"platform_seconds": res.PlatformSec, "store_seconds": res.StoreSec,
		"rider_seconds":    res.RiderSec, "exempted_seconds": res.ExemptedSec,
		"store_exempted":    exempted,
		"rider_subsidy_total": res.SubsidyTotal,
	})
}
