package http

import "testing"

// TestPathReplaceBaseName  設計書 04 §4.4 の「親ディレクトリは保ったまま basename だけ差し替える」を確認。
func TestPathReplaceBaseName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, newBase, want string
	}{
		{"/foo/bar.txt", "baz.txt", "/foo/baz.txt"},
		{"/Reports/Q2.docx", "Q2 (conflict 2026-04-29 14-32 device-Mac).docx", "/Reports/Q2 (conflict 2026-04-29 14-32 device-Mac).docx"},
		{"/test.txt", "renamed.txt", "/renamed.txt"},
		{"file.txt", "other.txt", "other.txt"}, // 先頭スラッシュ無し → ベース名だけ
		{"/", "x.txt", "/x.txt"},
	}
	for _, c := range cases {
		if got := pathReplaceBaseName(c.path, c.newBase); got != c.want {
			t.Errorf("pathReplaceBaseName(%q, %q) = %q, want %q", c.path, c.newBase, got, c.want)
		}
	}
}

// TestDeviceLabelFromUA  サーバ側で User-Agent からデバイス略称を引くロジック。
// コンフリクトコピー名のデバイス部に使われる（例: 「device-iPhone」）。
func TestDeviceLabelFromUA(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ua, want string
	}{
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/...", "iPhone"},
		{"Mozilla/5.0 (iPad; ...)", "iPad"},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 9) ...", "Android"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) ...", "Mac"},
		{"Mozilla/5.0 (Windows NT 10.0; ...)", "Windows"},
		{"Mozilla/5.0 (X11; Linux x86_64) ...", "Linux"},
		{"curl/8.4.0", "browser"},
		{"", "browser"},
	}
	for _, c := range cases {
		if got := deviceLabelFromUA(c.ua); got != c.want {
			t.Errorf("deviceLabelFromUA(%q) = %q, want %q", c.ua, got, c.want)
		}
	}
}
