package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("record not found")

type Postgres struct {
	pool *pgxpool.Pool
}

type AccessRecord struct {
	TenantID   string
	TenantName string
	APIKeyID   string
	Policy     *policy.Compiled
}

func NewPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}

	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() {
	p.pool.Close()
}

func (p *Postgres) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

func (p *Postgres) LookupAPIKey(ctx context.Context, rawAPIKey string) (AccessRecord, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT
			t.id,
			t.name,
			t.is_active,
			t.policy,
			k.id,
			k.is_active
		FROM api_keys AS k
		JOIN tenants AS t ON t.id = k.tenant_id
		WHERE k.key_hash = $1
		LIMIT 1
	`, HashAPIKey(rawAPIKey))

	var (
		record       AccessRecord
		tenantActive bool
		keyActive    bool
		policyJSON   []byte
	)

	if err := row.Scan(&record.TenantID, &record.TenantName, &tenantActive, &policyJSON, &record.APIKeyID, &keyActive); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AccessRecord{}, ErrNotFound
		}
		return AccessRecord{}, err
	}

	if !tenantActive || !keyActive {
		return AccessRecord{}, ErrNotFound
	}

	var doc policy.Document
	if err := json.Unmarshal(policyJSON, &doc); err != nil {
		return AccessRecord{}, fmt.Errorf("failed to parse policy JSON: %w", err)
	}

	compiled, err := policy.Compile(doc)
	if err != nil {
		return AccessRecord{}, fmt.Errorf("failed to compile policy: %w", err)
	}

	record.Policy = compiled
	return record, nil
}

func GenerateAPIKey() (string, string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}

	raw := "avxk_" + base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashAPIKey(raw), nil
}

func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}
