package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Import von Modrinth-Modpacks (.mrpack). Format: modrinth.index.json plus overrides/ und client-overrides/.

type mrpackFile struct {
	Path   string            `json:"path"`
	Hashes map[string]string `json:"hashes"`
	Env    struct {
		Client string `json:"client"`
		Server string `json:"server"`
	} `json:"env"`
	Downloads []string `json:"downloads"`
	FileSize  int64    `json:"fileSize"`
}

type mrpackIndex struct {
	FormatVersion int               `json:"formatVersion"`
	Game          string            `json:"game"`
	Name          string            `json:"name"`
	VersionID     string            `json:"versionId"`
	Files         []mrpackFile      `json:"files"`
	Dependencies  map[string]string `json:"dependencies"`
}

const (
	maxPackFiles      = 3000
	maxPackFileSize   = 600 << 20
	maxOverridesTotal = 2 << 30
	overridesMarker   = ".jenoxx-overrides"
)

// Erlaubte Download-Hosts laut Modrinth-Modpack-Spezifikation.
var packHosts = map[string]bool{
	"cdn.modrinth.com": true, "github.com": true, "raw.githubusercontent.com": true, "gitlab.com": true,
}

var (
	errBlockedExt = errors.New("ausfuehrbare Datei")
	reSafeID      = regexp.MustCompile(`^[a-z0-9\-]{1,40}$`)
	reMrpackName  = regexp.MustCompile(`^[A-Za-z0-9._+()\[\] \-]{1,150}\.mrpack$`)
	blockedExt    = map[string]bool{".exe": true, ".bat": true, ".cmd": true, ".com": true, ".scr": true,
		".ps1": true, ".vbs": true, ".msi": true, ".lnk": true, ".dll": true}
)

func packsDir() (string, error) {
	wd, err := workDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(wd, "packs")
	return d, os.MkdirAll(d, 0o755)
}

func packFilePath(id string) (string, error) {
	if !reSafeID.MatchString(id) {
		return "", errors.New("ungueltige Profil-ID")
	}
	d, err := packsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id+".mrpack"), nil
}

// safeRelPath prueft einen relativen Pfad aus einem Modpack (nur innerhalb des Spielordners).
func safeRelPath(rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) || strings.Contains(rel, ":") {
		return "", fmt.Errorf("unsicherer Pfad im Modpack: %q", rel)
	}
	parts := strings.Split(rel, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." || strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return "", fmt.Errorf("unsicherer Pfad im Modpack: %q", rel)
		}
	}
	if blockedExt[strings.ToLower(filepath.Ext(rel))] {
		return "", errBlockedExt
	}
	return filepath.FromSlash(rel), nil
}

func readMrpackIndex(path string) (*mrpackIndex, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, errors.New("keine gueltige .mrpack-Datei (kein ZIP-Archiv)")
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "modrinth.index.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, 16<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		var idx mrpackIndex
		if err := json.Unmarshal(data, &idx); err != nil {
			return nil, errors.New("modrinth.index.json ist nicht lesbar")
		}
		return &idx, validatePack(&idx)
	}
	return nil, errors.New("modrinth.index.json fehlt - das ist kein Modrinth-Modpack")
}

func validatePack(idx *mrpackIndex) error {
	if idx.FormatVersion != 1 || idx.Game != "minecraft" {
		return errors.New("unbekanntes Modpack-Format")
	}
	if !reMC.MatchString(idx.Dependencies["minecraft"]) {
		return errors.New("das Modpack nennt keine gueltige Minecraft-Version")
	}
	if idx.Dependencies["fabric-loader"] == "" {
		for _, other := range []string{"quilt-loader", "forge", "neoforge"} {
			if idx.Dependencies[other] != "" {
				return fmt.Errorf("dieses Modpack braucht %s - JenoXx unterstuetzt nur Fabric-Modpacks", other)
			}
		}
		return errors.New("das Modpack ist nicht fuer Fabric gebaut - JenoXx unterstuetzt nur Fabric-Modpacks")
	}
	if len(idx.Files) > maxPackFiles {
		return fmt.Errorf("das Modpack enthaelt zu viele Dateien (%d)", len(idx.Files))
	}
	return nil
}

func packDownloadURL(f mrpackFile) (string, error) {
	for _, raw := range f.Downloads {
		u, err := url.Parse(raw)
		if err == nil && u.Scheme == "https" && packHosts[u.Hostname()] {
			return raw, nil
		}
	}
	return "", fmt.Errorf("kein erlaubter Download-Link fuer %s", f.Path)
}

// packTasks wandelt die Dateiliste in Download-Aufgaben (inkl. Pfad- und Hostpruefung).
func packTasks(gameDir string, idx *mrpackIndex) ([]task, error) {
	root := filepath.Clean(gameDir) + string(os.PathSeparator)
	var tasks []task
	for _, f := range idx.Files {
		if strings.EqualFold(f.Env.Client, "unsupported") {
			continue
		}
		rel, err := safeRelPath(f.Path)
		if errors.Is(err, errBlockedExt) {
			logln("      uebersprungen (ausfuehrbare Datei): " + f.Path)
			continue
		}
		if err != nil {
			return nil, err
		}
		dest := filepath.Join(gameDir, rel)
		if !strings.HasPrefix(dest, root) {
			return nil, fmt.Errorf("unsicherer Pfad im Modpack: %q", f.Path)
		}
		sum := strings.ToLower(f.Hashes["sha1"])
		if sum == "" {
			return nil, fmt.Errorf("keine SHA-1-Pruefsumme fuer %s", f.Path)
		}
		if f.FileSize > maxPackFileSize {
			return nil, fmt.Errorf("Datei zu gross: %s", f.Path)
		}
		u, err := packDownloadURL(f)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task{url: u, dest: dest, sha1: sum, size: f.FileSize})
	}
	return tasks, nil
}

// extractOverrides kopiert overrides/ und danach client-overrides/ in den Spielordner.
func extractOverrides(zipPath, gameDir string) (int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	root := filepath.Clean(gameDir) + string(os.PathSeparator)
	var total int64
	count := 0
	for _, prefix := range []string{"overrides/", "client-overrides/"} {
		for _, f := range zr.File {
			if !strings.HasPrefix(f.Name, prefix) || f.FileInfo().IsDir() || f.Mode()&os.ModeSymlink != 0 {
				continue
			}
			rel, err := safeRelPath(strings.TrimPrefix(f.Name, prefix))
			if errors.Is(err, errBlockedExt) {
				logln("      uebersprungen (ausfuehrbare Datei): " + f.Name)
				continue
			}
			if err != nil {
				return count, err
			}
			target := filepath.Join(gameDir, rel)
			if !strings.HasPrefix(target, root) {
				return count, fmt.Errorf("unsicherer Pfad im Modpack: %q", f.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return count, err
			}
			n, err := writeZipEntry(f, target, maxOverridesTotal-total+1)
			total += n
			if err != nil {
				return count, err
			}
			if total > maxOverridesTotal {
				return count, errors.New("die Konfigurationsdateien des Modpacks sind zu gross")
			}
			count++
		}
	}
	return count, nil
}

func writeZipEntry(f *zip.File, target string, limit int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, io.LimitReader(rc, limit))
}

// importPackFile legt aus einer .mrpack-Datei ein neues Profil an (installiert wird danach separat).
func importPackFile(src string) (Profile, error) {
	idx, err := readMrpackIndex(src)
	if err != nil {
		return Profile{}, err
	}
	name := strings.TrimSpace(idx.Name)
	if name == "" {
		name = "Modpack"
	}
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40])
	}
	ram := 6144
	if t := totalRAMMB(); t > 0 && t < 10240 {
		ram = 4096
	}
	p := Profile{ID: newProfileID(name), Name: name, MC: idx.Dependencies["minecraft"], RAM: ram, Pack: name}
	dst, err := packFilePath(p.ID)
	if err != nil {
		return Profile{}, err
	}
	if err := copyFile(src, dst); err != nil {
		return Profile{}, err
	}
	if err := profiles.add(p); err != nil {
		_ = os.Remove(dst)
		return Profile{}, err
	}
	return p, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// installPackMods laedt alle Dateien des Modpacks (Schritt 4 der Installation).
func installPackMods(p Profile, mcDir string) (int, error) {
	path, err := packFilePath(p.ID)
	if err != nil {
		return 0, err
	}
	idx, err := readMrpackIndex(path)
	if err != nil {
		return 0, fmt.Errorf("Modpack-Datei fehlt oder ist beschaedigt - bitte erneut importieren: %w", err)
	}
	tasks, err := packTasks(profileGameDir(mcDir, p), idx)
	if err != nil {
		return 0, err
	}
	logln(fmt.Sprintf("Lade %d Modpack-Dateien...", len(tasks)))
	if err := fetchAll(tasks, 8, "Modpack"); err != nil {
		return 0, err
	}
	return len(tasks), nil
}

// installPackOverrides kopiert die Konfiguration einmalig (spaetere Reparaturen ueberschreiben nichts).
func installPackOverrides(p Profile, mcDir string) (int, bool, error) {
	gameDir := profileGameDir(mcDir, p)
	marker := filepath.Join(gameDir, overridesMarker)
	if exists(marker) {
		return 0, true, nil
	}
	path, err := packFilePath(p.ID)
	if err != nil {
		return 0, false, err
	}
	n, err := extractOverrides(path, gameDir)
	if err != nil {
		return n, false, err
	}
	return n, false, os.WriteFile(marker, []byte("Modpack-Konfiguration wurde kopiert.\n"), 0o644)
}
