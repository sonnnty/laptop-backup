package auth

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestCredentialsFormatsAndAuthURL(t *testing.T) {
	for _, body := range []string{
		`{"client_id":"cid.apps.googleusercontent.com","client_secret":"s3"}`,
		`{"installed":{"client_id":"cid.apps.googleusercontent.com","client_secret":"s3","redirect_uris":["http://localhost"]}}`,
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(body), 0o600)
		m, err := New(filepath.Join(dir, "credentials.json"), filepath.Join(dir, "token.json"))
		if err != nil {
			t.Fatal(err)
		}
		if st := m.Status(); !st.HasCredentials || st.Connected {
			t.Fatalf("status = %+v", st)
		}
		if _, err := m.HTTPClient(); err != ErrNotConnected {
			t.Fatalf("HTTPClient err = %v", err)
		}
		raw, err := m.AuthURL("http://backup/settings")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(raw)
		q := u.Query()
		for k, want := range map[string]string{
			"client_id": "cid.apps.googleusercontent.com", "redirect_uri": RedirectURL, "response_type": "code",
			"scope": Scope, "access_type": "offline", "prompt": "consent",
		} {
			if q.Get(k) != want {
				t.Errorf("%s = %q, want %q", k, q.Get(k), want)
			}
		}
		if _, err := m.Exchange(t.Context(), "wrong-state", "code"); err == nil {
			t.Error("unknown state accepted")
		}
	}
}

func TestMissingCredentials(t *testing.T) {
	dir := t.TempDir()
	m, err := New(filepath.Join(dir, "credentials.json"), filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.HasCredentials || st.Connected {
		t.Fatalf("status = %+v", st)
	}
	if _, err := m.AuthURL("/"); err != ErrNoCredentials {
		t.Fatalf("AuthURL err = %v", err)
	}
}

func TestTokenLoadAndDisconnect(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"client_id":"c","client_secret":"s"}`), 0o600)
	tok := filepath.Join(dir, "token.json")
	os.WriteFile(tok, []byte(`{"refresh_token":"r","email":"me@example.com"}`), 0o600)
	m, err := New(filepath.Join(dir, "credentials.json"), tok)
	if err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); !st.Connected || st.Email != "me@example.com" {
		t.Fatalf("status = %+v", st)
	}
	if err := m.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tok); !os.IsNotExist(err) {
		t.Fatal("token.json not removed")
	}
	if m.Status().Connected {
		t.Fatal("still connected")
	}
}

func TestIsInvalidGrant(t *testing.T) {
	re := &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	wrapped := &url.Error{Op: "Post", URL: "https://oauth2.googleapis.com/token", Err: re}
	if !IsInvalidGrant(fmt.Errorf("upload: %w", wrapped)) {
		t.Error("wrapped invalid_grant not detected")
	}
	if IsInvalidGrant(&oauth2.RetrieveError{ErrorCode: "invalid_client"}) || IsInvalidGrant(nil) {
		t.Error("false positive")
	}
	if !strings.Contains(ErrNotConnected.Error(), "Google") {
		t.Error("message")
	}
}
