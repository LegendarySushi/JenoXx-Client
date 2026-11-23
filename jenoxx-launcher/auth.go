package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Microsoft-Anmeldung per Device-Code-Flow -> Xbox Live -> XSTS -> Minecraft Services.
// Es wird eine eigene Azure-App (Client-ID) benoetigt, siehe README.

const (
	msDeviceURL    = "https://login.microsoftonline.com/consumers/oauth2/v2.0/devicecode"
	msAuthorizeURL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/authorize"
	msTokenURL     = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
	msScope        = "XboxLive.signin offline_access"
	xblURL         = "https://user.auth.xboxlive.com/user/authenticate"
	xstsURL        = "https://xsts.auth.xboxlive.com/xsts/authorize"
	mcLoginURL     = "https://api.minecraftservices.com/authentication/login_with_xbox"
	mcProfURL      = "https://api.minecraftservices.com/minecraft/profile"
)

// Optional beim Bauen eingebettete Client-ID (client_id.txt). Dann muessen Nutzer nichts einrichten.
//
//go:embed client_id.txt
var embeddedClientID string

var reClientID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ---------- Konfiguration + gespeichertes Konto ----------

type config struct {
	ClientID string `json:"clientId"`
}

type accountFile struct {
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	Refresh string `json:"refresh"` // base64(DPAPI(refresh_token))
}

type authState struct {
	mu        sync.Mutex
	clientID  string
	name      string
	uuid      string
	refresh   string // MS-Refresh-Token (Klartext nur im Speicher)
	mcToken   string
	mcExpires time.Time
	loggingIn bool
	fixedID   bool // Client-ID kommt vom Programm, nicht vom Nutzer
}

var auth = &authState{}

func authDir() (string, error) { return workDir() }

func (a *authState) load() {
	a.mu.Lock()
	defer a.mu.Unlock()
	dir, err := authDir()
	if err != nil {
		return
	}
	if data, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		var c config
		if json.Unmarshal(data, &c) == nil {
			a.clientID = c.ClientID
		}
	}
	// Reihenfolge: eingebettet < client_id.txt neben der .exe < Umgebungsvariable
	if v := strings.TrimSpace(embeddedClientID); reClientID.MatchString(v) {
		a.clientID, a.fixedID = v, true
	}
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "client_id.txt")); err == nil {
			if v := strings.TrimSpace(string(b)); reClientID.MatchString(v) {
				a.clientID, a.fixedID = v, true
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("JENOXX_CLIENT_ID")); reClientID.MatchString(v) {
		a.clientID, a.fixedID = v, true
	}
	data, err := os.ReadFile(filepath.Join(dir, "account.json"))
	if err != nil {
		return
	}
	var f accountFile
	if json.Unmarshal(data, &f) != nil || f.Refresh == "" {
		return
	}
	enc, err := base64.StdEncoding.DecodeString(f.Refresh)
	if err != nil {
		return
	}
	plain, err := unprotectData(enc)
	if err != nil {
		return
	}
	a.name, a.uuid, a.refresh = f.Name, f.UUID, string(plain)
}

func (a *authState) saveAccountLocked() error {
	dir, err := authDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "account.json")
	if a.refresh == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	enc, err := protectData([]byte(a.refresh))
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(accountFile{Name: a.name, UUID: a.uuid, Refresh: base64.StdEncoding.EncodeToString(enc)}, "", "  ")
	return os.WriteFile(path, out, 0o600)
}

func (a *authState) setClientID(id string) error {
	id = strings.TrimSpace(id)
	if a.fixedID {
		return nil
	}
	if !reClientID.MatchString(id) {
		return errors.New("Die Client-ID muss eine GUID sein (z. B. 12345678-abcd-1234-abcd-1234567890ab)")
	}
	dir, err := authDir()
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out, _ := json.MarshalIndent(config{ClientID: id}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), out, 0o600); err != nil {
		return err
	}
	if a.clientID != id { // andere App -> altes Konto ungueltig
		a.refresh, a.name, a.uuid, a.mcToken = "", "", "", ""
		_ = a.saveAccountLocked()
	}
	a.clientID = id
	return nil
}

type accountInfo struct {
	ClientIDSet bool   `json:"clientIdSet"`
	LoggedIn    bool   `json:"loggedIn"`
	Name        string `json:"name"`
	UUID        string `json:"uuid"`
	LoggingIn   bool   `json:"loggingIn"`
	Fixed       bool   `json:"fixed"`
}

func (a *authState) info() accountInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return accountInfo{ClientIDSet: a.clientID != "", LoggedIn: a.refresh != "", Name: a.name, UUID: a.uuid, LoggingIn: a.loggingIn, Fixed: a.fixedID}
}

func (a *authState) logout() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh, a.name, a.uuid, a.mcToken = "", "", "", ""
	_ = a.saveAccountLocked()
}

// ---------- HTTP-Helfer ----------

var authClient = &http.Client{Timeout: 30 * time.Second, CheckRedirect: secureRedirect}

func postForm(u string, form url.Values, out any) (int, error) {
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	return doJSON(req, out)
}

func postJSON(u string, body any, out any) (int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	return doJSON(req, out)
}

func doJSON(req *http.Request, out any) (int, error) {
	resp, err := authClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil && len(data) > 0 {
		_ = json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

// ---------- Device-Code-Anmeldung ----------

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

func (a *authState) startDeviceLogin() (*DeviceCode, error) {
	a.mu.Lock()
	cid := a.clientID
	if cid == "" {
		a.mu.Unlock()
		return nil, errors.New("Zuerst die Client-ID deiner Azure-App eintragen")
	}
	if a.loggingIn {
		a.mu.Unlock()
		return nil, errors.New("Anmeldung laeuft bereits")
	}
	a.loggingIn = true
	a.mu.Unlock()

	var dc DeviceCode
	code, err := postForm(msDeviceURL, url.Values{"client_id": {cid}, "scope": {msScope}}, &dc)
	if err != nil || code != 200 || dc.DeviceCode == "" {
		a.setLoggingIn(false)
		if err == nil {
			err = fmt.Errorf("Microsoft antwortet mit HTTP %d: %s %s", code, dc.Error, dc.ErrorDesc)
		}
		return nil, err
	}
	go a.pollDevice(cid, dc)
	return &dc, nil
}

func (a *authState) setLoggingIn(b bool) {
	a.mu.Lock()
	a.loggingIn = b
	a.mu.Unlock()
}

func (a *authState) pollDevice(cid string, dc DeviceCode) {
	defer a.setLoggingIn(false)
	interval := time.Duration(dc.Interval) * time.Second
	if interval < 2*time.Second {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		var tok struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Error        string `json:"error"`
			ErrorDesc    string `json:"error_description"`
		}
		_, err := postForm(msTokenURL, url.Values{
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":  {cid}, "device_code": {dc.DeviceCode},
		}, &tok)
		if err != nil {
			continue
		}
		switch tok.Error {
		case "":
			if err := a.finishLogin(tok.AccessToken, tok.RefreshToken); err != nil {
				ui.emit(event{Type: "account", OK: false, Text: err.Error()})
			} else {
				ui.emit(event{Type: "account", OK: true, Text: a.info().Name})
			}
			return
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			ui.emit(event{Type: "account", OK: false, Text: tok.Error + ": " + tok.ErrorDesc})
			return
		}
	}
	ui.emit(event{Type: "account", OK: false, Text: "Der Code ist abgelaufen. Bitte erneut anmelden."})
}

// finishLogin: MS-Token -> Xbox -> XSTS -> Minecraft; speichert Konto.
func (a *authState) finishLogin(msAccess, msRefresh string) error {
	mcToken, expires, name, uuid, err := minecraftLogin(msAccess)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if msRefresh != "" {
		a.refresh = msRefresh
	}
	a.name, a.uuid, a.mcToken, a.mcExpires = name, uuid, mcToken, expires
	return a.saveAccountLocked()
}

func minecraftLogin(msAccess string) (token string, expires time.Time, name, uuid string, err error) {
	var xbl struct {
		Token         string `json:"Token"`
		DisplayClaims struct {
			Xui []struct {
				Uhs string `json:"uhs"`
			} `json:"xui"`
		} `json:"DisplayClaims"`
	}
	code, err := postJSON(xblURL, map[string]any{
		"Properties":   map[string]string{"AuthMethod": "RPS", "SiteName": "user.auth.xboxlive.com", "RpsTicket": "d=" + msAccess},
		"RelyingParty": "http://auth.xboxlive.com", "TokenType": "JWT",
	}, &xbl)
	if err != nil {
		return "", time.Time{}, "", "", err
	}
	if code != 200 || xbl.Token == "" {
		return "", time.Time{}, "", "", fmt.Errorf("Xbox-Live-Anmeldung fehlgeschlagen (HTTP %d)", code)
	}

	var xsts struct {
		Token         string `json:"Token"`
		XErr          int64  `json:"XErr"`
		DisplayClaims struct {
			Xui []struct {
				Uhs string `json:"uhs"`
			} `json:"xui"`
		} `json:"DisplayClaims"`
	}
	code, err = postJSON(xstsURL, map[string]any{
		"Properties":   map[string]any{"SandboxId": "RETAIL", "UserTokens": []string{xbl.Token}},
		"RelyingParty": "rp://api.minecraftservices.com/", "TokenType": "JWT",
	}, &xsts)
	if err != nil {
		return "", time.Time{}, "", "", err
	}
	if code != 200 {
		return "", time.Time{}, "", "", errors.New(xstsError(xsts.XErr, code))
	}
	if len(xsts.DisplayClaims.Xui) == 0 {
		return "", time.Time{}, "", "", errors.New("XSTS lieferte keine Benutzerdaten")
	}
	uhs := xsts.DisplayClaims.Xui[0].Uhs

	var mc struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorMsg    string `json:"errorMessage"`
	}
	code, err = postJSON(mcLoginURL, map[string]string{"identityToken": "XBL3.0 x=" + uhs + ";" + xsts.Token}, &mc)
	if err != nil {
		return "", time.Time{}, "", "", err
	}
	if code != 200 || mc.AccessToken == "" {
		if code == 403 {
			return "", time.Time{}, "", "", errors.New("Minecraft Services lehnt die Azure-App ab (HTTP 403, \"Invalid app registration\"). Die App muss bei Mojang freigeschaltet sein: https://aka.ms/AppRegInfo")
		}
		return "", time.Time{}, "", "", fmt.Errorf("Minecraft-Anmeldung fehlgeschlagen (HTTP %d) %s %s", code, mc.Error, mc.ErrorMsg)
	}

	req, _ := http.NewRequest(http.MethodGet, mcProfURL, nil)
	req.Header.Set("Authorization", "Bearer "+mc.AccessToken)
	req.Header.Set("User-Agent", userAgent)
	var prof struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	code, err = doJSON(req, &prof)
	if err != nil {
		return "", time.Time{}, "", "", err
	}
	if code == 404 {
		return "", time.Time{}, "", "", errors.New("Dieses Microsoft-Konto besitzt kein Minecraft: Java Edition")
	}
	if code != 200 || prof.ID == "" {
		return "", time.Time{}, "", "", fmt.Errorf("Minecraft-Profil nicht abrufbar (HTTP %d)", code)
	}
	exp := time.Now().Add(time.Duration(mc.ExpiresIn) * time.Second)
	if mc.ExpiresIn <= 0 {
		exp = time.Now().Add(20 * time.Hour)
	}
	return mc.AccessToken, exp, prof.Name, prof.ID, nil
}

func xstsError(x int64, code int) string {
	switch x {
	case 2148916233:
		return "Dieses Microsoft-Konto hat kein Xbox-Konto. Erstelle eines auf xbox.com."
	case 2148916235:
		return "Xbox Live ist in deinem Land nicht verfuegbar."
	case 2148916236, 2148916237:
		return "Das Konto braucht eine Altersverifizierung bei Xbox."
	case 2148916238:
		return "Kinderkonto: Ein Erwachsener muss das Konto zu einer Xbox-Familie hinzufuegen."
	}
	return fmt.Sprintf("Xbox-Autorisierung fehlgeschlagen (HTTP %d, XErr %d)", code, x)
}

// session liefert einen gueltigen Minecraft-Token (erneuert ihn bei Bedarf).
type session struct{ Name, UUID, Token string }

func (a *authState) session() (session, error) {
	a.mu.Lock()
	cid, refresh := a.clientID, a.refresh
	if refresh == "" || cid == "" {
		a.mu.Unlock()
		return session{}, errors.New("Nicht angemeldet. Bitte zuerst mit Microsoft anmelden.")
	}
	if a.mcToken != "" && time.Until(a.mcExpires) > 5*time.Minute {
		s := session{a.name, a.uuid, a.mcToken}
		a.mu.Unlock()
		return s, nil
	}
	a.mu.Unlock()

	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	code, err := postForm(msTokenURL, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {cid}, "refresh_token": {refresh}, "scope": {msScope},
	}, &tok)
	if err != nil {
		return session{}, fmt.Errorf("Anmeldung konnte nicht erneuert werden (Internet?): %w", err)
	}
	if code != 200 || tok.AccessToken == "" {
		return session{}, fmt.Errorf("Anmeldung abgelaufen - bitte neu anmelden (%s)", tok.Error)
	}
	if err := a.finishLogin(tok.AccessToken, tok.RefreshToken); err != nil {
		return session{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return session{a.name, a.uuid, a.mcToken}, nil
}

// ---------- Anmeldung im Fenster (Authorization Code + PKCE, Loopback-Weiterleitung) ----------

func randomURLString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func newPKCE() (verifier, challenge string) {
	verifier = randomURLString(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func buildAuthorizeURL(cid, redirect, state, challenge string) string {
	q := url.Values{
		"client_id":             {cid},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"response_mode":         {"query"},
		"scope":                 {msScope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return msAuthorizeURL + "?" + q.Encode()
}

const callbackPage = `<!DOCTYPE html><html lang="de"><meta charset="utf-8"><title>JenoXx Client</title>
<body style="margin:0;height:100vh;display:grid;place-items:center;background:#0b0e13;color:#e8edf3;font:16px 'Segoe UI',sans-serif;text-align:center">
<div><div style="font-size:42px">%s</div><h2 style="margin:8px 0">%s</h2><p style="color:#8b98a9">%s</p></div>
<script>setTimeout(function(){window.close()},1500)</script></body></html>`

// loginCallback verarbeitet die Weiterleitung von Microsoft. onDone wird genau einmal aufgerufen.
func (a *authState) loginCallback(cid, redirect, state, verifier string, onDone func(ok bool, msg string)) http.HandlerFunc {
	var once sync.Once
	var okFinal bool
	var msgFinal string
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "Ungueltiger Anmeldestatus", http.StatusBadRequest)
			return
		}
		once.Do(func() {
			switch {
			case q.Get("error") != "":
				okFinal, msgFinal = false, "Anmeldung abgebrochen: "+q.Get("error_description")
				if q.Get("error_description") == "" {
					msgFinal = "Anmeldung abgebrochen: " + q.Get("error")
				}
			case q.Get("code") == "":
				okFinal, msgFinal = false, "Microsoft hat keinen Code geliefert"
			default:
				var tok struct {
					AccessToken  string `json:"access_token"`
					RefreshToken string `json:"refresh_token"`
					Error        string `json:"error"`
					ErrorDesc    string `json:"error_description"`
				}
				code, err := postForm(msTokenURL, url.Values{
					"grant_type": {"authorization_code"}, "client_id": {cid}, "code": {q.Get("code")},
					"redirect_uri": {redirect}, "code_verifier": {verifier}, "scope": {msScope},
				}, &tok)
				switch {
				case err != nil:
					okFinal, msgFinal = false, err.Error()
				case code != 200 || tok.AccessToken == "":
					okFinal, msgFinal = false, "Microsoft lehnt die Anmeldung ab: "+tok.Error+" "+tok.ErrorDesc
				default:
					if err := a.finishLogin(tok.AccessToken, tok.RefreshToken); err != nil {
						okFinal, msgFinal = false, err.Error()
					} else {
						okFinal, msgFinal = true, a.info().Name
					}
				}
			}
			onDone(okFinal, msgFinal)
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if okFinal {
			fmt.Fprintf(w, callbackPage, "&#10003;", "Angemeldet", "Du kannst dieses Fenster schliessen.")
		} else {
			fmt.Fprintf(w, callbackPage, "&#10005;", "Anmeldung fehlgeschlagen", msgFinal)
		}
	}
}

// startBrowserLogin oeffnet das Microsoft-Anmeldefenster (open) und wartet im Hintergrund auf die Weiterleitung.
func (a *authState) startBrowserLogin(open func(u string)) error {
	a.mu.Lock()
	cid := a.clientID
	if cid == "" {
		a.mu.Unlock()
		return errors.New("Zuerst die Client-ID deiner Azure-App eintragen")
	}
	if a.loggingIn {
		a.mu.Unlock()
		return errors.New("Anmeldung laeuft bereits")
	}
	a.loggingIn = true
	a.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		a.setLoggingIn(false)
		return err
	}
	redirect := fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)
	verifier, challenge := newPKCE()
	state := randomURLString(16)

	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	var closeOnce sync.Once
	finish := func(ok bool, msg string) {
		closeOnce.Do(func() {
			a.setLoggingIn(false)
			ui.emit(event{Type: "account", OK: ok, Text: msg})
			go func() { time.Sleep(2 * time.Second); _ = srv.Close() }()
		})
	}
	srv.Handler = a.loginCallback(cid, redirect, state, verifier, finish)
	go func() { _ = srv.Serve(ln) }()
	go func() {
		time.Sleep(5 * time.Minute)
		finish(false, "Zeitueberschreitung bei der Anmeldung. Bitte erneut versuchen.")
	}()
	open(buildAuthorizeURL(cid, redirect, state, challenge))
	return nil
}
