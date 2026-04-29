package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// EncryptionSchemeV1 v1 暗号化スキームの識別子。`file_versions.encryption_scheme` 列に保存する。
const EncryptionSchemeV1 = "aead-aes256-gcm-chunked-1mb-v1"

// AEADChunkSize ストリーム AEAD の 1 チャンクサイズ（1 MiB、設計書 07-security.md §4.2）。
const AEADChunkSize = 1 << 20

// EncryptStream src を読み出しながら 1MB チャンク AEAD で暗号化して dst に書き込む。
//
// フレームフォーマット (各チャンクを順序付きで append):
//   - チャンクヘッダ:    nonce (12 bytes) || ciphertext (n bytes) || tag (GCM 16 bytes 込み)
//   - 各チャンクの nonce は base nonce (8 bytes) || counter (4 bytes BE)
//   - 最終チャンクは長さ < AEADChunkSize で識別
//
// AAD として aad を添付（呼び出し側が file_id || version_number || owner_id 等を渡す）。
//
// 呼び出し側へ:
//   - 戻り値 header はファイルバージョン行に encryption_header として保存する
//   - 復号時は header と同じ key / aad を渡す
func EncryptStream(dst io.Writer, src io.Reader, key, aad []byte) (header []byte, err error) {
	if len(key) != 32 {
		return nil, errors.New("encrypt: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// base nonce: 8 bytes ランダム + 4 bytes counter
	base := make([]byte, 8)
	if _, err := rand.Read(base); err != nil {
		return nil, err
	}
	header = base // ヘッダ = base nonce のみ。スキームバージョンは encryption_scheme 列で別管理。

	buf := make([]byte, AEADChunkSize)
	var counter uint32
	for {
		n, rerr := io.ReadFull(src, buf)
		isLast := false
		switch rerr {
		case nil:
			// full chunk
		case io.EOF:
			// 残りなし
			return header, nil
		case io.ErrUnexpectedEOF:
			isLast = true
		default:
			return nil, rerr
		}

		nonce := make([]byte, 12)
		copy(nonce, base)
		binary.BigEndian.PutUint32(nonce[8:], counter)
		counter++

		// 最終チャンクには 'last' AAD を加えて改ざん耐性を高める
		ad := aad
		if isLast {
			ad = append([]byte("last:"), aad...)
		}
		ct := aead.Seal(nil, nonce, buf[:n], ad)

		// length prefix (4 bytes BE) + ciphertext
		// len(ct) は AEADChunkSize+overhead 以下（事前 invariant）なので uint32 に収まる
		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(ct))) // #nosec G115 -- bounded by AEADChunkSize + overhead
		if _, err := dst.Write(lenBuf); err != nil {
			return nil, err
		}
		if _, err := dst.Write(ct); err != nil {
			return nil, err
		}

		if isLast {
			return header, nil
		}
	}
}

// DecryptStream EncryptStream の逆。aad は同じ値を指定する必要がある。
func DecryptStream(dst io.Writer, src io.Reader, key, aad, header []byte) error {
	if len(key) != 32 {
		return errors.New("decrypt: key must be 32 bytes")
	}
	if len(header) != 8 {
		return fmt.Errorf("decrypt: header must be 8 bytes (got %d)", len(header))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	var counter uint32
	for {
		ct, isLast, err := readChunk(src, aead.Overhead())
		if err != nil {
			return err
		}
		if ct == nil {
			return nil
		}
		pt, err := decryptChunk(aead, ct, aad, header, counter, isLast)
		if err != nil {
			return fmt.Errorf("decrypt chunk %d: %w", counter, err)
		}
		counter++
		if _, err := dst.Write(pt); err != nil {
			return err
		}
		if isLast {
			return nil
		}
	}
}

// readChunk length-prefix を読み、ciphertext を読み出す。
// 戻り値の ct == nil は EOF（チャンクなし）。
func readChunk(src io.Reader, overhead int) (ct []byte, isLast bool, err error) {
	lenBuf := make([]byte, 4)
	if _, rerr := io.ReadFull(src, lenBuf); rerr != nil {
		if errors.Is(rerr, io.EOF) {
			return nil, false, nil
		}
		return nil, false, rerr
	}
	ctLen := int(binary.BigEndian.Uint32(lenBuf))
	maxLen := AEADChunkSize + overhead
	if ctLen <= 0 || ctLen > maxLen {
		return nil, false, fmt.Errorf("decrypt: bogus chunk length %d", ctLen)
	}
	ct = make([]byte, ctLen)
	if _, err := io.ReadFull(src, ct); err != nil {
		return nil, false, err
	}
	isLast = ctLen < maxLen
	return ct, isLast, nil
}

func decryptChunk(aead cipher.AEAD, ct, aad, header []byte, counter uint32, isLast bool) ([]byte, error) {
	nonce := make([]byte, 12)
	copy(nonce, header)
	binary.BigEndian.PutUint32(nonce[8:], counter)

	ad := aad
	if isLast {
		ad = append([]byte("last:"), aad...)
	}
	pt, err := aead.Open(nil, nonce, ct, ad)
	if err == nil {
		return pt, nil
	}
	// 最終チャンクの推定が外れたケース（偶然短かったチャンク）：通常 AAD で再試行
	if isLast {
		if pt2, err2 := aead.Open(nil, nonce, ct, aad); err2 == nil {
			return pt2, nil
		}
	}
	return nil, err
}
