// Package labdb is the lab API's database (docs/lab-api/environments.md
// §2, §5): the environments the lab serves, each with its token, its config
// and its kit — the credentials its platform puts in its .env — and each
// one's merchant registry. SQLite in one file on the lab API's volume,
// through a pure-Go driver so the image stays static.
//
// The default environment is not a row: it is the lab as it always was,
// with its fixed credentials and its registry.json.
package labdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is an environment the lab does not have.
var ErrNotFound = errors.New("labdb: no such environment")

// ErrExists is an environment id already taken.
var ErrExists = errors.New("labdb: environment already exists")

// Config is what an environment is set to: the knobs a deployment used to
// set lab-wide (docs/lab-api/environments.md §5). Empty is the defaults.
type Config struct {
	// SettlementIdentifier names Worldline's settlement files for this
	// environment (WORLDLINE_SETTLEMENT_IDENTIFIER).
	SettlementIdentifier string `json:"settlement_identifier,omitempty"`
	// WebhookHosts are the platform's callback hosts its subscriptions and
	// callbacks may reach.
	WebhookHosts []string `json:"webhook_hosts,omitempty"`
	// ACIWebhookURL is where ACI delivers this environment's webhooks.
	ACIWebhookURL string `json:"aci_webhook_url,omitempty"`
	// B4BCallbackURL is where B4B calls back a record that names no
	// callback_url of its own.
	B4BCallbackURL string `json:"b4b_callback_url,omitempty"`
	// Clock is "follows" or "wall" (§8): a seen "wall" environment blocks
	// moving the lab clock.
	Clock string `json:"clock,omitempty"`
	// PollURL and PollToken are the platform's own Worldline poll
	// (plan.md, phase 6): the lab POSTs {"fileType"} there, with the token
	// as a bearer, when the card's *Poll now* is pressed.
	PollURL   string `json:"poll_url,omitempty"`
	PollToken string `json:"poll_token,omitempty"`
	// Databases are the platform's own databases, by the names the lab's
	// seeder asks for ("platform", "settle"): the lab seeds them
	// (plan.md, phase 6.1). Connection URLs with their credentials, so a
	// view never shows them back.
	Databases map[string]string `json:"databases,omitempty"`
	// Tunnel is the SSH jump host the lab reaches those databases through
	// when they are not reachable from the lab itself: nil connects to the
	// URLs as they are.
	Tunnel *Tunnel `json:"tunnel,omitempty"`
}

// Tunnel is an SSH jump host. For each seeder call the lab connects to it
// and forwards a port of its own to every database the call hands over,
// so the seeder is given the database's URL with the forward's address in
// it. The private key, its passphrase and the password are secrets: a view
// never shows them back.
type Tunnel struct {
	Host string `json:"host"`
	// Port is 22 when left out.
	Port int    `json:"port,omitempty"`
	User string `json:"user"`
	// PrivateKey is a PEM private key (OpenSSH, PKCS#1 or PKCS#8);
	// Passphrase decrypts it when it is encrypted.
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	// Password is tried as well as the key, or alone.
	Password string `json:"password,omitempty"`
	// HostKey pins the jump host's key, as ssh-keygen -lf shows it
	// (SHA256:…). Empty accepts any key, and the lab reports the one it met.
	HostKey string `json:"host_key,omitempty"`
}

// Kit is an environment's credentials at each vendor, made when it is
// created. The lab API hands them out and never uses them.
type Kit struct {
	BankingCircleUsername string `json:"banking_circle_username"`
	BankingCirclePassword string `json:"banking_circle_password"`
	B4BKeyID              string `json:"b4b_key_id"`
	B4BPrivateKeyPEM      string `json:"b4b_private_key_pem"`
	B4BPublicKeyPEM       string `json:"b4b_public_key_pem"`
	SFTPUsername          string `json:"sftp_username"`
	SFTPPassword          string `json:"sftp_password"`
	VerificationUsername  string `json:"verification_username"`
	VerificationSecret    string `json:"verification_secret"`
	VerifyKey             string `json:"verify_key"`
}

// Environment is one row.
type Environment struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Token     string    `json:"-"`
	Config    Config    `json:"config"`
	Kit       Kit       `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// DB is the lab API's database.
type DB struct{ sql *sql.DB }

// Open opens (creating) the database at path. ":memory:" is a database
// that lives as long as the process, for tests.
func Open(path string) (*DB, error) {
	dsn := path
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("labdb: %w", err)
		}
		dsn = "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	s, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("labdb: open %s: %w", path, err)
	}
	// One connection: SQLite serialises writers anyway, and an in-memory
	// database is per connection.
	s.SetMaxOpenConns(1)
	db := &DB{sql: s}
	if err := db.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return db, nil
}

// Close closes it.
func (db *DB) Close() error { return db.sql.Close() }

func (db *DB) migrate() error {
	_, err := db.sql.Exec(`
CREATE TABLE IF NOT EXISTS environments (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	token      TEXT NOT NULL UNIQUE,
	config     TEXT NOT NULL DEFAULT '{}',
	kit        TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS default_config (
	id     INTEGER PRIMARY KEY CHECK (id = 1),
	config TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS registries (
	env TEXT PRIMARY KEY REFERENCES environments(id) ON DELETE CASCADE,
	doc BLOB NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("labdb: migrate: %w", err)
	}
	return nil
}

// Create adds an environment.
func (db *DB) Create(ctx context.Context, e Environment) error {
	cfg, kit, err := encode(e)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO environments (id, name, token, config, kit, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.Name, e.Token, cfg, kit, e.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrExists
	}
	return err
}

// Update writes an environment's name, token and config back.
func (db *DB) Update(ctx context.Context, e Environment) error {
	cfg, kit, err := encode(e)
	if err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE environments SET name = ?, token = ?, config = ?, kit = ? WHERE id = ?`,
		e.Name, e.Token, cfg, kit, e.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes an environment and its registry.
func (db *DB) Delete(ctx context.Context, id string) error {
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM registries WHERE env = ?`, id); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Get is one environment.
func (db *DB) Get(ctx context.Context, id string) (Environment, error) {
	return db.one(ctx, `WHERE id = ?`, id)
}

// ByToken is the environment a token belongs to.
func (db *DB) ByToken(ctx context.Context, token string) (Environment, error) {
	return db.one(ctx, `WHERE token = ?`, token)
}

// List is every environment, oldest first.
func (db *DB) List(ctx context.Context) ([]Environment, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT id, name, token, config, kit, created_at FROM environments ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DefaultConfig is the default environment's config: what can be set for
// it at all (its poll), empty until someone does.
func (db *DB) DefaultConfig(ctx context.Context) (Config, error) {
	var raw string
	err := db.sql.QueryRowContext(ctx, `SELECT config FROM default_config WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, fmt.Errorf("labdb: the default's config: %w", err)
	}
	return c, nil
}

// SaveDefaultConfig writes the default environment's config.
func (db *DB) SaveDefaultConfig(ctx context.Context, c Config) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO default_config (id, config) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET config = excluded.config`, string(b))
	return err
}

// Registry is env's merchant registry document, nil when it has none yet.
func (db *DB) Registry(ctx context.Context, env string) ([]byte, error) {
	var doc []byte
	err := db.sql.QueryRowContext(ctx, `SELECT doc FROM registries WHERE env = ?`, env).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return doc, err
}

// SaveRegistry writes env's merchant registry document.
func (db *DB) SaveRegistry(ctx context.Context, env string, doc []byte) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO registries (env, doc) VALUES (?, ?) ON CONFLICT(env) DO UPDATE SET doc = excluded.doc`, env, doc)
	return err
}

func (db *DB) one(ctx context.Context, where string, arg any) (Environment, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT id, name, token, config, kit, created_at FROM environments `+where, arg)
	e, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Environment{}, ErrNotFound
	}
	return e, err
}

type scanner interface{ Scan(dest ...any) error }

func scan(s scanner) (Environment, error) {
	var e Environment
	var cfg, kit, created string
	if err := s.Scan(&e.ID, &e.Name, &e.Token, &cfg, &kit, &created); err != nil {
		return Environment{}, err
	}
	if err := json.Unmarshal([]byte(cfg), &e.Config); err != nil {
		return Environment{}, fmt.Errorf("labdb: %s's config: %w", e.ID, err)
	}
	if err := json.Unmarshal([]byte(kit), &e.Kit); err != nil {
		return Environment{}, fmt.Errorf("labdb: %s's kit: %w", e.ID, err)
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return e, nil
}

func encode(e Environment) (string, string, error) {
	cfg, err := json.Marshal(e.Config)
	if err != nil {
		return "", "", err
	}
	kit, err := json.Marshal(e.Kit)
	if err != nil {
		return "", "", err
	}
	return string(cfg), string(kit), nil
}
