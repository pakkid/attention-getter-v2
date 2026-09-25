package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const (
	StatusPending   = "pending"   // created, not yet received by the PC
	StatusDelivered = "delivered" // popup is showing
	StatusReplied   = "replied"
	StatusDismissed = "dismissed"
	StatusCancelled = "cancelled"
)

// Requester identifies who triggered an alert: a signed-in user or an API key.
type Requester struct {
	Email    string // user email, or the API key owner's email (may be empty)
	APIKeyID int64  // non-zero when triggered through an API key
	Name     string // display name shown in the popup
}

type AlertRequest struct {
	Name    string `json:"name"`
	Email   string `json:"email,omitempty"`
	Message string `json:"message"`
	Created int64  `json:"created"`
}

type Alert struct {
	ID         int64          `json:"id"`
	DeviceID   int64          `json:"device_id"`
	DeviceName string         `json:"device_name"`
	TypeID     *int64         `json:"type_id"`
	TypeName   string         `json:"type_name"`
	Status     string         `json:"status"`
	Reply      string         `json:"reply"`
	Created    int64          `json:"created"`
	Resolved   int64          `json:"resolved"`
	Requests   []AlertRequest `json:"requests"`
}

func (a *Alert) Open() bool { return a.Status == StatusPending || a.Status == StatusDelivered }

const alertSelect = `SELECT a.id, a.device_id, d.name, a.type_id, COALESCE(t.name, ''), a.status, a.reply, a.created, a.resolved
	FROM alerts a JOIN devices d ON d.id = a.device_id LEFT JOIN types t ON t.id = a.type_id`

func (s *Store) scanAlerts(ctx context.Context, rows *sql.Rows) ([]*Alert, error) {
	defer rows.Close()
	var out []*Alert
	byID := map[int64]*Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.DeviceName, &a.TypeID, &a.TypeName, &a.Status, &a.Reply, &a.Created, &a.Resolved); err != nil {
			return nil, err
		}
		a.Requests = []AlertRequest{}
		out = append(out, &a)
		byID[a.ID] = &a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(out))
	for _, a := range out {
		args = append(args, a.ID)
	}
	rrows, err := s.db.QueryContext(ctx, `SELECT alert_id, requester_name, COALESCE(requester_email, ''), message, created
		FROM alert_requests WHERE alert_id IN (?`+strings.Repeat(",?", len(args)-1)+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var id int64
		var r AlertRequest
		if err := rrows.Scan(&id, &r.Name, &r.Email, &r.Message, &r.Created); err != nil {
			return nil, err
		}
		byID[id].Requests = append(byID[id].Requests, r)
	}
	return out, rrows.Err()
}

func (s *Store) GetAlert(ctx context.Context, id int64) (*Alert, error) {
	rows, err := s.db.QueryContext(ctx, alertSelect+` WHERE a.id = ?`, id)
	if err != nil {
		return nil, err
	}
	as, err := s.scanAlerts(ctx, rows)
	if err != nil {
		return nil, err
	}
	if len(as) == 0 {
		return nil, ErrNotFound
	}
	return as[0], nil
}

// OpenAlert returns the device's unresolved alert, if any.
func (s *Store) OpenAlert(ctx context.Context, deviceID int64) (*Alert, error) {
	rows, err := s.db.QueryContext(ctx, alertSelect+` WHERE a.device_id = ? AND a.status IN ('pending', 'delivered') ORDER BY a.id LIMIT 1`, deviceID)
	if err != nil {
		return nil, err
	}
	as, err := s.scanAlerts(ctx, rows)
	if err != nil {
		return nil, err
	}
	if len(as) == 0 {
		return nil, ErrNotFound
	}
	return as[0], nil
}

func (s *Store) ListAlerts(ctx context.Context, limit int) ([]*Alert, error) {
	rows, err := s.db.QueryContext(ctx, alertSelect+` ORDER BY a.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return s.scanAlerts(ctx, rows)
}

func (s *Store) CreateAlert(ctx context.Context, deviceID int64, typeID *int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO alerts(device_id, type_id, status, created) VALUES(?, ?, ?, ?) RETURNING id`,
		deviceID, typeID, StatusPending, now()).Scan(&id)
	return id, err
}

func (s *Store) AddAlertRequest(ctx context.Context, alertID int64, r Requester, message string) error {
	var email any
	if r.Email != "" {
		email = r.Email
	}
	var key any
	if r.APIKeyID != 0 {
		key = r.APIKeyID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO alert_requests(alert_id, requester_email, api_key_id, requester_name, message, created)
		VALUES(?, ?, ?, ?, ?, ?)`, alertID, email, key, r.Name, message, now())
	return err
}

func (s *Store) MarkDelivered(ctx context.Context, alertID, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET status = 'delivered' WHERE id = ? AND device_id = ? AND status = 'pending'`, alertID, deviceID)
	return err
}

// ErrNotOpen is returned when resolving an alert that is already resolved.
var ErrNotOpen = errors.New("alert is not open")

// ResolveAlert closes an open alert. deviceID 0 skips the ownership check (web cancel).
func (s *Store) ResolveAlert(ctx context.Context, alertID, deviceID int64, status, reply string) error {
	q := `UPDATE alerts SET status = ?, reply = ?, resolved = ? WHERE id = ? AND status IN ('pending', 'delivered')`
	args := []any{status, reply, now(), alertID}
	if deviceID != 0 {
		q += ` AND device_id = ?`
		args = append(args, deviceID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotOpen
	}
	return nil
}

// NotifyRecipients returns who should get a push about an alert's outcome:
// everyone with notify_pref 'all', plus requesters (or API key owners) with 'mine'.
func (s *Store) NotifyRecipients(ctx context.Context, alertID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT email FROM users WHERE notify_pref = 'all'
		UNION
		SELECT u.email FROM alert_requests r
			LEFT JOIN api_keys k ON k.id = r.api_key_id
			JOIN users u ON u.email = COALESCE(r.requester_email, k.owner_email)
			WHERE r.alert_id = ? AND u.notify_pref = 'mine'`, alertID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
