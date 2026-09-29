package provider

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// kiroFakeSignIn answers Kiro's auth service and AWS's sign-in, checking
// what the IDE would send; it returns the sign-in's page and a browser that
// doesn't follow redirects.
func kiroFakeSignIn(t *testing.T) (start func() (SignInState, url.Values), browser *http.Client) {
	t.Helper()
	kiroSandbox(t)
	var challenge, redirect string
	var awsChallenge string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		str := func(k string) string { s, _ := in[k].(string); return s }
		verify := func(verifier, want string) bool {
			sum := sha256.Sum256([]byte(verifier))
			return base64.RawURLEncoding.EncodeToString(sum[:]) == want
		}
		switch r.URL.Path {
		case "/oauth/token": // Kiro's, for Google and GitHub
			if str("code") != "gcode" || !verify(str("code_verifier"), challenge) || str("redirect_uri") != redirect+"/oauth/callback?login_option=google" ||
				!strings.HasPrefix(r.Header.Get("User-Agent"), "KiroIDE-") {
				t.Errorf("Kiro token request %v", in)
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"accessToken": "social-at", "refreshToken": "social-rt", "profileArn": "arn:aws:codewhisperer:us-east-1:1:profile/P", "expiresIn": 3600})
		case "/client/register":
			if in["issuerUrl"] != "https://view.awsapps.com/start" || in["clientType"] != "public" {
				t.Errorf("register %v", in)
			}
			json.NewEncoder(w).Encode(map[string]any{"clientId": "cid", "clientSecret": "csecret", "clientSecretExpiresAt": time.Now().Add(90 * 24 * time.Hour).Unix()})
		case "/token": // AWS's
			if str("grantType") != "authorization_code" || str("code") != "awscode" || str("clientId") != "cid" || str("clientSecret") != "csecret" ||
				!verify(str("codeVerifier"), awsChallenge) || !strings.HasPrefix(str("redirectUri"), "http://127.0.0.1:") {
				t.Errorf("AWS token request %v", in)
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"accessToken": "idc-at", "refreshToken": "idc-rt", "expiresIn": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	auth, oidc, ports, ask := kiroAuthService, kiroOIDC, kiroCallbackPorts, askKiroIdentity
	kiroAuthService, kiroOIDC, kiroCallbackPorts = srv.URL, func(string) string { return srv.URL }, []int{0}
	askKiroIdentity = func(string) (string, string) {
		if c, ok := readKiro(""); ok && c.access == "social-at" {
			return "me@example.com", "KIRO PRO"
		}
		return "", ""
	}
	t.Cleanup(func() { kiroAuthService, kiroOIDC, kiroCallbackPorts, askKiroIdentity = auth, oidc, ports, ask })
	browser = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start = func() (SignInState, url.Values) {
		st, err := StartSignIn("kiro")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { CancelSignIn(st.ID) })
		u, _ := url.Parse(st.URL)
		q := u.Query()
		if !strings.HasPrefix(st.URL, "https://app.kiro.dev/signin?") || q.Get("redirect_from") != "KiroIDE" || q.Get("code_challenge_method") != "S256" {
			t.Fatalf("page %s", st.URL)
		}
		challenge, redirect = q.Get("code_challenge"), q.Get("redirect_uri")
		return st, q
	}
	kiroSetAWSChallenge = func(c string) { awsChallenge = c }
	return start, browser
}

// kiroSetAWSChallenge tells the fake AWS the challenge its page was sent.
var kiroSetAWSChallenge func(string)

// Signed in with Google on Kiro's page, the account is magpie's own and
// the one used, ahead of kiro-cli's, which is left as it was.
func TestKiroSignInWithGoogle(t *testing.T) {
	start, browser := kiroFakeSignIn(t)
	writeKiroCLI(t, map[string]any{"kirocli:social:token": map[string]any{"access_token": "cli-at", "expires_at": "2099-01-01T00:00:00Z"}})
	st, q := start()
	res, err := browser.Get(q.Get("redirect_uri") + "/oauth/callback?login_option=google&code=gcode&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if st, _ = SignInStatus(st.ID); st.State != "done" || st.User != "me@example.com" || st.Plan != "KIRO PRO" || !st.Using {
		t.Fatalf("status %+v", st)
	}
	c, ok := readKiro("")
	if !ok || c.access != "social-at" || c.refresh != "social-rt" || c.method != "social" || c.profile != "arn:aws:codewhisperer:us-east-1:1:profile/P" || !c.fresh() {
		t.Fatalf("cred %+v", c)
	}
	if row := readKiroRow(t, "kirocli:social:token"); row["access_token"] != "cli-at" {
		t.Fatalf("kiro-cli's sign-in changed: %v", row)
	}
	if p, ok := kiroAccount(); !ok || p.Account.User != "me@example.com" {
		t.Fatalf("account %+v", p.Account)
	}
}

// Builder ID goes on from Kiro's page to AWS's, with a client registered
// for it, and comes back an AWS sign-in magpie can refresh.
func TestKiroSignInWithBuilderID(t *testing.T) {
	start, browser := kiroFakeSignIn(t)
	st, q := start()
	res, err := browser.Get(q.Get("redirect_uri") + "/signin/callback?login_option=builderid&issuer_url=" + url.QueryEscape("https://view.awsapps.com/start") +
		"&idc_region=us-east-1&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	a := loc.Query()
	if res.StatusCode != http.StatusFound || loc.Path != "/authorize" || a.Get("client_id") != "cid" || a.Get("response_type") != "code" ||
		!strings.Contains(a.Get("scopes"), "codewhisperer:conversations") || !strings.HasSuffix(a.Get("redirect_uri"), "/oauth/callback") {
		t.Fatalf("on to AWS: %d %s", res.StatusCode, loc)
	}
	kiroSetAWSChallenge(a.Get("code_challenge"))
	// a stale tab's answer is turned away
	if res, err = browser.Get(a.Get("redirect_uri") + "?code=awscode&state=nope"); err == nil {
		res.Body.Close()
	}
	if st, _ = SignInStatus(st.ID); st.State != "waiting" {
		t.Fatalf("a stranger's answer ended it: %+v", st)
	}
	if res, err = browser.Get(a.Get("redirect_uri") + "?code=awscode&state=" + url.QueryEscape(a.Get("state"))); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if st, _ = SignInStatus(st.ID); st.State != "done" || st.User != "Kiro account" {
		t.Fatalf("status %+v", st)
	}
	c, ok := readKiro("")
	if !ok || c.access != "idc-at" || c.method != "idc" || c.clientID != "cid" || c.clientSecret != "csecret" || c.region != "us-east-1" {
		t.Fatalf("cred %+v", c)
	}
}

// A company's own identity provider is left to kiro-cli, and said so.
func TestKiroSignInExternalIdP(t *testing.T) {
	start, browser := kiroFakeSignIn(t)
	st, q := start()
	res, err := browser.Get(q.Get("redirect_uri") + "/oauth/callback?login_option=external_idp&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if st, _ = SignInStatus(st.ID); st.State != "failed" || !strings.Contains(st.Error, "kiro-cli login") {
		t.Fatalf("status %+v", st)
	}
}
