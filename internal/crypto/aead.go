package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// EncryptionScheme 列に保存する文字列。実装の互換性切替の主軸。
const EncryptionSchemeV1 = "aead-aes256-gcm-chunked-1mb-v1"

// AEADChunkSize は 1 MiB チャンク（設計書 07-security.md §4.2）。
const AEADChunkSize = 1 << 20

// EncryptStream は src を読み出しながら 1MB チャンク AEAD で暗号化して dst に書き込む。
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
		// counter (big-endian)
		nonce[8] = byte(counter >> 24)
		nonce[9] = byte(counter >> 16)
		nonce[10] = byte(counter >> 8)
		nonce[11] = byte(counter)
		counter++

		// 最終チャンクには 'last' AAD を加えて改ざん耐性を高める
		ad := aad
		if isLast {
			ad = append([]byte("last:"), aad...)
		}
		ct := aead.Seal(nil, nonce, buf[:n], ad)

		// length prefix (4 bytes BE) + ciphertext
		lenBuf := []byte{
			byte(len(ct) >> 24), byte(len(ct) >> 16), byte(len(ct) >> 8), byte(len(ct)),
		}
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

// DecryptStream は EncryptStream の逆。aad は同じ値を指定する必要がある。
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
	lenBuf := make([]byte, 4)
	for {
		_, rerr := io.ReadFull(src, lenBuf)
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		ctLen := int(lenBuf[0])<<24 | int(lenBuf[1])<<16 | int(lenBuf[2])<<8 | int(lenBuf[3])
		if ctLen <= 0 || ctLen > AEADChunkSize+aead.Overhead() {
			return fmt.Errorf("decrypt: bogus chunk length %d", ctLen)
		}
		ct := make([]byte, ctLen)
		if _, err := io.ReadFull(src, ct); err != nil {
			return err
		}

		nonce := make([]byte, 12)
		copy(nonce, header)
		nonce[8] = byte(counter >> 24)
		nonce[9] = byte(counter >> 16)
		nonce[10] = byte(counter >> 8)
		nonce[11] = byte(counter)
		counter++

		// 最終チャンクは length < AEADChunkSize（GCM tag 16 bytes 込み）
		isLast := ctLen < AEADChunkSize+aead.Overhead()
		ad := aad
		if isLast {
			ad = append([]byte("last:"), aad...)
		}
		pt, err := aead.Open(nil, nonce, ct, ad)
		if err != nil {
			// 「最終チャンクの推定」が外れたら通常 AAD で再試行（そのチャンクが偶然短かった場合）
			if isLast {
				pt, err = aead.Open(nil, nonce, ct, aad)
			}
			if err != nil {
				return fmt.Errorf("decrypt chunk %d: %w", counter-1, err)
			}
		}
		if _, err := dst.Write(pt); err != nil {
			return err
		}
		if isLast {
			return nil
		}
	}
}
