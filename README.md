# 咖啡连锁外卖出杯时效与骑手交接服务

面向咖啡连锁外卖场景的后端服务（Go + PostgreSQL + Redis）。把**门店、店员、骑手、顾客、外卖平台客服、财务赔付**串到同一张订单时间线上，覆盖：

- 多外卖平台（饿了么 / 美团）接单：美式、拿铁、冷萃、果咖、轻食、组合套餐；录入订单时间、门店产能、制作配方、冰热属性、糖度、杯型、封签码、骑手预计到店、顾客备注
- 接单即按配方扣减原料库存（杯型决定杯耗材、冰饮加冰与吸管、轻食/套餐扣餐具、每杯一封签）
- **门店爆单保护**：在制杯量超阈值自动拒单（`surge_full`），也可手动临时限制接单（`surge_paused`，可设时长）
- 门店按出杯顺序制作（队列按骑手 ETA 优先级排序，`FOR UPDATE SKIP LOCKED` 并发叫号）
- 店员扫描封签 → 生成封签码 → **热饮 / 冷饮 / 餐食自动分袋** → 全单封齐进入待取区
- 骑手到店核验：**订单号、袋数、冷热分袋、吸管、餐具**；未出杯禁止取走
- 全场景异常：骑手提前到店、饮品放置过久重做、顾客备注漏看、错杯、轻食缺货、骑手取错袋、平台取消订单、顾客漏液投诉
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
| 平台接单 | `POST /v1/platform/orders`（支持 `Idempotency-Key`）、`POST /v1/platform/orders/{id}/cancel` |
| 门店作业 | `GET /v1/staff/stores/{id}/queue`、`POST /v1/staff/stores/{id}/cups/next`、`POST /v1/staff/cups/{id}/start`、`POST /v1/staff/cups/{id}/seal`、`POST /v1/staff/cups/{id}/remake` |
| 缺货/下架 | `POST /v1/staff/orders/{id}/item-oos`、`POST /v1/staff/menu/{sku}/delist`、`GET /v1/stores/{id}/delistings`、`POST /v1/staff/inventory/restock` |
| 打印 | `GET /v1/staff/orders/{id}/cup-labels`、`GET /v1/staff/orders/{id}/bag-labels` |
| 爆单保护 | `POST /v1/staff/stores/{id}/surge` |
| 订单合并 | `POST /v1/staff/merge`、`GET /v1/merged-groups/{id}` |
| 骑手 | `POST /v1/rider/arrivals`、`GET /v1/rider/screen`、`GET /v1/rider/arrivals/{id}`、`POST /v1/rider/handoffs` |
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
- **骑手等待责任**：交接时计算等待时长；骑手早于自身 ETA 到店的提前量不计门店责任，
  超过 ETA 仍等不到杯的部分记为门店出杯慢（`store_fault_wait_seconds`）。
- **取消**：未做/制作中杯原料回库（`cancel_restock`），已封杯按报损处理；退款承担方由 `store_fault` 决定。
- **班组评分**：100 分起，慢单率（每 5% 扣 1）、错杯(-4)、漏吸管餐具(-2)、取错袋(-2)、饮品过久(-3)、漏备注(-2)、门店责任取消(-5)。

---

## 6. 已执行的验证结论

以下验证在交付环境中实际执行通过（日期 2026-09-29）：

1. `go vet ./...` 无告警；`go build ./cmd/server` 通过。
2. `go test ./...` 通过：含 8 个集成测试用例（自动建 `coffee_test` 库）与 OpenAPI 3.0 规范合法性/路径覆盖测试。
3. `scripts/e2e.sh` 端到端 **81/81 项断言全部通过**，覆盖 19 个场景段：
   健康检查、接单四杯生成、库存扣减（牛奶/封签/吸管/餐具数量逐一核对）、幂等重推、
   出杯队列与备注、四杯封签与 3 个冷热餐食分袋、杯贴袋贴、骑手提前到店与到店屏、
   未出杯拒交接、取错袋 409 与正确交接 200、漏吸管漏餐具识别、投诉受理与财务定责（含平台分摊）、
   过久重做报损与新杯回队、冷萃液缺货自动下架（`stock_shortage`→`item_delisted`）、
   补货后自动恢复上架、轻食接单后缺货单品退款下架、爆单手动暂停（`surge_paused`）与
   容量阈值（`surge_full`）、平台取消原料回库与退款归责、饿了么+美团多平台合并一次到店、
   门店/区域报表、班组评分扣分、督导复盘含重做成本、订单时间线含 platform/barista/rider/customer/finance 五类角色。
4. Swagger UI（`/swagger/`）、`/openapi.yaml`、内嵌静态资源均返回 200；`/healthz` 返回 PG/Redis 均 ok。
5. Dockerfile / docker-compose.yml 已提供，可 `docker compose up -d --build` 一键启动
   （交付沙箱无 Docker 守护进程，未在沙箱内实际构建；镜像构建仅依赖标准 `golang:1.22-alpine` 与 `alpine:3.19`，应用为 CGO_ENABLED=0 静态二进制）。

---

## 7. 工程结构

```
.
├── cmd/server/main.go          # 入口：读环境变量、连库、迁移、起 HTTP
├── internal/
│   ├── db/                     # pgxpool 连接 + 内嵌 SQL 迁移执行器
│   │   └── sql/                # 0001 建表 / 0002 种子数据（随二进制 embed）
│   └── app/
│       ├── server.go           # 路由、角色鉴权、登录
│       ├── orders.go           # 接单、配方库存、幂等、缺货下架、平台取消
│       ├── production.go       # 出杯队列、封签分袋、重做报损、轻食缺货、叫号
│       ├── rider.go            # 骑手到店、到店屏、核验交接与等待归责
│       ├── management.go       # 门店/库存/爆单/合并/杯贴袋贴/看板
│       ├── reports.go          # 投诉、财务定责、台账、门店/区域报表、评分、复盘
│       ├── models.go           # JSON 模型
│       ├── swagger.go          # 内嵌 Swagger UI + OpenAPI
│       └── web/                # openapi.yaml + swagger-ui-dist 静态资源
├── scripts/e2e.sh              # 端到端冒烟脚本（80 断言）
├── Dockerfile
└── docker-compose.yml
```
