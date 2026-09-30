package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// 骑手提前到店等待责任拆分与争议复核的集成测试。
// 通过把派单/到店/出杯时间回拨到固定时刻，精确验证三段拆分秒数。

var loc = func() *time.Location { l, _ := time.LoadLocation("Asia/Shanghai"); return l }()

func at(h, m int) time.Time { return time.Date(2026, 9, 15, h, m, 0, 0, loc) }

func mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := suite.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("SQL 失败 %s: %v", sql, err)
	}
}

// setStore1Thresholds 把门店1前置阈值调为 15 分钟（900 秒），其余宽限 60 秒、补贴 0.5 元/分钟。
func setStore1Thresholds(t *testing.T) {
	sc, b := do(t, "PUT", "/v1/management/wait-thresholds", "1001", map[string]any{
		"store_id":                         1,
		"platform_dispatch_lead_seconds":   900,
		"store_make_grace_seconds":         60,
		"rider_early_grace_seconds":        60,
		"rider_subsidy_per_minute":         0.5,
		"store_wait_score_seconds":         300,
	})
	mustStatus(t, sc, 200, b)
}

func backdateOrderTimes(t *testing.T, oid int64, ordered, ready time.Time) {
	mustExec(t, `UPDATE orders SET ordered_at=$2, all_sealed_at=$3 WHERE id=$1`, oid, ordered, ready)
	mustExec(t, `UPDATE order_cups SET queued_at=$2, started_at=$3, sealed_at=$4 WHERE order_id=$1`,
		oid, ordered, ordered.Add(2*time.Minute), ready)
}

func sealOrder(t *testing.T, oid int64) int64 {
	sc, b := do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	cid := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/start", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/seal", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	return cid
}

func attrNumbers(t *testing.T, oid int64) map[string]any {
	t.Helper()
	sc, b := do(t, "GET", fmt.Sprintf("/v1/orders/%d/wait-attribution", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	return b["attribution"].(map[string]any)
}

func doList(t *testing.T, path, code string) (int, []any) {
	t.Helper()
	req, _ := http.NewRequest("GET", suite.srv.URL+path, nil)
	req.Header.Set("X-Staff-Code", code)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败 %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out []any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestWaitAttributionThreeWaySplit(t *testing.T) {
	setStore1Thresholds(t)
	// 单杯热美式；ETA 12:10；门店1 派单前置 15 分钟
	sc, b := do(t, "POST", "/v1/platform/orders", "9001",
		intake("T-WAIT-A", []intakeItem{{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"}},
			map[string]any{"rider_eta": at(12, 10)}))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))

	// 平台 12:00 派单（应预留 15 分钟 → 理论最早出杯 12:15，晚于 ETA 12:10）
	sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/dispatch", oid), "9001", map[string]any{
		"rider_code": "4001", "eta": at(12, 10), "occurred_at": at(12, 0),
	})
	mustStatus(t, sc, 201, b)
	if b["event_type"] != "dispatched" {
		t.Fatalf("首次派单事件类型错误: %v", b)
	}
	mustExec(t, `UPDATE orders SET ordered_at=$2 WHERE id=$1`, oid, at(12, 0))

	// 骑手 12:05 到店（早于 ETA 5 分钟）
	sc, b = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, b)
	mustExec(t, `UPDATE rider_arrivals SET arrived_at=$2 WHERE order_id=$1 AND arrival_no=1`, oid, at(12, 5))

	// 未交接前为试算版本
	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d/wait-attribution", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	if b["provisional"] != true {
		t.Fatalf("交接前应为 provisional 试算: %v", b)
	}

	// 12:18 才出杯
	sealOrder(t, oid)
	backdateOrderTimes(t, oid, at(12, 0), at(12, 18))

	// 交接定稿
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_hot": 1})
	mustStatus(t, sc, 200, b)
	w := b["wait"].(map[string]any)
	if w["rider_early_arrival_seconds"].(float64) != 240 { // 12:05 → 12:09（扣 60 秒宽限）
		t.Fatalf("骑手提前段应为 240s: %v", w)
	}
	if w["platform_early_dispatch_seconds"].(float64) != 240 { // 12:11 → 12:15
		t.Fatalf("平台派单过早段应为 240s: %v", w)
	}
	if w["store_make_timeout_seconds"].(float64) != 180 { // 12:15 → 12:18
		t.Fatalf("门店制作超时段应为 180s: %v", w)
	}
	if w["rider_subsidy_store"].(float64) != 1.50 { // 180s × 0.5/分钟
		t.Fatalf("门店承担补贴应为 1.50: %v", w)
	}
	if w["rider_subsidy_platform"].(float64) != 2.00 { // 240s × 0.5/分钟
		t.Fatalf("平台承担补贴应为 2.00: %v", w)
	}
	// 段互不重叠
	segs := w["segments"].([]any)
	seenRider, seenPlatform, seenStore := false, false, false
	for _, s := range segs {
		switch s.(map[string]any)["segment_type"] {
		case "rider_early_arrival":
			seenRider = true
		case "platform_early_dispatch":
			seenPlatform = true
		case "store_make_timeout":
			seenStore = true
		}
	}
	if !seenRider || !seenPlatform || !seenStore {
		t.Fatalf("应同时存在三段: %v", segs)
	}
}

func TestWaitLeaveAndReturnNoDoubleCount(t *testing.T) {
	setStore1Thresholds(t)
	sc, b := do(t, "POST", "/v1/platform/orders", "9001",
		intake("T-WAIT-B", []intakeItem{{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"}},
			map[string]any{"rider_eta": at(12, 10)}))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/dispatch", oid), "9001", map[string]any{
		"rider_code": "4001", "eta": at(12, 10), "occurred_at": at(12, 0),
	})
	mustStatus(t, sc, 201, nil)

	// 第一次到店 12:05，12:07 离店
	sc, _ = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, nil)
	mustExec(t, `UPDATE rider_arrivals SET arrived_at=$2 WHERE order_id=$1 AND arrival_no=1`, oid, at(12, 5))
	sc, b = do(t, "POST", "/v1/rider/leaves", "4001",
		map[string]any{"order_id": oid, "reason": "other_order"})
	mustStatus(t, sc, 200, b)
	mustExec(t, `UPDATE rider_arrivals SET left_at=$2 WHERE order_id=$1 AND arrival_no=1`, oid, at(12, 7))

	// 离店后不在到店屏
	sc, screen := doList(t, "/v1/rider/screen?store_id=1", "4001")
	if sc != 200 {
		t.Fatalf("到店屏查询失败 %d", sc)
	}
	for _, x := range screen {
		if int64(x.(map[string]any)["order_id"].(float64)) == oid {
			t.Fatalf("离店骑手不应出现在到店屏: %v", screen)
		}
	}

	// 第二次到店 12:12
	sc, _ = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, nil)
	mustExec(t, `UPDATE rider_arrivals SET arrived_at=$2 WHERE order_id=$1 AND arrival_no=2`, oid, at(12, 12))

	sealOrder(t, oid)
	backdateOrderTimes(t, oid, at(12, 0), at(12, 18))
	sc, _ = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_hot": 1})
	mustStatus(t, sc, 200, nil)

	a := attrNumbers(t, oid)
	// 第一段 [12:05,12:07) 全部骑手提前 = 120s；离店空档 [12:07,12:12) 不计
	if a["rider_early_arrival_seconds"].(float64) != 120 {
		t.Fatalf("骑手段应为 120s: %v", a)
	}
	// 第二段 [12:12,12:15) 平台 180s；[12:15,12:18) 门店 180s
	if a["platform_early_dispatch_seconds"].(float64) != 180 {
		t.Fatalf("平台段应为 180s: %v", a)
	}
	if a["store_make_timeout_seconds"].(float64) != 180 {
		t.Fatalf("门店段应为 180s: %v", a)
	}
	// 到店记录门店责任只记在第二段
	var fault1, fault2 int
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT store_fault_wait_seconds FROM rider_arrivals WHERE order_id=$1 AND arrival_no=1`, oid).Scan(&fault1)
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT store_fault_wait_seconds FROM rider_arrivals WHERE order_id=$1 AND arrival_no=2`, oid).Scan(&fault2)
	if fault1 != 0 || fault2 != 180 {
		t.Fatalf("门店责任等待只应落在第二段: %d/%d", fault1, fault2)
	}
}

func TestWaitRedispatchReattributes(t *testing.T) {
	setStore1Thresholds(t)
	sc, b := do(t, "POST", "/v1/platform/orders", "9001",
		intake("T-WAIT-E", []intakeItem{{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"}},
			map[string]any{"rider_eta": at(12, 20)}))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	// 首次派单 11:50，ETA 12:20（给足 15 分钟以上前置，平台无责）
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/dispatch", oid), "9001", map[string]any{
		"rider_code": "4001", "eta": at(12, 20), "occurred_at": at(11, 50),
	})
	mustStatus(t, sc, 201, nil)
	mustExec(t, `UPDATE orders SET ordered_at=$2 WHERE id=$1`, oid, at(11, 50))
	sc, _ = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, nil)
	mustExec(t, `UPDATE rider_arrivals SET arrived_at=$2 WHERE order_id=$1 AND arrival_no=1`, oid, at(12, 10))

	// 改配：12:14 平台改派，ETA 提前到 12:16 → 门店客观来不及，重新归因
	sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/dispatch", oid), "9001", map[string]any{
		"rider_code": "4001", "eta": at(12, 16), "occurred_at": at(12, 14), "note": "顾客改地址改配",
	})
	mustStatus(t, sc, 201, b)
	if b["event_type"] != "redispatch" || b["seq"].(float64) != 2 {
		t.Fatalf("第二次派单应为改配 redispatch seq=2: %v", b)
	}

	sealOrder(t, oid)
	mustExec(t, `UPDATE orders SET ordered_at=$2, all_sealed_at=$3 WHERE id=$1`, oid, at(11, 50), at(12, 18))
	mustExec(t, `UPDATE order_cups SET queued_at=$2, started_at=$3, sealed_at=$4 WHERE order_id=$1`,
		oid, at(11, 50), at(12, 14), at(12, 18))

	sc, _ = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_hot": 1})
	mustStatus(t, sc, 200, nil)
	a := attrNumbers(t, oid)
	// 改配后：骑手提前 [12:10,12:15)=300s；宽限 [12:15,12:17)=120s；平台 [12:17,12:18)=60s；门店 0
	if a["rider_early_arrival_seconds"].(float64) != 300 {
		t.Fatalf("改配后骑手段应为 300s: %v", a)
	}
	if a["platform_early_dispatch_seconds"].(float64) != 60 {
		t.Fatalf("改配后平台段应为 60s: %v", a)
	}
	if a["store_make_timeout_seconds"].(float64) != 0 {
		t.Fatalf("改配后门店段应为 0s: %v", a)
	}
}

func TestWaitAppealReviewAndRollback(t *testing.T) {
	setStore1Thresholds(t)
	sc, b := do(t, "POST", "/v1/platform/orders", "9001",
		intake("T-WAIT-F", []intakeItem{{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"}},
			map[string]any{"rider_eta": at(12, 10)}))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/dispatch", oid), "9001", map[string]any{
		"rider_code": "4001", "eta": at(12, 10), "occurred_at": at(12, 0),
	})
	mustStatus(t, sc, 201, nil)
	sc, _ = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, nil)
	mustExec(t, `UPDATE rider_arrivals SET arrived_at=$2 WHERE order_id=$1 AND arrival_no=1`, oid, at(12, 5))
	sealOrder(t, oid)
	backdateOrderTimes(t, oid, at(12, 0), at(12, 18))
	sc, _ = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_hot": 1})
	mustStatus(t, sc, 200, nil)
	before := attrNumbers(t, oid)
	if before["store_make_timeout_seconds"].(float64) != 180 {
		t.Fatalf("前置条件：门店段 180s")
	}

	// 权限：骑手不能申诉
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/orders/%d/wait-appeals", oid), "4001",
		map[string]any{"reason_code": "machine_fault", "reason_detail": "咖啡机故障"})
	if sc != 403 {
		t.Fatalf("骑手申诉应 403, got %d", sc)
	}
	// 店长申诉机器故障
	sc, b = do(t, "POST", fmt.Sprintf("/v1/orders/%d/wait-appeals", oid), "2001", map[string]any{
		"reason_code": "machine_fault", "reason_detail": "12:11 咖啡机故障停机维修",
		"evidence":    map[string]any{"repair_ticket": "R-20260915-1", "photo": "ev1.jpg"},
	})
	mustStatus(t, sc, 201, b)
	rid := int64(b["review_id"].(float64))
	// 重复申诉被拒
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/orders/%d/wait-appeals", oid), "2001",
		map[string]any{"reason_code": "other", "reason_detail": "再申诉"})
	if sc != 409 {
		t.Fatalf("待复核中重复申诉应 409, got %d", sc)
	}
	// 店员不能复核
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/wait-reviews/%d/decision", rid), "3001",
		map[string]any{"decision": "approve"})
	if sc != 403 {
		t.Fatalf("店员复核应 403, got %d", sc)
	}
	// 区域经理复核通过
	sc, b = do(t, "POST", fmt.Sprintf("/v1/wait-reviews/%d/decision", rid), "1001",
		map[string]any{"decision": "approve", "review_note": "维修工单与监控核实属实，门店责任豁免转平台"})
	mustStatus(t, sc, 200, b)
	if b["handoff_fact_unchanged"] != true {
		t.Fatalf("改判不得改写交接事实: %v", b)
	}
	after := b["after"].(map[string]any)
	if after["store_seconds"].(float64) != 0 {
		t.Fatalf("改判后门店段应为 0: %v", after)
	}
	if after["exempted_seconds"].(float64) != 180 {
		t.Fatalf("改判后豁免段应为 180s: %v", after)
	}
	if after["rider_subsidy_platform"].(float64) != 3.50 { // (240+180)s × 0.5
		t.Fatalf("平台承担补贴应为 3.50: %v", after)
	}

	// 订单状态/交接事实保持不变
	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	if b["status"] != "handed" || b["handed_at"] == nil {
		t.Fatalf("交接事实不得被改写: %v", b["status"])
	}
	var handoffs int
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM handoffs WHERE order_id=$1 AND passed=TRUE`, oid).Scan(&handoffs)
	if handoffs != 1 {
		t.Fatalf("交接记录必须保留: %d", handoffs)
	}

	// 财务：门店承担补贴红冲后净额为 0；平台承担为 3.50
	var storeNet, platNet float64
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(amount),0)::float8 FROM finance_records
		 WHERE order_id=$1 AND kind='rider_wait_subsidy' AND bearer='store'`, oid).Scan(&storeNet)
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(amount),0)::float8 FROM finance_records
		 WHERE order_id=$1 AND kind='rider_wait_subsidy' AND bearer='platform'`, oid).Scan(&platNet)
	if storeNet != 0 {
		t.Fatalf("门店补贴红冲后净额应为 0, got %v", storeNet)
	}
	if platNet != -3.50 {
		t.Fatalf("平台补贴净额应为 -3.50, got %v", platNet)
	}

	// 原始归因保留：v1 已 superseded，v2 active，各版本段不可变
	var v1status string
	var v1Store int
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT status,store_seconds FROM wait_attributions WHERE order_id=$1 AND version_no=1`, oid).
		Scan(&v1status, &v1Store)
	if v1status != "superseded" || v1Store != 180 {
		t.Fatalf("原始归因 v1 必须保留门店 180s, got %s/%d", v1status, v1Store)
	}
	var nSegV1 int
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM wait_segments s JOIN wait_attributions w ON w.id=s.attribution_id
		 WHERE w.order_id=$1 AND w.version_no=1`, oid).Scan(&nSegV1)
	if nSegV1 == 0 {
		t.Fatalf("v1 原始拆分段必须保留")
	}

	// 班组评分已按订单日（09-15）回算：本单门店等待归零、计入豁免
	var fStore, fExempt int
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT store_seconds,exempted_seconds FROM wait_attributions
		 WHERE order_id=$1 AND status='active'`, oid).Scan(&fStore, &fExempt)
	if fStore != 0 || fExempt != 180 {
		t.Fatalf("生效版本门店 0/豁免 180, got %d/%d", fStore, fExempt)
	}
	var waitExemptDay int
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT COALESCE((metrics->'counts'->>'wait_exempt_sec')::int,-1)
		 FROM team_score_snapshots WHERE store_id=1 AND score_date='2026-09-15'`).Scan(&waitExemptDay)
	if waitExemptDay < 180 {
		t.Fatalf("改判回算后当日豁免秒数应 >=180, got %d", waitExemptDay)
	}
	var penaltyInMetrics bool
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT COALESCE((metrics->'penalties'->>'rider_wait_store_penalty')::int,-1) >= 0
		 FROM team_score_snapshots WHERE store_id=1 AND score_date='2026-09-15'`).Scan(&penaltyInMetrics)
	if !penaltyInMetrics {
		t.Fatalf("评分快照应包含骑手等待门店责任扣分项")
	}
}

func TestWaitAppealOnCanceledOnlySettlement(t *testing.T) {
	setStore1Thresholds(t)
	sc, b := do(t, "POST", "/v1/platform/orders", "9001",
		intake("T-WAIT-C", []intakeItem{{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"}},
			map[string]any{"rider_eta": at(12, 10)}))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	sc, _ = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, nil)
	mustExec(t, `UPDATE rider_arrivals SET arrived_at=$2 WHERE order_id=$1 AND arrival_no=1`, oid, at(12, 5))
	// 顾客取消（骑手已等待）
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/cancel", oid), "5001",
		map[string]any{"reason": "顾客取消", "store_fault": false})
	mustStatus(t, sc, 200, nil)

	a := attrNumbers(t, oid)
	if a["end_reason"] != "canceled" {
		t.Fatalf("取消单归因 end_reason 应为 canceled: %v", a)
	}
	// 店长就等待申诉，区域经理批准
	sc, b = do(t, "POST", fmt.Sprintf("/v1/orders/%d/wait-appeals", oid), "2001",
		map[string]any{"reason_code": "temp_shortage", "reason_detail": "牛奶临时断供"})
	mustStatus(t, sc, 201, b)
	rid := int64(b["review_id"].(float64))
	sc, b = do(t, "POST", fmt.Sprintf("/v1/wait-reviews/%d/decision", rid), "1001",
		map[string]any{"decision": "approve", "review_note": "补货单核实"})
	mustStatus(t, sc, 200, b)
	// 订单仍取消、无交接记录
	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	if b["status"] != "canceled" {
		t.Fatalf("申诉不得改变取消事实: %v", b["status"])
	}
	var handoffs int
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM handoffs WHERE order_id=$1`, oid).Scan(&handoffs)
	if handoffs != 0 {
		t.Fatalf("取消单不得产生交接记录: %d", handoffs)
	}
}

func TestWaitArrivalSnapshotAndThresholds(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001",
		intake("T-WAIT-S", []intakeItem{{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"}}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	// 开启爆单保护后骑手到店：快照应记录 surge_paused
	sc, _ = do(t, "POST", "/v1/staff/stores/1/surge", "2001",
		map[string]any{"paused": true, "reason": "测试快照", "minutes": 30})
	mustStatus(t, sc, 200, nil)
	sc, b = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, b)
	aid := int64(b["orders"].([]any)[0].(map[string]any)["arrival_id"].(float64))
	sc, b = do(t, "GET", fmt.Sprintf("/v1/rider/arrivals/%d", aid), "0000", nil)
	mustStatus(t, sc, 200, b)
	if b["surge_active"] != true || b["surge_snapshot"] != "surge_paused" {
		t.Fatalf("到店快照应记录爆单保护: %v", b)
	}
	if queueAhead, _ := b["queue_ahead_cups"].(float64); queueAhead != 1 {
		t.Fatalf("本单未出杯杯量应为 1: %v", b)
	}
	if _, ok := b["queue_cups"].(float64); !ok {
		t.Fatalf("应记录到店时全店排队杯量: %v", b)
	}
	sc, _ = do(t, "POST", "/v1/staff/stores/1/surge", "2001", map[string]any{"paused": false})
	mustStatus(t, sc, 200, nil)

	// 阈值查询含全局与门店覆盖
	sc, b = do(t, "GET", "/v1/management/wait-thresholds", "0000", nil)
	mustStatus(t, sc, 200, b)
	var found bool
	for _, x := range b["thresholds"].([]any) {
		m := x.(map[string]any)
		if m["store_id"].(float64) == 1 && m["platform_dispatch_lead_seconds"].(float64) == 900 {
			found = true
		}
	}
	if !found {
		t.Fatalf("门店1阈值覆盖未生效: %v", b)
	}
	// 平台服务指标可查
	sc, b = do(t, "GET", "/v1/reports/platform-service?from=2026-09-01&to=2026-12-31", "1001", nil)
	mustStatus(t, sc, 200, b)
	if b["total"] == nil {
		t.Fatalf("平台服务指标缺少合计: %v", b)
	}
}
