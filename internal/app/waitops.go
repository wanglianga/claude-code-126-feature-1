package app

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---------- 平台派单 / 改配 ----------

type dispatchReq struct {
	RiderName  string     `json:"rider_name"`
	RiderCode  string     `json:"rider_code"` // 可选：绑定骑手账号工号
	ETA        *time.Time `json:"eta"`        // 派单时承诺骑手的预计到店时间
	OccurredAt *time.Time `json:"occurred_at"` // 平台回传派单/改配实际发生时间，缺省 now()
	Note       string     `json:"note"`
}

// dispatchOrder 记录平台实际派单；同一订单第二次调用即“改配”（换骑手/改 ETA），
// 归因引擎只认最新一次派单/改配时间，历史派单保留可审计。
func (a *App) dispatchOrder(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	var req dispatchReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
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
	var status string
	if err := tx.QueryRow(ctx, `SELECT store_id,status FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&storeID, &status); err != nil {
		writeErr(w, err)
		return
	}
	if status == "handed" || status == "canceled" {
		writeErr(w, errf(409, "conflict", "订单已 %s，不能再派单/改配", status))
		return
	}

	var seq int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0) FROM dispatch_events WHERE order_id=$1`, orderID).
		Scan(&seq); err != nil {
		writeErr(w, err)
		return
	}
	seq++
	eventType := "dispatched"
	etype := "order_dispatched"
	if seq > 1 {
		eventType = "redispatch"
		etype = "order_redispatched"
	}
	riderName := req.RiderName
	if riderName == "" {
		riderName = ac.Name
	}
	var riderID *int64
	if req.RiderCode != "" {
		var rid int64
		if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE login_code=$1 AND active=TRUE`, req.RiderCode).
			Scan(&rid); err == nil {
			riderID = &rid
			var rn string
			if err := tx.QueryRow(ctx, `SELECT name FROM accounts WHERE id=$1`, rid).Scan(&rn); err == nil && req.RiderName == "" {
				riderName = rn
			}
		}
	}
	occurred := time.Now()
	if req.OccurredAt != nil {
		occurred = *req.OccurredAt
	}

	var dispatchID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO dispatch_events(order_id,platform,seq,event_type,rider_account_id,rider_name,eta,occurred_at,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		orderID, ac.Platform, seq, eventType, riderID, riderName, req.ETA, occurred, ac.ID).
		Scan(&dispatchID); err != nil {
		writeErr(w, err)
		return
	}
	// 改配后的最新 ETA 即订单时间线锚点
	if req.ETA != nil {
		if _, err := tx.Exec(ctx, `UPDATE orders SET rider_eta=$2 WHERE id=$1`, orderID, req.ETA); err != nil {
			writeErr(w, err)
			return
		}
	}
	// 改配换骑手：自动结束旧骑手在店区间（视为改配时刻离店），新骑手上店另起区间，
	// 避免把两段不同骑手的等待合并重复计入
	if eventType == "redispatch" {
		if _, err := tx.Exec(ctx, `
			UPDATE rider_arrivals SET left_at=COALESCE(left_at,$2)
			WHERE order_id=$1 AND left_at IS NULL AND handed_at IS NULL
			  AND ($3::bigint IS NULL OR rider_account_id<>$3)`,
			orderID, occurred, riderID); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := a.logEvent(ctx, tx, orderID, etype, actorOf(ac), map[string]any{
		"dispatch_id": dispatchID, "seq": seq, "rider": riderName,
		"eta": req.ETA, "occurred_at": occurred, "note": req.Note,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"dispatch_id": dispatchID, "order_id": orderID, "seq": seq,
		"event_type": eventType, "rider_name": riderName, "eta": req.ETA,
		"occurred_at": occurred,
		"note":        "改配后等待归因以本次派单时间为最新锚点重新计算",
	})
}

// ---------- 骑手离店（再到店） ----------

type leaveReq struct {
	OrderID  int64  `json:"order_id"`
	Reason   string `json:"reason"` // 转去取其他单 / 门店催等 / 其他
	Note     string `json:"note"`
}

// riderLeave 骑手离店：把该骑手在该单上未关闭的到店记录置 left_at；
// 之后再次到店登记生成第 2 段到店区间，归因时各段独立切分、互不重复。
func (a *App) riderLeave(w http.ResponseWriter, r *http.Request) {
	var req leaveReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.OrderID == 0 {
		writeErr(w, errf(400, "bad_request", "order_id 必填"))
		return
	}
	if req.Reason == "" {
		req.Reason = "other"
	}
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var orderStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, req.OrderID).Scan(&orderStatus); err != nil {
		writeErr(w, err)
		return
	}
	if orderStatus == "handed" || orderStatus == "canceled" {
		writeErr(w, errf(409, "conflict", "订单已 %s，无需离店登记", orderStatus))
		return
	}
	ct, err := tx.Exec(ctx, `
		UPDATE rider_arrivals SET left_at=now()
		WHERE order_id=$1 AND rider_account_id=$2 AND left_at IS NULL AND handed_at IS NULL`,
		req.OrderID, ac.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, errf(409, "no_open_arrival", "没有进行中的到店记录（可能已离店或已交接）"))
		return
	}
	if err := a.logEvent(ctx, tx, req.OrderID, "rider_left_store", actorOf(ac), map[string]any{
		"reason": req.Reason, "note": req.Note,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"order_id": req.OrderID, "left": true, "reason": req.Reason,
		"note": "再次到店请重新登记，等待按各到店区间独立归因",
	})
}
