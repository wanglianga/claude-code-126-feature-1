package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// 店长对骑手等待归因发起申诉（机器故障/临时缺货/爆单不可抗力等），
// 区域经理复核通过后改判：原门店责任段转为豁免（store_exempted）并由平台承担补贴；
// 改判在同一事务内完成归因换版、补贴红冲重结与班组评分回算。
// 已取消/已交接订单的申诉只调整绩效结算，绝不改写订单状态与交接事实。

var validWaitReviewReasons = map[string]string{
	"machine_fault":      "机器故障（咖啡机/封口机/冰柜等）",
	"temp_shortage":      "临时缺货（原料/包材突发短缺）",
	"surge_force_majeure": "爆单不可抗力（瞬时订单量超保护阈值）",
	"other":              "其他（需区域经理核实）",
}

// ---------- 店长申诉 ----------

type waitAppealReq struct {
	ReasonCode  string         `json:"reason_code"`
	ReasonDetail string        `json:"reason_detail"`
	Evidence    map[string]any `json:"evidence"` // 照片编号、维修单号、断电记录等
}

func (a *App) createWaitAppeal(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	var req waitAppealReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if _, ok := validWaitReviewReasons[req.ReasonCode]; !ok {
		writeErr(w, errf(400, "bad_request", "reason_code 非法：machine_fault/temp_shortage/surge_force_majeure/other"))
		return
	}
	if req.ReasonDetail == "" {
		req.ReasonDetail = validWaitReviewReasons[req.ReasonCode]
	}
	if req.Evidence == nil {
		req.Evidence = map[string]any{}
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
	var status string
	if err := tx.QueryRow(ctx, `SELECT store_id,status FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&storeID, &status); err != nil {
		writeErr(w, err)
		return
	}
	// 店长只能申诉本门店订单
	if ac.Role == "store_manager" && (ac.StoreID == nil || *ac.StoreID != storeID) {
		writeErr(w, errForbidden)
		return
	}
	// 申诉必须存在骑手等待事实
	var arrivalN int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM rider_arrivals WHERE order_id=$1`, orderID).Scan(&arrivalN); err != nil {
		writeErr(w, err)
		return
	}
	if arrivalN == 0 {
		writeErr(w, errf(409, "no_wait", "该订单没有骑手到店等待记录，无需申诉"))
		return
	}
	// 同一订单已有待复核申诉，不重复发起
	var pending int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM wait_reviews WHERE order_id=$1 AND status='pending'`, orderID).Scan(&pending); err != nil {
		writeErr(w, err)
		return
	}
	if pending > 0 {
		writeErr(w, errf(409, "appeal_pending", "该订单已有待区域经理复核的申诉"))
		return
	}

	var reviewID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO wait_reviews(order_id,store_id,status,reason_code,reason_detail,evidence,requested_by)
		VALUES($1,$2,'pending',$3,$4,$5,$6) RETURNING id`,
		orderID, storeID, req.ReasonCode, req.ReasonDetail, req.Evidence, ac.ID).Scan(&reviewID); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "wait_appeal_created", actorOf(ac), map[string]any{
		"review_id": reviewID, "reason_code": req.ReasonCode,
		"reason_detail": req.ReasonDetail, "evidence": req.Evidence,
		"order_status": status,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"review_id": reviewID, "order_id": orderID, "store_id": storeID,
		"status": "pending", "reason_code": req.ReasonCode, "reason_detail": req.ReasonDetail,
		"note": "申诉已提交区域经理复核；复核通过前不改变现行归因与结算",
	})
}

// ---------- 申诉列表 ----------

func (a *App) listWaitReviews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ac := actor(r)
	sql := `
		SELECT rv.id,rv.order_id,rv.store_id,s.name,rv.status,rv.reason_code,rv.reason_detail,
		       rv.evidence,rv.requested_by,rv.requested_at,COALESCE(rv.reviewed_by,0),rv.reviewed_at,
		       COALESCE(rv.decision,''),COALESCE(rv.review_note,''),
		       COALESCE(rv.attribution_before,0),COALESCE(rv.attribution_after,0),
		       rv.before_metrics,rv.after_metrics
		FROM wait_reviews rv JOIN stores s ON s.id=rv.store_id WHERE 1=1`
	args := []any{}
	if st := q.Get("status"); st != "" {
		args = append(args, st)
		sql += " AND rv.status=$" + itoa(int64(len(args)))
	}
	if sid := q.Get("store_id"); sid != "" {
		args = append(args, toInt64(sid))
		sql += " AND rv.store_id=$" + itoa(int64(len(args)))
	}
	// 店长只看本门店
	if ac.Role == "store_manager" {
		if ac.StoreID == nil {
			writeErr(w, errForbidden)
			return
		}
		args = append(args, *ac.StoreID)
		sql += " AND rv.store_id=$" + itoa(int64(len(args)))
	}
	sql += " ORDER BY rv.id DESC LIMIT 200"
	rows, err := a.Pool.Query(r.Context(), sql, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, oid, sid, reqBy, revBy, beforeID, afterID int64
		var storeName, status, code, detail, decision, note string
		var evidence, beforeM, afterM []byte
		var reqAt time.Time
		var revAt *time.Time
		if err := rows.Scan(&id, &oid, &sid, &storeName, &status, &code, &detail, &evidence,
			&reqBy, &reqAt, &revBy, &revAt, &decision, &note, &beforeID, &afterID, &beforeM, &afterM); err != nil {
			writeErr(w, err)
			return
		}
		var ev, bm, am map[string]any
		_ = json.Unmarshal(evidence, &ev)
		_ = json.Unmarshal(beforeM, &bm)
		_ = json.Unmarshal(afterM, &am)
		out = append(out, map[string]any{
			"id": id, "order_id": oid, "store_id": sid, "store_name": storeName,
			"status": status, "reason_code": code, "reason_detail": detail, "evidence": ev,
			"requested_by": reqBy, "requested_at": reqAt,
			"reviewed_by": revBy, "reviewed_at": revAt, "decision": decision, "review_note": note,
			"attribution_before": beforeID, "attribution_after": afterID,
			"before_metrics": bm, "after_metrics": am,
		})
	}
	writeJSON(w, 200, out)
}

// ---------- 区域经理复核 ----------

type reviewDecisionReq struct {
	Decision string `json:"decision"` // approve / reject
	ReviewNote string `json:"review_note"`
}

func (a *App) decideWaitReview(w http.ResponseWriter, r *http.Request) {
	reviewID := toInt64(chi.URLParam(r, "id"))
	var req reviewDecisionReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		writeErr(w, errf(400, "bad_request", "decision 必须是 approve/reject"))
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

	var orderID, storeID int64
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT order_id,store_id FROM wait_reviews WHERE id=$1 FOR UPDATE`, reviewID).
		Scan(&orderID, &storeID); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, orderID).Scan(&status); err != nil {
		writeErr(w, err)
		return
	}
	var curStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM wait_reviews WHERE id=$1`, reviewID).Scan(&curStatus); err != nil {
		writeErr(w, err)
		return
	}
	if curStatus != "pending" {
		writeErr(w, errf(409, "already_reviewed", "申诉已复核：%s", curStatus))
		return
	}

	if req.Decision == "reject" {
		if _, err := tx.Exec(ctx, `
			UPDATE wait_reviews SET status='rejected',reviewed_by=$2,reviewed_at=now(),
			  decision='reject',review_note=$3,updated_at=now() WHERE id=$1`,
			reviewID, ac.ID, req.ReviewNote); err != nil {
			writeErr(w, err)
			return
		}
		if err := a.logEvent(ctx, tx, orderID, "wait_appeal_rejected", actorOf(ac), map[string]any{
			"review_id": reviewID, "review_note": req.ReviewNote,
		}); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"review_id": reviewID, "status": "rejected", "order_id": orderID,
			"note": "申诉驳回，现行等待归因与结算不变",
		})
		return
	}

	// ---- 复核通过：改判 ----
	// 改判前指标快照（现行生效归因）
	beforeSnap := map[string]any{}
	var beforeAttrID int64
	if b, err := loadStoredAttribution(ctx, tx, orderID); err != nil && err != pgx.ErrNoRows {
		writeErr(w, err)
		return
	} else if b != nil {
		beforeAttrID = b.ID
		beforeSnap = map[string]any{
			"attribution_id": b.ID, "version_no": b.Version,
			"total_seconds": b.Total, "platform_seconds": b.Platform,
			"store_seconds": b.Store, "rider_seconds": b.Rider,
			"exempted_seconds": b.Exempted, "grace_seconds": b.Grace,
			"rider_subsidy_total": b.SubsidyTotal,
			"rider_subsidy_store": b.SubsidyStore,
			"rider_subsidy_platform": b.SubsidyPlatform,
		}
	}
	var scoreBefore int
	var scoreDay string
	if err := tx.QueryRow(ctx, `SELECT ordered_at::date::text FROM orders WHERE id=$1`, orderID).Scan(&scoreDay); err != nil {
		writeErr(w, err)
		return
	}
	_ = tx.QueryRow(ctx,
		`SELECT score FROM team_score_snapshots WHERE store_id=$1 AND score_date=$2`,
		storeID, scoreDay).Scan(&scoreBefore)
	beforeSnap["team_score_before"] = scoreBefore

	// 先置 approved：归因引擎的豁免是粘滞的，approved 申诉存在即自动把门店责任段转豁免
	if _, err := tx.Exec(ctx, `
		UPDATE wait_reviews SET status='approved',reviewed_by=$2,reviewed_at=now(),
		  decision='approve',review_note=$3,attribution_before=$4,before_metrics=$5,updated_at=now()
		WHERE id=$1`,
		reviewID, ac.ID, req.ReviewNote, nullableID(beforeAttrID), beforeSnap); err != nil {
		writeErr(w, err)
		return
	}

	// 改判重算（红冲旧补贴 → 按豁免后新版本重结；绝不更新订单状态/handed_at/handoffs）
	rid := reviewID
	afterAttrID, version, in, res, exempted, err := a.persistWaitAttribution(
		ctx, tx, orderID, "review", &rid, ac.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !exempted {
		// 无门店责任段（本来就没有门店超时）：仍保留 approved 结论，但没有金额/绩效变化
	}

	// 回算班组评分（订单当日快照）
	scoreAfter, scoreMetrics, err := a.refreshTeamScoreTx(ctx, tx, storeID, scoreDay)
	if err != nil {
		writeErr(w, err)
		return
	}
	afterSnap := map[string]any{
		"attribution_id": afterAttrID, "version_no": version,
		"end_reason": in.EndReason,
		"total_seconds": res.TotalSeconds, "platform_seconds": res.PlatformSec,
		"store_seconds": res.StoreSec, "rider_seconds": res.RiderSec,
		"exempted_seconds": res.ExemptedSec, "grace_seconds": res.GraceSec,
		"rider_subsidy_total": res.SubsidyTotal,
		"rider_subsidy_store": res.SubsidyStore,
		"rider_subsidy_platform": res.SubsidyPlatform,
		"team_score_before": scoreBefore, "team_score_after": scoreAfter,
		"team_score_metrics": scoreMetrics,
		"order_status": status,
		"handoff_fact_unchanged": true,
	}
	if _, err := tx.Exec(ctx, `
		UPDATE wait_reviews SET attribution_after=$2,after_metrics=$3,updated_at=now() WHERE id=$1`,
		reviewID, afterAttrID, afterSnap); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "wait_appeal_approved", actorOf(ac), map[string]any{
		"review_id": reviewID, "attribution_before": beforeAttrID,
		"attribution_after": afterAttrID, "version": version,
		"store_seconds_before": beforeSnap["store_seconds"],
		"store_seconds_after":  res.StoreSec,
		"exempted_seconds":     res.ExemptedSec,
		"team_score_before":    scoreBefore, "team_score_after": scoreAfter,
		"review_note":          req.ReviewNote,
		"order_status":         status,
		"note":                 "仅回算绩效结算与评分，订单交接事实不变",
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"review_id": reviewID, "status": "approved", "order_id": orderID,
		"order_status": status, "handoff_fact_unchanged": true,
		"attribution_before": beforeAttrID, "attribution_after": afterAttrID,
		"before": beforeSnap, "after": afterSnap,
		"segments": res.Segments,
	})
}

func nullableID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

// ---------- 班组评分回算（事务内，等待归因驱动） ----------

// refreshTeamScoreTx 重新计算某店某日班组评分：
// 慢单率/错杯/漏餐具/取错袋/过久/漏备注/门店责任取消之外，
// 新增「门店制作超时等待」扣分（累计每 store_wait_score_seconds 扣 1 分）。
// 评分口径直接取 active 归因版本，因此复核改判后自动回算、可重复执行。
func (a *App) refreshTeamScoreTx(ctx context.Context, tx pgx.Tx, storeID int64, day string) (int, map[string]any, error) {
	counts := map[string]int{}
	rows, err := tx.Query(ctx, `
		SELECT 'orders', count(*) FROM orders WHERE store_id=$1 AND ordered_at::date=$2
		UNION ALL SELECT 'slow', count(*) FROM orders WHERE store_id=$1 AND ordered_at::date=$2
		  AND all_sealed_at IS NOT NULL AND all_sealed_at > ordered_at + make_interval(mins => promised_minutes)
		UNION ALL SELECT 'wrong_cup', count(*) FROM exceptions WHERE store_id=$1 AND type='wrong_cup' AND created_at::date=$2
		UNION ALL SELECT 'missing', count(*) FROM exceptions WHERE store_id=$1 AND type IN ('missing_straw','missing_utensil') AND created_at::date=$2
		UNION ALL SELECT 'wrong_bag', count(*) FROM exceptions WHERE store_id=$1 AND type='wrong_bag' AND created_at::date=$2
		UNION ALL SELECT 'stale', count(*) FROM exceptions WHERE store_id=$1 AND type='stale_drink' AND created_at::date=$2
		UNION ALL SELECT 'note_missed', count(*) FROM exceptions WHERE store_id=$1 AND type='note_missed' AND created_at::date=$2
		UNION ALL SELECT 'store_cancel', count(*) FROM orders WHERE store_id=$1 AND status='canceled' AND cancel_store_fault AND canceled_at::date=$2
		UNION ALL SELECT 'wait_store_sec', COALESCE(sum(wa.store_seconds),0)
		  FROM wait_attributions wa JOIN orders o ON o.id=wa.order_id
		  WHERE wa.store_id=$1 AND wa.status='active' AND o.ordered_at::date=$2
		UNION ALL SELECT 'wait_exempt_sec', COALESCE(sum(wa.exempted_seconds),0)
		  FROM wait_attributions wa JOIN orders o ON o.id=wa.order_id
		  WHERE wa.store_id=$1 AND wa.status='active' AND o.ordered_at::date=$2
		UNION ALL SELECT 'wait_store_orders', count(*) FROM wait_attributions wa JOIN orders o ON o.id=wa.order_id
		  WHERE wa.store_id=$1 AND wa.status='active' AND wa.store_seconds>0 AND o.ordered_at::date=$2`,
		storeID, day)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var k string
		var v int
		_ = rows.Scan(&k, &v)
		counts[k] = v
	}
	rows.Close()

	th, err := getWaitThresholds(ctx, tx, storeID)
	if err != nil {
		return 0, nil, err
	}
	counts["wait_pen"] = 0
	if th.StoreWaitScoreSeconds > 0 {
		counts["wait_pen"] = counts["wait_store_sec"] / th.StoreWaitScoreSeconds
	}

	var remakeCost float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(amount),0)::float8 FROM finance_records
		WHERE store_id=$1 AND kind='remake_cost' AND period_date=$2`, storeID, day).Scan(&remakeCost); err != nil {
		return 0, nil, err
	}
	var waitSubsidyStore, waitSubsidyPlatform float64
	_ = tx.QueryRow(ctx, `
		SELECT COALESCE(sum(amount) FILTER (WHERE bearer='store'),0)::float8,
		       COALESCE(sum(amount) FILTER (WHERE bearer='platform'),0)::float8
		FROM finance_records
		WHERE store_id=$1 AND kind='rider_wait_subsidy' AND period_date=$2`,
		storeID, day).Scan(&waitSubsidyStore, &waitSubsidyPlatform)

	score, penalty := computeScore(counts)
	metrics := map[string]any{
		"counts": counts, "penalties": penalty, "remake_cost": remakeCost, "date": day,
		"rider_wait_subsidy_store":    waitSubsidyStore,
		"rider_wait_subsidy_platform": waitSubsidyPlatform,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO team_score_snapshots(store_id,score_date,metrics,score)
		VALUES($1,$2,$3,$4)
		ON CONFLICT (store_id,score_date) DO UPDATE SET metrics=$3,score=$4,updated_at=now()`,
		storeID, day, metrics, score); err != nil {
		return 0, nil, err
	}
	return score, metrics, nil
}
