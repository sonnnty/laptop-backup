// Package auth handles the Google OAuth 2.0 loopback flow and keeps the
// refresh token in ~/.config/laptop-backup/token.json (mode 0600).
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"laptop-backup/internal/config"
)

const (
	Scope       = "https://www.googleapis.com/auth/drive"
	RedirectURL = "http://127.0.0.1:8095/oauth/callback"
)

var (
	ErrNoCredentials = errors.New("ไม่พบ credentials.json (OAuth client) — รัน install.sh ก่อน")
	ErrNotConnected  = errors.New("ยังไม่ได้เชื่อมต่อบัญชี Google")
)

// Status is what the UI shows about the Google connection.
type Status struct {
	HasCredentials bool   `json:"has_credentials"`
	Connected      bool   `json:"connected"`
	Email          string `json:"email"`
	Error          string `json:"error"`
}

type tokenFile struct {
	RefreshToken string    `json:"refresh_token"`
	Email        string    `json:"email,omitempty"`
	Created      time.Time `json:"created"`
}

type pendingState struct {
	returnTo string
	expires  time.Time
}

// Manager is safe for concurrent use.
type Manager struct {
	credPath, tokenPath string
	httpClient          *http.Client // base transport for token and API calls

	mu      sync.Mutex
	conf    *oauth2.Config
	tok     *tokenFile
	ts      oauth2.TokenSource
	lastErr string
	states  map[string]pendingState
}

// New loads the OAuth client and token if present; missing files are not
// an error (the UI shows what to do).
func New(credPath, tokenPath string) (*Manager, error) {
	m := &Manager{credPath: credPath, tokenPath: tokenPath, states: map[string]pendingState{}, httpClient: baseClient()}
	if err := m.loadCredentials(); err != nil && !errors.Is(err, ErrNoCredentials) {
		return nil, err
	}
	data, err := os.ReadFile(tokenPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var t tokenFile
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", tokenPath, err)
		}
		if t.RefreshToken != "" {
			m.setToken(&t)
		}
	}
	return m, nil
}

// baseClient has connection-level timeouts but no overall deadline, so
// large uploads are not cut off.
func baseClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.ResponseHeaderTimeout = 5 * time.Minute
	tr.IdleConnTimeout = 60 * time.Second
	return &http.Client{Transport: tr}
}

// loadCredentials accepts {"client_id","client_secret"} or the JSON file
// Google Cloud Console downloads for a Desktop client ({"installed":{...}}).
func (m *Manager) loadCredentials() error {
	data, err := os.ReadFile(m.credPath)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoCredentials
	}
	if err != nil {
		return err
	}
	type creds struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	var c struct {
		creds
		Installed *creds `json:"installed"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("%s: %w", m.credPath, err)
	}
	if c.Installed != nil {
		c.creds = *c.Installed
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf("%s: client_id/client_secret missing", m.credPath)
	}
	m.conf = &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  RedirectURL,
		Scopes:       []string{Scope},
	}
	return nil
}

func (m *Manager) ctx() context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient, m.httpClient)
}

// setToken installs t; m.mu must be held or m not yet shared.
func (m *Manager) setToken(t *tokenFile) {
	m.tok, m.lastErr, m.ts = t, "", nil
	if m.conf != nil {
		src := m.conf.TokenSource(m.ctx(), &oauth2.Token{RefreshToken: t.RefreshToken})
		m.ts = oauth2.ReuseTokenSource(nil, watchSource{m: m, src: src})
	}
}

// watchSource notices a revoked or expired refresh token.
type watchSource struct {
	m   *Manager
	src oauth2.TokenSource
}

func (w watchSource) Token() (*oauth2.Token, error) {
	t, err := w.src.Token()
	if err != nil && IsInvalidGrant(err) {
		w.m.mu.Lock()
		w.m.lastErr = "Google ปฏิเสธ token (invalid_grant) — สิทธิ์อาจถูกเพิกถอน กรุณากด “เชื่อมต่อบัญชี Google” อีกครั้ง"
		w.m.mu.Unlock()
	}
	return t, err
}

// IsInvalidGrant reports whether err means the refresh token is no longer
// valid and the user must reconnect.
func IsInvalidGrant(err error) bool {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return re.ErrorCode == "invalid_grant" || strings.Contains(string(re.Body), "invalid_grant")
	}
	return err != nil && strings.Contains(err.Error(), "invalid_grant")
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{HasCredentials: m.conf != nil, Error: m.lastErr}
	if m.tok != nil {
		s.Connected = m.conf != nil && m.lastErr == ""
		s.Email = m.tok.Email
	}
	return s
}

// HTTPClient returns an authorised client for Drive API calls.
func (m *Manager) HTTPClient() (*http.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conf == nil {
		return nil, ErrNoCredentials
	}
	if m.tok == nil || m.ts == nil {
		return nil, ErrNotConnected
	}
	if m.lastErr != "" {
		return nil, errors.New(m.lastErr)
	}
	return oauth2.NewClient(m.ctx(), m.ts), nil
}

// AuthURL starts a connect flow. returnTo is where the browser is sent
// after the callback.
func (m *Manager) AuthURL(returnTo string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conf == nil {
		// credentials.json may have been added after start-up.
		if err := m.loadCredentials(); err != nil {
			return "", err
		}
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	now := time.Now()
	for k, v := range m.states {
		if now.After(v.expires) {
			delete(m.states, k)
		}
	}
	m.states[state] = pendingState{returnTo: returnTo, expires: now.Add(15 * time.Minute)}
	return m.conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce), nil
}

// Exchange completes the flow: it checks state, trades the code for a
// token, looks up the account email and saves token.json.
func (m *Manager) Exchange(ctx context.Context, state, code string) (returnTo string, err error) {
	m.mu.Lock()
	st, ok := m.states[state]
	delete(m.states, state)
	conf := m.conf
	m.mu.Unlock()
	if !ok || time.Now().After(st.expires) || conf == nil {
		return "", errors.New("ลิงก์เชื่อมต่อหมดอายุหรือไม่ถูกต้อง กรุณาลองใหม่")
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, m.httpClient)
	tok, err := conf.Exchange(ctx, code)
	if err != nil {
		return st.returnTo, fmt.Errorf("แลก code กับ Google ไม่สำเร็จ: %w", err)
	}
	if tok.RefreshToken == "" {
		return st.returnTo, errors.New("Google ไม่ได้ส่ง refresh token กลับมา กรุณาลองใหม่")
	}
	email, err := fetchEmail(conf.Client(ctx, tok))
	if err != nil {
		email = "" // not essential; shown as unknown
	}
	t := &tokenFile{RefreshToken: tok.RefreshToken, Email: email, Created: time.Now()}
	if err := config.WriteJSON(m.tokenPath, t, 0o600); err != nil {
		return st.returnTo, err
	}
	m.mu.Lock()
	m.setToken(t)
	m.mu.Unlock()
	return st.returnTo, nil
}

// fetchEmail asks Drive who the user is (works with the drive scope alone).
func fetchEmail(c *http.Client) (string, error) {
	resp, err := c.Get("https://www.googleapis.com/drive/v3/about?fields=user(emailAddress)")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("about: %s", resp.Status)
	}
	var v struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.User.EmailAddress, nil
}

// Disconnect forgets the local token. It deliberately does not call
// Google's revoke endpoint: revocation applies to the whole grant for this
// OAuth client, which other tools on the laptop share.
func (m *Manager) Disconnect() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tok, m.ts, m.lastErr = nil, nil, ""
	if err := os.Remove(m.tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
