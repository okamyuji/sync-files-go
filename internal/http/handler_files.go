package http

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/crypto"
	"github.com/okamyuji/sync-files-go/internal/domain"
	"github.com/okamyuji/sync-files-go/internal/http/middleware"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/storage/localfs"
)

// listFilesHandler 認証済みユーザのファイル一覧を JSON で返す。Phase 5 で HTML に置換。
func listFilesHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := middleware.SessionFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		files, err := d.Files.ListActiveByOwner(r.Context(), sess.UserID, 50, 0)
		if err != nil {
			internalError(w, d, r.Context(), "list files", err)
			return
		}
		writeJSON(w, http.StatusOK, fileSummaries(files))
	}
}

// fileSummaries domain.File を API 表現に変換。
func fileSummaries(files []*domain.File) []map[string]any {
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		var ver string
		if f.CurrentVersionID != nil {
			ver = f.CurrentVersionID.String()
		}
		out = append(out, map[string]any{
			"id":         f.ID.String(),
			"name":       f.Name,
			"path":       f.Path,
			"size_bytes": f.SizeBytes,
			"updated_at": f.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"version_id": ver,
		})
	}
	return out
}

// uploadResult uploadFileHandler の write phase 結果。
type uploadResult struct {
	fileID    uuid.UUID
	versionID uuid.UUID
	created   bool // 新規作成なら true、上書きなら false
}

// uploadFileHandler POST /api/files の handler。新規 + 上書き両対応。
//
// 設計書 04-sync-semantics.md §4 / 05 §1 に対応：
//   - 新規: If-None-Match: * 必須
//   - 上書き: If-Match: <version_id> 必須
//   - X-File-Path ヘッダで保存先パスを指定（Phase 5 でフォーム対応）
func uploadFileHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := middleware.SessionFrom(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		path := r.Header.Get("X-File-Path")
		if path == "" {
			http.Error(w, "X-File-Path required", http.StatusBadRequest)
			return
		}
		pre := domain.Precondition{
			IfMatch:     r.Header.Get("If-Match"),
			IfNoneMatch: r.Header.Get("If-None-Match"),
		}
		body := http.MaxBytesReader(w, r.Body, d.Cfg.MaxUploadBytes)
		defer func() { _ = body.Close() }()

		res, err := executeUpload(r.Context(), d, sess, path, pre, body, r)
		if err != nil {
			emitUploadError(w, d, r.Context(), err)
			return
		}

		// RAW window cookie で後続リクエストを Primary に向ける
		middleware.SetRAWCookie(w, sess.SessionID, time.Now().Add(d.Cfg.RAWWindow), d.Cfg.SessionKey)

		w.Header().Set("ETag", res.versionID.String())
		w.Header().Set("X-File-Id", res.fileID.String())
		if res.created {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// uploadHTTPError HTTP ステータスを伴う handler エラー。executeUpload から返してくる。
type uploadHTTPError struct {
	status int
	msg    string
	cur    *domain.File // OCCConflict の場合に詰める
}

func (e *uploadHTTPError) Error() string { return e.msg }

func httpErr(status int, msg string) error { return &uploadHTTPError{status: status, msg: msg} }

func conflictErr(cur *domain.File) error {
	return &uploadHTTPError{status: http.StatusConflict, msg: "version mismatch", cur: cur}
}

// emitUploadError handler から返ってきた error を HTTP レスポンスに変換。
func emitUploadError(w http.ResponseWriter, d *Deps, ctx context.Context, err error) {
	var ue *uploadHTTPError
	if errors.As(err, &ue) {
		if ue.cur != nil {
			writeConflictResponse(w, ue.cur)
			return
		}
		http.Error(w, ue.msg, ue.status)
		return
	}
	internalError(w, d, ctx, "upload", err)
}

// executeUpload 実書き込み + メタデータ確定の流れ。
//
// 設計書 04-sync-semantics.md §6.1 と FK 制約 (file_versions → files) を満たすため、
// 段階的にコネクションを使う：
//
//  1. OCC 事前チェック（Reader 経由、UNIQUE 制約が CR-2 を守る）
//  2. tmp に encrypt → versions/{file_id}/{version_id} に rename (immutable)
//  3. 新規時: files INSERT を単独コネクションで先に実施（FK 親作成）
//  4. トランザクション開始: 上書き時は SELECT FOR UPDATE で OCC を再確認
//     → file_versions INSERT → files UPDATE current_version_id → audit → COMMIT
//
// 大容量アップロード中はトランザクションを開かないので、ロック待ちタイムアウトが起きない。
func executeUpload(ctx context.Context, d *Deps, sess middleware.UserSession, path string, pre domain.Precondition, body io.Reader, r *http.Request) (*uploadResult, error) {
	curBefore, err := preflightOCC(ctx, d, sess.UserID, path, pre)
	if err != nil {
		return nil, err
	}

	fileID := uuid.New()
	if curBefore != nil {
		fileID = curBefore.ID
	}
	versionID := uuid.New()

	dek, err := generateDEK()
	if err != nil {
		return nil, err
	}
	header, sha, sizeBytes, err := writeEncryptedVersion(ctx, d, sess.UserID, fileID, versionID, dek, body)
	if err != nil {
		return nil, err
	}

	if curBefore == nil {
		if err := ensureFileRow(ctx, d, sess.UserID, fileID, path, r.Header.Get("Content-Type"), sha, sizeBytes); err != nil {
			return nil, err
		}
	}

	if err := commitVersion(ctx, d, sess, curBefore, fileID, versionID, dek, header, sha, sizeBytes, path, pre); err != nil {
		return nil, err
	}
	return &uploadResult{fileID: fileID, versionID: versionID, created: curBefore == nil}, nil
}

// preflightOCC OCC 事前チェック。FOR UPDATE 無しで Primary を読む。
func preflightOCC(ctx context.Context, d *Deps, ownerID uuid.UUID, path string, pre domain.Precondition) (*domain.File, error) {
	curBefore, err := d.Files.GetByOwnerPathActive(ctx, ownerID, path)
	if err != nil && !errors.Is(err, mysql.ErrNotFound) {
		return nil, err
	}
	if e := evaluateOCC(curBefore, pre); e != nil {
		return nil, e
	}
	return curBefore, nil
}

// commitVersion トランザクションを開いて file_versions + files UPDATE + audit を確定する。
// 上書き時は SELECT FOR UPDATE で OCC を再確認する（二重 OCC、設計書 04 §6.1）。
func commitVersion(ctx context.Context, d *Deps, sess middleware.UserSession, curBefore *domain.File, fileID, versionID uuid.UUID, dek, header, sha []byte, sizeBytes int64, path string, pre domain.Precondition) error {
	tx, err := d.Router.Writer(ctx).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if curBefore != nil {
		curNow, err := d.Files.FindActiveByOwnerPath(ctx, tx, sess.UserID, path)
		if err != nil {
			return err
		}
		if e := evaluateOCC(curNow, pre); e != nil {
			return e
		}
	}

	if err := persistVersion(ctx, d, tx, sess, curBefore, fileID, versionID, dek, header, sha, sizeBytes); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// evaluateOCC ResolveOCC のルールを HTTP ステータスへ写像。nil なら続行可。
func evaluateOCC(cur *domain.File, pre domain.Precondition) error {
	var ref *domain.CurrentRef
	if cur != nil && cur.CurrentVersionID != nil {
		ref = &domain.CurrentRef{State: cur.State, CurrentVersionID: *cur.CurrentVersionID}
	}
	switch domain.ResolveOCC(ref, pre) {
	case domain.OCCAccept, domain.OCCForceAccept:
		return nil
	case domain.OCCNeedPrecondition:
		return httpErr(http.StatusPreconditionRequired, "precondition required")
	case domain.OCCPreconditionFailed:
		return httpErr(http.StatusPreconditionFailed, "precondition failed")
	case domain.OCCFileGone:
		return httpErr(http.StatusGone, "gone")
	case domain.OCCConflict:
		return conflictErr(cur)
	}
	return httpErr(http.StatusInternalServerError, "occ unknown")
}

func generateDEK() ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	return dek, nil
}

// writeEncryptedVersion バイト列を tmp に書いてから versions/{file_id}/{version_id} に rename。
// SHA-256・サイズ・暗号化ヘッダを返す。
func writeEncryptedVersion(ctx context.Context, d *Deps, ownerID, fileID, versionID uuid.UUID, dek []byte, body io.Reader) (header, sha []byte, sizeBytes int64, err error) {
	ownerStr := ownerID.String()
	tmpW, err := d.Storage.CreateTemp(ctx, ownerStr, "")
	if err != nil {
		return nil, nil, 0, err
	}
	cleanup := func() { _ = d.Storage.RemoveTemp(ctx, ownerStr, tmpW.UploadUUID()) }

	// 平文のサイズと SHA-256 を流量しながら測る（io.TeeReader で hasher と counter に供給）
	hasher := sha256.New()
	counter := &countingWriter{}
	teedBody := io.TeeReader(body, io.MultiWriter(hasher, counter))

	aad := buildAAD(ownerID, fileID, versionID)
	header, err = crypto.EncryptStream(tmpW, teedBody, dek, aad)
	if err != nil {
		_ = tmpW.Close()
		cleanup()
		return nil, nil, 0, err
	}
	if err := tmpW.Close(); err != nil {
		cleanup()
		return nil, nil, 0, err
	}

	// versions/{file_id}/{version_id} に確定（immutable）
	if _, err := d.Storage.FinalizeVersion(ctx, ownerStr, tmpW.UploadUUID(), fileID.String(), versionID.String()); err != nil {
		return nil, nil, 0, err
	}
	return header, hasher.Sum(nil), counter.n, nil
}

// ensureFileRow 新規アップロード時に親 files 行を先に作る（FK 制約のため）。
//
// `current_version_id_bin` は NULL で INSERT し、その後 file_versions Insert + UPDATE で確定する。
// 失敗で残った CurrentVersionID=NULL の files 孤児は補正ジョブが掃除する。
func ensureFileRow(ctx context.Context, d *Deps, ownerID, fileID uuid.UUID, path, contentType string, sha []byte, sizeBytes int64) error {
	now := time.Now().UTC()
	f := &domain.File{
		ID:               fileID,
		OwnerID:          ownerID,
		Name:             pathBaseName(path),
		Path:             path,
		CurrentVersionID: nil, // file_versions Insert 後に UPDATE で埋める
		SizeBytes:        sizeBytes,
		ContentType:      contentType,
		SHA256:           sha,
		State:            domain.FileStateActive, // active_marker UNIQUE が CR-2 を守る
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	return d.Files.Insert(ctx, f)
}

// persistVersion file_versions / files / audit_logs を 1 トランザクションで確定する。
//
// 呼び出し前に親 files 行が存在することを前提とする（新規時は ensureFileRow で作成済み、
// 上書き時は OCC で SELECT FOR UPDATE 済み）。
func persistVersion(ctx context.Context, d *Deps, tx *sql.Tx, sess middleware.UserSession, cur *domain.File, fileID, versionID uuid.UUID, dek, header, sha []byte, sizeBytes int64) error {
	now := time.Now().UTC()
	u, err := d.Users.FindByID(ctx, sess.UserID)
	if err != nil {
		return err
	}
	dekEnc := wrapKeyDev(dek, u.KEKEnc)

	nextVer, err := d.FileVersions.NextVersionNumber(ctx, tx, fileID)
	if err != nil {
		return err
	}

	v := &domain.FileVersion{
		ID:                 versionID,
		FileID:             fileID,
		VersionNumber:      nextVer,
		SizeBytes:          sizeBytes,
		SHA256:             sha,
		StorageKey:         localfs.VersionStorageKey(sess.UserID.String(), fileID.String(), versionID.String()),
		DEKEnc:             dekEnc,
		KEKID:              u.KEKID,
		EncryptionScheme:   crypto.EncryptionSchemeV1,
		EncryptionHeader:   header,
		CreatedAt:          now,
		CreatedBySessionID: &sess.SessionID,
	}
	if err := d.FileVersions.Insert(ctx, tx, v); err != nil {
		return err
	}

	// 新規・上書きどちらも UPDATE で current_version_id を確定
	if err := d.Files.UpdateCurrentVersion(ctx, tx, fileID, versionID, sha, sizeBytes, now); err != nil {
		return err
	}

	return d.Audit.Insert(ctx, tx, &mysql.AuditEntry{
		ActorID: &sess.UserID, ActorKind: mysql.ActorUser,
		Action: actionForUpload(cur != nil), TargetKind: "file", TargetID: &fileID,
	})
}

// downloadFileHandler GET /api/files/{id}。
func downloadFileHandler(d *Deps) http.HandlerFunc {
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
		ctx := r.Context()

		f, v, dek, err := loadFileForDownload(ctx, d, sess.UserID, fileID)
		if err != nil {
			emitDownloadError(w, d, ctx, err)
			return
		}

		rc, err := d.Storage.OpenVersion(ctx, sess.UserID.String(), fileID.String(), v.ID.String())
		if err != nil {
			internalError(w, d, ctx, "open version", err)
			return
		}
		defer func() { _ = rc.Close() }()

		w.Header().Set("ETag", v.ID.String())
		w.Header().Set("X-File-Version", strconv.Itoa(v.VersionNumber))
		ct := f.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		if err := crypto.DecryptStream(w, rc, dek, buildAAD(sess.UserID, fileID, v.ID), v.EncryptionHeader); err != nil {
			d.Logger.ErrorContext(ctx, "decrypt stream", "err", err)
			// レスポンスは既に開始済みなので接続を切る
		}
	}
}

func loadFileForDownload(ctx context.Context, d *Deps, ownerID, fileID uuid.UUID) (*domain.File, *domain.FileVersion, []byte, error) {
	f, err := d.Files.FindByID(ctx, fileID)
	if err != nil {
		if errors.Is(err, mysql.ErrNotFound) {
			return nil, nil, nil, httpErr(http.StatusNotFound, "not found")
		}
		return nil, nil, nil, err
	}
	if f.OwnerID != ownerID {
		return nil, nil, nil, httpErr(http.StatusForbidden, "forbidden")
	}
	if f.State != domain.FileStateActive {
		return nil, nil, nil, httpErr(http.StatusGone, "gone")
	}
	if f.CurrentVersionID == nil {
		return nil, nil, nil, httpErr(http.StatusInternalServerError, "no version")
	}
	v, err := d.FileVersions.FindByID(ctx, *f.CurrentVersionID)
	if err != nil {
		return nil, nil, nil, err
	}
	u, err := d.Users.FindByID(ctx, ownerID)
	if err != nil {
		return nil, nil, nil, err
	}
	dek, err := unwrapKeyDev(v.DEKEnc, u.KEKEnc)
	if err != nil {
		return nil, nil, nil, err
	}
	return f, v, dek, nil
}

func emitDownloadError(w http.ResponseWriter, d *Deps, ctx context.Context, err error) {
	var ue *uploadHTTPError
	if errors.As(err, &ue) {
		http.Error(w, ue.msg, ue.status)
		return
	}
	internalError(w, d, ctx, "download", err)
}

// deleteFileHandler ソフト削除。
func deleteFileHandler(d *Deps) http.HandlerFunc {
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
			w.WriteHeader(http.StatusOK) // 冪等
			return
		}
		now := time.Now().UTC()
		if err := d.Files.SoftDelete(ctx, fileID, now); err != nil {
			internalError(w, d, ctx, "soft delete", err)
			return
		}
		_ = d.ShareLinks.RevokeByFile(ctx, fileID, now)
		_ = d.Audit.Insert(ctx, nil, &mysql.AuditEntry{
			ActorID: &sess.UserID, ActorKind: mysql.ActorUser,
			Action: "file.delete", TargetKind: "file", TargetID: &fileID,
		})
		middleware.SetRAWCookie(w, sess.SessionID, time.Now().Add(d.Cfg.RAWWindow), d.Cfg.SessionKey)
		w.WriteHeader(http.StatusOK)
	}
}

// restoreFileHandler ゴミ箱からの復元。
func restoreFileHandler(d *Deps) http.HandlerFunc {
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
		now := time.Now().UTC()
		if err := d.Files.Restore(r.Context(), fileID, now); err != nil {
			if errors.Is(err, mysql.ErrNotFound) {
				http.Error(w, "not in trash", http.StatusGone)
				return
			}
			internalError(w, d, r.Context(), "restore", err)
			return
		}
		_ = d.Audit.Insert(r.Context(), nil, &mysql.AuditEntry{
			ActorID: &sess.UserID, ActorKind: mysql.ActorUser,
			Action: "file.restore", TargetKind: "file", TargetID: &fileID,
		})
		middleware.SetRAWCookie(w, sess.SessionID, time.Now().Add(d.Cfg.RAWWindow), d.Cfg.SessionKey)
		w.WriteHeader(http.StatusOK)
	}
}

// writeConflictResponse 設計書 04 §4.3 の OCC 衝突 JSON を返す。
func writeConflictResponse(w http.ResponseWriter, cur *domain.File) {
	w.Header().Set("HX-Trigger", "openConflictModal")
	body := map[string]any{
		"kind":                "version_mismatch",
		"current_version_id":  "",
		"current_modified_at": cur.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"current_modified_by": "another-session",
	}
	if cur.CurrentVersionID != nil {
		body["current_version_id"] = cur.CurrentVersionID.String()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(body)
}

// buildAAD ファイル取り違え防止のための AAD（07-security.md §4.2）。
func buildAAD(ownerID, fileID, versionID uuid.UUID) []byte {
	return []byte("owner=" + ownerID.String() + "|file=" + fileID.String() + "|version=" + versionID.String())
}

// pathBaseName 「/foo/bar.txt」→「bar.txt」。空なら「untitled」。
func pathBaseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	if path == "" {
		return "untitled"
	}
	return path
}

func actionForUpload(isUpdate bool) string {
	if isUpdate {
		return "file.update"
	}
	return "file.upload"
}

// countingWriter 書き込まれたバイト数を数えるだけの io.Writer。
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// unwrapKeyDev wrapKeyDev の逆。
func unwrapKeyDev(wrapped, master []byte) ([]byte, error) {
	const macLen = 32
	if len(wrapped) < macLen+1 {
		return nil, errors.New("wrapped key too short")
	}
	plain := wrapped[:len(wrapped)-macLen]
	mac := wrapped[len(wrapped)-macLen:]
	expected := sha256.Sum256(append(append([]byte("kek-dev:"), master...), plain...))
	if !bytesEqual(mac, expected[:]) {
		return nil, errors.New("wrapped key mac mismatch")
	}
	return plain, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// _ コンパイラに hash パッケージが使われていないと判断させないための保険。
var _ hash.Hash = sha256.New()
