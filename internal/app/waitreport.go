package app

import (
	"net/http"
)

// 平台服务指标：按平台/门店汇总派单过早导致的骑手等待与平台承担补贴；
// 数据只取 active 归因版本，复核改判后自动反映最新口径。

func (a *App) platformServiceReport(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	q := r.URL.Query()
	platform := q.Get("platform")
	ac := actor(r)
	// 平台账号只能看本平台
	if ac.Role == "platform" || ac.Role == "platform_cs" {
		if platform != "" && platform != ac.Platform {
			writeErr(w, errForbidden)
			return
		}
		platform = ac.Platform
	}

	rows, err := a.Pool.Query(r.Context(), `
		SELECT o.platform,o.store_id,s.name,
		  count(DISTINCT o.id),
		  count(DISTINCT o.id) FILTER (WHERE wa.platform_seconds>0 OR wa.exempted_seconds>0),
		  COALESCE(sum(wa.total_wait_seconds),0),
		  COALESCE(sum(wa.platform_seconds),0),
		  COALESCE(sum(wa.store_seconds),0),
		  COALESCE(sum(wa.rider_seconds),0),
		  COALESCE(sum(wa.exempted_seconds),0),
		  COALESCE(sum(wa.rider_subsidy_platform),0)::float8,
		  COALESCE(sum(wa.rider_subsidy_store),0)::float8
		FROM orders o
		JOIN stores s ON s.id=o.store_id
		LEFT JOIN wait_attributions wa
		  ON wa.order_id=o.id AND wa.status='active'
		WHERE o.ordered_at::date BETWEEN $1 AND $2
		  AND ($3='' OR o.platform=$3)
		GROUP BY o.platform,o.store_id,s.name
		ORDER BY o.platform,o.store_id`, from, to, platform)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type row struct {
		Platform        string  `json:"platform"`
		StoreID         int64   `json:"store_id"`
		StoreName       string  `json:"store_name"`
		Orders          int     `json:"orders"`
		PlatformFaultN  int     `json:"platform_fault_orders"`
		WaitTotal       int     `json:"rider_wait_total_seconds"`
		WaitPlatform    int     `json:"platform_early_dispatch_seconds"`
		WaitStore       int     `json:"store_make_timeout_seconds"`
		WaitRider       int     `json:"rider_early_arrival_seconds"`
		WaitExempted    int     `json:"exempted_seconds"`
		SubsidyPlatform float64 `json:"rider_subsidy_platform"`
		SubsidyStore    float64 `json:"rider_subsidy_store"`
	}
	stores := []row{}
	tot := row{StoreName: "合计"}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.Platform, &x.StoreID, &x.StoreName, &x.Orders, &x.PlatformFaultN,
			&x.WaitTotal, &x.WaitPlatform, &x.WaitStore, &x.WaitRider, &x.WaitExempted,
			&x.SubsidyPlatform, &x.SubsidyStore); err != nil {
			writeErr(w, err)
			return
		}
		stores = append(stores, x)
		tot.Orders += x.Orders
		tot.PlatformFaultN += x.PlatformFaultN
		tot.WaitTotal += x.WaitTotal
		tot.WaitPlatform += x.WaitPlatform
		tot.WaitStore += x.WaitStore
		tot.WaitRider += x.WaitRider
		tot.WaitExempted += x.WaitExempted
		tot.SubsidyPlatform += x.SubsidyPlatform
		tot.SubsidyStore += x.SubsidyStore
	}
	writeJSON(w, 200, map[string]any{
		"platform": platform, "from": from, "to": to,
		"stores": stores, "total": tot,
	})
}

// ---------- 可配置阈值 ----------

type thresholdReq struct {
	StoreID               *int64   `json:"store_id"` // 0/缺省=全局默认；正整数=门店覆盖
	PlatformDispatchLeadS *int     `json:"platform_dispatch_lead_seconds"`
	StoreMakeGraceS       *int     `json:"store_make_grace_seconds"`
	RiderEarlyGraceS      *int     `json:"rider_early_grace_seconds"`
	RiderSubsidyPerMinute *float64 `json:"rider_subsidy_per_minute"`
	StoreWaitScoreSeconds *int     `json:"store_wait_score_seconds"`
}

func (a *App) listWaitThresholds(w http.ResponseWriter, r *http.Request) {
	ac := actor(r)
	sql := `
		SELECT store_id,platform_dispatch_lead_seconds,store_make_grace_seconds,
		       rider_early_grace_seconds,rider_subsidy_per_minute::float8,store_wait_score_seconds
		FROM wait_thresholds`
	args := []any{}
	if ac.Role == "store_manager" {
		if ac.StoreID == nil {
			writeErr(w, errForbidden)
			return
		}
		sql += ` WHERE store_id=0 OR store_id=$1 ORDER BY store_id`
		args = append(args, *ac.StoreID)
	} else {
		sql += ` ORDER BY store_id`
	}
	rows, err := a.Pool.Query(r.Context(), sql, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []waitThresholds{}
	for rows.Next() {
		var t waitThresholds
		if err := rows.Scan(&t.StoreID, &t.PlatformDispatchLeadS, &t.StoreMakeGraceS,
			&t.RiderEarlyGraceS, &t.RiderSubsidyPerMinute, &t.StoreWaitScoreSeconds); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, 200, map[string]any{"thresholds": out})
}

func (a *App) updateWaitThresholds(w http.ResponseWriter, r *http.Request) {
	var req thresholdReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	storeID := int64(0)
	if req.StoreID != nil {
		storeID = *req.StoreID
	}
	ac := actor(r)
	// 店长只能改本门店覆盖；全局默认（0）仅平台管理员/区域经理可改
	if storeID == 0 && ac.Role == "store_manager" {
		writeErr(w, errForbidden)
		return
	}
	if storeID != 0 {
		var exists int
		if err := a.Pool.QueryRow(r.Context(),
			`SELECT count(*) FROM stores WHERE id=$1`, storeID).Scan(&exists); err != nil || exists == 0 {
			writeErr(w, errf(404, "store_not_found", "门店 %d 不存在", storeID))
			return
		}
		// 店长只能改本门店
		if ac.Role == "store_manager" && (ac.StoreID == nil || *ac.StoreID != storeID) {
			writeErr(w, errForbidden)
			return
		}
	}
	if req.PlatformDispatchLeadS != nil && *req.PlatformDispatchLeadS < 0 {
		writeErr(w, errf(400, "bad_request", "platform_dispatch_lead_seconds 不能为负"))
		return
	}
	if req.RiderSubsidyPerMinute != nil && *req.RiderSubsidyPerMinute < 0 {
		writeErr(w, errf(400, "bad_request", "rider_subsidy_per_minute 不能为负"))
		return
	}

	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO wait_thresholds(store_id,updated_by) VALUES($1,$2)
		ON CONFLICT (store_id) DO NOTHING`, storeID, ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	var leadS, graceS, earlyS, scoreS *int
	var rate *float64
	leadS, graceS, earlyS = req.PlatformDispatchLeadS, req.StoreMakeGraceS, req.RiderEarlyGraceS
	scoreS, rate = req.StoreWaitScoreSeconds, req.RiderSubsidyPerMinute
	if _, err := tx.Exec(ctx, `
		UPDATE wait_thresholds SET
		  platform_dispatch_lead_seconds=COALESCE($2,platform_dispatch_lead_seconds),
		  store_make_grace_seconds=COALESCE($3,store_make_grace_seconds),
		  rider_early_grace_seconds=COALESCE($4,rider_early_grace_seconds),
		  rider_subsidy_per_minute=COALESCE($5,rider_subsidy_per_minute),
		  store_wait_score_seconds=COALESCE($6,store_wait_score_seconds),
		  updated_by=$7,updated_at=now()
		WHERE store_id=$1`,
		storeID, leadS, graceS, earlyS, rate, scoreS, ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	var t waitThresholds
	if err := tx.QueryRow(ctx, `
		SELECT store_id,platform_dispatch_lead_seconds,store_make_grace_seconds,
		       rider_early_grace_seconds,rider_subsidy_per_minute::float8,store_wait_score_seconds
		FROM wait_thresholds WHERE store_id=$1`, storeID).Scan(
		&t.StoreID, &t.PlatformDispatchLeadS, &t.StoreMakeGraceS,
		&t.RiderEarlyGraceS, &t.RiderSubsidyPerMinute, &t.StoreWaitScoreSeconds); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"thresholds": t,
		"note": "阈值调整只影响调整后新计算的归因版本；历史版本参数快照永久保留，需要时可手动重算",
	})
}
