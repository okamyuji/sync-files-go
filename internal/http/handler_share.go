package http

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/crypto"
	"github.com/okamyuji/sync-files-go/internal/domain"
	"github.com/okamyuji/sync-files-go/internal/http/middleware"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
)

// createShareLinkReq POST /api/files/{id}/share-links のリクエスト。
//
// HIGH-3 修正: v1 では「期限なし」を許さない。
type createShareLinkReq struct {
	ExpiresIn string `json:"expires_in"` // "1h" | "1d" | "7d"
	Password  string `json:"password,omitempty"`
}

// createShareLinkHandler 公開リンクを発行する。
func createShareLinkHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := middleware.SessionFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fileID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}

		var req createShareLinkReq
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		dur, ok := domain.ExpiresInOption(req.ExpiresIn).Duration()
		if !ok {
			http.Error(w, "expires_in must be 1h, 1d, or 7d (v1)", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		f, err := d.Files.FindByID(ctx, fileID)
		if err != nil {
			if errors.Is(err, mysql.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			internalError(w, d, ctx, "find file", err)
			return
		}
		if f.OwnerID != sess.UserID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if f.State != domain.FileStateActive {
			http.Error(w, "gone", http.StatusGone)
			return
		}

		tokenPlain, tokenHash, err := domain.GenerateShareToken()
		if err != nil {
			internalError(w, d, ctx, "generate token", err)
			return
		}
		var pwHash string
		if req.Password != "" {
			h, err := crypto.HashPassword(req.Password)
			if err != nil {
				internalError(w, d, ctx, "hash share password", err)
				return
			}
			pwHash = h
		}

		now := time.Now().UTC()
		s := &domain.ShareLink{
			ID:           uuid.New(),
			FileID:       fileID,
			CreatedBy:    sess.UserID,
			TokenHash:    tokenHash,
			PasswordHash: pwHash,
			ExpiresAt:    now.Add(dur),
			CreatedAt:    now,
		}
		if err := d.ShareLinks.Insert(ctx, s); err != nil {
			internalError(w, d, ctx, "insert share link", err)
			return
		}
		_ = d.Audit.Insert(ctx, nil, &mysql.AuditEntry{
			ActorID: &sess.UserID, ActorKind: mysql.ActorUser,
			Action: "share.create", TargetKind: "share_link", TargetID: &s.ID,
		})

		writeJSON(w, http.StatusCreated, map[string]any{
			"url":        d.Cfg.BaseURL + "/share/" + tokenPlain,
			"expires_at": s.ExpiresAt.Format(time.RFC3339Nano),
		})
	}
}

// revokeShareLinkHandler DELETE /api/share-links/{id} で取り消し。
func revokeShareLinkHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := middleware.SessionFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		if err := d.ShareLinks.Revoke(r.Context(), id, now); err != nil {
			if errors.Is(err, mysql.ErrNotFound) {
				http.Error(w, "not found or already revoked", http.StatusNotFound)
				return
			}
			internalError(w, d, r.Context(), "revoke", err)
			return
		}
		_ = d.Audit.Insert(r.Context(), nil, &mysql.AuditEntry{
			ActorID: &sess.UserID, ActorKind: mysql.ActorUser,
			Action: "share.revoke", TargetKind: "share_link", TargetID: &id,
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// publicShareDownloadHandler GET /share/{token} で公開リンクからダウンロード。
//
// 設計書 05 §2.1 / ADR-008 によれば、判定とファイル取得は **必ず Primary** で行う。
// ShareLinksRepo.FindByTokenHash が Primary を読むので RAW window 不要。
func publicShareDownloadHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if token == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		ctx := r.Context()
		s, err := resolveShareLink(ctx, d, token)
		if err != nil {
			emitShareError(w, d, ctx, err)
			return
		}
		if !checkSharePassword(s, r) {
			http.Error(w, "password required", http.StatusUnauthorized)
			return
		}
		f, v, dek, err := loadShareTarget(ctx, d, s)
		if err != nil {
			emitShareError(w, d, ctx, err)
			return
		}
		streamShareDownload(ctx, d, w, r, s, f, v, dek)
	}
}

func resolveShareLink(ctx context.Context, d *Deps, token string) (*domain.ShareLink, error) {
	tokenHash := domain.HashShareToken(token)
	s, err := d.ShareLinks.FindByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, mysql.ErrNotFound) {
			return nil, httpErr(http.StatusNotFound, "not found")
		}
		return nil, err
	}
	if !s.IsActive(time.Now().UTC()) {
		return nil, httpErr(http.StatusGone, "gone")
	}
	return s, nil
}

func checkSharePassword(s *domain.ShareLink, r *http.Request) bool {
	if s.PasswordHash == "" {
		return true
	}
	provided := r.Header.Get("X-Share-Password")
	if provided == "" {
		return false
	}
	return verifySharePassword(s.PasswordHash, provided)
}

func loadShareTarget(ctx context.Context, d *Deps, s *domain.ShareLink) (*domain.File, *domain.FileVersion, []byte, error) {
	f, err := d.Files.FindByID(ctx, s.FileID)
	if err != nil {
		if errors.Is(err, mysql.ErrNotFound) {
			return nil, nil, nil, httpErr(http.StatusGone, "gone")
		}
		return nil, nil, nil, err
	}
	if f.State != domain.FileStateActive {
		_ = d.ShareLinks.Revoke(ctx, s.ID, time.Now().UTC())
		return nil, nil, nil, httpErr(http.StatusGone, "gone")
	}
	if f.CurrentVersionID == nil {
		return nil, nil, nil, httpErr(http.StatusInternalServerError, "no version")
	}
	v, err := d.FileVersions.FindByID(ctx, *f.CurrentVersionID)
	if err != nil {
		return nil, nil, nil, err
	}
	owner, err := d.Users.FindByID(ctx, f.OwnerID)
	if err != nil {
		return nil, nil, nil, err
	}
	dek, err := unwrapKeyDev(v.DEKEnc, owner.KEKEnc)
	if err != nil {
		return nil, nil, nil, err
	}
	return f, v, dek, nil
}

func streamShareDownload(ctx context.Context, d *Deps, w http.ResponseWriter, r *http.Request, s *domain.ShareLink, f *domain.File, v *domain.FileVersion, dek []byte) {
	rc, err := d.Storage.OpenVersion(ctx, f.OwnerID.String(), f.ID.String(), v.ID.String())
	if err != nil {
		internalError(w, d, ctx, "open version", err)
		return
	}
	defer func() { _ = rc.Close() }()

	_ = d.ShareLinks.IncrementDownloadCount(ctx, s.ID)
	auditPublicAccess(ctx, d, s.ID, r)

	w.Header().Set("Content-Type", contentTypeOrDefault(f.ContentType))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+f.Name+"\"")
	if err := crypto.DecryptStream(w, rc, dek, buildAAD(f.OwnerID, f.ID, v.ID), v.EncryptionHeader); err != nil {
		d.Logger.ErrorContext(ctx, "share decrypt", "err", err)
	}
}

// emitShareError handler エラーを HTTP ステータスに反映。
func emitShareError(w http.ResponseWriter, d *Deps, ctx context.Context, err error) {
	var ue *uploadHTTPError
	if errors.As(err, &ue) {
		http.Error(w, ue.msg, ue.status)
		return
	}
	internalError(w, d, ctx, "share", err)
}

// verifySharePassword Argon2id ハッシュでパスワード検証。
func verifySharePassword(hash, plain string) bool {
	ok, err := crypto.VerifyPassword(hash, plain)
	return err == nil && ok
}

func auditPublicAccess(ctx context.Context, d *Deps, linkID uuid.UUID, r *http.Request) {
	_ = d.Audit.Insert(ctx, nil, &mysql.AuditEntry{
		ActorKind:  mysql.ActorPublicViewer,
		Action:     "share.download",
		TargetKind: "share_link", TargetID: &linkID,
		IPAddr: clientIPBytes(r), UserAgent: r.UserAgent(),
	})
}

func contentTypeOrDefault(ct string) string {
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}
