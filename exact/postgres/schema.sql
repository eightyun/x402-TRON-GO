-- 由部署方在专用且可信的 search_path 下显式应用，不授予运行账号删表/删记录权限。
CREATE TABLE IF NOT EXISTS x402_tron_settlements (
    id TEXT PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
    network TEXT NOT NULL,
    permit2_address TEXT NOT NULL CHECK (permit2_address ~ '^41[0-9a-f]{40}$'),
    payer TEXT NOT NULL,
    nonce NUMERIC(78, 0) NOT NULL CHECK (nonce >= 0 AND nonce < power(2::NUMERIC, 256)),
    digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
    state TEXT NOT NULL DEFAULT 'reserved' CONSTRAINT x402_tron_settlements_state_check
        CHECK (state IN ('reserved', 'prepared', 'broadcast_unknown', 'pending', 'succeeded')),
    transaction_id TEXT CHECK (transaction_id ~ '^[0-9a-f]{64}$' AND transaction_id <> repeat('0', 64)),
    signed_payload BYTEA CHECK (octet_length(signed_payload) BETWEEN 1 AND 1048576),
    confirmed_block_number BIGINT,
    confirmed_block_id TEXT,
    confirmed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (network, permit2_address, payer, nonce),
    UNIQUE (network, transaction_id),
    CHECK (
        (state = 'reserved' AND transaction_id IS NULL AND signed_payload IS NULL) OR
        (state <> 'reserved' AND transaction_id IS NOT NULL AND signed_payload IS NOT NULL)
    ),
    CONSTRAINT x402_tron_settlements_confirmation_check CHECK (
        (state = 'succeeded' AND confirmed_block_number IS NOT NULL AND confirmed_block_number > 0
            AND confirmed_block_id IS NOT NULL AND confirmed_block_id ~ '^[0-9a-f]{64}$'
            AND left(confirmed_block_id, 16) = lpad(to_hex(confirmed_block_number), 16, '0')
            AND confirmed_at IS NOT NULL) OR
        (state <> 'succeeded' AND confirmed_block_number IS NULL AND confirmed_block_id IS NULL AND confirmed_at IS NULL)
    )
);
