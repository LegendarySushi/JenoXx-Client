package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed ui/index.html
var indexHTML []byte

//go:embed ui/logo.png
var logoPNG []byte

// ---------- Ereignis-Hub (Log/Schritte/Fortschritt -> Browserfenster) ----------

type event struct {
	Type    string  `json:"type"` // log | step | progress | reset | done | account | game
	ID      int     `json:"id,omitempty"`
	State   string  `json:"state,omitempty"` // run | done | error
	Text    string  `json:"text,omitempty"`
	Percent float64 `json:"percent"`
	OK      bool    `json:"ok,omitempty"`
}

type hub struct {
	mu      sync.Mutex
	subs    map[chan event]struct{}
	history []event
}

var ui = &hub{subs: map[chan event]struct{}{}}

func (h *hub) emit(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch e.Type {
	case "log", "step", "done":
		h.history = append(h.history, e)
		if len(h.history) > 1500 {
			h.history = h.history[len(h.history)-1500:]
		}
	case "reset":
		h.history = nil
	}
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (h *hub) subscribe() (chan event, []event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan event, 512)
	h.subs[ch] = struct{}{}
	return ch, append([]event(nil), h.history...)
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
}

func (h *hub) log(text string) { h.emit(event{Type: "log", Text: text}) }
func (h *hub) step(id int, state, text string) {
	h.emit(event{Type: "step", ID: id, State: state, Text: text})
}

// progress: percent >= 0 bestimmt, -1 unbestimmt, -2 ausblenden.
func (h *hub) progress(percent float64, label string) {
	h.emit(event{Type: "progress", Percent: percent, Text: label})
}

func logln(a ...any) {
	text := strings.TrimSpace(fmt.Sprint(a...))
	if text == "" {
		return
	}
	ui.log(text)
	_, _ = fmt.Fprintln(os.Stdout, text)
}

// ---------- HTTP-Server ----------

type server struct {
	token string
	addr  string

	mu       sync.Mutex
	busy     bool
	subCount int
	everConn bool
	zeroAt   time.Time
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("kein Zufall verfuegbar: " + err.Error()) // lieber abbrechen als einen erratbaren Token nutzen
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) setBusy(b bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b && s.busy {
		return false
	}
	s.busy = b
	return true
}

func (s *server) isBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

func (s *server) connChange(delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subCount += delta
	if delta > 0 {
		s.everConn = true
	}
	if s.subCount == 0 {
		s.zeroAt = time.Now()
	}
}

// watchdog beendet das Programm, wenn das Fenster geschlossen wurde.
func (s *server) watchdog(started time.Time) {
	for range time.Tick(2 * time.Second) {
		s.mu.Lock()
		idle := s.subCount == 0 && !s.busy
		gone := s.everConn && idle && time.Since(s.zeroAt) > 8*time.Second
		never := !s.everConn && time.Since(started) > 2*time.Minute
		s.mu.Unlock()
		if gone || never {
			os.Exit(0)
		}
	}
}

func (s *server) handler() http.Handler {
	base := "/" + s.token + "/"
	get := map[string]http.HandlerFunc{
		"events":          s.events,
		"api/state":       s.state,
		"api/versions":    s.versions,
		"api/mods":        s.modsList,
		"api/account":     s.accountState,
		"api/health":      s.healthHandler,
		"api/diagnose":    s.diagnoseHandler,
		"api/mods/hashes": s.modsHashes,
		"api/cf/status":   s.cfStatus,
		"api/avatar":      s.avatar,
		"api/cf/search":   s.cfSearch,
	}
	post := map[string]http.HandlerFunc{
		"api/profile/create":    s.profileCreate,
		"api/profile/duplicate": s.profileDuplicate,
		"api/profile/official":  s.profileOfficial,
		"api/profile/update":    s.profileUpdate,
		"api/profile/delete":    s.profileDelete,
		"api/profile/install":   s.profileInstall,
		"api/profile/play":      s.profilePlay,
		"api/profile/stop":      s.profileStop,
		"api/account/clientid":  s.accountClientID,
		"api/account/login":     s.accountLogin,
		"api/account/logout":    s.accountLogout,
		"api/openurl":           s.openURL,
		"api/mods/toggle":       s.modsToggle,
		"api/mods/delete":       s.modsDelete,
		"api/mods/install":      s.modsInstall,
		"api/mods/upload":       s.modsUpload,
		"api/openfolder":        s.openFolder,
		"api/crash/clear":       s.crashClear,
		"api/modrinth":          s.modrinthProxy,
		"api/cf/key":            s.cfSetKey,
		"api/cf/install":        s.cfInstallHandler,
		"api/modpack/import":    s.modpackImport,
		"api/modpack/upload":    s.modpackUpload,
		"api/quit":              s.quit,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		if r.Host != s.addr || len(r.URL.Path) < len(base) || !tokenOK(r.URL.Path[:len(base)], base) {
			http.NotFound(w, r)
			return
		}
		if !sameOrigin(r, s.addr) {
			http.Error(w, "Anfrage von fremder Herkunft abgelehnt", http.StatusForbidden)
			return
		}
		rel := r.URL.Path[len(base):]
		switch {
		case rel == "":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(indexHTML)
		case rel == "logo.png":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "max-age=3600")
			_, _ = w.Write(logoPNG)
		default:
			if h, ok := get[rel]; ok && r.Method == http.MethodGet {
				h(w, r)
				return
			}
			if h, ok := post[rel]; ok {
				if r.Method != http.MethodPost {
					http.Error(w, "POST erforderlich", http.StatusMethodNotAllowed)
					return
				}
				if rel != "api/mods/upload" && rel != "api/modpack/upload" {
					r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
				}
				h(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// profileFrom liest die Profil-ID aus Body-Struktur oder Query.
func profileByID(id string) (Profile, error) {
	p, ok := profiles.get(id)
	if !ok {
		return Profile{}, errors.New("Profil nicht gefunden")
	}
	return p, nil
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "kein Streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch, hist := ui.subscribe()
	defer ui.unsubscribe(ch)
	s.connChange(1)
	defer s.connChange(-1)

	send := func(e event) {
		b, _ := json.Marshal(e)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	for _, e := range hist {
		send(e)
	}
	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			send(e)
		case <-ping.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

type profileView struct {
	Profile
	Installed bool       `json:"installed"`
	ModCount  int        `json:"modCount"`
	OwnMod    bool       `json:"ownMod"`
	Running   bool       `json:"running"`
	Crash     *crashDiag `json:"crash,omitempty"`
}

func viewOf(mcDir string, p Profile) profileView {
	v := profileView{Profile: p, Installed: installedOnDisk(mcDir, p), OwnMod: ownModSupported(p.MC) && p.Pack == "", Running: isRunning(p.ID)}
	if d, ok := crashOf(p.ID); ok {
		v.Crash = &d
	}
	if dir, err := modsDirOf(p); err == nil {
		for _, m := range listMods(dir) {
			if m.Enabled {
				v.ModCount++
			}
		}
	}
	return v
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	mcDir, _ := minecraftDir()
	views := []profileView{}
	for _, p := range profiles.all() {
		views = append(views, viewOf(mcDir, p))
	}
	writeJSON(w, 200, map[string]any{
		"app":      appName,
		"ownMC":    mcVersion,
		"mcDir":    mcDir,
		"busy":     s.isBusy(),
		"windows":  runtime.GOOS == "windows",
		"sysRamMB": totalRAMMB(),
		"profiles": views,
	})
}

func (s *server) versions(w http.ResponseWriter, r *http.Request) {
	v, err := fabricGameVersions()
	if err != nil {
		fail(w, 502, fmt.Errorf("Versionsliste nicht abrufbar: %w", err))
		return
	}
	writeJSON(w, 200, map[string]any{"versions": v})
}

func (s *server) profileCreate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
		MC   string `json:"mc"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	name, err := validateName(b.Name)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if !reMC.MatchString(b.MC) {
		fail(w, 400, errors.New("ungueltige Minecraft-Version"))
		return
	}
	p := Profile{ID: newProfileID(name), Name: name, MC: b.MC, RAM: 4096}
	if err := profiles.add(p); err != nil {
		fail(w, 500, err)
		return
	}
	mcDir, _ := minecraftDir()
	writeJSON(w, 200, viewOf(mcDir, p))
}

func (s *server) profileUpdate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		RAM      int    `json:"ram"`
		JavaPath string `json:"javaPath"`
		JVMArgs  string `json:"jvmArgs"`
		Server   string `json:"server"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		Full     bool   `json:"fullscreen"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	server, err := validateServer(b.Server)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := validateWindow(b.Width, b.Height); err != nil {
		fail(w, 400, err)
		return
	}
	name, err := validateName(b.Name)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := validateSettings(b.RAM, b.JavaPath); err != nil {
		fail(w, 400, err)
		return
	}
	if err := validateJVMArgs(b.JVMArgs); err != nil {
		fail(w, 400, err)
		return
	}
	p, ok := profiles.update(b.ID, func(p *Profile) {
		p.Name, p.RAM = name, b.RAM
		p.JavaPath, p.JVMArgs = strings.TrimSpace(b.JavaPath), strings.TrimSpace(b.JVMArgs)
		p.Server, p.Width, p.Height, p.Fullscreen = server, b.Width, b.Height, b.Full
	})
	if !ok {
		fail(w, 404, errors.New("Profil nicht gefunden"))
		return
	}
	mcDir, _ := minecraftDir()
	writeJSON(w, 200, viewOf(mcDir, p))
}

func (s *server) profileDelete(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID          string `json:"id"`
		DeleteFiles bool   `json:"deleteFiles"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	if s.isBusy() {
		fail(w, 409, errors.New("Es laeuft gerade eine Installation."))
		return
	}
	if isRunning(b.ID) {
		fail(w, 409, errors.New("Das Spiel laeuft noch - zuerst beenden."))
		return
	}
	p, ok := profiles.remove(b.ID)
	if !ok {
		fail(w, 404, errors.New("Profil nicht gefunden"))
		return
	}
	mcDir, _ := minecraftDir()
	if b.DeleteFiles {
		if err := deleteProfileFiles(mcDir, p); err != nil {
			fail(w, 500, err)
			return
		}
	}
	if pf, err := packFilePath(p.ID); err == nil {
		_ = os.Remove(pf)
	}
	clearCrash(p.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) profileInstall(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID    string `json:"id"`
		Force bool   `json:"force"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	if _, err := profileByID(b.ID); err != nil {
		fail(w, 404, err)
		return
	}
	if !s.setBusy(true) {
		fail(w, 409, errors.New("Es laeuft bereits eine Installation."))
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
	go func() {
		defer s.setBusy(false)
		err := runInstall(b.ID, b.Force)
		if err != nil {
			logln("FEHLER: " + err.Error())
			ui.emit(event{Type: "done", OK: false, Text: err.Error()})
			return
		}
		ui.emit(event{Type: "done", OK: true, Text: "Alles bereit."})
	}()
}

func (s *server) profilePlay(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID string `json:"id"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	p, err := profileByID(b.ID)
	if err != nil {
		fail(w, 404, err)
		return
	}
	mcDir, _ := minecraftDir()
	if !installedOnDisk(mcDir, p) {
		fail(w, 400, errors.New("Profil ist noch nicht installiert"))
		return
	}
	if _, err := auth.session(); err != nil {
		fail(w, 401, err)
		return
	}
	if err := launchGame(p); err != nil {
		logln("FEHLER: " + err.Error())
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) profileStop(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID string `json:"id"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	if err := stopGame(b.ID); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- Konto ----------

func (s *server) accountState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, auth.info())
}

func (s *server) accountClientID(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ClientID string `json:"clientId"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	if err := auth.setClientID(b.ClientID); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, auth.info())
}

func (s *server) accountLogin(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Device bool `json:"device"`
	}
	_ = decode(r, &b)
	if b.Device { // Ausweichverfahren: Code auf microsoft.com/link eingeben
		dc, err := auth.startDeviceLogin()
		if err != nil {
			fail(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"mode": "code", "userCode": dc.UserCode, "uri": dc.VerificationURI, "expiresIn": dc.ExpiresIn})
		return
	}
	err := auth.startBrowserLogin(func(u string) {
		if runtime.GOOS == "windows" {
			openAppWindow(u, 520, 760, "login-profile")
		} else {
			fmt.Println("Anmeldung:", u)
		}
	})
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"mode": "browser"})
}

func (s *server) accountLogout(w http.ResponseWriter, r *http.Request) {
	auth.logout()
	writeJSON(w, 200, auth.info())
}

// openURL oeffnet nur bekannte Microsoft-/Mojang-Seiten im Standardbrowser.
var openAllowed = map[string]bool{
	"www.microsoft.com": true, "microsoft.com": true, "login.microsoftonline.com": true, "login.live.com": true,
	"aka.ms": true, "portal.azure.com": true, "learn.microsoft.com": true, "help.minecraft.net": true,
	"console.curseforge.com": true, "www.curseforge.com": true, "modrinth.com": true,
	"support.curseforge.com": true,
}

func (s *server) openURL(w http.ResponseWriter, r *http.Request) {
	var b struct {
		URL string `json:"url"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	u, err := url.Parse(b.URL)
	if err != nil || u.Scheme != "https" || !openAllowed[u.Hostname()] {
		fail(w, 400, errors.New("diese Adresse darf nicht geoeffnet werden"))
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u.String()).Start()
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) modsDirFor(id string) (string, error) {
	p, err := profileByID(id)
	if err != nil {
		return "", err
	}
	return modsDirOf(p)
}

func (s *server) modsList(w http.ResponseWriter, r *http.Request) {
	dir, err := s.modsDirFor(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	mods := listMods(dir)
	fillModMeta(dir, mods)
	writeJSON(w, 200, map[string]any{"mods": mods})
}

func (s *server) modsToggle(w http.ResponseWriter, r *http.Request) {
	s.modsFileOp(w, r, toggleMod)
}

func (s *server) modsDelete(w http.ResponseWriter, r *http.Request) {
	s.modsFileOp(w, r, deleteMod)
}

func (s *server) modsFileOp(w http.ResponseWriter, r *http.Request, op func(dir, file string) error) {
	var b struct {
		ID   string `json:"id"`
		File string `json:"file"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	dir, err := s.modsDirFor(b.ID)
	if err != nil {
		fail(w, 404, err)
		return
	}
	if err := op(dir, b.File); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) modsInstall(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID       string `json:"id"`
		URL      string `json:"url"`
		Filename string `json:"filename"`
		Replace  string `json:"replace"` // beim Update: alte Datei, die ersetzt wird
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	dir, err := s.modsDirFor(b.ID)
	if err != nil {
		fail(w, 404, err)
		return
	}
	skipped, err := installModReplace(dir, b.URL, b.Filename, b.Replace)
	if err != nil {
		fail(w, 502, err)
		return
	}
	logln("Mod installiert: " + b.Filename)
	writeJSON(w, 200, map[string]bool{"ok": true, "skipped": skipped})
}

func (s *server) modsUpload(w http.ResponseWriter, r *http.Request) {
	dir, err := s.modsDirFor(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 150<<20)
	filename := r.URL.Query().Get("filename")
	skipped, err := saveUploadedModReplace(dir, filename, r.URL.Query().Get("replace"), r.Body)
	if err != nil {
		fail(w, 400, err)
		return
	}
	logln("Mod installiert: " + filename)
	writeJSON(w, 200, map[string]bool{"ok": true, "skipped": skipped})
}

func (s *server) openFolder(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID    string `json:"id"`
		Which string `json:"which"` // mods (Standard) | crash | game
	}
	_ = decode(r, &b)
	p, err := profileByID(b.ID)
	if err != nil {
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	mcDir, _ := minecraftDir()
	dir := filepath.Join(profileGameDir(mcDir, p), "mods")
	switch b.Which {
	case "crash":
		dir = filepath.Join(profileGameDir(mcDir, p), "crash-reports")
		if !exists(dir) {
			dir = profileGameDir(mcDir, p)
		}
	case "game":
		dir = profileGameDir(mcDir, p)
	}
	_ = os.MkdirAll(dir, 0o755)
	if runtime.GOOS == "windows" {
		_ = exec.Command("explorer.exe", dir).Start()
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) quit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"ok": true})
	go func() { time.Sleep(200 * time.Millisecond); os.Exit(0) }()
}

// ---------- Start ----------

func startGUI(noOpen bool) error {
	if err := profiles.load(); err != nil {
		return err
	}
	auth.load()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s := &server{token: newToken(), addr: ln.Addr().String()}
	pageURL := "http://" + s.addr + "/" + s.token + "/"

	go s.watchdog(time.Now())
	if noOpen {
		fmt.Println(pageURL)
	} else {
		openWindow(pageURL)
	}
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	return srv.Serve(ln)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// openWindow oeffnet die Oberflaeche als App-Fenster (Edge/Chrome), sonst im Standardbrowser.
func openWindow(u string) { openAppWindow(u, 1000, 740, "window-profile") }

// openAppWindow oeffnet u als eigenes App-Fenster (Edge/Chrome ohne Adressleiste).
func openAppWindow(u string, width, height int, profileName string) {
	if runtime.GOOS != "windows" {
		fmt.Println("Oeffne im Browser:", u)
		return
	}
	var cands []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if b := os.Getenv(env); b != "" {
			cands = append(cands, filepath.Join(b, "Microsoft", "Edge", "Application", "msedge.exe"))
		}
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		if b := os.Getenv(env); b != "" {
			cands = append(cands, filepath.Join(b, "Google", "Chrome", "Application", "chrome.exe"))
		}
	}
	profile := ""
	if wd, err := workDir(); err == nil {
		profile = filepath.Join(wd, profileName)
	}
	for _, c := range cands {
		if !exists(c) {
			continue
		}
		args := []string{"--app=" + u, fmt.Sprintf("--window-size=%d,%d", width, height), "--no-first-run", "--no-default-browser-check"}
		if profile != "" {
			args = append(args, "--user-data-dir="+profile)
		}
		if exec.Command(c, args...).Start() == nil {
			return
		}
	}
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

// reportFatal schreibt Startfehler in eine Datei (das Programm hat kein Konsolenfenster).
func reportFatal(err error) {
	fmt.Fprintln(os.Stderr, "Fehler:", err)
	if wd, werr := workDir(); werr == nil {
		_ = os.WriteFile(filepath.Join(wd, "error.log"), []byte(err.Error()+"\n"), 0o644)
	}
}

// ---------- Diagnose, Zustand, Updates, Modpacks ----------

func (s *server) healthHandler(w http.ResponseWriter, r *http.Request) {
	p, err := profileByID(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	mcDir, _ := minecraftDir()
	writeJSON(w, 200, map[string]any{"items": healthCheck(mcDir, p)})
}

func (s *server) diagnoseHandler(w http.ResponseWriter, r *http.Request) {
	p, err := profileByID(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	mcDir, _ := minecraftDir()
	writeJSON(w, 200, map[string]string{"text": buildDiagnostics(mcDir, p)})
}

func (s *server) crashClear(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID string `json:"id"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	clearCrash(b.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) modsHashes(w http.ResponseWriter, r *http.Request) {
	dir, err := s.modsDirFor(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"hashes": hashMods(dir)})
}

// importTemp legt eine Zwischendatei fuer einen Modpack-Import an.
func importTemp() (string, error) {
	d, err := packsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "tmp-"+newToken()[:8]+".mrpack"), nil
}

func (s *server) modpackImport(w http.ResponseWriter, r *http.Request) {
	var b struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	u, err := url.Parse(b.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "cdn.modrinth.com" || !reMrpackName.MatchString(b.Filename) {
		fail(w, 400, errors.New("nur .mrpack-Dateien von cdn.modrinth.com erlaubt"))
		return
	}
	tmp, err := importTemp()
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer os.Remove(tmp)
	if err := download(b.URL, tmp); err != nil {
		fail(w, 502, err)
		return
	}
	s.finishImport(w, tmp)
}

func (s *server) modpackUpload(w http.ResponseWriter, r *http.Request) {
	if !reMrpackName.MatchString(r.URL.Query().Get("filename")) {
		fail(w, 400, errors.New("bitte eine .mrpack-Datei waehlen"))
		return
	}
	tmp, err := importTemp()
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer os.Remove(tmp)
	r.Body = http.MaxBytesReader(w, r.Body, 400<<20)
	f, err := os.Create(tmp)
	if err != nil {
		fail(w, 500, err)
		return
	}
	_, cerr := io.Copy(f, r.Body)
	f.Close()
	if cerr != nil {
		fail(w, 400, cerr)
		return
	}
	s.finishImport(w, tmp)
}

func (s *server) finishImport(w http.ResponseWriter, tmp string) {
	p, err := importPackFile(tmp)
	if err != nil {
		fail(w, 400, err)
		return
	}
	logln("Modpack importiert: " + p.Name + " (Minecraft " + p.MC + ")")
	mcDir, _ := minecraftDir()
	writeJSON(w, 200, viewOf(mcDir, p))
}

func (s *server) profileDuplicate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID string `json:"id"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	src, err := profileByID(b.ID)
	if err != nil {
		fail(w, 404, err)
		return
	}
	mcDir, _ := minecraftDir()
	p, err := duplicateProfile(mcDir, src)
	if err != nil {
		fail(w, 500, err)
		return
	}
	logln("Profil dupliziert: " + src.Name + " -> " + p.Name)
	writeJSON(w, 200, viewOf(mcDir, p))
}
