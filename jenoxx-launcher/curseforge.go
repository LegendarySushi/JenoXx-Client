package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CurseForge als zweite Mod-Quelle. Jeder Nutzer braucht einen eigenen API-Schluessel
// (console.curseforge.com). Er wird mit Windows DPAPI verschluesselt gespeichert und
// laut CurseForge-Bedingungen nicht in die EXE eingebaut oder weitergegeben.

var cfBase = "https://api.curseforge.com/v1"

const (
	cfGameMinecraft = 432
	cfClassMods     = 6
	cfLoaderFabric  = 4
	cfFabricAPIID   = 306612
	cfRelRequired   = 3
)

var (
	cfHosts       = map[string]bool{"edge.forgecdn.net": true, "mediafilez.forgecdn.net": true, "media.forgecdn.net": true}
	cfCDNHostOK   = func(h string) bool { return cfHosts[h] }
	cfAllowHTTP   = false // nur fuer Tests
	reCFKey       = regexp.MustCompile(`^[\x21-\x7e]{20,120}$`)
	reCFPath      = regexp.MustCompile(`^/(mods/search|games/432|mods/\d{1,9}|mods/\d{1,9}/files|mods/\d{1,9}/files/\d{1,12}/download-url)$`)
	errNoCFKey    = errors.New("kein CurseForge-Schluessel hinterlegt")
	errCFDistrib  = errors.New("der Autor erlaubt keine Downloads ausserhalb von CurseForge - lade die Datei auf curseforge.com herunter und lege sie in den Mods-Ordner")
	cfKeyOverride struct {
		sync.Mutex
		set string
	}
)

func cfKeyPath() (string, error) {
	wd, err := workDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, "curseforge.key"), nil
}

// loadCFKey: Umgebungsvariable JENOXX_CF_KEY, sonst die gespeicherte (verschluesselte) Datei.
func loadCFKey() string {
	if k := strings.TrimSpace(os.Getenv("JENOXX_CF_KEY")); reCFKey.MatchString(k) {
		return k
	}
	path, err := cfKeyPath()
	if err != nil {
		return ""
	}
	enc, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	plain, err := unprotectData(enc)
	if err != nil {
		return ""
	}
	k := strings.TrimSpace(string(plain))
	if !reCFKey.MatchString(k) {
		return ""
	}
	return k
}

func saveCFKey(k string) error {
	path, err := cfKeyPath()
	if err != nil {
		return err
	}
	if k == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	enc, err := protectData([]byte(k))
	if err != nil {
		return err
	}
	return os.WriteFile(path, enc, 0o600)
}

// cfRequestKey sendet eine GET-Anfrage an die CurseForge-API (nur freigegebene Pfade).
func cfRequestKey(key, path string, q url.Values) (int, []byte, error) {
	if !reCFPath.MatchString(path) {
		return 0, nil, errors.New("diese CurseForge-Anfrage ist nicht erlaubt")
	}
	target := cfBase + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		c := httpClient
		if attempt%2 == 1 {
			c = httpClient11
		}
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("x-api-key", key)
		resp, err := c.Do(req)
		if err == nil {
			data, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if rerr == nil {
				return resp.StatusCode, data, nil
			}
			err = rerr
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 700 * time.Millisecond)
	}
	return 0, nil, fmt.Errorf("CurseForge nicht erreichbar: %w", lastErr)
}

func cfRequest(path string, q url.Values) (int, []byte, error) {
	key := loadCFKey()
	if key == "" {
		return 0, nil, errNoCFKey
	}
	return cfRequestKey(key, path, q)
}

// ---------- Datenmodell ----------

type cfFile struct {
	ID          int    `json:"id"`
	FileName    string `json:"fileName"`
	DownloadURL string `json:"downloadUrl"`
	ReleaseType int    `json:"releaseType"`
	IsAvailable bool   `json:"isAvailable"`
	FileLength  int64  `json:"fileLength"`
	Hashes      []struct {
		Value string `json:"value"`
		Algo  int    `json:"algo"`
	} `json:"hashes"`
	Dependencies []struct {
		ModID        int `json:"modId"`
		RelationType int `json:"relationType"`
	} `json:"dependencies"`
}

func (f cfFile) sha1() string {
	for _, h := range f.Hashes {
		if h.Algo == 1 {
			return strings.ToLower(h.Value)
		}
	}
	return ""
}

// cfPickFile waehlt die neueste Release-Datei (sonst die neueste verfuegbare).
func cfPickFile(files []cfFile) (cfFile, bool) {
	var best cfFile
	found := false
	better := func(a, b cfFile) bool { // a besser als b?
		if (a.ReleaseType == 1) != (b.ReleaseType == 1) {
			return a.ReleaseType == 1
		}
		return a.ID > b.ID
	}
	for _, f := range files {
		if !f.IsAvailable {
			continue
		}
		if !found || better(f, best) {
			best, found = f, true
		}
	}
	return best, found
}

func cfLatestFile(modID int, mc string) (cfFile, error) {
	q := url.Values{"gameVersion": {mc}, "modLoaderType": {strconv.Itoa(cfLoaderFabric)}, "pageSize": {"50"}}
	code, data, err := cfRequest("/mods/"+strconv.Itoa(modID)+"/files", q)
	if err != nil {
		return cfFile{}, err
	}
	if code == 401 || code == 403 {
		return cfFile{}, errors.New("CurseForge lehnt den Schluessel ab (HTTP " + strconv.Itoa(code) + ")")
	}
	if code != 200 {
		return cfFile{}, fmt.Errorf("CurseForge antwortet mit HTTP %d", code)
	}
	var resp struct {
		Data []cfFile `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return cfFile{}, err
	}
	f, ok := cfPickFile(resp.Data)
	if !ok {
		return cfFile{}, errors.New("keine passende Fabric-Version fuer Minecraft " + mc)
	}
	return f, nil
}

// cfDownloadURL liefert die Download-Adresse (Feld downloadUrl oder Endpunkt download-url).
func cfDownloadURL(modID int, f cfFile) (string, error) {
	if f.DownloadURL != "" {
		return f.DownloadURL, nil
	}
	code, data, err := cfRequest(fmt.Sprintf("/mods/%d/files/%d/download-url", modID, f.ID), nil)
	if err != nil {
		return "", err
	}
	if code == 403 || code == 404 {
		return "", errCFDistrib
	}
	if code != 200 {
		return "", fmt.Errorf("CurseForge antwortet mit HTTP %d", code)
	}
	var resp struct {
		Data string `json:"data"`
	}
	if json.Unmarshal(data, &resp) != nil || resp.Data == "" {
		return "", errCFDistrib
	}
	return resp.Data, nil
}

// cfDownload laedt eine Datei vom CurseForge-CDN (mit Schluessel) und prueft SHA-1.
func cfDownload(rawURL, dest, wantSHA1 string) error {
	u, err := url.Parse(rawURL)
	if err != nil || !cfCDNHostOK(u.Hostname()) || (u.Scheme != "https" && !(cfAllowHTTP && u.Scheme == "http")) {
		return errors.New("Download-Adresse von CurseForge ist nicht erlaubt")
	}
	key := loadCFKey()
	if key == "" {
		return errNoCFKey
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		c := httpClient
		if attempt%2 == 1 {
			c = httpClient11
		}
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("x-api-key", key)
		resp, err := c.Do(req)
		if err == nil {
			data, rerr := io.ReadAll(io.LimitReader(resp.Body, 600<<20))
			resp.Body.Close()
			switch {
			case rerr != nil:
				err = rerr
			case resp.StatusCode == 403:
				return errCFDistrib
			case resp.StatusCode != 200:
				return fmt.Errorf("Download fehlgeschlagen (HTTP %d)", resp.StatusCode)
			default:
				if wantSHA1 != "" {
					sum := sha1.Sum(data)
					if hex.EncodeToString(sum[:]) != wantSHA1 {
						return errors.New("Pruefsumme der heruntergeladenen Datei stimmt nicht")
					}
				}
				tmp := dest + ".part"
				if err := os.WriteFile(tmp, data, 0o644); err != nil {
					return err
				}
				return os.Rename(tmp, dest)
			}
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 700 * time.Millisecond)
	}
	return lastErr
}

// ---------- Installation ----------

type cfResult struct {
	Installed []string `json:"installed"`
	Skipped   []string `json:"skipped"`
}

// cfInstall installiert eine Mod samt Pflicht-Abhaengigkeiten.
// Eine Mod mit gleicher Mod-ID wie eine schon vorhandene Datei wird nicht doppelt abgelegt.
func cfInstall(modsDir, mc string, modID int, seen map[int]bool, depth int, res *cfResult) error {
	if seen[modID] {
		return nil
	}
	seen[modID] = true
	if modID == cfFabricAPIID {
		if fabricAPIPresent(modsDir) {
			res.Skipped = append(res.Skipped, "Fabric API")
			return nil
		}
		if err := installFabricAPI(modsDir, mc); err != nil {
			return fmt.Errorf("Fabric API: %w", err)
		}
		res.Installed = append(res.Installed, "Fabric API")
		return nil
	}
	f, err := cfLatestFile(modID, mc)
	if err != nil {
		return err
	}
	name, err := safeModFile(f.FileName, false)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(modsDir, name)
	if exists(dest) || exists(dest+".disabled") {
		res.Skipped = append(res.Skipped, name)
	} else {
		dl, err := cfDownloadURL(modID, f)
		if err != nil {
			return err
		}
		if err := cfDownload(dl, dest, f.sha1()); err != nil {
			return err
		}
		if dup := duplicateModID(modsDir, name); dup != "" {
			_ = os.Remove(dest)
			res.Skipped = append(res.Skipped, name+" (gleiche Mod wie "+dup+")")
		} else {
			res.Installed = append(res.Installed, name)
		}
	}
	if depth < 3 {
		for _, d := range f.Dependencies {
			if d.RelationType != cfRelRequired {
				continue
			}
			if err := cfInstall(modsDir, mc, d.ModID, seen, depth+1, res); err != nil {
				res.Skipped = append(res.Skipped, fmt.Sprintf("Abhaengigkeit %d: %v", d.ModID, err))
			}
		}
	}
	return nil
}

// duplicateModID: gibt den Dateinamen einer anderen aktiven Mod mit derselben Mod-ID zurueck.
func duplicateModID(dir, file string) string {
	id := readModMeta(filepath.Join(dir, file)).ID
	if id == "" {
		return ""
	}
	for _, m := range listMods(dir) {
		if m.Enabled && m.File != file && readModMeta(filepath.Join(dir, m.File)).ID == id {
			return m.File
		}
	}
	return ""
}

// ---------- HTTP-Handler ----------

func (s *server) cfStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"hasKey": loadCFKey() != ""})
}

func (s *server) cfSetKey(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Key string `json:"key"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	key := strings.TrimSpace(b.Key)
	if key == "" {
		if err := saveCFKey(""); err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"hasKey": loadCFKey() != ""})
		return
	}
	if !reCFKey.MatchString(key) {
		fail(w, 400, errors.New("das sieht nicht nach einem CurseForge-Schluessel aus"))
		return
	}
	code, _, err := cfRequestKey(key, "/games/432", nil)
	if err != nil {
		fail(w, 502, err)
		return
	}
	if code == 401 || code == 403 {
		fail(w, 400, errors.New("CurseForge lehnt diesen Schluessel ab"))
		return
	}
	if code != 200 {
		fail(w, 502, fmt.Errorf("CurseForge antwortet mit HTTP %d", code))
		return
	}
	if err := saveCFKey(key); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"hasKey": true})
}

func (s *server) cfSearch(w http.ResponseWriter, r *http.Request) {
	p, err := profileByID(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 || offset > 9000 {
		offset = 0
	}
	sortField := "6" // meiste Downloads
	if query != "" {
		sortField = "2" // Beliebtheit
	}
	q := url.Values{
		"gameId": {strconv.Itoa(cfGameMinecraft)}, "classId": {strconv.Itoa(cfClassMods)},
		"gameVersion": {p.MC}, "modLoaderType": {strconv.Itoa(cfLoaderFabric)},
		"searchFilter": {query}, "sortField": {sortField}, "sortOrder": {"desc"},
		"pageSize": {"20"}, "index": {strconv.Itoa(offset)},
	}
	code, data, err := cfRequest("/mods/search", q)
	if err != nil {
		code = 502
		if errors.Is(err, errNoCFKey) {
			code = 401
		}
		fail(w, code, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func (s *server) cfInstallHandler(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID    string `json:"id"`
		ModID int    `json:"modId"`
	}
	if err := decode(r, &b); err != nil || b.ModID <= 0 {
		fail(w, 400, errors.New("ungueltige Anfrage"))
		return
	}
	p, err := profileByID(b.ID)
	if err != nil {
		fail(w, 404, err)
		return
	}
	dir, err := modsDirOf(p)
	if err != nil {
		fail(w, 500, err)
		return
	}
	res := &cfResult{Installed: []string{}, Skipped: []string{}}
	if err := cfInstall(dir, p.MC, b.ModID, map[int]bool{}, 0, res); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, res)
}
