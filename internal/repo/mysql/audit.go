package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/repo"
)

// ActorKind audit_logs.actor_kind の値域。
type ActorKind string

const (
	ActorUser         ActorKind = "user"
	ActorPublicViewer ActorKind = "public_viewer"
	ActorSystem       ActorKind = "system"
)

// AuditEntry audit_logs に追加する 1 行。
type AuditEntry struct {
	ID           uuid.UUID
	OccurredAt   time.Time
	ActorID      *uuid.UUID
	ActorKind    ActorKind
	Action       string
	TargetKind   string
	TargetID     *uuid.UUID
	Details      map[string]any
	IPAddr       []byte
	UserAgent    string
	Irreversible bool
}

// AuditRepo audit_logs への INSERT-only アクセス。
type AuditRepo struct {
	router *repo.DBRouter
}

// NewAuditRepo コンストラクタ。
func NewAuditRepo(router *repo.DBRouter) *AuditRepo {
	return &AuditRepo{router: router}
}

// ListByActor アクティビティ画面用。指定ユーザの audit_logs を新しい順に取り出す。
//
// idx_audit_logs_actor_time (actor_id_bin, occurred_at DESC) があるので O(log n) で最新が引ける。
func (r *AuditRepo) ListByActor(ctx context.Context, actorID uuid.UUID, limit, offset int) ([]*AuditEntry, error) {
	const q = `
SELECT id_bin, occurred_at, actor_id_bin, actor_kind, action,
       target_kind, target_id_bin, details_json, ip_addr, user_agent, irreversible
FROM audit_logs
WHERE actor_id_bin = ?
ORDER BY occurred_at DESC
LIMIT ? OFFSET ?
`
	rows, err := r.router.Reader(ctx).QueryContext(ctx, q, uuidToBin(actorID), limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*AuditEntry, 0)
	for rows.Next() {
		e, err := scanAuditEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanAuditEntry rows.Next() 後の現在行を AuditEntry に展開する。
func scanAuditEntry(rows *sql.Rows) (*AuditEntry, error) {
	var (
		e          AuditEntry
		idBin      []byte
		actorBin   sql.RawBytes
		targetBin  sql.RawBytes
		details    []byte
		ipAddr     sql.RawBytes
		userAgent  sql.NullString
		actorKind  string
		targetKind string
	)
	if err := rows.Scan(
		&idBin, &e.OccurredAt, &actorBin, &actorKind, &e.Action,
		&targetKind, &targetBin, &details, &ipAddr, &userAgent, &e.Irreversible,
	); err != nil {
		return nil, err
	}
	e.ID, _ = binToUUID(idBin)
	e.ActorKind = ActorKind(actorKind)
	e.TargetKind = targetKind
	if len(actorBin) > 0 {
		id, _ := binToUUID(actorBin)
		e.ActorID = &id
	}
	if len(targetBin) > 0 {
		id, _ := binToUUID(targetBin)
		e.TargetID = &id
	}
	if len(details) > 0 {
		var m map[string]any
		if err := json.Unmarshal(details, &m); err == nil {
			e.Details = m
		}
	}
	if len(ipAddr) > 0 {
		e.IPAddr = append([]byte(nil), ipAddr...)
	}
	if userAgent.Valid {
		e.UserAgent = userAgent.String
	}
	return &e, nil
}

// Insert 監査ログを 1 行追加する。トランザクション内なら *sql.Tx を渡す（オプション）。
func (r *AuditRepo) Insert(ctx context.Context, tx *sql.Tx, e *AuditEntry) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	js, err := json.Marshal(e.Details)
	if err != nil {
		return err
	}
	if e.Details == nil {
		js = []byte(`{}`)
	}

	var actor any
	if e.ActorID != nil {
		actor = uuidToBin(*e.ActorID)
	}
	var target any
	if e.TargetID != nil {
		target = uuidToBin(*e.TargetID)
	}
	var ua any
	if e.UserAgent != "" {
		ua = e.UserAgent
	}
	var ip any
	if len(e.IPAddr) > 0 {
		ip = e.IPAddr
	}

	const q = `
INSERT INTO audit_logs (
  id_bin, occurred_at, actor_id_bin, actor_kind, action,
  target_kind, target_id_bin, details_json, ip_addr, user_agent, irreversible
) VALUES (?,?,?,?,?,?,?,?,?,?,?)
`
	args := []any{
		uuidToBin(e.ID), e.OccurredAt.UTC(), actor, string(e.ActorKind), e.Action,
		e.TargetKind, target, js, ip, ua, e.Irreversible,
	}
	if tx != nil {
		_, err = tx.ExecContext(ctx, q, args...)
	} else {
		_, err = r.router.Writer(ctx).ExecContext(ctx, q, args...)
	}
	return err
}
