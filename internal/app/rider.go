package app

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---------- 骑手到店 ----------

type arriveReq struct {
	OrderID         int64  `json:"order_id"`
	PlatformOrderNo string `json:"platform_order_no"`
	GroupID         *int64 `json:"group_id"`
	RiderName       string `json:"rider_name"`
}

type arriveItem struct {
	ArrivalID   int64  `json:"arrival_id"`
	OrderID     int64  `json:"order_id"`
	Ready       bool   `json:"ready"`
	OrderStatus string `json:"order_status"`
	SealedCups  int    `json:"sealed_cups"`
	TotalCups   int    `json:"total_cups"`
	Early       bool   `json:"early"`
	Bags        any    `json:"bags"`
	ExceptionID *int64 `json:"exception_id,omitempty"`
}

func (a *App) riderArrive(w http.ResponseWriter, r *http.Request) {
	var req arriveReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	ac := actor(r)
	if req.RiderName == "" {
		req.RiderName = ac.Name
	}
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	// 确定订单（支持合并组：组内全部订单一起到店登记）
	var orderIDs []int64
	var storeID int64
	switch {
	case req.OrderID != 0:
		var st int64
		var grp *int64
		if err := tx.QueryRow(ctx, `SELECT store_id,merged_group_id FROM orders WHERE id=$1`,
			req.OrderID).Scan(&st, &grp); err != nil {
			writeErr(w, err)
			return
		}
		storeID, orderIDs = st, []int64{req.OrderID}
		if req.GroupID != nil {
			_ = tx.QueryRow(ctx, `SELECT store_id FROM merged_groups WHERE id=$1`, *req.GroupID).Scan(&storeID)
			rows, err := tx.Query(ctx, `SELECT id FROM orders WHERE merged_group_id=$1 ORDER BY id`, *req.GroupID)
			if err != nil {
				writeErr(w, err)
				return
			}
			defer rows.Close()
			orderIDs = orderIDs[:0]
			for rows.Next() {
				var id int64
				_ = rows.Scan(&id)
				orderIDs = append(orderIDs, id)
			}
		}
	case req.PlatformOrderNo != "":
		if err := tx.QueryRow(ctx, `SELECT id,store_id FROM orders WHERE platform=$1 AND platform_order_no=$2`,
			ac.Platform, req.PlatformOrderNo).Scan(&req.OrderID, &storeID); err != nil {
			writeErr(w, errf(404, "order_not_found", "平台单号未找到"))
			return
		}
		orderIDs = []int64{req.OrderID}
	default:
		writeErr(w, errf(400, "bad_request", "order_id 或 platform_order_no 必填"))
		return
	}

	var results []arriveItem
	for _, oid := range orderIDs {
		var status string
		var sealedN, totalN int
		if err := tx.QueryRow(ctx,
			`SELECT status FROM orders WHERE id=$1 FOR UPDATE`, oid).Scan(&status); err != nil {
			writeErr(w, err)
			return
		}
		if status == "canceled" {
			results = append(results, arriveItem{OrderID: oid, Ready: false, OrderStatus: status})
			continue
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE status='sealed'),count(*) FILTER (WHERE status<>'voided')
			 FROM order_cups WHERE order_id=$1`, oid).Scan(&sealedN, &totalN); err != nil {
			writeErr(w, err)
			return
		}
		ready := status == "active" || (totalN > 0 && sealedN == totalN)

		// 已存在“同一骑手进行中”（未离店、未交接）的到店记录则复用；
		// 换骑手或离店后再次到店生成新区间
		var arrivalID int64
		err := tx.QueryRow(ctx,
			`SELECT id FROM rider_arrivals
			 WHERE order_id=$1 AND rider_account_id=$2 AND handed_at IS NULL AND left_at IS NULL
			 ORDER BY id DESC LIMIT 1`,
			oid, ac.ID).Scan(&arrivalID)
		if err != nil {
			// 到店快照：全店排队+制作中杯量、本单未出杯杯量、门店爆单保护状态
			var arrivalNo int
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(max(arrival_no),0)+1 FROM rider_arrivals WHERE order_id=$1`, oid).
				Scan(&arrivalNo); err != nil {
				writeErr(w, err)
				return
			}
			var queueCups int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM order_cups c JOIN orders o ON o.id=c.order_id
				WHERE o.store_id=$1 AND c.status IN ('queued','making')
				  AND o.status IN ('received','active')`, storeID).Scan(&queueCups); err != nil {
				writeErr(w, err)
				return
			}
			var surgeManual bool
			var surgeUntil *time.Time
			var maxActive int
			if err := tx.QueryRow(ctx,
				`SELECT surge_manual,surge_until,max_active_cups FROM stores WHERE id=$1`,
				storeID).Scan(&surgeManual, &surgeUntil, &maxActive); err != nil {
				writeErr(w, err)
				return
			}
			surgeActive := surgeManual && (surgeUntil == nil || surgeUntil.After(time.Now()))
			surgeSnapshot := "none"
			switch {
			case surgeActive:
				surgeSnapshot = "surge_paused"
			case queueCups >= maxActive:
				surgeSnapshot = "surge_full"
			}
			var dispatchID *int64
			_ = tx.QueryRow(ctx,
				`SELECT id FROM dispatch_events WHERE order_id=$1 ORDER BY seq DESC LIMIT 1`, oid).
				Scan(&dispatchID)
			queueAhead := totalN - sealedN

			var aid int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO rider_arrivals(store_id,order_id,group_id,rider_account_id,rider_name,platform,
				  ready_at,arrival_no,queue_cups,queue_ahead_cups,surge_active,surge_snapshot,dispatch_id)
				VALUES($1,$2,$3,$4,$5,$6,CASE WHEN $7 THEN now() ELSE NULL END,$8,$9,$10,$11,$12,$13)
				RETURNING id`,
				storeID, oid, req.GroupID, ac.ID, req.RiderName, ac.Platform, ready,
				arrivalNo, queueCups, queueAhead, surgeActive, surgeSnapshot, dispatchID).Scan(&aid); err != nil {
				writeErr(w, err)
				return
			}
			arrivalID = aid
			etype := "rider_arrived"
			detail := map[string]any{"rider": req.RiderName, "ready": ready}
			if !ready {
				etype = "rider_arrived_early"
				var excID *int64
				eid, existed, err := openException(ctx, tx, oid, "rider_early")
				if err != nil {
					writeErr(w, err)
					return
				}
				if !existed {
					var newID int64
					if err := tx.QueryRow(ctx, `
						INSERT INTO exceptions(order_id,store_id,type,status,responsibility,detail,created_by)
						VALUES($1,$2,'rider_early','open','pending',$3,$4) RETURNING id`,
						oid, storeID, map[string]any{"rider": req.RiderName, "sealed": sealedN, "total": totalN},
						ac.ID).Scan(&newID); err != nil {
						writeErr(w, err)
						return
					}
					eid = newID
				}
				excID = &eid
				detail["exception_id"] = eid
				results = append(results, arriveItem{
					ArrivalID: arrivalID, OrderID: oid, Ready: false, OrderStatus: status,
					SealedCups: sealedN, TotalCups: totalN, Early: true,
					Bags: bagSummary(ctx, tx, oid), ExceptionID: excID,
				})
			}
			if err := a.logEvent(ctx, tx, oid, etype, actorOf(ac), detail); err != nil {
				writeErr(w, err)
				return
			}
			if ready {
				results = append(results, arriveItem{
					ArrivalID: arrivalID, OrderID: oid, Ready: true, OrderStatus: status,
					SealedCups: sealedN, TotalCups: totalN, Bags: bagSummary(ctx, tx, oid),
				})
			}
			continue
		}
		results = append(results, arriveItem{
			ArrivalID: arrivalID, OrderID: oid, Ready: ready, OrderStatus: status,
			SealedCups: sealedN, TotalCups: totalN, Early: !ready, Bags: bagSummary(ctx, tx, oid),
		})
	}

	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"rider": req.RiderName, "orders": results})
}

func (a *App) getArrival(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	var v struct {
		ID             int64      `json:"id"`
		StoreID        int64      `json:"store_id"`
		OrderID        int64      `json:"order_id"`
		ArrivalNo      int        `json:"arrival_no"`
		RiderName      string     `json:"rider_name"`
		Platform       string     `json:"platform"`
		ArrivedAt      time.Time  `json:"arrived_at"`
		LeftAt         *time.Time `json:"left_at,omitempty"`
		ReadyAt        *time.Time `json:"ready_at,omitempty"`
		WaitSec        *int       `json:"wait_seconds,omitempty"`
		StoreWait      int        `json:"store_fault_wait_seconds"`
		QueueCups      *int       `json:"queue_cups,omitempty"`
		QueueAheadCups *int       `json:"queue_ahead_cups,omitempty"`
		SurgeActive    bool       `json:"surge_active"`
		SurgeSnapshot  string     `json:"surge_snapshot"`
		HandedAt       *time.Time `json:"handed_at,omitempty"`
	}
	err := a.Pool.QueryRow(r.Context(), `
		SELECT id,store_id,order_id,arrival_no,rider_name,COALESCE(platform,''),arrived_at,left_at,ready_at,
		       wait_seconds,store_fault_wait_seconds,queue_cups,queue_ahead_cups,
		       surge_active,COALESCE(surge_snapshot,'none'),handed_at
		FROM rider_arrivals WHERE id=$1`, id).Scan(
		&v.ID, &v.StoreID, &v.OrderID, &v.ArrivalNo, &v.RiderName, &v.Platform, &v.ArrivedAt,
		&v.LeftAt, &v.ReadyAt, &v.WaitSec, &v.StoreWait, &v.QueueCups, &v.QueueAheadCups,
		&v.SurgeActive, &v.SurgeSnapshot, &v.HandedAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

// riderScreen 骑手到店屏：在店等待中的骑手与订单状态（可按门店筛选）。
func (a *App) riderScreen(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sql := `
		SELECT ra.id,ra.store_id,s.name,ra.order_id,o.platform,o.platform_order_no,ra.rider_name,
		       COALESCE(ra.platform,''),ra.arrived_at,
		       o.status,
		       (SELECT count(*) FROM order_cups c WHERE c.order_id=o.id AND c.status='sealed'),
		       (SELECT count(*) FROM order_cups c WHERE c.order_id=o.id AND c.status<>'voided'),
		       EXTRACT(EPOCH FROM (now()-ra.arrived_at))::int
		FROM rider_arrivals ra
		JOIN stores s ON s.id=ra.store_id
		JOIN orders o ON o.id=ra.order_id
		WHERE ra.handed_at IS NULL AND ra.left_at IS NULL AND o.status<>'canceled'`
	args := []any{}
	if sid := q.Get("store_id"); sid != "" {
		sql += " AND ra.store_id=$1"
		args = append(args, toInt64(sid))
	}
	if actor(r).Role == "rider" {
		sql += " AND ra.rider_account_id=$" + itoa(int64(len(args)+1))
		args = append(args, actor(r).ID)
	}
	sql += " ORDER BY ra.arrived_at LIMIT 100"
	rows, err := a.Pool.Query(r.Context(), sql, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type row struct {
		ArrivalID   int64     `json:"arrival_id"`
		StoreID     int64     `json:"store_id"`
		StoreName   string    `json:"store_name"`
		OrderID     int64     `json:"order_id"`
		Platform    string    `json:"platform"`
		PlatformNo  string    `json:"platform_order_no"`
		RiderName   string    `json:"rider_name"`
		ArrivedAt   time.Time `json:"arrived_at"`
		OrderStatus string    `json:"order_status"`
		SealedCups  int       `json:"sealed_cups"`
		TotalCups   int       `json:"total_cups"`
		WaitingSec  int       `json:"waiting_seconds"`
	}
	var out []row
	out = []row{}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.ArrivalID, &x.StoreID, &x.StoreName, &x.OrderID, &x.Platform,
			&x.PlatformNo, &x.RiderName, &x.Platform, &x.ArrivedAt, &x.OrderStatus,
			&x.SealedCups, &x.TotalCups, &x.WaitingSec); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}

// ---------- 核验交接 ----------

type handoffReq struct {
	OrderID         int64  `json:"order_id"`
	PlatformOrderNo string `json:"platform_order_no"`
	Platform        string `json:"platform"` // 店员代核验时指定平台
	RiderName       string `json:"rider_name"`
	ActualBags      *int   `json:"actual_bags"`
	ActualHot       *int   `json:"actual_hot"`
	ActualCold      *int   `json:"actual_cold"`
	ActualFood      *int   `json:"actual_food"`
	HasStraw        *bool  `json:"has_straw"`
	HasUtensil      *bool  `json:"has_utensil"`
}

func (a *App) handoff(w http.ResponseWriter, r *http.Request) {
	var req handoffReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	ac := actor(r)
	ctx := r.Context()

	// 支持店员代骑手做核验；骑手也可只提供平台单号
	riderName := req.RiderName
	if riderName == "" {
		riderName = ac.Name
	}
	if req.OrderID == 0 && req.PlatformOrderNo != "" {
		platform := ac.Platform
		if req.Platform != "" {
			platform = req.Platform
		}
		if err := a.Pool.QueryRow(ctx,
			`SELECT id FROM orders WHERE platform=$1 AND platform_order_no=$2`,
			platform, req.PlatformOrderNo).Scan(&req.OrderID); err != nil {
			writeErr(w, errf(404, "order_not_found", "平台单号未找到"))
			return
		}
	}
	if req.OrderID == 0 {
		writeErr(w, errf(400, "bad_request", "order_id 或 platform_order_no 必填"))
		return
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	o, err := scanOrder(tx.QueryRow(ctx, "SELECT "+orderCols+" FROM orders WHERE id=$1 FOR UPDATE", req.OrderID))
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.PlatformOrderNo != "" && req.PlatformOrderNo != o.PlatformOrderNo {
		writeErr(w, errf(409, "order_no_mismatch", "核验失败：平台单号与订单不一致"))
		return
	}
	if o.Status == "canceled" {
		writeErr(w, errf(409, "order_canceled", "订单已被平台取消，禁止交接"))
		return
	}
	if o.Status == "handed" {
		writeErr(w, errf(409, "already_handed", "订单已交接"))
		return
	}

	// 未出杯完成 → 不允许取走
	var sealedN, totalN int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status='sealed'),count(*) FILTER (WHERE status<>'voided')
		 FROM order_cups WHERE order_id=$1`, req.OrderID).Scan(&sealedN, &totalN); err != nil {
		writeErr(w, err)
		return
	}
	if sealedN < totalN {
		writeErr(w, errf(409, "drinks_not_ready",
			"饮品尚未全部出杯（%d/%d），骑手请等待，门店赶制中", sealedN, totalN))
		return
	}

	// 期望袋数 / 冷热分袋
	expHot, expCold, expFood := 0, 0, 0
	type b struct{ code, kind string }
	bagSet := map[string]string{}
	brows, err := tx.Query(ctx, `SELECT DISTINCT bag_code,kind FROM order_bags WHERE order_id=$1`, req.OrderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	for brows.Next() {
		var code, kind string
		_ = brows.Scan(&code, &kind)
		bagSet[code] = kind
		switch kind {
		case "hot":
			expHot++
		case "cold":
			expCold++
		case "food":
			expFood++
		}
	}
	brows.Close()
	expBags := len(bagSet)

	// 是否需要吸管（含冰饮）/餐具（含轻食、套餐）
	needStraw, needUtensil := false, false
	mrows, err := tx.Query(ctx,
		`SELECT DISTINCT m.is_drink,m.includes_drink,c.temp
		 FROM order_cups c JOIN menu_items m ON m.id=c.menu_item_id
		 WHERE c.order_id=$1 AND c.status<>'voided'`, req.OrderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	for mrows.Next() {
		var isDrink, includesDrink bool
		var temp string
		_ = mrows.Scan(&isDrink, &includesDrink, &temp)
		if (isDrink || includesDrink) && temp == "ice" {
			needStraw = true
		}
		if !isDrink {
			needUtensil = true
		}
	}
	mrows.Close()

	issues := []string{}
	actualBags := expBags
	if req.ActualBags != nil {
		actualBags = *req.ActualBags
		if actualBags != expBags {
			issues = append(issues, "wrong_bag_count")
		}
	}
	actualHot, actualCold, actualFood := expHot, expCold, expFood
	if req.ActualHot != nil {
		actualHot = *req.ActualHot
		if actualHot != expHot {
			issues = append(issues, "hot_bag_mismatch")
		}
	}
	if req.ActualCold != nil {
		actualCold = *req.ActualCold
		if actualCold != expCold {
			issues = append(issues, "cold_bag_mismatch")
		}
	}
	if req.ActualFood != nil {
		actualFood = *req.ActualFood
		if actualFood != expFood {
			issues = append(issues, "food_bag_mismatch")
		}
	}
	hasStraw, hasUtensil := true, true
	if req.HasStraw != nil {
		hasStraw = *req.HasStraw
	}
	if req.HasUtensil != nil {
		hasUtensil = *req.HasUtensil
	}
	if needStraw && !hasStraw {
		issues = append(issues, "missing_straw")
	}
	if needUtensil && !hasUtensil {
		issues = append(issues, "missing_utensil")
	}

	var staffID *int64
	if ac.Role == "store_manager" || ac.Role == "barista" {
		staffID = &ac.ID
	}

	if len(issues) > 0 {
		// 核验不通过：登记 handoff，异常按类型归责（取错袋=骑手；漏放吸管餐具=门店）
		if _, err := tx.Exec(ctx, `
			INSERT INTO handoffs(store_id,order_id,rider_name,staff_id,passed,
				expect_bags,expect_hot,expect_cold,expect_food,
				actual_bags,actual_hot,actual_cold,has_straw,has_utensil,issues)
			VALUES($1,$2,$3,$4,FALSE,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			o.StoreID, req.OrderID, ac.Name, staffID,
			expBags, expHot, expCold, expFood,
			actualBags, actualHot, actualCold, hasStraw, hasUtensil, issues); err != nil {
			writeErr(w, err)
			return
		}
		for _, iss := range issues {
			typ, resp := iss, "store"
			switch iss {
			case "wrong_bag_count", "hot_bag_mismatch", "cold_bag_mismatch", "food_bag_mismatch":
				typ, resp = "wrong_bag", "rider"
			case "missing_straw":
				typ = "missing_straw"
			case "missing_utensil":
				typ = "missing_utensil"
			}
			var excID int64
			err := tx.QueryRow(ctx, `
				INSERT INTO exceptions(order_id,store_id,type,status,responsibility,detail,created_by)
				VALUES($1,$2,$3,'open',$4,$5,$6) RETURNING id`,
				req.OrderID, o.StoreID, typ, resp,
				map[string]any{"issues": issues, "rider": riderName}, ac.ID).Scan(&excID)
			if err != nil {
				writeErr(w, err)
				return
			}
		}
		if err := a.logEvent(ctx, tx, req.OrderID, "handoff_check_failed", actorOf(ac), map[string]any{
			"issues": issues, "expect_bags": expBags, "actual_bags": actualBags,
			"expect_hot": expHot, "expect_cold": expCold, "expect_food": expFood,
		}); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 409, map[string]any{
			"passed": false, "issues": issues,
			"expect": map[string]any{"bags": expBags, "hot": expHot, "cold": expCold, "food": expFood,
				"straw": needStraw, "utensil": needUtensil},
			"actual": map[string]any{"bags": actualBags, "hot": actualHot, "cold": actualCold, "food": actualFood,
				"straw": hasStraw, "utensil": hasUtensil},
		})
		return
	}

	// 核验通过 → 交接
	if _, err := tx.Exec(ctx, `
		INSERT INTO handoffs(store_id,order_id,rider_name,staff_id,passed,
			expect_bags,expect_hot,expect_cold,expect_food,
			actual_bags,actual_hot,actual_cold,has_straw,has_utensil,issues)
		VALUES($1,$2,$3,$4,TRUE,$5,$6,$7,$8,$9,$10,$11,$12,$13,'[]')`,
		o.StoreID, req.OrderID, riderName, staffID,
		expBags, expHot, expCold, expFood,
		actualBags, actualHot, actualCold, hasStraw, hasUtensil); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE orders SET status='handed',handed_at=now() WHERE id=$1`, req.OrderID); err != nil {
		writeErr(w, err)
		return
	}
	// 交接完成确认营业收入（以订单实际金额为准；此前的单品退款/取消已调整 total_amount）
	if o.TotalAmount > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO finance_records(store_id,order_id,kind,amount,bearer,note,created_by)
			VALUES($1,$2,'revenue',$3,'store','骑手核验交接确认收入',$4)`,
			o.StoreID, req.OrderID, o.TotalAmount, ac.ID); err != nil {
			writeErr(w, err)
			return
		}
	}

	// 交接时刻对等待做三段归因定稿：平台派单过早 / 门店制作超时 / 骑手提前到店；
	// 同事务完成到店记录回算、rider_early 归责与骑手等待补贴结算（整版换版可红冲，天然去重）。
	attrID, version, _, attrRes, _, err := a.persistWaitAttribution(
		ctx, tx, req.OrderID, "initial", nil, ac.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	waitSec, storeWaitSec := attrRes.TotalSeconds, attrRes.StoreSec

	if err := a.logEvent(ctx, tx, req.OrderID, "order_handed", actorOf(ac), map[string]any{
		"rider": riderName, "bags": expBags, "hot": expHot, "cold": expCold, "food": expFood,
		"straw": hasStraw, "utensil": hasUtensil, "wait_seconds": waitSec,
		"store_fault_wait_seconds": storeWaitSec,
		"wait_attribution_id":       attrID,
		"wait_attribution_version":  version,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"passed": true, "order_id": req.OrderID, "status": "handed",
		"bags": expBags, "hot": expHot, "cold": expCold, "food": expFood,
		"rider_wait_seconds": waitSec, "store_fault_wait_seconds": storeWaitSec,
		"wait": map[string]any{
			"attribution_id":                   attrID,
			"version_no":                       version,
			"total_seconds":                    attrRes.TotalSeconds,
			"platform_early_dispatch_seconds":  attrRes.PlatformSec,
			"store_make_timeout_seconds":       attrRes.StoreSec,
			"rider_early_arrival_seconds":      attrRes.RiderSec,
			"grace_seconds":                    attrRes.GraceSec,
			"rider_subsidy_total":              attrRes.SubsidyTotal,
			"rider_subsidy_store":              attrRes.SubsidyStore,
			"rider_subsidy_platform":           attrRes.SubsidyPlatform,
			"segments":                         attrRes.Segments,
		},
	})
}
