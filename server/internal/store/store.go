// Package store holds all persistent state in a single SQLite database.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS users(
	email TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	picture TEXT NOT NULL DEFAULT '',
	role TEXT NOT NULL DEFAULT 'user',
	notify_pref TEXT NOT NULL DEFAULT 'mine',
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions(
	token_hash TEXT PRIMARY KEY,
	email TEXT NOT NULL REFERENCES users(email) ON DELETE CASCADE,
	expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS push_subscriptions(
	endpoint TEXT PRIMARY KEY,
	email TEXT NOT NULL REFERENCES users(email) ON DELETE CASCADE,
	p256dh TEXT NOT NULL,
	auth TEXT NOT NULL,
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS devices(
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL UNIQUE,
	created INTEGER NOT NULL,
	last_seen INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS pairing_codes(
	code TEXT PRIMARY KEY,
	created_by TEXT NOT NULL,
	expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS types(
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	sound_mime TEXT NOT NULL,
	hash TEXT NOT NULL,
	updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys(
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	key_hash TEXT NOT NULL UNIQUE,
	owner_email TEXT REFERENCES users(email) ON DELETE SET NULL,
	pinned_type INTEGER REFERENCES types(id) ON DELETE SET NULL,
	pinned_device INTEGER REFERENCES devices(id) ON DELETE SET NULL,
	created INTEGER NOT NULL,
	last_used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS alerts(
	id INTEGER PRIMARY KEY,
	device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	type_id INTEGER REFERENCES types(id) ON DELETE SET NULL,
	status TEXT NOT NULL,
	reply TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	resolved INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS alerts_device_status ON alerts(device_id, status);
CREATE INDEX IF NOT EXISTS alerts_created ON alerts(created);
CREATE TABLE IF NOT EXISTS alert_requests(
	id INTEGER PRIMARY KEY,
	alert_id INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
	requester_email TEXT,
	api_key_id INTEGER,
	requester_name TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS alert_requests_alert ON alert_requests(alert_id);
CREATE TABLE IF NOT EXISTS presets(
	position INTEGER PRIMARY KEY,
	text TEXT NOT NULL
);
`

// migrations upgrade a database created from schema; PRAGMA user_version counts how many ran.
var migrations = []string{
	`ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
	CREATE TABLE groups(
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE COLLATE NOCASE,
		created INTEGER NOT NULL
	);
	CREATE TABLE group_devices(
		group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
		device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
		PRIMARY KEY(group_id, device_id)
	);
	CREATE TABLE settings(
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,
}

var defaultPresets = []string{"Coming now", "5 min", "15 min", "Busy, later"}

// Open opens (creating if needed) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serialises writes anyway; one connection avoids SQLITE_BUSY and keeps :memory: shared.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM presets`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		if err := s.SetPresets(context.Background(), defaultPresets); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for ; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// ErrConflict is returned when a name is already taken.
var ErrConflict = errors.New("name already in use")

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func now() int64 { return time.Now().Unix() }

// HashToken returns the hex sha256 of a secret; only hashes are stored.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

var b32 = base32.NewEncoding("ABCDEFGHJKLMNPQRSTUVWXYZ23456789").WithPadding(base32.NoPadding)

// RandomToken returns n random bytes encoded with an unambiguous base32 alphabet.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// ---- users & sessions ----

type User struct {
	Email       string `json:"email"`
	Name        string `json:"name"`         // from Google, refreshed at every sign-in
	DisplayName string `json:"display_name"` // chosen by the user; overrides Name when set
	Picture     string `json:"picture"`
	Role        string `json:"role"`
	NotifyPref  string `json:"notify_pref"`
}

func (u *User) IsAdmin() bool { return u.Role == "admin" }

const userCols = `u.email, u.name, u.display_name, u.picture, u.role, u.notify_pref`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	if err := sc.Scan(&u.Email, &u.Name, &u.DisplayName, &u.Picture, &u.Role, &u.NotifyPref); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (s *Store) GetUser(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.email = ?`, strings.ToLower(email)))
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users u ORDER BY u.role, u.email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// UpsertUser adds an allowlisted user or changes their role, keeping profile data.
func (s *Store) UpsertUser(ctx context.Context, email, role string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(email, role, created) VALUES(?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET role = excluded.role`, strings.ToLower(email), role, now())
	return err
}

func (s *Store) UpdateProfile(ctx context.Context, email, name, picture string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET name = ?, picture = ? WHERE email = ?`, name, picture, email)
	return err
}

// SetDisplayName sets the name shown on popups; empty falls back to the Google name.
func (s *Store) SetDisplayName(ctx context.Context, email, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET display_name = ? WHERE email = ?`, name, email)
	return err
}

func (s *Store) SetNotifyPref(ctx context.Context, email, pref string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET notify_pref = ? WHERE email = ?`, pref, email)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, email string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE email = ?`, email)
	return err
}

func (s *Store) CreateSession(ctx context.Context, email string, ttl time.Duration) (string, error) {
	tok := RandomToken(32)
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash, email, expires) VALUES(?, ?, ?)`,
		HashToken(tok), email, time.Now().Add(ttl).Unix())
	return tok, err
}

func (s *Store) SessionUser(ctx context.Context, tok string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+`
		FROM sessions s JOIN users u ON u.email = s.email WHERE s.token_hash = ? AND s.expires > ?`, HashToken(tok), now()))
}

func (s *Store) DeleteSession(ctx context.Context, tok string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(tok))
	return err
}

// ---- push subscriptions ----

type PushSub struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

func (s *Store) AddPushSub(ctx context.Context, email string, p PushSub) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO push_subscriptions(endpoint, email, p256dh, auth, created) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET email = excluded.email, p256dh = excluded.p256dh, auth = excluded.auth`,
		p.Endpoint, email, p.P256dh, p.Auth, now())
	return err
}

func (s *Store) DeletePushSub(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

func (s *Store) PushSubsFor(ctx context.Context, emails []string) ([]PushSub, error) {
	if len(emails) == 0 {
		return nil, nil
	}
	args := make([]any, len(emails))
	for i, e := range emails {
		args[i] = e
	}
	q := `SELECT endpoint, p256dh, auth FROM push_subscriptions WHERE email IN (?` + strings.Repeat(",?", len(emails)-1) + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSub
	for rows.Next() {
		var p PushSub
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- devices & pairing ----

type Device struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	LastSeen int64  `json:"last_seen"`
	Online   bool   `json:"online"`
}

func (s *Store) CreatePairingCode(ctx context.Context, by string) (string, error) {
	code := RandomToken(5) // 8 chars
	_, err := s.db.ExecContext(ctx, `INSERT INTO pairing_codes(code, created_by, expires) VALUES(?, ?, ?)`,
		code, by, time.Now().Add(15*time.Minute).Unix())
	return code, err
}

// PairDevice consumes a pairing code and returns a new device token. Pairing with an existing
// name re-issues that device's token (e.g. after reinstalling the client).
func (s *Store) PairDevice(ctx context.Context, code, name string) (int64, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code = ? AND expires > ?`, strings.ToUpper(strings.TrimSpace(code)), now())
	if err != nil {
		return 0, "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, "", ErrNotFound
	}
	tok := RandomToken(32)
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO devices(name, token_hash, created) VALUES(?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET token_hash = excluded.token_hash RETURNING id`, name, HashToken(tok), now()).Scan(&id)
	if err != nil {
		return 0, "", err
	}
	return id, tok, tx.Commit()
}

func (s *Store) DeviceByToken(ctx context.Context, tok string) (*Device, error) {
	var d Device
	err := s.db.QueryRowContext(ctx, `SELECT id, name, last_seen FROM devices WHERE token_hash = ?`, HashToken(tok)).Scan(&d.ID, &d.Name, &d.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

func (s *Store) DeviceByName(ctx context.Context, name string) (*Device, error) {
	var d Device
	err := s.db.QueryRowContext(ctx, `SELECT id, name, last_seen FROM devices WHERE name = ? COLLATE NOCASE`, name).Scan(&d.ID, &d.Name, &d.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, last_seen FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RenameDevice changes a PC's name. The PC itself keeps working: it authenticates by token.
func (s *Store) RenameDevice(ctx context.Context, id int64, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET name = ? WHERE id = ?`, name, id)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchDevice(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen = ? WHERE id = ?`, now(), id)
	return err
}

func (s *Store) DeleteDevice(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	return err
}

// ---- attention types ----

type Type struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	SoundMime string `json:"sound_mime"`
	Hash      string `json:"hash"`
	Updated   int64  `json:"updated"`
}

const typeCols = `id, name, sound_mime, hash, updated`

func scanType(sc interface{ Scan(...any) error }) (*Type, error) {
	var t Type
	if err := sc.Scan(&t.ID, &t.Name, &t.SoundMime, &t.Hash, &t.Updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (s *Store) ListTypes(ctx context.Context) ([]Type, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+typeCols+` FROM types ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Type
	for rows.Next() {
		t, err := scanType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) GetType(ctx context.Context, id int64) (*Type, error) {
	return scanType(s.db.QueryRowContext(ctx, `SELECT `+typeCols+` FROM types WHERE id = ?`, id))
}

func (s *Store) TypeByName(ctx context.Context, name string) (*Type, error) {
	return scanType(s.db.QueryRowContext(ctx, `SELECT `+typeCols+` FROM types WHERE name = ? COLLATE NOCASE`, name))
}

func (s *Store) CreateType(ctx context.Context, name, soundMime, hash string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO types(name, sound_mime, hash, updated) VALUES(?, ?, ?, ?) RETURNING id`,
		name, soundMime, hash, now()).Scan(&id)
	return id, err
}

func (s *Store) UpdateType(ctx context.Context, t *Type) error {
	_, err := s.db.ExecContext(ctx, `UPDATE types SET name = ?, sound_mime = ?, hash = ?, updated = ? WHERE id = ?`,
		t.Name, t.SoundMime, t.Hash, now(), t.ID)
	return err
}

func (s *Store) DeleteType(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM types WHERE id = ?`, id)
	return err
}

// ---- API keys ----

type APIKey struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	OwnerEmail   string `json:"owner_email"`
	PinnedType   *int64 `json:"pinned_type"`
	PinnedDevice *int64 `json:"pinned_device"`
	Created      int64  `json:"created"`
	LastUsed     int64  `json:"last_used"`
}

const keyCols = `id, name, COALESCE(owner_email, ''), pinned_type, pinned_device, created, last_used`

func scanKey(sc interface{ Scan(...any) error }) (*APIKey, error) {
	var k APIKey
	if err := sc.Scan(&k.ID, &k.Name, &k.OwnerEmail, &k.PinnedType, &k.PinnedDevice, &k.Created, &k.LastUsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

// CreateAPIKey returns the plaintext key; it is never retrievable again.
func (s *Store) CreateAPIKey(ctx context.Context, name, owner string, pinnedType, pinnedDevice *int64) (string, error) {
	key := "ag_" + RandomToken(20)
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(name, key_hash, owner_email, pinned_type, pinned_device, created) VALUES(?, ?, ?, ?, ?, ?)`,
		name, HashToken(key), owner, pinnedType, pinnedDevice, now())
	return key, err
}

func (s *Store) APIKeyBySecret(ctx context.Context, key string) (*APIKey, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE key_hash = ?`, HashToken(key)))
	if err != nil {
		return nil, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, now(), k.ID)
	return k, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

// ---- presets ----

func (s *Store) Presets(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT text FROM presets ORDER BY position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SetPresets(ctx context.Context, presets []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM presets`); err != nil {
		return err
	}
	for i, p := range presets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO presets(position, text) VALUES(?, ?)`, i, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}
