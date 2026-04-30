INSERT INTO tenants (id, name, policy)
VALUES (
    'bombard-app',
    'Bombard Demo App',
    '{
      "allowedCidrs": ["127.0.0.1/32"],
      "allowedMethods": [
        "eth_chainId",
        "net_version",
        "eth_blockNumber",
        "eth_getBalance",
        "eth_getTransactionCount",
        "eth_getBlockByNumber",
        "eth_call",
        "eth_estimateGas",
        "eth_sendRawTransaction"
      ],
      "allowContractCreation": false,
      "maxGasLimit": 120000,
      "maxValueWei": "100000000000000000000",
      "requestsPerMinute": 500000
    }'::jsonb
)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    policy = EXCLUDED.policy,
    updated_at = NOW();

INSERT INTO api_keys (id, tenant_id, label, key_hash)
VALUES (
    'bombard-key',
    'bombard-app',
    'Local bombard key',
    'REPLACE_WITH_SHA256_HASH'
)
ON CONFLICT (id) DO UPDATE SET
    key_hash = EXCLUDED.key_hash,
    is_active = TRUE;
