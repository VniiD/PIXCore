// Package config carrega e valida a configuração (falha rápida; segredos via arquivo).
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env         string // production | development
	LogLevel    string
	DatabaseURL string
	MigrateURL  string

	GRPCAddr string
	OpsAddr  string

	TLSCert, TLSKey, TLSClientCA string
	ACLFile                      string
	RatePerSecond                float64
	RateBurst                    int

	Currencies []string

	KafkaBrokers      []string
	KafkaTLSCA        string
	KafkaSASLUser     string
	KafkaSASLPassword string
	KafkaGroup        string
	KafkaDLQTopic     string

	HMACSigningID    string
	HMACKeys         map[string][]byte
	RequireSignature bool

	VerifyInterval time.Duration
	MaxHoldAge     time.Duration
	RetentionAge   time.Duration
}

// secret lê VAR ou, preferencialmente, o conteúdo do arquivo apontado por VAR_FILE.
func secret(name string) (string, error) {
	if f := os.Getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f) // #nosec G304 G703 -- caminho vem da configuração do operador, não de entrada externa
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(name), nil
}

func get(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func dur(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid duration %q", name, v)
	}
	return d, nil
}

func Load() (Config, error) {
	var c Config
	var err error
	c.Env = get("LEDGER_ENV", "production")
	c.LogLevel = get("LEDGER_LOG_LEVEL", "info")
	if c.DatabaseURL, err = secret("LEDGER_DATABASE_URL"); err != nil {
		return c, err
	}
	if c.MigrateURL, err = secret("LEDGER_MIGRATE_DATABASE_URL"); err != nil {
		return c, err
	}
	c.GRPCAddr = get("LEDGER_GRPC_ADDR", ":9443")
	c.OpsAddr = get("LEDGER_OPS_ADDR", "127.0.0.1:9090")
	c.TLSCert, c.TLSKey, c.TLSClientCA = os.Getenv("LEDGER_TLS_CERT_FILE"), os.Getenv("LEDGER_TLS_KEY_FILE"), os.Getenv("LEDGER_TLS_CLIENT_CA_FILE")
	c.ACLFile = os.Getenv("LEDGER_ACL_FILE")
	c.RatePerSecond, c.RateBurst = 500, 1000
	if v := os.Getenv("LEDGER_RATE_PER_SECOND"); v != "" {
		if c.RatePerSecond, err = strconv.ParseFloat(v, 64); err != nil || c.RatePerSecond <= 0 {
			return c, errors.New("LEDGER_RATE_PER_SECOND invalid")
		}
		c.RateBurst = int(c.RatePerSecond) * 2
	}
	for _, cur := range strings.Split(get("LEDGER_CURRENCIES", "BRL"), ",") {
		if cur = strings.TrimSpace(cur); cur != "" {
			c.Currencies = append(c.Currencies, cur)
		}
	}
	for _, b := range strings.Split(os.Getenv("LEDGER_KAFKA_BROKERS"), ",") {
		if b = strings.TrimSpace(b); b != "" {
			c.KafkaBrokers = append(c.KafkaBrokers, b)
		}
	}
	c.KafkaTLSCA = os.Getenv("LEDGER_KAFKA_TLS_CA_FILE")
	c.KafkaSASLUser = os.Getenv("LEDGER_KAFKA_SASL_USER")
	if c.KafkaSASLPassword, err = secret("LEDGER_KAFKA_SASL_PASSWORD"); err != nil {
		return c, err
	}
	c.KafkaGroup = get("LEDGER_KAFKA_GROUP", "ledger-service")
	c.KafkaDLQTopic = get("LEDGER_KAFKA_DLQ_TOPIC", "pixcore.ledger.commands.v1.dlq")
	c.RequireSignature = get("LEDGER_REQUIRE_SIGNATURE", "true") == "true"

	if f := os.Getenv("LEDGER_HMAC_KEYS_FILE"); f != "" {
		b, err := os.ReadFile(f) // #nosec G304 G703 -- caminho vem da configuração do operador, não de entrada externa
		if err != nil {
			return c, fmt.Errorf("LEDGER_HMAC_KEYS_FILE: %w", err)
		}
		var kf struct {
			SigningKeyID string            `json:"signing_key_id"`
			Keys         map[string]string `json:"keys"`
		}
		if err := json.Unmarshal(b, &kf); err != nil {
			return c, fmt.Errorf("HMAC keys file: %w", err)
		}
		c.HMACSigningID, c.HMACKeys = kf.SigningKeyID, map[string][]byte{}
		for id, v := range kf.Keys {
			raw, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return c, fmt.Errorf("HMAC key %q: not base64", id)
			}
			c.HMACKeys[id] = raw
		}
	}

	if c.VerifyInterval, err = dur("LEDGER_VERIFY_INTERVAL", time.Minute); err != nil {
		return c, err
	}
	if c.MaxHoldAge, err = dur("LEDGER_MAX_HOLD_AGE", 24*time.Hour); err != nil {
		return c, err
	}
	if c.RetentionAge, err = dur("LEDGER_RETENTION_AGE", 7*24*time.Hour); err != nil {
		return c, err
	}
	return c, nil
}

func sslModeOf(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return u.Query().Get("sslmode")
}

// Validate aplica as regras de segurança; em produção nada de transporte inseguro.
func (c Config) Validate(forServe bool) error {
	if c.Env != "production" && c.Env != "development" {
		return errors.New("LEDGER_ENV must be production or development")
	}
	if len(c.Currencies) == 0 {
		return errors.New("LEDGER_CURRENCIES is empty")
	}
	prod := c.Env == "production"
	check := func(name, dsn string) error {
		if dsn == "" {
			return fmt.Errorf("%s is required", name)
		}
		if prod {
			if m := sslModeOf(dsn); m != "verify-full" && m != "verify-ca" {
				return fmt.Errorf("%s: sslmode must be verify-full or verify-ca in production", name)
			}
		}
		return nil
	}
	if !forServe {
		return check("LEDGER_MIGRATE_DATABASE_URL", c.MigrateURL)
	}
	if err := check("LEDGER_DATABASE_URL", c.DatabaseURL); err != nil {
		return err
	}
	if c.TLSCert == "" || c.TLSKey == "" || c.TLSClientCA == "" {
		return errors.New("mTLS is mandatory: set LEDGER_TLS_CERT_FILE, LEDGER_TLS_KEY_FILE and LEDGER_TLS_CLIENT_CA_FILE")
	}
	if c.ACLFile == "" {
		return errors.New("LEDGER_ACL_FILE is mandatory")
	}
	if len(c.KafkaBrokers) > 0 {
		if prod && c.KafkaTLSCA == "" {
			return errors.New("LEDGER_KAFKA_TLS_CA_FILE is required in production")
		}
		if prod && (!c.RequireSignature || len(c.HMACKeys) == 0 || c.HMACSigningID == "") {
			return errors.New("HMAC message signing (LEDGER_HMAC_KEYS_FILE) is required in production when messaging is enabled")
		}
	}
	return nil
}
