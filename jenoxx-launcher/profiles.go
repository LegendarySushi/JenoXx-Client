package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- Profile ----------

const defaultJVMArgs = "-XX:+UnlockExperimentalVMOptions -XX:+UseG1GC -XX:G1NewSizePercent=20 -XX:G1ReservedPercent=20 -XX:MaxGCPauseMillis=50 -XX:G1HeapRegionSize=32M"

// Profile ist eine eigene Minecraft-Instanz (Version, Mods, Einstellungen).
type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MC          string `json:"mc"`
	Loader      string `json:"loader,omitempty"`
	VersionID   string `json:"versionId,omitempty"`
	RAM         int    `json:"ram"`
	JavaPath    string `json:"javaPath"`
	JVMArgs     string `json:"jvmArgs"`
	LastPlayed  string `json:"lastPlayed,omitempty"`
	PlaySeconds int64  `json:"playSeconds,omitempty"`
	Ready       bool   `json:"ready"`
	Pack        string `json:"pack,omitempty"`   // Name des importierten Modpacks (leer = normales Profil)
	Server      string `json:"server,omitempty"` // optional: Server, dem das Spiel direkt beitritt
	Width       int    `json:"width,omitempty"`  // optional: Fenstergroesse
	Height      int    `json:"height,omitempty"`
	Fullscreen  bool   `json:"fullscreen,omitempty"` // Vollbild
}

func (p Profile) javaArgs() string {
	ram := p.RAM
	if ram < 512 {
		ram = 4096
	}
	args := strings.TrimSpace(p.JVMArgs)
	if args == "" {
		args = defaultJVMArgs
	}
	return fmt.Sprintf("-Xmx%dM %s", ram, args)
}

// profileKeyOf: Schluessel in launcher_profiles.json und Ordnername. Das Standardprofil "client"
// heisst "jenoxx-client" (wie in frueheren Versionen).
func profileKeyOf(p Profile) string { return "jenoxx-" + p.ID }

func profileGameDir(mcDir string, p Profile) string { return filepath.Join(mcDir, profileKeyOf(p)) }

var (
	reMC   = regexp.MustCompile(`^[0-9A-Za-z._\-]{1,40}$`)
	reSlug = regexp.MustCompile(`[^a-z0-9]+`)
)

func newProfileID(name string) string {
	slug := strings.Trim(reSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 16 {
		slug = strings.Trim(slug[:16], "-")
	}
	if slug == "" {
		slug = "profil"
	}
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return slug + "-" + hex.EncodeToString(b)
}

type store struct {
	mu   sync.Mutex
	path string
	list []Profile
}

var profiles = &store{}

func defaultProfile() Profile {
	return Profile{ID: "client", Name: "JenoXx", MC: mcVersion, RAM: 4096}
}

func (s *store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	wd, err := workDir()
	if err != nil {
		return err
	}
	s.path = filepath.Join(wd, "profiles.json")
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		var f struct {
			Profiles []Profile `json:"profiles"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("profiles.json nicht lesbar: %w", err)
		}
		s.list = f.Profiles
	case errors.Is(err, fs.ErrNotExist):
		s.list = []Profile{defaultProfile()}
		return s.saveLocked()
	default:
		return err
	}
	return nil
}

func (s *store) saveLocked() error {
	out, err := json.MarshalIndent(struct {
		Profiles []Profile `json:"profiles"`
	}{s.list}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, out, 0o644)
}

func (s *store) all() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Profile(nil), s.list...)
}

func (s *store) get(id string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.list {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

func (s *store) add(p Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, p)
	return s.saveLocked()
}

func (s *store) update(id string, fn func(*Profile)) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID == id {
			fn(&s.list[i])
			_ = s.saveLocked()
			return s.list[i], true
		}
	}
	return Profile{}, false
}

func (s *store) remove(id string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID == id {
			p := s.list[i]
			s.list = append(s.list[:i], s.list[i+1:]...)
			_ = s.saveLocked()
			return p, true
		}
	}
	return Profile{}, false
}

// ---------- Validierung ----------

func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 40 {
		return "", errors.New("Name: 1 bis 40 Zeichen")
	}
	return name, nil
}

func validateSettings(ram int, javaPath string) error {
	if ram < 1024 || ram > 65536 {
		return errors.New("RAM muss zwischen 1024 und 65536 MB liegen")
	}
	return validateJavaPath(javaPath)
}

// ---------- Zustand auf der Platte ----------

// installedOnDisk: Profil wurde vollstaendig installiert und die wichtigsten Dateien sind noch da.
func installedOnDisk(mcDir string, p Profile) bool {
	if !p.Ready || p.VersionID == "" {
		return false
	}
	v, vanillaID, err := loadMerged(mcDir, p.VersionID)
	if err != nil || v == nil {
		return false
	}
	if !exists(filepath.Join(mcDir, "versions", vanillaID, vanillaID+".jar")) {
		return false
	}
	mods := filepath.Join(profileGameDir(mcDir, p), "mods")
	if a, _ := filepath.Glob(filepath.Join(mods, "fabric-api-*.jar")); len(a) == 0 {
		return false
	}
	if ownModSupported(p.MC) && p.Pack == "" {
		b, _ := filepath.Glob(filepath.Join(mods, modJarGlob))
		return len(b) > 0
	}
	return true
}

// ---------- Fabric-Versionen ----------

var (
	verMu    sync.Mutex
	verCache []string
	verTime  time.Time
)

func fabricGameVersions() ([]string, error) {
	verMu.Lock()
	defer verMu.Unlock()
	if len(verCache) > 0 && time.Since(verTime) < 10*time.Minute {
		return verCache, nil
	}
	var list []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := getJSON("https://meta.fabricmc.net/v2/versions/game", &list); err != nil {
		return nil, err
	}
	var out []string
	for _, v := range list {
		if v.Stable && reMC.MatchString(v.Version) {
			out = append(out, v.Version)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("keine Versionen erhalten")
	}
	verCache, verTime = out, time.Now()
	return out, nil
}

// ---------- Mods verwalten ----------

type modInfo struct {
	File    string `json:"file"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Size    int64  `json:"size"`
	Core    bool   `json:"core"`
	ID      string `json:"id,omitempty"`      // Mod-ID aus fabric.mod.json
	Title   string `json:"title,omitempty"`   // Anzeigename aus fabric.mod.json
	Version string `json:"version,omitempty"` // Mod-Version aus fabric.mod.json
}

var reModFile = regexp.MustCompile(`^[A-Za-z0-9._+()\[\] \-]{1,180}$`)

// safeModFile prueft, dass es ein reiner Dateiname auf .jar bzw. .jar.disabled ist.
func safeModFile(name string, allowDisabled bool) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", errors.New("ungueltiger Dateiname")
	}
	if !reModFile.MatchString(name) {
		return "", errors.New("Dateiname enthaelt unerlaubte Zeichen")
	}
	if !(strings.HasSuffix(name, ".jar") || (allowDisabled && strings.HasSuffix(name, ".jar.disabled"))) {
		return "", errors.New("nur .jar-Dateien erlaubt")
	}
	return name, nil
}

func modsDirOf(p Profile) (string, error) {
	mcDir, err := minecraftDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(profileGameDir(mcDir, p), "mods"), nil
}

func listMods(dir string) []modInfo {
	entries, _ := os.ReadDir(dir)
	out := []modInfo{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !(strings.HasSuffix(n, ".jar") || strings.HasSuffix(n, ".jar.disabled")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		enabled := strings.HasSuffix(n, ".jar")
		disp := strings.TrimSuffix(strings.TrimSuffix(n, ".disabled"), ".jar")
		out = append(out, modInfo{
			File: n, Name: disp, Enabled: enabled, Size: info.Size(),
			Core: strings.HasPrefix(n, "fabric-api-") || strings.HasPrefix(n, "jenoxx-client"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func toggleMod(dir, file string) error {
	f, err := safeModFile(file, true)
	if err != nil {
		return err
	}
	src := filepath.Join(dir, f)
	var dst string
	if strings.HasSuffix(f, ".disabled") {
		dst = strings.TrimSuffix(src, ".disabled")
	} else {
		dst = src + ".disabled"
	}
	return os.Rename(src, dst)
}

func deleteMod(dir, file string) error {
	f, err := safeModFile(file, true)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, f))
}

// fabricAPIPresent: verhindert eine doppelte Fabric API (Fabric Loader bricht sonst ab).
func fabricAPIPresent(dir string) bool {
	a, _ := filepath.Glob(filepath.Join(dir, "fabric-api-*.jar*"))
	return len(a) > 0
}

// installModFromURL laedt eine Mod-Datei von cdn.modrinth.com.
func installModFromURL(dir, rawURL, filename string) (skipped bool, err error) {
	return installModReplace(dir, rawURL, filename, "")
}

// installModReplace wie installModFromURL; mit replace wird die alte Datei beim Update ersetzt.
func installModReplace(dir, rawURL, filename, replace string) (skipped bool, err error) {
	name, err := safeModFile(filename, false)
	if err != nil {
		return false, err
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "cdn.modrinth.com" {
		return false, errors.New("nur Downloads von cdn.modrinth.com erlaubt")
	}
	if replace != "" {
		if _, err := safeModFile(replace, true); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if replace == "" && strings.HasPrefix(name, "fabric-api-") && fabricAPIPresent(dir) {
		return true, nil
	}
	if err := download(rawURL, filepath.Join(dir, name)); err != nil {
		return false, err
	}
	return false, finishReplace(dir, name, replace)
}

// saveUploadedMod speichert eine vom Browser heruntergeladene Mod-Datei.
func saveUploadedMod(dir, filename string, r io.Reader) (skipped bool, err error) {
	return saveUploadedModReplace(dir, filename, "", r)
}

func saveUploadedModReplace(dir, filename, replace string, r io.Reader) (skipped bool, err error) {
	name, err := safeModFile(filename, false)
	if err != nil {
		return false, err
	}
	if replace != "" {
		if _, err := safeModFile(replace, true); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if replace == "" && strings.HasPrefix(name, "fabric-api-") && fabricAPIPresent(dir) {
		return true, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, 150<<20))
	if err != nil {
		return false, err
	}
	if len(data) < 4 || string(data[:2]) != "PK" {
		return false, errors.New("Datei ist keine gueltige .jar")
	}
	tmp := filepath.Join(dir, name+".part")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return false, err
	}
	return false, finishReplace(dir, name, replace)
}

// deleteProfileFiles loescht den Spielordner eines Profils (nur innerhalb von .minecraft/jenoxx-*).
func deleteProfileFiles(mcDir string, p Profile) error {
	dir := profileGameDir(mcDir, p)
	if filepath.Dir(dir) != filepath.Clean(mcDir) || !strings.HasPrefix(filepath.Base(dir), "jenoxx-") || p.ID == "" {
		return errors.New("unsicherer Pfad - nicht geloescht")
	}
	return os.RemoveAll(dir)
}
