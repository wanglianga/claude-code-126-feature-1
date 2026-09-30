-- 骑手提前到店等待责任拆分与争议复核
-- 1) 到店快照：排队杯量/排队位序/爆单保护状态；离店再到店
-- 2) 可配置阈值（制作超时阈值/早到宽限/等待补贴单价）
-- 3) 等待三段归因：平台派单过早 / 门店制作超时 / 骑手提前到店（保留每次归因版本）
-- 4) 店长申诉 + 区域经理复核改判（原始归因与改判依据全保留）

-- ---------- 订单扩展：平台实际派单时间（改配/重派以 order_events 留痕） ----------
ALTER TABLE orders ADD COLUMN IF NOT EXISTS dispatched_at TIMESTAMPTZ;

-- ---------- 到店记录扩展 ----------
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS left_at TIMESTAMPTZ;
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS queue_cups INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS queue_position INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS surge_snapshot JSONB NOT NULL DEFAULT '{}';
-- 最近一次归因结果投影：{rider_arrive_early,platform_dispatch_early,store_make_timeout,waived}(秒)
ALTER TABLE rider_arrivals ADD COLUMN IF NOT EXISTS wait_split JSONB NOT NULL DEFAULT '{}';

-- ---------- 可配置阈值（门店级，单行） ----------
CREATE TABLE IF NOT EXISTS wait_thresholds (
    store_id               BIGINT PRIMARY KEY REFERENCES stores(id),
    -- 门店制作超时阈值：从实际派单到出杯允许的制作秒数，超过部分计门店制作超时
    make_target_seconds    INTEGER NOT NULL DEFAULT 900,
    -- 骑手早到宽限：早于 ETA 该秒数内不计骑手责任
    early_grace_seconds    INTEGER NOT NULL DEFAULT 0,
    -- 骑手等待补贴单价（元/分钟）：非骑手责任等待按此补贴，由平台/门店按归因承担
    subsidy_per_minute     NUMERIC(10,2) NOT NULL DEFAULT 0.50,
    updated_by             BIGINT,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO wait_thresholds(store_id)
SELECT id FROM stores
ON CONFLICT (store_id) DO NOTHING;

-- ---------- 等待归因版本（每次重算一行，历史全保留） ----------
CREATE TABLE IF NOT EXISTS wait_attribution_runs (
    id            BIGSERIAL PRIMARY KEY,
    order_id      BIGINT NOT NULL REFERENCES orders(id),
    version       INTEGER NOT NULL,
    -- handoff / dispatched / redispatched / rider_left / rider_returned / remade / ready / canceled / review_overturn
    reason        TEXT NOT NULL,
    -- 计算依据快照：派单/ETA 时间线、到店离店区间、开做/出杯/封签/交接、排队杯量、爆单状态、阈值
    basis         JSONB NOT NULL DEFAULT '{}',
    appeal_id     BIGINT,
    created_by    BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (order_id, version)
);
CREATE INDEX IF NOT EXISTS idx_wait_runs_order ON wait_attribution_runs(order_id, version DESC);

-- ---------- 等待时段拆分（一个版本多段，按责任方不重叠、不重复计时） ----------
CREATE TABLE IF NOT EXISTS wait_segments (
    id            BIGSERIAL PRIMARY KEY,
    run_id        BIGINT NOT NULL REFERENCES wait_attribution_runs(id),
    order_id      BIGINT NOT NULL REFERENCES orders(id),
    arrival_id    BIGINT REFERENCES rider_arrivals(id),
    segment_no    INTEGER NOT NULL,
    -- rider_arrive_early / platform_dispatch_early / store_make_timeout / waived
    segment_type  TEXT NOT NULL,
    -- rider / platform / store / waived
    party         TEXT NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    ended_at      TIMESTAMPTZ NOT NULL,
    seconds       INTEGER NOT NULL,
    note          TEXT
);
CREATE INDEX IF NOT EXISTS idx_wait_seg_order ON wait_segments(order_id);
CREATE INDEX IF NOT EXISTS idx_wait_seg_run ON wait_segments(run_id);

-- ---------- 等待责任申诉 / 复核 ----------
CREATE TABLE IF NOT EXISTS wait_appeals (
    id               BIGSERIAL PRIMARY KEY,
    order_id         BIGINT NOT NULL REFERENCES orders(id),
    store_id         BIGINT NOT NULL REFERENCES stores(id),
    arrival_id       BIGINT REFERENCES rider_arrivals(id),
    -- machine_fault 机器故障 / temp_shortage 临时缺货 / staff_shortage 人手不足 / surge_knockon 爆单连锁 / other
    reason_code      TEXT NOT NULL,
    reason_detail    TEXT NOT NULL,
    evidence         JSONB NOT NULL DEFAULT '{}',
    -- 申诉针对的时间窗（可空=整单等待）
    window_from      TIMESTAMPTZ,
    window_to        TIMESTAMPTZ,
    -- pending / approved / rejected
    status           TEXT NOT NULL DEFAULT 'pending',
    submitted_by     BIGINT NOT NULL,
    submitted_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_by      BIGINT,
    reviewed_at      TIMESTAMPTZ,
    review_note      TEXT,
    -- 复核改判：把窗口内门店制作超时秒数改判给 platform / rider / waived
    move_to_party    TEXT,
    -- 订单已取消/已交接时，仅调整绩效与结算，不得改写交接事实
    settlement_only  BOOLEAN NOT NULL DEFAULT FALSE,
    original_run_id  BIGINT REFERENCES wait_attribution_runs(id),
    effective_run_id BIGINT REFERENCES wait_attribution_runs(id)
);
CREATE INDEX IF NOT EXISTS idx_wait_appeals_store ON wait_appeals(store_id, status);
CREATE INDEX IF NOT EXISTS idx_wait_appeals_order ON wait_appeals(order_id);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='fk_wait_runs_appeal') THEN
        ALTER TABLE wait_attribution_runs
            ADD CONSTRAINT fk_wait_runs_appeal FOREIGN KEY (appeal_id) REFERENCES wait_appeals(id);
    END IF;
END $$;

-- 骑手等待补贴结算：kind='rider_wait_subsidy'（负=平台/门店赔付骑手，正=改判冲回），
-- 通过 run_id 关联归因版本，复核改判时按版本整组冲回重算，避免重复结算。
ALTER TABLE finance_records ADD COLUMN IF NOT EXISTS run_id BIGINT REFERENCES wait_attribution_runs(id);
CREATE INDEX IF NOT EXISTS idx_fin_run ON finance_records(run_id) WHERE run_id IS NOT NULL;
