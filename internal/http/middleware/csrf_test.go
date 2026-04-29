package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCSRF_FormField  hidden _csrf フィールドで送信した CSRF トークンを受理する。
// プログレッシブエンハンスメント（JS 無効ブラウザ）対応の回帰テスト。
func TestCSRF_FormField(t *testing.T) {
	t.Parallel()

	called := false
	handler := CSRF([]byte("test-key"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	tokenValue := "abc123token"
	body := strings.NewReader("_csrf=" + tokenValue + "&email=u@example.com&password=ineedaverylongpassword")
	req := httptest.NewRequest(http.MethodPost, "/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tokenValue})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !called {
		t.Error("downstream handler not called")
	}
}

func TestCSRF_FormField_Mismatch(t *testing.T) {
	t.Parallel()

	handler := CSRF([]byte("test-key"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream should not be called")
	}))

	body := strings.NewReader("_csrf=wrong&payload=x")
	req := httptest.NewRequest(http.MethodPost, "/login", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "right"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestCSRF_HeaderTakesPrecedence(t *testing.T) {
	t.Parallel()

	called := false
	handler := CSRF([]byte("test-key"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	tokenValue := "headertoken"
	req := httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader("nothing here"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeaderName, tokenValue)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tokenValue})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !called {
		t.Error("downstream handler not called")
	}
}

func TestCSRF_NoCookieRejected(t *testing.T) {
	t.Parallel()
	handler := CSRF([]byte("test-key"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream should not run without cookie")
	}))
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader("_csrf=anything"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestIsFormURLEncoded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"application/x-www-form-urlencoded", true},
		{"application/x-www-form-urlencoded; charset=UTF-8", true},
		{"application/x-www-form-urlencoded ; charset=UTF-8", true},
		{"application/json", false},
		{"multipart/form-data; boundary=---", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isFormURLEncoded(c.in); got != c.want {
			t.Errorf("isFormURLEncoded(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
