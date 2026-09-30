-- 骑手提前到店等待：派单事件、等待三段归因、申诉复核、可配置阈值、结算冲销
-- 可重复执行（IF NOT EXISTS / ADD COLUMN IF NOT EXISTS）

-- ---------- 平台派单 / 改配事件 ----------
CREATE TABLE IF NOT EXISTS dispatch_events (
    id               BIGSERIAL PRIMARY KEY,
    order_id         BIGINT NOT NULL REFERENCES orders(id),
    platform         TEXT NOT NULL,
    seq              INTEGER NOT NULL,            -- 同一订单第几次派单（1 起）
    event_type       TEXT NOT NULL,               -- dispatched(首次派单) / redispatch(改配)
    rider_account_id BIGINT,
    rider_name       TEXT NOT NULL,
    eta              TIMESTAMPTZ,                 -- 派单时给骑手的预计到店时间
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by       BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (order_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_dispatch_order ON dispatch_events(order_id, id);

-- ---------- 骑手到店记录增列：离店再到店 / 到店时排队杯量 / 爆单快照 ----------
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS arrival_no       INTEGER NOT NULL DEFAULT 1;
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS left_at         TIMESTAMPTZ;
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS queue_cups      INTEGER;       -- 到店时全店排队+制作中杯量
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS queue_ahead_cups INTEGER;      -- 本单当时尚未出杯的杯量
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS surge_active    BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS surge_snapshot  TEXT;          -- none / surge_paused / surge_full
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS dispatch_id     BIGINT REFERENCES dispatch_events(id);
CREATE INDEX IF NOT EXISTS idx_arrivals_order_no ON rider_arrivals(order_id, arrival_no);

-- ---------- 等待归因阈值（store_id=0 为全局默认；门店行覆盖全局） ----------
CREATE TABLE IF NOT EXISTS wait_thresholds (
    store_id                         BIGINT PRIMARY KEY,
    -- 平台派单后应给门店预留的最短制作前置秒数（派单时刻 + 该阈值 > 骑手应到时刻 => 派单过早）
    platform_dispatch_lead_seconds   INTEGER NOT NULL DEFAULT 600,
    -- 骑手应到时间之后门店出杯宽限秒数，宽限内不计门店制作超时
    store_make_grace_seconds         INTEGER NOT NULL DEFAULT 60,
    -- 骑手早于 ETA 的宽限秒数，该值以内不记骑手提前到店
    rider_early_grace_seconds        INTEGER NOT NULL DEFAULT 60,
    -- 骑手等待补贴单价（元/分钟），按秒折算
    rider_subsidy_per_minute         NUMERIC(8,2) NOT NULL DEFAULT 0.50,
    -- 班组评分：每累计多少秒门店责任等待扣 1 分
    store_wait_score_seconds         INTEGER NOT NULL DEFAULT 300,
    updated_by                       BIGINT,
    updated_at                       TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO wait_thresholds(store_id) VALUES (0)
ON CONFLICT (store_id) DO NOTHING;

-- ---------- 等待归因版本（每次归因一行；新版本生效时旧版本置 superseded，永久保留原始归因） ----------
CREATE TABLE IF NOT EXISTS wait_attributions (
    id                    BIGSERIAL PRIMARY KEY,
    order_id              BIGINT NOT NULL REFERENCES orders(id),
    store_id              BIGINT NOT NULL REFERENCES stores(id),
    version_no            INTEGER NOT NULL,
    status                TEXT NOT NULL DEFAULT 'active',  -- active / superseded
    -- initial(交接/取消首次) / manual(重算) / review(申诉复核改判)
    reason                TEXT NOT NULL DEFAULT 'initial',
    end_reason            TEXT NOT NULL DEFAULT 'handed',  -- handed / canceled / provisional
    total_wait_seconds    INTEGER NOT NULL DEFAULT 0,
    platform_seconds      INTEGER NOT NULL DEFAULT 0,      -- 平台派单过早
    store_seconds         INTEGER NOT NULL DEFAULT 0,      -- 门店制作超时
    rider_seconds         INTEGER NOT NULL DEFAULT 0,      -- 骑手提前到店
    exempted_seconds      INTEGER NOT NULL DEFAULT 0,      -- 复核改判豁免（原门店责任，转平台承担）
    grace_seconds         INTEGER NOT NULL DEFAULT 0,      -- 门店宽限（不归任何一方）
    rider_subsidy_total   NUMERIC(10,2) NOT NULL DEFAULT 0,
    rider_subsidy_store   NUMERIC(10,2) NOT NULL DEFAULT 0,
    rider_subsidy_platform NUMERIC(10,2) NOT NULL DEFAULT 0,
    params                JSONB NOT NULL DEFAULT '{}',     -- 计算时阈值快照
    timeline              JSONB NOT NULL DEFAULT '{}',     -- 计算时时间线锚点快照
    review_id             BIGINT,
    created_by            BIGINT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (order_id, version_no)
);
CREATE INDEX IF NOT EXISTS idx_waitattr_order ON wait_attributions(order_id, id);
CREATE INDEX IF NOT EXISTS idx_waitattr_active ON wait_attributions(store_id, status) WHERE status='active';

-- ---------- 等待拆分明细（段间互不重叠；挂在归因版本下，不可变） ----------
CREATE TABLE IF NOT EXISTS wait_segments (
    id             BIGSERIAL PRIMARY KEY,
    attribution_id BIGINT NOT NULL REFERENCES wait_attributions(id),
    order_id       BIGINT NOT NULL REFERENCES orders(id),
    store_id       BIGINT NOT NULL REFERENCES stores(id),
    arrival_id     BIGINT REFERENCES rider_arrivals(id),
    -- platform_early_dispatch / store_make_timeout / rider_early_arrival / store_exempted
    segment_type   TEXT NOT NULL,
    start_time     TIMESTAMPTZ NOT NULL,
    end_time       TIMESTAMPTZ NOT NULL,
    seconds        INTEGER NOT NULL,
    queue_cups     INTEGER,
    surge_active   BOOLEAN NOT NULL DEFAULT FALSE,
    rider_name     TEXT,
    note           TEXT
);
CREATE INDEX IF NOT EXISTS idx_waitseg_attr ON wait_segments(attribution_id);
CREATE INDEX IF NOT EXISTS idx_waitseg_order ON wait_segments(order_id);
CREATE INDEX IF NOT EXISTS idx_waitseg_store_type ON wait_segments(store_id, segment_type);

-- ---------- 店长申诉 / 区域经理复核 ----------
CREATE TABLE IF NOT EXISTS wait_reviews (
    id                   BIGSERIAL PRIMARY KEY,
    order_id             BIGINT NOT NULL REFERENCES orders(id),
    store_id             BIGINT NOT NULL REFERENCES stores(id),
    status               TEXT NOT NULL DEFAULT 'pending', -- pending / approved / rejected
    reason_code          TEXT NOT NULL,                   -- machine_fault / temp_shortage / surge_force_majeure / other
    reason_detail        TEXT NOT NULL,
    evidence             JSONB NOT NULL DEFAULT '{}',
    requested_by         BIGINT NOT NULL,
    requested_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_by          BIGINT,
    reviewed_at          TIMESTAMPTZ,
    decision             TEXT,                            -- approve / reject
    review_note          TEXT,
    attribution_before   BIGINT,
    attribution_after    BIGINT,
    before_metrics       JSONB NOT NULL DEFAULT '{}',
    after_metrics        JSONB NOT NULL DEFAULT '{}',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_waitreview_status ON wait_reviews(status, id);
CREATE INDEX IF NOT EXISTS idx_waitreview_order ON wait_reviews(order_id, id);
CREATE INDEX IF NOT EXISTS idx_waitreview_store ON wait_reviews(store_id, id);

-- ---------- 财务流水增列：归因关联 / 红冲 / 复核 ----------
ALTER TABLE finance_records ADD COLUMN IF NOT EXISTS attribution_id BIGINT REFERENCES wait_attributions(id);
ALTER TABLE finance_records ADD COLUMN IF NOT EXISTS reversal_of    BIGINT REFERENCES finance_records(id);
ALTER TABLE finance_records ADD COLUMN IF NOT EXISTS review_id      BIGINT REFERENCES wait_reviews(id);
CREATE INDEX IF NOT EXISTS idx_fin_attribution ON finance_records(attribution_id);
CREATE INDEX IF NOT EXISTS idx_fin_reversal ON finance_records(reversal_of);
