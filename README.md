# 咖啡连锁外卖出杯时效与骑手交接服务

面向咖啡连锁外卖场景的后端服务（Go + PostgreSQL + Redis）。把**门店、店员、骑手、顾客、外卖平台客服、财务赔付**串到同一张订单时间线上，覆盖：

- 多外卖平台（饿了么 / 美团）接单：美式、拿铁、冷萃、果咖、轻食、组合套餐；录入订单时间、门店产能、制作配方、冰热属性、糖度、杯型、封签码、骑手预计到店、顾客备注
- 接单即按配方扣减原料库存（杯型决定杯耗材、冰饮加冰与吸管、轻食/套餐扣餐具、每杯一封签）
- **门店爆单保护**：在制杯量超阈值自动拒单（`surge_full`），也可手动临时限制接单（`surge_paused`，可设时长）
- 门店按出杯顺序制作（队列按骑手 ETA 优先级排序，`FOR UPDATE SKIP LOCKED` 并发叫号）
- 店员扫描封签 → 生成封签码 → **热饮 / 冷饮 / 餐食自动分袋** → 全单封齐进入待取区
- 骑手到店核验：**订单号、袋数、冷热分袋、吸管、餐具**；未出杯禁止取走
- 全场景异常：骑手提前到店、饮品放置过久重做、顾客备注漏看、错杯、轻食缺货、骑手取错袋、平台取消订单、顾客漏液投诉
- **骑手提前到店等待责任拆分与争议复核**：记录骑手到店、实际派单（含改配）、门店开始制作、出杯、封签、交接时间，以及到店时**排队杯量、排队位序与门店爆单保护状态快照**；按可配置阈值把等待拆为 **平台派单过早 / 门店制作超时 / 骑手提前到店（+ 复核豁免）** 四段，互不重叠、不重复计时；改配、重做、骑手离店再到店时按事件时间线整单重归因；店长可对机器故障/临时缺货等申诉，区域经理复核后才生效，改判自动回算门店时效、骑手等待补贴、平台服务指标与班组评分，原始归因版本全保留；订单已取消/已交接时申诉只改绩效结算，不改交接事实
- **重做报损**：旧杯作废+报损成本，新杯重新入队，重做成本计入门店班组复盘与财务
- **缺货自动临时下架**（按原料反查商品），补货且原料齐备后自动恢复上架
- 平台取消：未制作杯原料回库、已封杯报损、按门店/平台责任退款
- 多平台订单**合并**（同一骑手顺路单一次到店登记、分别核验）
- 杯贴 / 袋贴打印数据、骑手到店屏、异常报损台账
- 财务赔付定责（门店 / 骑手 / 平台 / 顾客 / 共担），平台分摊补贴
- 区域经理按门店看**出杯慢、错杯、漏餐具、重做、平台取消对利润的影响**；区域督导复盘同步；门店班组评分更新
- 完整 **OpenAPI 3.x** 规范与离线内嵌 **Swagger UI**

---

## 1. 一键启动（Docker Compose）

需要 Docker（含 Compose 插件）。

```bash
docker compose up -d --build
```

首次启动会自动：
1. 拉起 PostgreSQL 15（端口 5432）与 Redis 7（端口 6379），健康检查通过后再启动应用；
2. 应用自动执行数据库迁移并写入种子数据（2 家门店、14 个账号、6 类商品、配方与库存）。

验证：

```bash
curl http://localhost:8080/healthz
# {"status":"ok","postgres":"ok","redis":"ok"}
```

- API 根地址：`http://localhost:8080`
- Swagger UI（可直接点选接口调试）：<http://localhost:8080/swagger/>
- OpenAPI 3.x 规范：<http://localhost:8080/openapi.yaml>

停止并清理数据卷：

```bash
docker compose down -v
```

---

## 2. 测试账号（X-Staff-Code 工号即密钥）

所有 `/v1` 接口（登录接口除外）都用请求头鉴权，二选一：

- 简单方式：每个请求带 `X-Staff-Code: <工号>`
- 或先 `POST /v1/auth/login`（body `{"code":"3001"}`）拿 Bearer Token，再带 `Authorization: Bearer <token>`

| 角色 | 登录名 | 工号 code | 说明 |
| --- | --- | --- | --- |
| 平台管理员 | admin | `0000` | 拥有所有接口权限 |
| 区域经理 | mgr.huadong | `1001` | 区域对比报表、班组评分 |
| 区域督导 | sup.li | `1002` | 门店复盘 |
| 店长 SH001 | sm.001 | `2001` | 接单、爆单保护、补货、下架 |
| 店员（门店1） | barista.001 | `3001` | 叫号、封签、重做、缺货 |
| 店员（门店1） | barista.002 | `3002` | |
| 饿了么骑手 | rider.eleme | `4001` | 到店登记、核验交接 |
| 美团骑手 | rider.meituan | `4002` | |
| 饿了么客服 | cs.eleme | `5001` | 取消订单 |
| 美团客服 | cs.meituan | `5002` | |
| 财务 | finance | `6001` | 赔付定责、财务台账 |
| 顾客 | customer.001 | `7001` | 投诉 |
| 饿了么平台 | platform.eleme | `9001` | 平台推单（自动带 platform=eleme） |
| 美团平台 | platform.meituan | `9002` | 平台推单（自动带 platform=meituan） |

种子门店：`SH001 上海人民广场店`（id=1）、`SH002 上海陆家嘴店`（id=2），均属「华东」。
种子商品 SKU：`AM 美式`、`LT 拿铁`、`CB 冷萃`、`FC 果咖`、`LF 鸡肉轻食卷`、`SET 元气套餐`。

> 服务对角色做了接口级权限控制（如骑手不能看区域报表、平台不能封签），越权返回 `403`。

---

## 3. 快速体验（核心链路）

```bash
# 1) 平台推单（冰拿铁 + 热美式 + 套餐 + 轻食）
curl -s -X POST http://localhost:8080/v1/platform/orders \
  -H "X-Staff-Code: 9001" -H "Content-Type: application/json" -d '{
    "platform_order_no":"EM20260929001","store_id":1,
    "customer_note":"杯盖贴紧不要摇","rider_eta":"2026-09-29T12:30:00+08:00",
    "items":[
      {"sku":"LT","qty":1,"temp":"ice","sugar":"三分糖","cup_size":"L","note":"少冰"},
      {"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"},
      {"sku":"SET","qty":1,"temp":"ice","sugar":"五分糖","cup_size":"M"},
      {"sku":"LF","qty":1}
    ]}'

# 2) 店员看出杯顺序（含顾客备注/杯备注/骑手ETA）
curl -s http://localhost:8080/v1/staff/stores/1/queue -H "X-Staff-Code: 3001"

# 3) 叫号取下一杯 → 扫描封签（重复到所有杯封齐；返回袋号与冷热分袋）
curl -s -X POST http://localhost:8080/v1/staff/stores/1/cups/next \
  -H "X-Staff-Code: 3001" -H "Content-Type: application/json" -d '{"store_id":1}'
curl -s -X POST http://localhost:8080/v1/staff/cups/1/seal -H "X-Staff-Code: 3001"

# 4) 骑手提前到店（饮品未出杯 → rider_early 异常 + 到店屏可见）
curl -s -X POST http://localhost:8080/v1/rider/arrivals \
  -H "X-Staff-Code: 4001" -H "Content-Type: application/json" -d '{"order_id":1}'

# 5) 取餐核验（订单号/袋数/冷热/吸管餐具；先故意取错袋）
curl -s -X POST http://localhost:8080/v1/rider/handoffs \
  -H "X-Staff-Code: 4001" -H "Content-Type: application/json" \
  -d '{"order_id":1,"actual_bags":2}'          # 409 + wrong_bag_count
curl -s -X POST http://localhost:8080/v1/rider/handoffs \
  -H "X-Staff-Code: 4001" -H "Content-Type: application/json" \
  -d '{"order_id":1,"actual_bags":3,"actual_hot":1,"actual_cold":1,"actual_food":1,
       "has_straw":true,"has_utensil":true}'   # 200 交接完成

# 6) 顾客漏液投诉 → 财务定责
curl -s -X POST http://localhost:8080/v1/customers/orders/1/complain \
  -H "X-Staff-Code: 7001" -H "Content-Type: application/json" \
  -d '{"type":"leak_complaint","detail":"拿铁漏液半杯"}'
curl -s -X PATCH http://localhost:8080/v1/finance/exceptions/1 \
  -H "X-Staff-Code: 6001" -H "Content-Type: application/json" \
  -d '{"responsibility":"shared","comp_amount":22,"platform_share":10,"resolution":"门店封盖不当与平台共担"}'

# 7) 一张订单的完整时间线（六类角色串联）
curl -s http://localhost:8080/v1/orders/1/events -H "X-Staff-Code: 0000"
```

更多场景（重做、缺货下架、爆单、取消、合并、报表、班组评分、复盘）均在
`scripts/e2e.sh` 中有可运行的完整演示，建议配合阅读。

### 骑手提前到店：等待三段拆分 → 店长申诉 → 区域经理复核改判

```bash
# 1) 店长配置阈值：制作目标 900 秒、早到宽限 0、骑手等待补贴 0.5 元/分钟
curl -s -X PATCH http://localhost:8080/v1/stores/1/wait-thresholds   -H "X-Staff-Code: 2001" -H "Content-Type: application/json"   -d '{"make_target_seconds":900,"early_grace_seconds":0,"subsidy_per_minute":0.5}'

# 2) 平台推单（实际派单时间自动记录）；骑手提前到店（自动快照排队杯量/位序/爆单状态）
curl -s -X POST http://localhost:8080/v1/rider/arrivals   -H "X-Staff-Code: 4001" -H "Content-Type: application/json" -d '{"order_id":1}'

# 骑手离店（离店期间不计等待，再到店自动新开一条到店记录）
curl -s -X POST http://localhost:8080/v1/rider/arrivals/1/leave -H "X-Staff-Code: 4001"
# 平台改配（更新实际派单时间线，收口旧骑手等待并整单重归因）
curl -s -X POST http://localhost:8080/v1/platform/orders/1/redispatch   -H "X-Staff-Code: 5001" -H "Content-Type: application/json"   -d '{"rider_name":"新骑手老吴","reason":"原骑手爆胎改派"}'

# 3) 交接后查看三段拆分（含全部历史版本与每次的计算依据 basis）
curl -s http://localhost:8080/v1/orders/1/wait-attribution -H "X-Staff-Code: 2001"
# summary_seconds: {rider_arrive_early, platform_dispatch_early, store_make_timeout, waived}

# 4) 店长就「机器故障」对等待责任发起申诉（订单已交接时自动 settlement_only）
curl -s -X POST http://localhost:8080/v1/orders/1/wait-appeals   -H "X-Staff-Code: 2001" -H "Content-Type: application/json"   -d '{"reason_code":"machine_fault","reason_detail":"12:02意式机故障停机，工单FIX-77",
       "evidence":{"ticket":"FIX-77"},"window_from":"2026-09-30T12:10:00+08:00",
       "window_to":"2026-09-30T12:23:00+08:00"}'

# 5) 区域经理复核通过，把窗口内门店制作超时改判豁免（waived），同事务自动回算：
#    门店时效、骑手等待补贴（净额冲回重结）、平台服务指标、当日班组评分
curl -s -X POST http://localhost:8080/v1/wait-appeals/1/review   -H "X-Staff-Code: 1001" -H "Content-Type: application/json"   -d '{"decision":"approved","move_to_party":"waived",
       "note":"核实机器故障13分钟，非门店班组责任，豁免，平台承担骑手补贴"}'
```

- 复核后再查 `GET /v1/orders/1/wait-attribution` 可见新版本（门店超时归零、出现 `waived` 段），
  而 `run_history` 中**原始交接版本仍保留**；`GET /v1/stores/1/wait-appeals` 可看 `original_run_id` 与 `effective_run_id`。
- 订单已交接/取消时，复核结果带 `"settlement_only":true`，订单状态、`handed_at`、交接核验记录**不被改写**，只改绩效与结算。

---

## 4. 本地开发（不使用 Docker）

依赖：Go 1.19+、PostgreSQL 13+、Redis 6+。

```bash
# 1) 准备数据库与 Redis（示例：本机 PG 5432 / Redis 6379）
createdb coffee   # 账号需有建库权限；迁移会自动建表+种子

# 2) 运行（启动时自动执行内嵌迁移 migrations）
export DATABASE_URL="postgres://用户:密码@127.0.0.1:5432/coffee?sslmode=disable"
export REDIS_ADDR="127.0.0.1:6379"
export PORT=8080
go run ./cmd/server
```

数据库迁移 SQL 内嵌在二进制里（`internal/db/sql/*.sql`），按文件名顺序、每文件仅执行一次（记录在 `schema_migrations` 表）。修改种子后重置开发库：

```bash
psql -d postgres -c "DROP DATABASE coffee;" -c "CREATE DATABASE coffee;"
# 重启服务即自动重新迁移
```

### 运行测试

- **Go 集成测试**：`go test ./...`
  会自动创建并使用独立的 `coffee_test` 数据库（默认连 `127.0.0.1:5433`、Redis `localhost:6380`，
  可用 `TEST_DATABASE_URL` / `TEST_REDIS_ADDR` 覆盖；连不上数据库时测试自动跳过）。
  覆盖：接单扣库存、冷热分袋、提前到店、取错袋核验、爆单拒单、缺货自动下架、取消回库、重做报损、报表与班组评分、OpenAPI 合法性。
- **端到端冒烟脚本（81 项断言）**：`scripts/e2e.sh`
  面向一个已运行在 `localhost:8099` 的服务（可用 `BASE=http://...` 改地址）。
  脚本默认会自动重置本地 `coffee` 库（`psql -h /tmp/cc126 -p 5433`）与 6380 Redis 并重启
  `/tmp/cc126/server`，如对接已运行环境请设 `SKIP_RESET=1`。

```bash
go build -o /tmp/cc126/server ./cmd/server
./scripts/e2e.sh
```

---

## 5. 主要 API 一览（完整定义见 Swagger UI / openapi.yaml）

| 模块 | 接口 |
| --- | --- |
| 鉴权 | `POST /v1/auth/login`、`GET /v1/auth/me` |
| 基础数据 | `GET /v1/stores`、`GET /v1/menu`、`GET /v1/stores/{id}/inventory`、`GET /v1/stores/{id}/board` |
| 平台接单 | `POST /v1/platform/orders`（支持 `Idempotency-Key`）、`POST /v1/platform/orders/{id}/cancel`、`POST /v1/platform/orders/{id}/redispatch`（改配，触发等待重归因） |
| 门店作业 | `GET /v1/staff/stores/{id}/queue`、`POST /v1/staff/stores/{id}/cups/next`、`POST /v1/staff/cups/{id}/start`、`POST /v1/staff/cups/{id}/seal`、`POST /v1/staff/cups/{id}/remake` |
| 缺货/下架 | `POST /v1/staff/orders/{id}/item-oos`、`POST /v1/staff/menu/{sku}/delist`、`GET /v1/stores/{id}/delistings`、`POST /v1/staff/inventory/restock` |
| 打印 | `GET /v1/staff/orders/{id}/cup-labels`、`GET /v1/staff/orders/{id}/bag-labels` |
| 爆单保护 | `POST /v1/staff/stores/{id}/surge` |
| 订单合并 | `POST /v1/staff/merge`、`GET /v1/merged-groups/{id}` |
| 骑手 | `POST /v1/rider/arrivals`、`POST /v1/rider/arrivals/{id}/leave`（离店，离店再到店新开记录）、`GET /v1/rider/screen`、`GET /v1/rider/arrivals/{id}`、`POST /v1/rider/handoffs` |
| 等待拆分/申诉 | `GET/PATCH /v1/stores/{id}/wait-thresholds`（可配置阈值）、`GET /v1/orders/{id}/wait-attribution`（三段归因+历史版本）、`POST /v1/orders/{id}/wait-appeals`（店长申诉）、`GET /v1/stores/{id}/wait-appeals`、`POST /v1/wait-appeals/{id}/review`（区域经理复核改判） |
| 顾客/客服 | `POST /v1/customers/orders/{id}/complain` |
| 财务 | `PATCH /v1/finance/exceptions/{id}`、`GET /v1/stores/{id}/finance` |
| 订单查询 | `GET /v1/orders/{id}`、`GET /v1/orders/{id}/events`、`GET /v1/orders/{id}/exceptions` |
| 管理报表 | `GET /v1/reports/stores/{id}/summary`、`GET /v1/reports/region` |
| 班组/复盘 | `POST /v1/management/stores/{id}/team-score/refresh`、`GET /v1/management/team-scores`、`POST /v1/management/stores/{id}/reviews` |

### 关键业务规则

- **库存扣减**：接单事务内对门店加 `pg_advisory_xact_lock` 串行化，锁行 `SELECT ... FOR UPDATE`
  后汇总扣减，不允许为负；任一原料不足整单回滚，并把相关商品**独立持久化临时下架**（不会因回滚丢失）。
- **出杯顺序**：队列先取制作中的杯，再按骑手 ETA（缺省按接单+30 分钟）、接单时间、杯序号排序。
- **分袋**：热饮→热袋、冰饮→冷袋、轻食→餐食袋；含饮品的组合套餐按其饮品冰热入袋。同温层首杯封签时扣减对应袋耗材。
- **重做**：旧杯 `voided` 并报损（按配方成本），新杯继承原杯序号重新入队，`remake_no+1`；
  重做成本写 `finance_records(kind=remake_cost, bearer=team)`，进入班组复盘。
- **骑手等待责任三段拆分**：每次到店记录实际派单时间、开做/出杯/封签/交接时间，以及到店时排队杯量、排队位序、爆单保护状态快照；等待只在「骑手在店且未出杯」区间计时，按可配置阈值（`wait_thresholds`：制作目标秒数、早到宽限、补贴单价）切成互不重叠的四段：
  - `rider_arrive_early` 骑手提前到店：仍在制作目标窗口内且早于「ETA-早到宽限」；
  - `platform_dispatch_early` 平台派单过早：制作目标窗口内骑手已在等的其余部分（含准点到店等待）；
  - `store_make_timeout` 门店制作超时：超过「实际派单时间 + 制作目标秒数」仍未出杯（门店制作 SLA 优先，与骑手是否早到无关）；
  - `waived` 豁免段：区域经理复核改判产生，不计门店绩效。
  - 改配、重做、骑手离店再到店时按 `order_events` 时间线**整单重新归因**，每次生成一个新版本（`wait_attribution_runs`/`wait_segments`），同一段等待只在最新版本计入，避免重复计入门店绩效和骑手补偿。
  - 非骑手责任等待按秒折算骑手等待补贴（`finance_records(kind=rider_wait_subsidy)`），终态（离店/交接/取消/复核改判）结算；改判时对原版本净额整组冲回（`[冲回]` 流水保留可审计）再按新版本重结，累计不重复。
- **争议复核**：店长对机器故障/临时缺货/人手不足/爆单连锁等发起申诉（`wait_appeals`，状态 pending）；区域经理（且属同区域）复核通过后可把申诉窗口内门店超时段改判给 `platform`/`rider`/`waived`，生成新归因版本（**原始归因与改判依据保留**），并在同一事务回算门店时效报表、骑手等待补贴、平台服务指标和当日班组评分（新增「门店制作超时等待每满 5 分钟扣 1 分」）；驳回不改任何结算。**订单已交接或已取消的，`settlement_only=true`，只调整绩效结算，`orders`/`handoffs` 等交接事实一律不改写。**
- **取消**：未做/制作中杯原料回库（`cancel_restock`），已封杯按报损处理；退款承担方由 `store_fault` 决定。
- **班组评分**：100 分起，慢单率（每 5% 扣 1）、错杯(-4)、漏吸管餐具(-2)、取错袋(-2)、饮品过久(-3)、漏备注(-2)、门店责任取消(-5)、门店制作超时等待（每满 5 分钟扣 1，取每单最新归因版本，复核改判自动回算）。

---

## 6. 已执行的验证结论

以下验证在交付环境中实际执行通过（最近一次完整执行日期 2026-09-30）：

1. `go vet ./...` 无告警；`go build ./cmd/server` 通过。
2. `go test ./...` 通过：新增 **13 个等待归因纯逻辑单测**（三段切分、早到宽限、离店再到店、
   改配重置制作目标、重做不重复计时、复核改判窗口切割（含跨窗口起止边界）、补贴折算）与 **2 个新集成测试**
   （三段拆分→交接结算→店长申诉→区域经理复核回算全链路；离店再到店双记录），
   连同既有 8 个集成测试与 OpenAPI 3.0 合法性/路径覆盖测试全部通过。
3. `scripts/e2e.sh` 端到端 **115/115 项断言全部通过**，覆盖 21 个场景段；除原有 19 段外，新增：
   - **第 20 段 等待责任拆分与争议复核**：配置可编辑阈值（制作目标/宽限/补贴单价）、
     到店快照排队杯量与爆单状态、交接返回四段拆分与骑手等待补贴、归因历史版本可查、
     店长机器故障申诉（已交接单 `settlement_only=true`）、店员复核 403、区域经理改判豁免后
     门店超时归零并产生豁免段、原始/生效版本分离、订单仍 handed（交接事实未改写）、
     门店承担补贴净额冲回为 0、门店报表统计已核准申诉；
   - **第 21 段 离店再到店 + 改配重归因**：离店收口等待区间、再到店新开记录（该单 2 条区间）、
     平台改派触发 `redispatched` 重归因版本、新骑手到店交接、归因段覆盖多个到店区间且互不重叠。
4. Swagger UI（`/swagger/`）、`/openapi.yaml`、内嵌静态资源均返回 200；`/healthz` 返回 PG/Redis 均 ok。
5. 新增数据库迁移 `0003_wait_attribution.sql`（可重复执行）：`orders.dispatched_at`、
   `rider_arrivals` 快照与离店字段、`wait_thresholds`、`wait_attribution_runs`、
   `wait_segments`、`wait_appeals`、`finance_records.run_id`；升级库重启即自动迁移，不影响存量数据。
6. Dockerfile / docker-compose.yml 已提供，可 `docker compose up -d --build` 一键启动
   （交付沙箱无 Docker 守护进程，未在沙箱内实际构建；镜像构建仅依赖标准 `golang:1.22-alpine` 与 `alpine:3.19`，应用为 CGO_ENABLED=0 静态二进制）。

---

## 7. 工程结构

```
.
├── cmd/server/main.go          # 入口：读环境变量、连库、迁移、起 HTTP
├── internal/
│   ├── db/                     # pgxpool 连接 + 内嵌 SQL 迁移执行器
│   │   └── sql/                # 0001 建表 / 0002 种子 / 0003 等待归因与申诉（随二进制 embed）
│   └── app/
│       ├── server.go           # 路由、角色鉴权、登录
│       ├── orders.go           # 接单、配方库存、幂等、缺货下架、平台取消
│       ├── production.go       # 出杯队列、封签分袋、重做报损、轻食缺货、叫号
│       ├── rider.go            # 骑手到店、到店屏、离店再到店、核验交接
│       ├── waitattr.go         # 等待三段拆分引擎、版本化重归因、补贴结算、申诉与复核回算
│       ├── management.go       # 门店/库存/爆单/合并/杯贴袋贴/看板
│       ├── reports.go          # 投诉、财务定责、台账、门店/区域报表、评分、复盘
│       ├── models.go           # JSON 模型
│       ├── swagger.go          # 内嵌 Swagger UI + OpenAPI
│       └── web/                # openapi.yaml + swagger-ui-dist 静态资源
├── scripts/e2e.sh              # 端到端冒烟脚本（115 断言 / 21 场景段）
├── Dockerfile
└── docker-compose.yml
```
