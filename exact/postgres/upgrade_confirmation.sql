-- 仅升级已存在的日志表，部署方需备份并在专用可信 search_path 下显式执行。
-- 先升级再启动新版；不清理历史，不提供有损降级脚本，New 不执行此脚本。
BEGIN;
ALTER TABLE x402_tron_settlements
    ADD COLUMN IF NOT EXISTS confirmed_block_number BIGINT,
    ADD COLUMN IF NOT EXISTS confirmed_block_id TEXT,
    ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ;
ALTER TABLE x402_tron_settlements DROP CONSTRAINT IF EXISTS x402_tron_settlements_state_check;
ALTER TABLE x402_tron_settlements ADD CONSTRAINT x402_tron_settlements_state_check
    CHECK (state IN ('reserved', 'prepared', 'broadcast_unknown', 'pending', 'succeeded'));
ALTER TABLE x402_tron_settlements DROP CONSTRAINT IF EXISTS x402_tron_settlements_confirmation_check;
ALTER TABLE x402_tron_settlements ADD CONSTRAINT x402_tron_settlements_confirmation_check CHECK (
    (state = 'succeeded' AND confirmed_block_number IS NOT NULL AND confirmed_block_number > 0
        AND confirmed_block_id IS NOT NULL AND confirmed_block_id ~ '^[0-9a-f]{64}$'
        AND left(confirmed_block_id, 16) = lpad(to_hex(confirmed_block_number), 16, '0')
        AND confirmed_at IS NOT NULL) OR
    (state <> 'succeeded' AND confirmed_block_number IS NULL AND confirmed_block_id IS NULL AND confirmed_at IS NULL)
);
COMMIT;
