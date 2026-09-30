#!/usr/bin/env bash
# 端到端冒烟测试：覆盖接单→库存→出杯→封签分袋→骑手到店→核验交接→异常→财务→报表全链路
set -uo pipefail
BASE="${BASE:-http://localhost:8099}"
CURL="curl -s --noproxy *"
PASS=0; FAIL=0
ok(){ echo "PASS: $1"; PASS=$((PASS+1)); }
bad(){ echo "FAIL: $1"; FAIL=$((FAIL+1)); }

# api <method> <path> <staff_code> [json] [idempotency-key]
api(){
  local m="$1" p="$2" code="$3" body="${4:-}" idem="${5:-}"
  if [ -n "$body" ]; then
    if [ -n "$idem" ]; then
      curl -s --noproxy '*' -X "$m" "$BASE$p" -H "X-Staff-Code: $code" \
        -H "Content-Type: application/json" -H "Idempotency-Key: $idem" -d "$body"
    else
      curl -s --noproxy '*' -X "$m" "$BASE$p" -H "X-Staff-Code: $code" \
        -H "Content-Type: application/json" -d "$body"
    fi
  else
    curl -s --noproxy '*' -X "$m" "$BASE$p" -H "X-Staff-Code: $code"
  fi
}
# http_code: 单次请求，响应体存 /tmp/cc126/last.json，HTTP 码打印到 stdout（调用方用 RC=$(...) 捕获）
api_code(){
  local m="$1" p="$2" code="$3" body="${4:-}" idem="${5:-}"
  local args=(-s --noproxy '*' -o /tmp/cc126/last.json -w '%{http_code}' -X "$m" "$BASE$p" \
    -H "X-Staff-Code: $code" -H "Content-Type: application/json")
  [ -n "$idem" ] && args+=(-H "Idempotency-Key: $idem")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}"
}
J(){ jq -r "$1" /tmp/cc126/last.json; }
expect_code(){ # expected desc ; use last call's code var $RC
  if [ "$RC" = "$1" ]; then ok "$2 (HTTP $RC)"; else bad "$2 (期望 $1 实际 $RC): $(cat /tmp/cc126/last.json)"; fi
}
save(){ cp /tmp/cc126/last.json "/tmp/cc126/saved_$1.json"; }
getf(){ jq -r "$1" "/tmp/cc126/saved_$2.json"; }

PSQL="psql -h /tmp/cc126 -p 5433 -U coffee -d coffee -tA"

# 重置数据库与 Redis（测试脚本专用：drop/create 后由服务启动迁移自动重建）
if [ "${SKIP_RESET:-0}" != "1" ]; then
  echo "== 重置测试环境 =="
  pgrep -x server | xargs -r kill
  sleep 1.5
  psql -h /tmp/cc126 -p 5433 -U coffee -d postgres \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='coffee' AND pid<>pg_backend_pid();" >/dev/null
  psql -h /tmp/cc126 -p 5433 -U coffee -d postgres -c "DROP DATABASE IF EXISTS coffee;" -c "CREATE DATABASE coffee;" >/dev/null
  redis-cli -p 6380 FLUSHALL >/dev/null
  DATABASE_URL="postgres://coffee@127.0.0.1:5433/coffee?sslmode=disable" \
    REDIS_ADDR=localhost:6380 PORT=8099 nohup /tmp/cc126/server > /tmp/cc126/app.log 2>&1 &
  for i in $(seq 1 20); do
    curl -s --noproxy '*' "$BASE/healthz" | grep -q '"status":"ok"' && break
    sleep 0.5
  done
fi

echo "== 0. 健康检查 =="
curl -s --noproxy '*' "$BASE/healthz" | jq -e '.postgres=="ok" and .redis=="ok"' >/dev/null && ok "healthz" || bad "healthz"

echo "== 1. 接单：冰拿铁+热美式+冰套餐+轻食 =="
BODY='{"platform_order_no":"EM-E2E-1","store_id":1,"customer_note":"杯盖贴紧不要摇","rider_eta":"2030-01-01T00:00:00Z",
 "items":[{"sku":"LT","qty":1,"temp":"ice","sugar":"三分糖","cup_size":"L","note":"少冰"},
          {"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"},
          {"sku":"SET","qty":1,"temp":"ice","sugar":"五分糖","cup_size":"M"},
          {"sku":"LF","qty":1,"temp":"none","sugar":"无糖","cup_size":"-"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY"); expect_code 201 "接单成功"
save order1
OID=$(getf .id order1)
[ "$(J '.cups|length')" = "4" ] && ok "生成 4 杯" || bad "杯数: $(cat /tmp/cc126/last.json)"

echo "== 2. 库存扣减校验 =="
api GET /v1/stores/1/inventory 0000 > /tmp/cc126/last.json
MILK=$(J '.[]|select(.sku=="milk")|.qty'); SEAL=$(J '.[]|select(.sku=="seal")|.qty')
STRAW=$(J '.[]|select(.sku=="straw")|.qty'); UT=$(J '.[]|select(.sku=="utensil")|.qty')
[ "$MILK" = "19820" ] && ok "牛奶扣 180ml (20000->$MILK)" || bad "牛奶库存 $MILK"
[ "$SEAL" = "797" ] && ok "3 杯饮品各扣 1 张封签，轻食不贴 (800->$SEAL)" || bad "封签 $SEAL"
[ "$STRAW" = "398" ] && ok "冰饮 2 杯扣 2 支吸管 (400->$STRAW)" || bad "吸管 $STRAW"
[ "$UT" = "198" ] && ok "轻食/套餐扣 2 份餐具 (200->$UT)" || bad "餐具 $UT"

echo "== 3. 幂等重推 =="
BODYIDEM='{"platform_order_no":"EM-E2E-IDEM","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
api POST /v1/platform/orders 9001 "$BODYIDEM" "key-e2e-1" > /tmp/cc126/r1.json
api POST /v1/platform/orders 9001 "$BODYIDEM" "key-e2e-1" > /tmp/cc126/r2.json
[ "$(jq -r .error /tmp/cc126/r1.json)" = "null" ] && diff <(jq -S . /tmp/cc126/r1.json) <(jq -S . /tmp/cc126/r2.json) >/dev/null \
  && ok "幂等返回一致（同 Idempotency-Key 不重复建单）" || bad "幂等失效: $(cat /tmp/cc126/r1.json)"
IDEM1=$(jq -r .id /tmp/cc126/r1.json); IDEM2=$(jq -r .id /tmp/cc126/r2.json)
[ "$IDEM1" = "$IDEM2" ] && ok "两次返回同一订单 id=$IDEM1" || bad "订单 id 不一致 $IDEM1/$IDEM2"
# 做完幂等单，避免污染后续出杯队列
ICID=$(api GET "/v1/orders/$IDEM1" 0000 | jq -r '.cups[0].id')
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' >/dev/null
api POST "/v1/staff/cups/$ICID/seal" 3001 >/dev/null
api POST /v1/rider/handoffs 4001 "{\"order_id\":$IDEM1,\"actual_bags\":1,\"actual_hot\":1}" >/dev/null

echo "== 4. 出杯队列顺序 + 顾客备注可见 =="
api GET /v1/staff/stores/1/queue 3001 > /tmp/cc126/last.json
[ "$(J 'length')" = "4" ] && ok "队列 4 杯" || bad "队列长度 $(J 'length')"
[ "$(J '.[0].customer_note')" = "杯盖贴紧不要摇" ] && ok "队列显示顾客备注" || bad "顾客备注缺失"

echo "== 5. 逐杯制作+封签，冷热分袋 =="
for i in 1 2 3 4; do
  api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' > /tmp/cc126/next.json
  CID=$(jq -r .id /tmp/cc126/next.json)
  RC=$(api_code POST "/v1/staff/cups/$CID/seal" 3001)
  [ "$RC" = "200" ] || { bad "第 $i 杯封签失败: $(cat /tmp/cc126/last.json)"; }
done
ok "4 杯依次叫号制作并封签"
api GET "/v1/orders/$OID" 0000 > /tmp/cc126/last.json
[ "$(J .status)" = "active" ] && ok "全部封齐进入待取区(active)" || bad "订单状态 $(J .status)"
NB=$(J '.bags|map(.bag_code)|unique|length')
[ "$NB" = "3" ] && ok "冷热餐食 3 个物理袋 ($NB)" || bad "物理袋数 $NB"
api GET "/v1/staff/orders/$OID/bag-labels" 3001 > /tmp/cc126/last.json
[ "$(J '.labels|length')" = "3" ] && ok "袋贴 3 张" || bad "袋贴数"
api GET "/v1/staff/orders/$OID/cup-labels" 3001 > /tmp/cc126/last.json
[ "$(J '[.labels[].seal_code]|map(length>0)|all')" = "true" ] && ok "杯贴含封签码/冰热糖度杯型" || bad "杯贴数据"

echo "== 6. 骑手提前到店 =="
BODY2='{"platform_order_no":"EM-E2E-2","store_id":1,"rider_eta":"2030-01-01T00:00:00Z",
 "items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"全糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY2"); expect_code 201 "第二单接单"
save order2; OID2=$(getf .id order2)
RC=$(api_code POST /v1/rider/arrivals 4001 "{\"order_id\":$OID2}"); expect_code 200 "骑手到店登记"
[ "$(J '.orders[0].early')" = "true" ] && ok "未出杯判定为提前到店" || bad "early 标志"
[ "$(J '.orders[0].exception_id')" != "null" ] && ok "自动生成 rider_early 异常" || bad "无早到异常"
api GET "/v1/rider/screen?store_id=1" 4001 > /tmp/cc126/last.json
[ "$(J "map(select(.order_id==$OID2))|length")" = "1" ] && ok "骑手到店屏显示等待单" || bad "到店屏"

echo "== 7. 未出杯禁止交接 =="
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID2,\"actual_bags\":1}")
expect_code 409 "未出杯交接被拒"
[ "$(J .error)" = "drinks_not_ready" ] || bad "错误类型 $(J .error)"

echo "== 8. 出杯后核验：先模拟取错袋，再正确交接 =="
CID2=$(api GET "/v1/orders/$OID2" 0000 | jq -r '.cups[0].id')
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' > /dev/null
api POST "/v1/staff/cups/$CID2/seal" 3001 > /dev/null
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID2,\"actual_bags\":2}")
expect_code 409 "袋数不符核验不通过"
[ "$(J '.issues|index("wrong_bag_count")')" != "null" ] && ok "识别取错袋问题" || bad "issues: $(cat /tmp/cc126/last.json)"
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID2,\"actual_bags\":1,\"actual_hot\":1,\"actual_cold\":0}")
expect_code 200 "核验通过完成交接"
[ "$(J .status)" = "handed" ] && ok "订单状态 handed" || bad "状态 $(J .status)"

echo "== 9. 订单1：漏餐具核验失败后补放交接 =="
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID,\"actual_bags\":3,\"actual_hot\":1,\"actual_cold\":1,\"actual_food\":1,\"has_straw\":false,\"has_utensil\":false}")
expect_code 409 "漏吸管餐具核验不通过"
[ "$(J '.issues|index("missing_straw")')" != "null" ] && [ "$(J '.issues|index("missing_utensil")')" != "null" ] \
  && ok "同时识别漏吸管+漏餐具" || bad "issues: $(cat /tmp/cc126/last.json)"
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID,\"actual_bags\":3,\"actual_hot\":1,\"actual_cold\":1,\"actual_food\":1,\"has_straw\":true,\"has_utensil\":true}")
expect_code 200 "补齐后交接成功"

echo "== 10. 顾客漏液投诉 + 财务定责 =="
RC=$(api_code POST "/v1/customers/orders/$OID/complain" 7001 '{"type":"leak_complaint","detail":"拿铁漏液半杯"}')
expect_code 201 "投诉受理"
EXC=$(J .exception_id)
RC=$(api_code PATCH "/v1/finance/exceptions/$EXC" 6001 '{"responsibility":"shared","comp_amount":22,"platform_share":10,"resolution":"门店封盖不当与平台共担"}')
expect_code 200 "财务定责完成"
api GET "/v1/stores/1/finance?from=2026-09-01&to=2030-12-31" 6001 > /tmp/cc126/last.json
[ "$(J '[.summary[]|select(.kind=="customer_comp")]|length')" -ge 1 ] && ok "补偿入财务台账" || bad "财务无补偿记录"
[ "$(J '[.summary[]|select(.kind=="platform_comp")]|length')" -ge 1 ] && ok "平台分摊到账" || bad "无平台分摊"
[ "$(J '[.summary[]|select(.kind=="revenue")]|length')" -ge 1 ] && ok "交接订单确认营业收入" || bad "无收入记录"

echo "== 11. 重做（放置过久）：旧杯报损、新杯回队、成本入班组 =="
BODY3='{"platform_order_no":"EM-E2E-3","store_id":1,"items":[{"sku":"CB","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY3"); expect_code 201 "第三单接单"
save order3; OID3=$(getf .id order3)
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' > /tmp/cc126/n3.json
CID3=$(jq -r .id /tmp/cc126/n3.json)
api POST "/v1/staff/cups/$CID3/seal" 3001 > /dev/null
RC=$(api_code POST "/v1/staff/cups/$CID3/remake" 3001 '{"reason":"stale_drink","detail":{"waited_minutes":20}}')
expect_code 201 "过久重做受理"
[ "$(J .loss_cost)" != "0" ] && ok "重做产生报损成本 ($(J .loss_cost))" || bad "重做成本为 0"
api GET "/v1/orders/$OID3" 0000 > /tmp/cc126/last.json
[ "$(J '.cups|map(select(.status=="queued"))|length')" = "1" ] && ok "新杯重新入队" || bad "新杯未入队"
NEWCID=$(J '.cups|map(select(.status=="queued"))[0].id')
api POST "/v1/staff/cups/$NEWCID/start" 3001 > /dev/null
api POST "/v1/staff/cups/$NEWCID/seal" 3001 > /dev/null
api GET "/v1/staff/stores/1/loss-reports" 3001 > /tmp/cc126/last.json
[ "$(J "[.[]|select(.reason==\"stale_drink\")]|length")" -ge 1 ] && ok "报损记录可查" || bad "无报损记录"
api POST /v1/rider/handoffs 4001 "{\"order_id\":$OID3,\"actual_bags\":1,\"actual_cold\":1,\"has_straw\":true}" > /dev/null

echo "== 12. 缺货自动临时下架（冷萃液 260ml，单杯需 300ml） =="
$PSQL -c "UPDATE inventory SET qty=260 WHERE ingredient_id=(SELECT id FROM ingredients WHERE sku='coldbase') AND store_id=1" >/dev/null
BODY4='{"platform_order_no":"EM-E2E-4","store_id":1,"items":[{"sku":"CB","qty":1,"temp":"ice","sugar":"无糖","cup_size":"L"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY4"); expect_code 409 "库存不足接单被拒"
[ "$(J .error)" = "stock_shortage" ] && ok "错误码 stock_shortage" || bad "错误码 $(J .error)"
[ "$(J .details.short_ingredient)" = "coldbase" ] && ok "定位缺货原料 coldbase" || bad "缺货原料 $(J .details)"
RC=$(api_code POST /v1/platform/orders 9001 "$BODY4"); expect_code 409 "缺货商品再次接单被拒"
[ "$(J .error)" = "item_delisted" ] && ok "冷萃已自动临时下架" || bad "未下架 $(J .error)"
api GET /v1/stores/1/delistings 3001 > /tmp/cc126/last.json
[ "$(J '[.[]|select(.sku=="CB" and .active==true)]|length')" = "1" ] && ok "下架记录可查" || bad "无下架记录"

echo "== 13. 补货后自动恢复上架 =="
RC=$(api_code POST /v1/staff/inventory/restock 3001 '{"store_id":1,"resolve_delistings":true,"items":[{"ingredient_sku":"coldbase","qty":5000}]}')
expect_code 200 "冷萃液补货"
[ "$(J '.relisted_menu_item_ids|index(3)')" != "null" ] && ok "原料齐备自动恢复 CB 上架" || bad "未自动上架: $(cat /tmp/cc126/last.json)"

echo "== 14. 轻食接单后缺货：单品退款+临时下架 =="
BODY5='{"platform_order_no":"EM-E2E-5","store_id":1,"items":[{"sku":"LF","qty":1,"temp":"none","sugar":"无糖","cup_size":"-"},{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY5"); expect_code 201 "轻食混合单接单"
save order5; OID5=$(getf .id order5); LFREFUND=$(getf '.cups[0].unit_price' order5)
RC=$(api_code POST "/v1/staff/orders/$OID5/item-oos" 3001 '{"sku":"LF","note":"鸡肉卷售罄"}')
expect_code 200 "轻食缺货处理"
[ "$(J .refund_amount)" = "$LFREFUND" ] && ok "轻食单品退款 $LFREFUND" || bad "退款金额 $(J .refund_amount)"
api GET /v1/stores/1/delistings 3001 > /tmp/cc126/last.json
[ "$(J '[.[]|select(.sku=="LF" and .active==true)]|length')" = "1" ] && ok "轻食临时下架" || bad "轻食未下架"
# 剩余美式做好交接
AMCUP=$(api GET "/v1/orders/$OID5" 0000 | jq -r '.cups|map(select(.status!="voided"))[0].id')
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' >/dev/null
api POST "/v1/staff/cups/$AMCUP/start" 3001 >/dev/null
api POST "/v1/staff/cups/$AMCUP/seal" 3001 >/dev/null
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$OID5,\"actual_bags\":1,\"actual_hot\":1}")
expect_code 200 "缺货后剩余商品正常交接"

echo "== 15. 爆单保护：手动暂停 + 自动容量阈值 =="
RC=$(api_code POST /v1/staff/stores/1/surge 2001 '{"paused":true,"reason":"午高峰爆单","minutes":30}')
expect_code 200 "开启爆单暂停"
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-X","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}')
expect_code 429 "爆单期间拒单"
[ "$(J .error)" = "surge_paused" ] && ok "错误码 surge_paused" || bad "错误码 $(J .error)"
api POST /v1/staff/stores/1/surge 2001 '{"paused":false}' > /dev/null
# 容量阈值：门店2 阈值调到 1，下 2 杯单
$PSQL -c "UPDATE stores SET max_active_cups=1 WHERE id=2" >/dev/null
RC=$(api_code POST /v1/platform/orders 9002 '{"platform_order_no":"MT-E2E-CAP","store_id":2,"items":[{"sku":"AM","qty":2,"temp":"hot","sugar":"无糖","cup_size":"M"}]}')
expect_code 429 "超在制杯量阈值拒单"
[ "$(J .error)" = "surge_full" ] && ok "错误码 surge_full" || bad "错误码 $(J .error)"
$PSQL -c "UPDATE stores SET max_active_cups=12 WHERE id=2" >/dev/null

echo "== 16. 平台取消：未做回库、退款 =="
BEFORE=$($PSQL -c "SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')")
BODY6='{"platform_order_no":"EM-E2E-6","store_id":1,"items":[{"sku":"AM","qty":2,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODY6"); expect_code 201 "第六单接单"
save order6; OID6=$(getf .id order6)
CUP6=$(api GET "/v1/orders/$OID6" 0000 | jq -r '.cups[0].id')
api POST /v1/staff/stores/1/cups/next 3001 '{"store_id":1}' >/dev/null
api POST "/v1/staff/cups/$CUP6/start" 3001 >/dev/null   # 一杯制作中，一杯未动
RC=$(api_code POST "/v1/platform/orders/$OID6/cancel" 5001 '{"reason":"顾客取消","store_fault":false}')
expect_code 200 "平台取消成功"
[ "$(J .bearer)" = "platform" ] && ok "非门店责任，退款承担方=平台" || bad "承担方 $(J .bearer)"
AFTER=$($PSQL -c "SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')")
[ "$BEFORE" = "$AFTER" ] && ok "未制作杯原料已回库 ($BEFORE)" || bad "回库异常 $BEFORE -> $AFTER"
api GET "/v1/orders/$OID6/events" 0000 > /tmp/cc126/last.json
[ "$(J '[.[]|select(.event_type=="order_canceled")]|length')" = "1" ] && ok "取消事件进订单时间线" || bad "无取消事件"

echo "== 17. 多平台订单合并 =="
RC=$(api_code POST /v1/platform/orders 9001 '{"platform_order_no":"EM-E2E-M1","store_id":2,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}')
expect_code 201 "饿了么单(门店2)"
save m1; A=$(getf .id m1)
RC=$(api_code POST /v1/platform/orders 9002 "{\"platform_order_no\":\"MT-E2E-M2\",\"store_id\":2,\"items\":[{\"sku\":\"LT\",\"qty\":1,\"temp\":\"ice\",\"sugar\":\"全糖\",\"cup_size\":\"L\"}]}")
expect_code 201 "美团单(门店2)"
save m2; B=$(getf .id m2)
RC=$(api_code POST /v1/staff/merge 3001 "{\"store_id\":2,\"order_ids\":[$A,$B],\"note\":\"同一骑手顺路单\"}")
expect_code 201 "两平台订单合并"
GID=$(J .group_id)
# 两单都做掉
for O in $A $B; do
  C=$(api GET "/v1/orders/$O" 0000 | jq -r '.cups[0].id')
  api POST "/v1/staff/cups/$C/start" 3001 >/dev/null
  api POST "/v1/staff/cups/$C/seal" 3001 >/dev/null
done
RC=$(api_code POST /v1/rider/arrivals 4002 "{\"group_id\":$GID,\"order_id\":$A}")
expect_code 200 "骑手按合并组一次到店登记"
[ "$(J '.orders|length')" = "2" ] && ok "合并组 2 单同时登记" || bad "登记单数 $(J '.orders|length')"
for O in $A $B; do
  if [ "$O" = "$A" ]; then
    # 饿了么单：骑手本人按 order_id 核验
    api POST /v1/rider/handoffs 4002 "{\"order_id\":$O,\"actual_bags\":1,\"actual_hot\":1}" >/dev/null
  else
    # 美团冰饮单：店员按平台单号代骑手核验（含骑手姓名与吸管）
    RC=$(api_code POST /v1/rider/handoffs 3001 "{\"platform_order_no\":\"MT-E2E-M2\",\"platform\":\"meituan\",\"rider_name\":\"老吴\",\"actual_bags\":1,\"actual_cold\":1,\"has_straw\":true}")
    [ "$RC" = "200" ] && ok "店员按平台单号代骑手核验交接成功" || bad "店员代核验失败: $(cat /tmp/cc126/last.json)"
  fi
done

echo "== 18. 报表 / 班组评分 / 督导复盘 =="
api GET "/v1/reports/stores/1/summary?from=2026-09-01&to=2030-12-31" 1001 > /tmp/cc126/last.json
[ "$(J '.metrics.remakes')" -ge 1 ] && ok "门店报表统计到重做" || bad "报表重做数 $(cat /tmp/cc126/last.json | head -c 300)"
[ "$(J '.metrics.missing_straw + .metrics.missing_utensil')" -ge 2 ] && ok "漏吸管/餐具入报表" || bad "漏餐具统计"
[ "$(J '.metrics.food_oos')" -ge 1 ] && ok "轻食缺货入报表" || bad "缺货统计"
api GET "/v1/reports/region?region=华东&from=2026-09-01&to=2030-12-31" 1001 > /tmp/cc126/last.json
[ "$(J '.stores|length')" = "2" ] && ok "区域经理看到 2 家门店对比" || bad "区域门店数"
api POST /v1/management/stores/1/team-score/refresh 1002 > /tmp/cc126/last.json
SCORE=$(J .score)
[ "$SCORE" -lt 100 ] && ok "班组评分因异常扣分 (<100, 实际 $SCORE)" || bad "评分未扣分 $SCORE"
api GET /v1/management/team-scores 1001 > /tmp/cc126/last.json
[ "$(J 'length')" = "1" ] && ok "班组评分榜可查" || bad "评分榜"
RC=$(api_code POST /v1/management/stores/1/reviews 1002 '{"from":"2026-09-01","to":"2030-12-31","summary":"午高峰出杯慢、漏餐具集中，加强封签核对","actions":["高峰加派1人","出杯叫号培训"]}')
expect_code 201 "督导复盘创建"
jq -e '.remake_cost_total < 0' /tmp/cc126/last.json >/dev/null && ok "重做成本计入复盘 ($(J .remake_cost_total))" || bad "复盘重做成本"

echo "== 19. 时间线串联六类角色 =="
api GET "/v1/orders/$OID/events" 0000 > /tmp/cc126/last.json
ROLES=$(J '[.[]|select(.actor_role!="")|.actor_role]|unique|join(",")')
echo "  订单1出现的角色: $ROLES"
for role in platform barista rider customer finance; do
  echo "$ROLES" | grep -q "$role" && ok "时间线含 $role" || bad "时间线缺 $role"
done

echo
echo "== 20. 等待三段归因阈值配置 =="
RC=$(api_code PUT /v1/management/wait-thresholds 1001 '{"store_id":1,"platform_dispatch_lead_seconds":900,"store_make_grace_seconds":60,"rider_early_grace_seconds":60,"rider_subsidy_per_minute":0.5,"store_wait_score_seconds":300}')
expect_code 200 "门店1等待归因阈值配置"
[ "$(J '.thresholds.platform_dispatch_lead_seconds')" = "900" ] && ok "派单前置阈值=900秒" || bad "阈值未生效"
api GET /v1/management/wait-thresholds 0000 > /tmp/cc126/last.json
[ "$(J '[.thresholds[]|select(.store_id==0)]|length')" = "1" ] && ok "全局默认阈值存在" || bad "无全局阈值"
RC=$(api_code GET /v1/management/wait-thresholds 4001); expect_code 403 "骑手不能看阈值配置"

echo "== 21. 派单过早+骑手提前+门店超时：等待三段拆分 =="
BODYW1='{"platform_order_no":"EM-WAIT-1","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODYW1"); expect_code 201 "等待单W1接单"
save w1; W1=$(getf .id w1)
RC=$(api_code POST "/v1/platform/orders/$W1/dispatch" 9001 '{"rider_code":"4001","eta":"2026-09-15T12:10:00+08:00","occurred_at":"2026-09-15T12:00:00+08:00"}')
expect_code 201 "W1平台派单记录"
[ "$(J .event_type)" = "dispatched" ] && ok "首次为 dispatched" || bad "派单类型"
$PSQL -c "UPDATE orders SET ordered_at='2026-09-15 12:00:00+08' WHERE id=$W1" >/dev/null
RC=$(api_code POST /v1/rider/arrivals 4001 "{\"order_id\":$W1}"); expect_code 200 "W1骑手到店"
$PSQL -c "UPDATE rider_arrivals SET arrived_at='2026-09-15 12:05:00+08' WHERE order_id=$W1 AND arrival_no=1" >/dev/null
api GET "/v1/orders/$W1/wait-attribution" 0000 > /tmp/cc126/last.json
[ "$(J .provisional)" = "true" ] && ok "交接前为 provisional 实时试算" || bad "试算标记"
WCUP=$(api GET "/v1/orders/$W1" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$WCUP/start" 3001 >/dev/null
api POST "/v1/staff/cups/$WCUP/seal" 3001 >/dev/null
$PSQL -c "UPDATE orders SET all_sealed_at='2026-09-15 12:18:00+08' WHERE id=$W1" >/dev/null
$PSQL -c "UPDATE order_cups SET started_at='2026-09-15 12:02:00+08',sealed_at='2026-09-15 12:18:00+08' WHERE order_id=$W1" >/dev/null
RC=$(api_code POST /v1/rider/handoffs 4001 "{\"order_id\":$W1,\"actual_bags\":1,\"actual_hot\":1}")
expect_code 200 "W1交接并完成等待归因"
[ "$(J '.wait.rider_early_arrival_seconds')" = "240" ] && ok "骑手提前到店段=240秒(12:05→12:09)" || bad "骑手段 $(cat /tmp/cc126/last.json|jq -c .wait)"
[ "$(J '.wait.platform_early_dispatch_seconds')" = "240" ] && ok "平台派单过早段=240秒(12:11→12:15)" || bad "平台段"
[ "$(J '.wait.store_make_timeout_seconds')" = "180" ] && ok "门店制作超时段=180秒(12:15→12:18)" || bad "门店段"
[ "$(J '.wait.rider_subsidy_store')" = "1.5" ] && ok "门店承担等待补贴1.50元" || bad "门店补贴 $(J '.wait.rider_subsidy_store')"
[ "$(J '.wait.rider_subsidy_platform')" = "2" ] && ok "平台承担等待补贴2.00元" || bad "平台补贴 $(J '.wait.rider_subsidy_platform')"
api GET "/v1/orders/$W1/wait-attribution" 0000 > /tmp/cc126/last.json
[ "$(J '[.segments[]|.segment_type]|unique|length')" -ge 3 ] && ok "段明细多类型可查且互不重叠" || bad "段明细"
[ "$(J '[.segments[].seconds]|add')" = "$(J '.segments[].seconds' | awk '{s+=$1}END{print s}')" ] && ok "段秒数合计自洽" || bad "段秒数合计"

echo "== 22. 离店再到店：区间独立归因、空档不计、不重复 =="
BODYW2='{"platform_order_no":"EM-WAIT-2","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODYW2"); expect_code 201 "等待单W2接单"
save w2; W2=$(getf .id w2)
api POST "/v1/platform/orders/$W2/dispatch" 9001 '{"rider_code":"4001","eta":"2026-09-15T12:10:00+08:00","occurred_at":"2026-09-15T12:00:00+08:00"}' >/dev/null
$PSQL -c "UPDATE orders SET ordered_at='2026-09-15 12:00:00+08' WHERE id=$W2" >/dev/null
api POST /v1/rider/arrivals 4001 "{\"order_id\":$W2}" >/dev/null
$PSQL -c "UPDATE rider_arrivals SET arrived_at='2026-09-15 12:05:00+08' WHERE order_id=$W2 AND arrival_no=1" >/dev/null
RC=$(api_code POST /v1/rider/leaves 4001 '{"order_id":'$W2',"reason":"other_order","note":"先取隔壁单"}')
expect_code 200 "W2骑手第一次离店"
$PSQL -c "UPDATE rider_arrivals SET left_at='2026-09-15 12:07:00+08' WHERE order_id=$W2 AND arrival_no=1" >/dev/null
api GET "/v1/rider/screen?store_id=1" 4001 > /tmp/cc126/last.json
[ "$(J "map(select(.order_id==$W2))|length")" = "0" ] && ok "离店后从到店屏消失" || bad "到店屏未过滤离店: $(cat /tmp/cc126/last.json)"
api POST /v1/rider/arrivals 4001 "{\"order_id\":$W2}" >/dev/null
$PSQL -c "UPDATE rider_arrivals SET arrived_at='2026-09-15 12:12:00+08' WHERE order_id=$W2 AND arrival_no=2" >/dev/null
WCUP2=$(api GET "/v1/orders/$W2" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$WCUP2/start" 3001 >/dev/null
api POST "/v1/staff/cups/$WCUP2/seal" 3001 >/dev/null
$PSQL -c "UPDATE orders SET all_sealed_at='2026-09-15 12:18:00+08' WHERE id=$W2" >/dev/null
$PSQL -c "UPDATE order_cups SET started_at='2026-09-15 12:12:30+08',sealed_at='2026-09-15 12:18:00+08' WHERE order_id=$W2" >/dev/null
api POST /v1/rider/handoffs 4001 "{\"order_id\":$W2,\"actual_bags\":1,\"actual_hot\":1}" >/dev/null
api GET "/v1/orders/$W2/wait-attribution" 0000 > /tmp/cc126/last.json
[ "$(J '.attribution.rider_early_arrival_seconds')" = "120" ] && ok "第一段骑手段=120秒(12:05→12:07)" || bad "W2骑手段"
[ "$(J '.attribution.platform_early_dispatch_seconds')" = "180" ] && ok "第二段平台段=180秒(12:12→12:15)" || bad "W2平台段"
[ "$(J '.attribution.store_make_timeout_seconds')" = "180" ] && ok "第二段门店段=180秒(12:15→12:18)" || bad "W2门店段"
A1=$($PSQL -c "SELECT store_fault_wait_seconds FROM rider_arrivals WHERE order_id=$W2 AND arrival_no=1")
A2=$($PSQL -c "SELECT store_fault_wait_seconds FROM rider_arrivals WHERE order_id=$W2 AND arrival_no=2")
[ "$A1" = "0" ] && [ "$A2" = "180" ] && ok "门店责任只落第二段，离店空档不重复计入 ($A1/$A2)" || bad "两段责任 $A1/$A2"

echo "== 23. 改配后按最新派单时间线重新归因 =="
BODYW3='{"platform_order_no":"EM-WAIT-3","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODYW3"); expect_code 201 "等待单W3接单"
save w3; W3=$(getf .id w3)
api POST "/v1/platform/orders/$W3/dispatch" 9001 '{"rider_code":"4001","eta":"2026-09-15T12:20:00+08:00","occurred_at":"2026-09-15T11:50:00+08:00"}' >/dev/null
$PSQL -c "UPDATE orders SET ordered_at='2026-09-15 11:50:00+08' WHERE id=$W3" >/dev/null
api POST /v1/rider/arrivals 4001 "{\"order_id\":$W3}" >/dev/null
$PSQL -c "UPDATE rider_arrivals SET arrived_at='2026-09-15 12:10:00+08' WHERE order_id=$W3 AND arrival_no=1" >/dev/null
RC=$(api_code POST "/v1/platform/orders/$W3/dispatch" 9001 '{"rider_code":"4001","eta":"2026-09-15T12:16:00+08:00","occurred_at":"2026-09-15T12:14:00+08:00","note":"顾客改地址改配"}')
expect_code 201 "W3平台改配"
[ "$(J .event_type)" = "redispatch" ] && [ "$(J .seq)" = "2" ] && ok "第二次派单识别为改配 seq=2" || bad "改配类型"
WCUP3=$(api GET "/v1/orders/$W3" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$WCUP3/start" 3001 >/dev/null
api POST "/v1/staff/cups/$WCUP3/seal" 3001 >/dev/null
$PSQL -c "UPDATE orders SET all_sealed_at='2026-09-15 12:18:00+08' WHERE id=$W3" >/dev/null
$PSQL -c "UPDATE order_cups SET started_at='2026-09-15 12:14:30+08',sealed_at='2026-09-15 12:18:00+08' WHERE order_id=$W3" >/dev/null
api POST /v1/rider/handoffs 4001 "{\"order_id\":$W3,\"actual_bags\":1,\"actual_hot\":1}" >/dev/null
api GET "/v1/orders/$W3/wait-attribution" 0000 > /tmp/cc126/last.json
[ "$(J '.attribution.platform_early_dispatch_seconds')" = "60" ] && ok "改配后平台段=60秒(12:17→12:18)" || bad "W3平台段 $(J .attribution.platform_early_dispatch_seconds)"
[ "$(J '.attribution.store_make_timeout_seconds')" = "0" ] && ok "改配后门店段=0(改配过晚门店免责)" || bad "W3门店段"

echo "== 24. 重做后按最终出杯时间一次归因（不重复计门店） =="
BODYW4='{"platform_order_no":"EM-WAIT-4","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
RC=$(api_code POST /v1/platform/orders 9001 "$BODYW4"); expect_code 201 "等待单W4接单"
save w4; W4=$(getf .id w4)
api POST "/v1/platform/orders/$W4/dispatch" 9001 '{"rider_code":"4001","eta":"2026-09-15T12:10:00+08:00","occurred_at":"2026-09-15T12:00:00+08:00"}' >/dev/null
$PSQL -c "UPDATE orders SET ordered_at='2026-09-15 12:00:00+08' WHERE id=$W4" >/dev/null
api POST /v1/rider/arrivals 4001 "{\"order_id\":$W4}" >/dev/null
$PSQL -c "UPDATE rider_arrivals SET arrived_at='2026-09-15 12:05:00+08' WHERE order_id=$W4 AND arrival_no=1" >/dev/null
OLD4=$(api GET "/v1/orders/$W4" 0000 | jq -r '.cups[0].id')
api POST "/v1/staff/cups/$OLD4/start" 3001 >/dev/null
api POST "/v1/staff/cups/$OLD4/seal" 3001 >/dev/null
$PSQL -c "UPDATE order_cups SET started_at='2026-09-15 12:02:00+08',sealed_at='2026-09-15 12:18:00+08' WHERE id=$OLD4" >/dev/null
$PSQL -c "UPDATE orders SET all_sealed_at='2026-09-15 12:18:00+08' WHERE id=$W4" >/dev/null
api POST "/v1/staff/cups/$OLD4/remake" 3001 '{"reason":"stale_drink","detail":{"waited_minutes":20}}' >/dev/null
NEW4=$(api GET "/v1/orders/$W4" 0000 | jq -r '.cups|map(select(.status=="queued"))[0].id')
api POST "/v1/staff/cups/$NEW4/start" 3001 >/dev/null
api POST "/v1/staff/cups/$NEW4/seal" 3001 >/dev/null
$PSQL -c "UPDATE order_cups SET started_at='2026-09-15 12:20:00+08',sealed_at='2026-09-15 12:25:00+08' WHERE id=$NEW4" >/dev/null
$PSQL -c "UPDATE orders SET all_sealed_at='2026-09-15 12:25:00+08' WHERE id=$W4" >/dev/null
api POST /v1/rider/handoffs 4001 "{\"order_id\":$W4,\"actual_bags\":1,\"actual_hot\":1}" >/dev/null
api GET "/v1/orders/$W4/wait-attribution" 0000 > /tmp/cc126/last.json
[ "$(J '.attribution.store_make_timeout_seconds')" = "600" ] && ok "重做后门店段按最终出杯=600秒(12:15→12:25)，不重复累计" || bad "W4门店段 $(J .attribution.store_make_timeout_seconds)"

echo "== 25. 店长申诉→区域经理复核改判→自动回算且不改交接事实 =="
RC=$(api_code POST "/v1/orders/$W1/wait-appeals" 4001 '{"reason_code":"machine_fault","reason_detail":"x"}')
expect_code 403 "骑手不能发起门店申诉"
RC=$(api_code POST "/v1/orders/$W1/wait-appeals" 2001 '{"reason_code":"machine_fault","reason_detail":"12:11咖啡机故障停机","evidence":{"repair_ticket":"R-1","photo":"ev.jpg"}}')
expect_code 201 "店长机器故障申诉受理"
RV=$(J .review_id)
RC=$(api_code POST "/v1/orders/$W1/wait-appeals" 2001 '{"reason_code":"other","reason_detail":"重复申诉"}')
expect_code 409 "待复核中重复申诉被拒"
RC=$(api_code POST "/v1/wait-reviews/$RV/decision" 3001 '{"decision":"approve"}')
expect_code 403 "店员不能复核申诉"
RC=$(api_code POST "/v1/wait-reviews/$RV/decision" 1001 '{"decision":"approve","review_note":"维修工单与监控核实，门店豁免转平台"}')
expect_code 200 "区域经理复核通过"
[ "$(J .handoff_fact_unchanged)" = "true" ] && ok "改判声明不改交接事实" || bad "交接事实标记"
[ "$(J .after.store_seconds)" = "0" ] && ok "改判后门店责任秒数归零" || bad "改判门店秒数"
[ "$(J .after.exempted_seconds)" = "180" ] && ok "原门店180秒转豁免" || bad "豁免秒数 $(J .after.exempted_seconds)"
[ "$(J .after.rider_subsidy_platform)" = "3.5" ] && ok "平台承担补贴改判为3.50元" || bad "改判平台补贴"
SN=$($PSQL -c "SELECT COALESCE(sum(amount),0) FROM finance_records WHERE order_id=$W1 AND kind='rider_wait_subsidy' AND bearer='store'")
PN=$($PSQL -c "SELECT COALESCE(sum(amount),0) FROM finance_records WHERE order_id=$W1 AND kind='rider_wait_subsidy' AND bearer='platform'")
[ "$SN" = "0.00" ] && ok "门店补贴红冲后净额0.00 ($SN)" || bad "门店红冲净额 $SN"
[ "$PN" = "-3.50" ] && ok "平台补贴净额-3.50 ($PN)" || bad "平台净额 $PN"
$PSQL -c "SELECT status,store_seconds FROM wait_attributions WHERE order_id=$W1 ORDER BY version_no"
[ "$($PSQL -c "SELECT status||'/'||store_seconds FROM wait_attributions WHERE order_id=$W1 AND version_no=1")" = "superseded/180" ] && ok "原始归因v1(superseded,门店180秒)保留" || bad "原始归因未保留"
[ "$($PSQL -c "SELECT count(*) FROM wait_segments s JOIN wait_attributions w ON w.id=s.attribution_id WHERE w.order_id=$W1 AND w.version_no=1")" -ge 1 ] && ok "v1原始拆分段永久保留" || bad "v1段丢失"
api GET "/v1/orders/$W1" 0000 > /tmp/cc126/last.json
[ "$(J .status)" = "handed" ] && ok "订单仍为 handed，交接事实未被改写" || bad "订单状态被改 $(J .status)"
[ "$($PSQL -c "SELECT count(*) FROM handoffs WHERE order_id=$W1 AND passed=TRUE")" = "1" ] && ok "交接核验记录保留1条" || bad "交接记录被改"
api POST /v1/management/stores/1/team-score/refresh 1002 >/dev/null
api GET /v1/management/team-scores 1001 > /tmp/cc126/last.json
[ "$(J 'length')" -ge 1 ] && ok "改判后班组评分榜可刷新" || bad "评分榜"
api GET "/v1/wait-reviews?status=approved" 1001 > /tmp/cc126/last.json
[ "$(J "[.[]|select(.id==$RV and .status==\"approved\")]|length")" = "1" ] && ok "复核单含改判前后指标快照可查" || bad "复核单查询"

echo "== 26. 平台服务指标（派单过早口径） =="
api GET "/v1/reports/platform-service?from=2026-09-01&to=2030-12-31&platform=eleme" 1001 > /tmp/cc126/last.json
[ "$(J '.total.platform_fault_orders')" -ge 2 ] && ok "平台指标统计到派单过早/豁免订单" || bad "平台指标 $(J .total)"
[ "$(J '[.stores[]|select(.platform=="eleme")]|length')" -ge 1 ] && ok "按平台+门店分组返回" || bad "平台分组"
RC=$(api_code GET "/v1/reports/platform-service" 4002); expect_code 403 "骑手不能查平台服务指标"
RC=$(api_code GET "/v1/reports/platform-service" 5002); expect_code 200 "美团客服可查（自动限定美团口径）"
api GET "/v1/reports/stores/1/summary?from=2026-09-01&to=2030-12-31" 1001 > /tmp/cc126/last.json
[ "$(J '.metrics.wait_platform_early_dispatch_seconds')" -ge 240 ] && ok "门店报表含平台派单过早秒数" || bad "门店报表等待口径"
[ "$(J '.metrics.wait_exempted_seconds')" = "180" ] && ok "门店报表含复核豁免180秒" || bad "门店报表豁免 $(J '.metrics.wait_exempted_seconds')"
[ "$(J '.metrics.rider_wait_subsidy_platform')" != "0" ] && ok "门店报表含平台承担等待补贴" || bad "门店报表补贴"

echo
echo "================ 结果: PASS=$PASS FAIL=$FAIL ================"
[ "$FAIL" = 0 ]
