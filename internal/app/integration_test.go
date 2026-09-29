package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"coffee-chain/internal/app"
	"coffee-chain/internal/db"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// 集成测试：默认连接本地 5433/6380（见 README）。
// 可用 TEST_DATABASE_URL / TEST_REDIS_ADDR 覆盖；数据库不存在会自动 drop/create。
var (
	testDSN = envOr("TEST_DATABASE_URL", "postgres://coffee@127.0.0.1:5433/coffee_test?sslmode=disable")
	testRDS = envOr("TEST_REDIS_ADDR", "localhost:6380")
	suite   *testEnv
)

type testEnv struct {
	pool *pgxpool.Pool
	rdb  *redis.Client
	srv  *httptest.Server
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	// 管理连接：drop/create 测试库
	adminDSN := strings.Replace(testDSN, "/coffee_test?", "/postgres?", 1)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：无法连接 PostgreSQL: %v\n", err)
		os.Exit(0)
	}
	_, _ = admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='coffee_test' AND pid<>pg_backend_pid()`)
	_, err = admin.Exec(ctx, `DROP DATABASE IF EXISTS coffee_test`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：drop 测试库失败: %v\n", err)
		os.Exit(0)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE coffee_test`); err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：create 测试库失败: %v\n", err)
		os.Exit(0)
	}
	admin.Close()

	pool, err := db.Connect(ctx, testDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：%v\n", err)
		os.Exit(0)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "迁移失败: %v\n", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(&redis.Options{Addr: testRDS, DB: 15})
	if err := rdb.Ping(ctx).Err(); err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：无法连接 Redis: %v\n", err)
		os.Exit(0)
	}
	_ = rdb.FlushDB(ctx)

	a := app.New(pool, rdb)
	suite = &testEnv{pool: pool, rdb: rdb, srv: httptest.NewServer(a.Router())}
	code := m.Run()
	suite.srv.Close()
	_ = rdb.FlushDB(ctx)
	os.Exit(code)
}

func do(t *testing.T, method, path, code string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, suite.srv.URL+path, rdr)
	req.Header.Set("X-Staff-Code", code)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败 %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func mustStatus(t *testing.T, got, want int, body map[string]any) {
	t.Helper()
	if got != want {
		t.Fatalf("HTTP %d, 期望 %d, body=%v", got, want, body)
	}
}

type intakeItem map[string]any

func intake(platformNo string, items []intakeItem, extra map[string]any) map[string]any {
	body := map[string]any{
		"platform_order_no": platformNo,
		"store_id":          1,
		"items":             items,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestHealthAndAuth(t *testing.T) {
	resp, err := http.Get(suite.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	// 错误工号
	if sc, b := do(t, "GET", "/v1/auth/me", "9999", nil); sc != 401 {
		t.Fatalf("错误工号应 401, got %d %v", sc, b)
	}
	// 正确工号
	if sc, b := do(t, "GET", "/v1/auth/me", "3001", nil); sc != 200 || b["role"] != "barista" {
		t.Fatalf("登录失败 %d %v", sc, b)
	}
}

func TestIntakeDeductsStock(t *testing.T) {
	var before float64
	err := suite.pool.QueryRow(context.Background(),
		`SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='milk')`).
		Scan(&before)
	if err != nil {
		t.Fatal(err)
	}
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-LT-1", []intakeItem{
		{"sku": "LT", "qty": 1, "temp": "ice", "sugar": "三分糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	cups := b["cups"].([]any)
	if len(cups) != 1 {
		t.Fatalf("应生成 1 杯, got %v", b)
	}
	var after float64
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='milk')`).
		Scan(&after)
	if before-after != 180 {
		t.Fatalf("牛奶应扣 180ml, before=%v after=%v", before, after)
	}
}

func TestMakeSealColdBagAndHandoff(t *testing.T) {
	// 冰美式一杯
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-AM-ICE", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))

	// 骑手提前到店
	sc, b = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, b)
	orders := b["orders"].([]any)
	if !orders[0].(map[string]any)["early"].(bool) {
		t.Fatalf("未出杯应判定提前到店: %v", b)
	}
	// 未出杯禁止交接
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{"order_id": oid, "actual_bags": 1})
	if sc != 409 || b["error"] != "drinks_not_ready" {
		t.Fatalf("应拒绝未出杯交接: %d %v", sc, b)
	}
	// 叫号（显式指定本单的杯，避免与其他用例的队列互相干扰）
	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	cid := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/start", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/seal", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	if b["bag_kind"] != "cold" {
		t.Fatalf("冰饮应入冷饮袋 cold, got %v", b["bag_kind"])
	}
	// 取错袋
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{"order_id": oid, "actual_bags": 5})
	if sc != 409 {
		t.Fatalf("袋数不符应 409, got %d", sc)
	}
	issues := b["issues"].([]any)
	if issues[0] != "wrong_bag_count" {
		t.Fatalf("应识别 wrong_bag_count: %v", issues)
	}
	// 正确交接（冰饮需吸管）
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_cold": 1, "has_straw": true})
	mustStatus(t, sc, 200, b)
	if b["status"] != "handed" {
		t.Fatalf("交接后状态应为 handed: %v", b)
	}
}

func TestSurgeProtection(t *testing.T) {
	sc, b := do(t, "POST", "/v1/staff/stores/2/surge", "2001",
		map[string]any{"paused": true, "reason": "测试爆单", "minutes": 10})
	mustStatus(t, sc, 200, b)
	sc, b = do(t, "POST", "/v1/platform/orders", "9002", intake("T-SURGE", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"},
	}, map[string]any{"store_id": 2}))
	if sc != 429 || b["error"] != "surge_paused" {
		t.Fatalf("爆单应拒单: %d %v", sc, b)
	}
	sc, _ = do(t, "POST", "/v1/staff/stores/2/surge", "2001", map[string]any{"paused": false})
	if sc != 200 {
		t.Fatalf("解除爆单失败 %d", sc)
	}
}

func TestShortageTriggersDelisting(t *testing.T) {
	ctx := context.Background()
	if _, err := suite.pool.Exec(ctx,
		`UPDATE inventory SET qty=100 WHERE store_id=1
		 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='coldbase')`); err != nil {
		t.Fatal(err)
	}
	body := intake("T-OOS", []intakeItem{
		{"sku": "CB", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil)
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", body)
	if sc != 409 || b["error"] != "stock_shortage" {
		t.Fatalf("库存不足应 stock_shortage: %d %v", sc, b)
	}
	// 事务回滚后下架已独立持久化 → 再次推单得到 item_delisted
	sc, b = do(t, "POST", "/v1/platform/orders", "9001", body)
	if sc != 409 || b["error"] != "item_delisted" {
		t.Fatalf("缺货商品应被临时下架: %d %v", sc, b)
	}
	var n int
	if err := suite.pool.QueryRow(ctx,
		`SELECT count(*) FROM delistings d JOIN menu_items m ON m.id=d.menu_item_id
		 WHERE d.store_id=1 AND m.sku='CB' AND d.active=TRUE`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("应存在 1 条 CB 下架记录, n=%d err=%v", n, err)
	}
}

func TestCancelRestocksUnmadeCups(t *testing.T) {
	ctx := context.Background()
	var before float64
	if err := suite.pool.QueryRow(ctx,
		`SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')`).
		Scan(&before); err != nil {
		t.Fatal(err)
	}
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-CXL", []intakeItem{
		{"sku": "AM", "qty": 2, "temp": "hot", "sugar": "无糖", "cup_size": "M"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))

	sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/cancel", oid), "5001",
		map[string]any{"reason": "顾客取消", "store_fault": false})
	mustStatus(t, sc, 200, b)
	if b["bearer"] != "platform" {
		t.Fatalf("非门店责任取消承担方应为 platform: %v", b)
	}
	var after float64
	if err := suite.pool.QueryRow(ctx,
		`SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')`).
		Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("未制作原料应回库: before=%v after=%v", before, after)
	}
}

func TestRemakeAddsLossCost(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-RMK", []intakeItem{
		{"sku": "LT", "qty": 1, "temp": "hot", "sugar": "全糖", "cup_size": "M"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))

	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	cid := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/start", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/seal", cid), "3001", nil)
	if sc != 200 {
		t.Fatalf("封签失败 %d", sc)
	}
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", cid), "3001",
		map[string]any{"reason": "stale_drink", "detail": map[string]any{"waited": 20}})
	mustStatus(t, sc, 201, b)
	if b["loss_cost"].(float64) <= 0 {
		t.Fatalf("重做应产生报损成本: %v", b)
	}
	// 旧杯作废、新杯入队
	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	var queued, voided int
	for _, c := range b["cups"].([]any) {
		switch c.(map[string]any)["status"] {
		case "queued":
			queued++
		case "voided":
			voided++
		}
	}
	if queued != 1 || voided != 1 {
		t.Fatalf("重做后应 1 新杯入队 1 旧杯作废, queued=%d voided=%d", queued, voided)
	}
}

func TestReportAndTeamScore(t *testing.T) {
	// 权限：骑手不能看区域报表
	if sc, _ := do(t, "GET", "/v1/reports/region", "4001", nil); sc != 403 {
		t.Fatalf("骑手访问区域报表应 403, got %d", sc)
	}
	sc, b := do(t, "GET", "/v1/reports/stores/1/summary", "1001", nil)
	mustStatus(t, sc, 200, b)
	metrics := b["metrics"].(map[string]any)
	if metrics["orders_received"].(float64) < 3 {
		t.Fatalf("报表订单数异常: %v", metrics["orders_received"])
	}
	sc, b = do(t, "POST", "/v1/management/stores/1/team-score/refresh", "1002", nil)
	mustStatus(t, sc, 200, b)
	if b["score"].(float64) > 100 || b["score"].(float64) < 0 {
		t.Fatalf("班组评分越界: %v", b["score"])
	}
	// 督导复盘必须带时间范围与结论
	sc, b = do(t, "POST", "/v1/management/stores/1/reviews", "1002", map[string]any{
		"from":    time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
		"to":      time.Now().Format("2006-01-02"),
		"summary": "集成测试复盘",
		"actions": []string{"整改1"},
	})
	mustStatus(t, sc, 201, b)
}

var _ = strings.TrimSpace
