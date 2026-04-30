INSERT INTO tenants (id, name, policy)
VALUES (
    'demo-app',
    'Demo App',
    '{
      "allowedCidrs": ["127.0.0.1/32"],
      "allowedMethods": [
        "eth_chainId",
        "eth_blockNumber",
        "eth_getBalance",
        "eth_call",
        "eth_estimateGas",
        "eth_sendRawTransaction"
      ],
      "allowedFromAddresses": [
        "0x8db97C7ceCe249c2b98bdC0226Cc4C2A57BF52FC"
      ],
      "allowedToAddresses": [
        "0xB0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5B0B5"
      ],
      "allowedFunctionSelectors": [
        "0xa9059cbb"
      ],
      "allowContractCreation": false,
      "maxGasLimit": 120000,
      "maxValueWei": "0",
      "requestsPerMinute": 600
    }'::jsonb
)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    policy = EXCLUDED.policy,
    updated_at = NOW();

INSERT INTO api_keys (id, tenant_id, label, key_hash)
VALUES (
    'demo-key',
    'demo-app',
    'Local demo key',
    'REPLACE_WITH_SHA256_HASH'
)
ON CONFLICT (id) DO UPDATE SET
    key_hash = EXCLUDED.key_hash,
    is_active = TRUE;
