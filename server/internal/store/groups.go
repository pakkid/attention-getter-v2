package store

import (
	"context"
	"database/sql"
	"errors"
)

// ---- PC groups ----

type Group struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	DeviceIDs []int64 `json:"device_ids"`
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, g.name, gd.device_id FROM groups g
		LEFT JOIN group_devices gd ON gd.group_id = g.id ORDER BY g.name, g.id, gd.device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var id int64
		var name string
		var dev sql.NullInt64
		if err := rows.Scan(&id, &name, &dev); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != id {
			out = append(out, Group{ID: id, Name: name, DeviceIDs: []int64{}})
		}
		if dev.Valid {
			g := &out[len(out)-1]
			g.DeviceIDs = append(g.DeviceIDs, dev.Int64)
		}
	}
	return out, rows.Err()
}

// GroupByName returns a group (case-insensitive) with its member PCs.
func (s *Store) GroupByName(ctx context.Context, name string) (*Group, []Device, error) {
	var g Group
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM groups WHERE name = ?`, name).Scan(&g.ID, &g.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	ds, err := s.GroupDevices(ctx, g.ID)
	return &g, ds, err
}

func (s *Store) GroupDevices(ctx context.Context, groupID int64) ([]Device, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups WHERE id = ?)`, groupID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.id, d.name, d.last_seen FROM group_devices gd
		JOIN devices d ON d.id = gd.device_id WHERE gd.group_id = ? ORDER BY d.name`, groupID)
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

// SaveGroup creates (id 0) or updates a group and replaces its members.
func (s *Store) SaveGroup(ctx context.Context, id int64, name string, deviceIDs []int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if id == 0 {
		err = tx.QueryRowContext(ctx, `INSERT INTO groups(name, created) VALUES(?, ?) RETURNING id`, name, now()).Scan(&id)
	} else {
		var res sql.Result
		if res, err = tx.ExecContext(ctx, `UPDATE groups SET name = ? WHERE id = ?`, name, id); err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return 0, ErrNotFound
			}
		}
	}
	if isUnique(err) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_devices WHERE group_id = ?`, id); err != nil {
		return 0, err
	}
	for _, d := range deviceIDs {
		// Unknown device ids are skipped rather than failing the whole save.
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO group_devices(group_id, device_id)
			SELECT ?, id FROM devices WHERE id = ?`, id, d); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

func (s *Store) DeleteGroup(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id)
	return err
}

// NameTaken reports whether a PC or group other than the given ones already uses name
// (case-insensitive). PCs and groups share one namespace so `pc=NAME` is unambiguous.
func (s *Store) NameTaken(ctx context.Context, name string, exceptDevice, exceptGroup int64) (bool, error) {
	var taken bool
	err := s.db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM devices WHERE name = ? COLLATE NOCASE AND id != ?) OR
		EXISTS(SELECT 1 FROM groups WHERE name = ? AND id != ?)`, name, exceptDevice, name, exceptGroup).Scan(&taken)
	return taken, err
}

// ---- settings ----

type Settings struct {
	// DeliverOffline queues alerts for offline PCs until they reconnect. Off by default:
	// an alert for an offline PC is recorded as missed instead.
	DeliverOffline bool `json:"deliver_offline"`
}

func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	var st Settings
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'deliver_offline'`).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.DeliverOffline = v == "1"
	return st, nil
}

func (s *Store) SaveSettings(ctx context.Context, st Settings) error {
	v := "0"
	if st.DeliverOffline {
		v = "1"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES('deliver_offline', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, v)
	return err
}
