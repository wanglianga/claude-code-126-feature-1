-- 咖啡连锁外卖出杯时效与骑手交接服务 - schema
-- 可重复执行（IF NOT EXISTS / ON CONFLICT）

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------- 组织与人员 ----------
CREATE TABLE IF NOT EXISTS stores (
    id              BIGSERIAL PRIMARY KEY,
    code            TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    region          TEXT NOT NULL,
    -- 产能：每 5 分钟可出杯数；爆单阈值=在制杯量上限
    capacity_per_5m INTEGER NOT NULL DEFAULT 20,
    max_active_cups INTEGER NOT NULL DEFAULT 24,
    stale_minutes   INTEGER NOT NULL DEFAULT 15,  -- 饮品在待取区放置多久算过久
    -- 爆单保护：手动临时限制接单
    surge_manual    BOOLEAN NOT NULL DEFAULT FALSE,
    surge_reason    TEXT,
    surge_until     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS accounts (
    id          BIGSERIAL PRIMARY KEY,
    login       TEXT NOT NULL UNIQUE,
    login_code  TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    -- admin / area_manager / supervisor / store_manager / barista / rider / platform_cs / finance / platform
    role        TEXT NOT NULL,
    store_id    BIGINT REFERENCES stores(id),
    region      TEXT,
    platform    TEXT,
    active      BOOLEAN NOT NULL DEFAULT TRUE
);

-- ---------- 商品 / 配方 / 原料 ----------
CREATE TABLE IF NOT EXISTS menu_items (
    id            BIGSERIAL PRIMARY KEY,
    sku           TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    -- americano / latte / cold_brew / fruit_coffee / light_food / combo
    category      TEXT NOT NULL,
    is_drink      BOOLEAN NOT NULL DEFAULT TRUE,
    includes_drink BOOLEAN NOT NULL DEFAULT FALSE, -- 套餐含饮品
    price         NUMERIC(10,2) NOT NULL,
    make_seconds  INTEGER NOT NULL DEFAULT 180,
    on_sale       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ingredients (
    id            BIGSERIAL PRIMARY KEY,
    sku           TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    unit          TEXT NOT NULL,
    cost_per_unit NUMERIC(10,4) NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS recipes (
    id              BIGSERIAL PRIMARY KEY,
    menu_item_id    BIGINT NOT NULL REFERENCES menu_items(id),
    ingredient_id   BIGINT NOT NULL REFERENCES ingredients(id),
    qty_per_cup     NUMERIC(10,3) NOT NULL,
    qty_if_ice      NUMERIC(10,3) NOT NULL DEFAULT 0, -- 冰饮额外消耗（如冰块）
    -- 适用杯型：NULL=不限杯型；否则 'M'/'L'/'M,L' 等，扣库存时按订单杯型匹配
    applicable_sizes TEXT,
    UNIQUE (menu_item_id, ingredient_id)
);

CREATE TABLE IF NOT EXISTS inventory (
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    ingredient_id BIGINT NOT NULL REFERENCES ingredients(id),
    qty           NUMERIC(12,3) NOT NULL DEFAULT 0,
    PRIMARY KEY (store_id, ingredient_id)
);

CREATE TABLE IF NOT EXISTS stock_movements (
    id            BIGSERIAL PRIMARY KEY,
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    ingredient_id BIGINT NOT NULL REFERENCES ingredients(id),
    delta         NUMERIC(12,3) NOT NULL,
    -- intake / remake / loss / restock / cancel_restock
    reason        TEXT NOT NULL,
    order_id      BIGINT,
    cup_id        BIGINT,
    staff_id      BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 门店级临时下架（缺货触发）
CREATE TABLE IF NOT EXISTS delistings (
    id            BIGSERIAL PRIMARY KEY,
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    menu_item_id  BIGINT NOT NULL REFERENCES menu_items(id),
    reason        TEXT NOT NULL,
    active        BOOLEAN NOT NULL DEFAULT TRUE,
    created_by    BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ
);

-- ---------- 订单 / 杯 / 袋 ----------
CREATE TABLE IF NOT EXISTS merged_groups (
    id          BIGSERIAL PRIMARY KEY,
    store_id    BIGINT NOT NULL REFERENCES stores(id),
    created_by  BIGINT,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
    id                 BIGSERIAL PRIMARY KEY,
    platform           TEXT NOT NULL,
    platform_order_no  TEXT NOT NULL,
    store_id           BIGINT NOT NULL REFERENCES stores(id),
    -- received / active / handed / canceled
    status             TEXT NOT NULL DEFAULT 'received',
    customer_id        TEXT,
    customer_note      TEXT,
    total_amount       NUMERIC(10,2) NOT NULL DEFAULT 0,
    promised_minutes   INTEGER NOT NULL DEFAULT 15,
    ordered_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    rider_eta          TIMESTAMPTZ,
    all_sealed_at      TIMESTAMPTZ,
    handed_at          TIMESTAMPTZ,
    canceled_at        TIMESTAMPTZ,
    cancel_reason      TEXT,
    cancel_store_fault BOOLEAN,
    merged_group_id    BIGINT REFERENCES merged_groups(id),
    created_by         BIGINT,
    UNIQUE (platform, platform_order_no)
);
CREATE INDEX IF NOT EXISTS idx_orders_store_status ON orders(store_id, status);

CREATE TABLE IF NOT EXISTS order_cups (
    id             BIGSERIAL PRIMARY KEY,
    order_id       BIGINT NOT NULL REFERENCES orders(id),
    menu_item_id   BIGINT NOT NULL REFERENCES menu_items(id),
    name_snapshot  TEXT NOT NULL,
    unit_price     NUMERIC(10,2) NOT NULL,
    temp           TEXT NOT NULL,         -- hot / ice
    sugar          TEXT NOT NULL,         -- 无糖/三分/五分/七分/全糖
    cup_size       TEXT NOT NULL,        -- M / L / XL
    cup_note       TEXT,
    seq            INTEGER NOT NULL,      -- 出杯顺序
    seal_code      TEXT UNIQUE,
    -- queued / making / sealed / voided
    status         TEXT NOT NULL DEFAULT 'queued',
    queued_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    sealed_at      TIMESTAMPTZ,
    voided_at      TIMESTAMPTZ,
    void_reason    TEXT,
    remake_of_cup_id BIGINT REFERENCES order_cups(id),
    remake_no      INTEGER NOT NULL DEFAULT 0,
    staff_start_id BIGINT,
    staff_seal_id  BIGINT
);
CREATE INDEX IF NOT EXISTS idx_cups_order ON order_cups(order_id);
CREATE INDEX IF NOT EXISTS idx_cups_status ON order_cups(status);

-- 每个物理袋一行（杯-袋映射），交接时按 distinct bag_code 聚合核验
CREATE TABLE IF NOT EXISTS order_bags (
    id        BIGSERIAL PRIMARY KEY,
    store_id  BIGINT NOT NULL REFERENCES stores(id),
    order_id  BIGINT NOT NULL REFERENCES orders(id),
    group_id  BIGINT REFERENCES merged_groups(id),
    bag_code  TEXT NOT NULL,
    kind      TEXT NOT NULL, -- hot / cold / food
    cup_id    BIGINT NOT NULL REFERENCES order_cups(id)
);

-- ---------- 骑手到店 / 交接 ----------
CREATE TABLE IF NOT EXISTS rider_arrivals (
    id               BIGSERIAL PRIMARY KEY,
    store_id         BIGINT NOT NULL REFERENCES stores(id),
    order_id         BIGINT NOT NULL REFERENCES orders(id),
    group_id         BIGINT REFERENCES merged_groups(id),
    rider_account_id BIGINT,
    rider_name       TEXT NOT NULL,
    platform         TEXT,
    arrived_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at         TIMESTAMPTZ,
    wait_seconds     INTEGER,
    store_fault_wait_seconds INTEGER NOT NULL DEFAULT 0,
    handed_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_arrivals_open ON rider_arrivals(order_id) WHERE handed_at IS NULL;

CREATE TABLE IF NOT EXISTS handoffs (
    id            BIGSERIAL PRIMARY KEY,
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    order_id      BIGINT NOT NULL REFERENCES orders(id),
    rider_name    TEXT NOT NULL,
    staff_id      BIGINT,
    passed        BOOLEAN NOT NULL,
    expect_bags   INTEGER, expect_hot INTEGER, expect_cold INTEGER, expect_food INTEGER,
    actual_bags   INTEGER, actual_hot INTEGER, actual_cold INTEGER,
    has_straw     BOOLEAN, has_utensil BOOLEAN,
    issues        JSONB NOT NULL DEFAULT '[]',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- 异常 / 报损 / 财务 ----------
-- rider_early / stale_drink / note_missed / wrong_cup / food_oos / wrong_bag /
-- missing_straw / missing_utensil / platform_cancel / leak_complaint
CREATE TABLE IF NOT EXISTS exceptions (
    id            BIGSERIAL PRIMARY KEY,
    order_id      BIGINT NOT NULL REFERENCES orders(id),
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    type          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'open', -- open / resolved
    responsibility TEXT NOT NULL DEFAULT 'pending', -- store / rider / platform / customer / pending / shared
    occurrences   INTEGER NOT NULL DEFAULT 1,
    detail        JSONB NOT NULL DEFAULT '{}',
    comp_amount   NUMERIC(10,2) NOT NULL DEFAULT 0, -- 顾客补偿
    cost_amount   NUMERIC(10,2) NOT NULL DEFAULT 0, -- 门店重做/报损成本
    created_by    BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ,
    resolved_by   BIGINT,
    resolution    TEXT
);
CREATE INDEX IF NOT EXISTS idx_exc_store ON exceptions(store_id, created_at);
CREATE INDEX IF NOT EXISTS idx_exc_order ON exceptions(order_id);

CREATE TABLE IF NOT EXISTS loss_reports (
    id            BIGSERIAL PRIMARY KEY,
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    order_id      BIGINT REFERENCES orders(id),
    cup_id        BIGINT REFERENCES order_cups(id),
    exception_id  BIGINT REFERENCES exceptions(id),
    reason        TEXT NOT NULL, -- stale / wrong_cup / note_missed / canceled / manual
    qty_cups      INTEGER NOT NULL DEFAULT 1,
    cost_amount   NUMERIC(10,2) NOT NULL,
    note          TEXT,
    staff_id      BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- revenue(+) / refund(-) / remake_cost(-) / loss(-) / customer_comp(-) / platform_comp(+)
CREATE TABLE IF NOT EXISTS finance_records (
    id            BIGSERIAL PRIMARY KEY,
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    order_id      BIGINT REFERENCES orders(id),
    exception_id  BIGINT REFERENCES exceptions(id),
    kind          TEXT NOT NULL,
    amount        NUMERIC(10,2) NOT NULL, -- 正=收入/流入，负=成本/赔付
    bearer        TEXT NOT NULL DEFAULT 'store', -- store / team / platform / rider
    period_date   DATE NOT NULL DEFAULT CURRENT_DATE,
    note          TEXT,
    created_by    BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_fin_store_date ON finance_records(store_id, period_date);

-- ---------- 时间线（把门店/店员/骑手/顾客/客服/财务串到同一订单）----------
CREATE TABLE IF NOT EXISTS order_events (
    id          BIGSERIAL PRIMARY KEY,
    order_id    BIGINT NOT NULL REFERENCES orders(id),
    event_type  TEXT NOT NULL,
    actor_role  TEXT,
    actor_id    BIGINT,
    actor_name  TEXT,
    detail      JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_events_order ON order_events(order_id, id);

-- ---------- 班组评分 / 复盘 ----------
CREATE TABLE IF NOT EXISTS team_score_snapshots (
    id          BIGSERIAL PRIMARY KEY,
    store_id    BIGINT NOT NULL REFERENCES stores(id),
    score_date  DATE NOT NULL,
    metrics     JSONB NOT NULL DEFAULT '{}',
    score       INTEGER NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (store_id, score_date)
);

CREATE TABLE IF NOT EXISTS store_reviews (
    id            BIGSERIAL PRIMARY KEY,
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    from_date     DATE NOT NULL,
    to_date       DATE NOT NULL,
    supervisor_id BIGINT,
    summary       TEXT NOT NULL,
    actions       JSONB NOT NULL DEFAULT '[]',
    score_snapshot INTEGER NOT NULL,
    remake_cost_total NUMERIC(10,2) NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
