package store

import (
	"context"
	"database/sql"
	"errors"
)

// ---- installs: merging and splitting PCs ----

// ErrLastInstall is returned when removing or splitting off a PC's only install.
var ErrLastInstall = errors.New("this is the PC's only install")

// MergeDevices folds PC src into dst, e.g. the two OSes of a dual-boot PC paired separately.
// src's installs, alert history, group memberships and trigger-key pins move to dst, then src
// is deleted. Resolve src's open alerts first: dst keeps only one open alert.
func (s *Store) MergeDevices(ctx context.Context, src, dst int64) error {
	if src == dst {
		return errors.New("can't merge a PC into itself")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE id IN (?, ?)`, src, dst).Scan(&n); err != nil {
		return err
	}
	if n != 2 {
		return ErrNotFound
	}
	for _, q := range []string{
		`UPDATE installs SET device_id = :dst WHERE device_id = :src`,
		`UPDATE alerts SET device_id = :dst WHERE device_id = :src`,
		`INSERT OR IGNORE INTO group_devices(group_id, device_id) SELECT group_id, :dst FROM group_devices WHERE device_id = :src`,
		`UPDATE api_keys SET pinned_device = :dst WHERE pinned_device = :src`,
		`UPDATE devices SET last_seen = MAX(last_seen, (SELECT last_seen FROM devices WHERE id = :src)) WHERE id = :dst`,
		`DELETE FROM devices WHERE id = :src`,
	} {
		if _, err := tx.ExecContext(ctx, q, sql.Named("src", src), sql.Named("dst", dst)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// installOf checks that an install belongs to a PC that has other installs too.
func installOf(ctx context.Context, tx *sql.Tx, deviceID, installID int64) error {
	var mine bool
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id = ?), 0), COUNT(*) FROM installs WHERE device_id = ?`,
		installID, deviceID).Scan(&mine, &n)
	switch {
	case err != nil:
		return err
	case !mine:
		return ErrNotFound
	case n < 2:
		return ErrLastInstall
	}
	return nil
}

// SplitInstall moves one install of a PC into a new PC called name, undoing a merge.
// History stays with the original PC.
func (s *Store) SplitInstall(ctx context.Context, deviceID, installID int64, name string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := installOf(ctx, tx, deviceID, installID); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO devices(name, token_hash, created, last_seen)
		SELECT ?, ?, ?, last_seen FROM installs WHERE id = ? RETURNING id`, name, unusedTokenHash(), now(), installID).Scan(&id)
	if isUnique(err) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE installs SET device_id = ?, label = ? WHERE id = ?`, id, name, installID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// RemoveInstall revokes one install's token (e.g. an OS that was wiped); that copy of the
// client has to pair again. A PC's last install can't be removed: delete the PC instead.
func (s *Store) RemoveInstall(ctx context.Context, deviceID, installID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := installOf(ctx, tx, deviceID, installID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM installs WHERE id = ?`, installID); err != nil {
		return err
	}
	return tx.Commit()
}
