package main

import (
	"archive/zip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	manifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"
	runtimeURL  = "https://launchermeta.mojang.com/v1/products/java-runtime/2ec0cc96c44e5a76b9c8b7c39df7210883d12871/all.json"
	assetsBase  = "https://resources.download.minecraft.net/"
)

// ---------- Download mit Pruefsumme ----------

func fileSHA1(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// deepVerify: bei "Reparieren" auch den Inhalt (SHA-1) pruefen statt nur die Dateigroesse.
var deepVerify bool

// upToDate: Datei vorhanden und (Groesse bzw. SHA-1) passend.
func upToDate(dest string, size int64, sum string) bool {
	st, err := os.Stat(dest)
	if err != nil || st.IsDir() {
		return false
	}
	if deepVerify && sum != "" {
		if size > 0 && st.Size() != size {
			return false
		}
		got, err := fileSHA1(dest)
		return err == nil && strings.EqualFold(got, sum)
	}
	if size > 0 {
		return st.Size() == size
	}
	if sum != "" {
		got, err := fileSHA1(dest)
		return err == nil && strings.EqualFold(got, sum)
	}
	return st.Size() > 0
}

// fetchVerified laedt u nach dest (bis zu 3 Versuche) und prueft SHA-1, falls angegeben.
func fetchVerified(u, dest, sum string, size int64) error {
	if upToDate(dest, size, sum) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if last = fetchOnce(u, dest, sum); last == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return fmt.Errorf("%s: %w", u, last)
}

func fetchOnce(u, dest, sum string) error {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := bigClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha1.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if sum != "" && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), sum) {
		os.Remove(tmp)
		return errors.New("Pruefsumme stimmt nicht")
	}
	return os.Rename(tmp, dest)
}

type task struct {
	url, dest, sha1 string
	size            int64
}

// fetchAll laedt alle Dateien parallel und meldet den Fortschritt (0-100).
func fetchAll(tasks []task, workers int, label string) error {
	if len(tasks) == 0 {
		return nil
	}
	var (
		wg      sync.WaitGroup
		done    int64
		mu      sync.Mutex
		firstEr error
	)
	ch := make(chan task)
	total := int64(len(tasks))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				if err := fetchVerified(t.url, t.dest, t.sha1, t.size); err != nil {
					mu.Lock()
					if firstEr == nil {
						firstEr = err
					}
					mu.Unlock()
				}
				n := atomic.AddInt64(&done, 1)
				if n%10 == 0 || n == total {
					ui.progress(float64(n)*100/float64(total), fmt.Sprintf("%s: %d / %d", label, n, total))
				}
			}
		}()
	}
	for _, t := range tasks {
		mu.Lock()
		stop := firstEr != nil
		mu.Unlock()
		if stop {
			break
		}
		ch <- t
	}
	close(ch)
	wg.Wait()
	return firstEr
}

// ---------- Minecraft (Vanilla) ----------

func installMinecraft(mcDir, mc string) error {
	var man struct {
		Versions []struct {
			ID   string `json:"id"`
			URL  string `json:"url"`
			SHA1 string `json:"sha1"`
		} `json:"versions"`
	}
	if err := getJSON(manifestURL, &man); err != nil {
		return fmt.Errorf("Versionsliste von Mojang nicht abrufbar: %w", err)
	}
	var url, sum string
	for _, v := range man.Versions {
		if v.ID == mc {
			url, sum = v.URL, v.SHA1
		}
	}
	if url == "" {
		return errNoVersion
	}
	jsonPath := filepath.Join(mcDir, "versions", mc, mc+".json")
	if err := fetchVerified(url, jsonPath, sum, 0); err != nil {
		return err
	}
	v, err := readVersion(mcDir, mc)
	if err != nil {
		return err
	}
	c := v.Downloads.Client
	if c.URL == "" {
		return errors.New("keine Client-Jar in der Versionsbeschreibung")
	}
	logln("Lade Minecraft " + mc + " (Client)...")
	return fetchVerified(c.URL, filepath.Join(mcDir, "versions", mc, mc+".jar"), c.SHA1, c.Size)
}

// installLibraries laedt Bibliotheken, entpackt alte Natives und laedt Assets.
func installLibraries(mcDir string, v *version, versionID string) error {
	var tasks []task
	var natives []libFile
	for _, l := range v.Libraries {
		fs, err := l.files()
		if err != nil {
			return err
		}
		for _, f := range fs {
			tasks = append(tasks, task{f.URL, filepath.Join(mcDir, "libraries", filepath.FromSlash(f.Path)), f.SHA1, f.Size})
			if f.Natives {
				natives = append(natives, f)
			}
		}
	}
	logln(fmt.Sprintf("Lade %d Bibliotheken...", len(tasks)))
	if err := fetchAll(tasks, 8, "Bibliotheken"); err != nil {
		return err
	}
	nativesDir := filepath.Join(mcDir, "versions", versionID, "natives")
	if err := os.MkdirAll(nativesDir, 0o755); err != nil {
		return err
	}
	for _, f := range natives {
		if err := extractNatives(filepath.Join(mcDir, "libraries", filepath.FromSlash(f.Path)), nativesDir, f.Exclude); err != nil {
			return err
		}
	}
	return installAssets(mcDir, v)
}

func extractNatives(jar, dst string, exclude []string) error {
	r, err := zip.OpenReader(jar)
	if err != nil {
		return err
	}
	defer r.Close()
	root := filepath.Clean(dst) + string(os.PathSeparator)
	for _, f := range r.File {
		skip := f.FileInfo().IsDir()
		for _, ex := range exclude {
			if strings.HasPrefix(f.Name, ex) {
				skip = true
			}
		}
		if skip {
			continue
		}
		target := filepath.Join(dst, f.Name)
		if !strings.HasPrefix(target, root) {
			return fmt.Errorf("unsicherer Pfad im Archiv: %s", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := extractFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

func installAssets(mcDir string, v *version) error {
	if v.AssetIndex.URL == "" {
		return nil
	}
	idxPath := filepath.Join(mcDir, "assets", "indexes", v.AssetIndex.ID+".json")
	if err := fetchVerified(v.AssetIndex.URL, idxPath, v.AssetIndex.SHA1, 0); err != nil {
		return err
	}
	data, err := os.ReadFile(idxPath)
	if err != nil {
		return err
	}
	var idx struct {
		Objects map[string]struct {
			Hash string `json:"hash"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return err
	}
	seen := map[string]bool{}
	var tasks []task
	for _, o := range idx.Objects {
		if len(o.Hash) < 2 || seen[o.Hash] {
			continue
		}
		seen[o.Hash] = true
		tasks = append(tasks, task{assetsBase + o.Hash[:2] + "/" + o.Hash,
			filepath.Join(mcDir, "assets", "objects", o.Hash[:2], o.Hash), o.Hash, o.Size})
	}
	logln(fmt.Sprintf("Pruefe/lade %d Assets (Sounds, Texturen)...", len(tasks)))
	return fetchAll(tasks, 16, "Assets")
}

// ---------- Java-Laufzeit (von Mojang) ----------

func runtimePlatform() (string, error) {
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		return "windows-x64", nil
	}
	return "", errors.New("automatischer Java-Download nur fuer Windows x64 - bitte Java-Pfad in den Einstellungen angeben")
}

func javaExeIn(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "bin", "java.exe")
	}
	return filepath.Join(dir, "bin", "java")
}

// ensureRuntime stellt die von der Version geforderte Java-Laufzeit sicher (Mojang-Layout wie der offizielle Launcher).
func ensureRuntime(mcDir, component string) (string, error) {
	if component == "" {
		component = "jre-legacy"
	}
	plat, err := runtimePlatform()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(mcDir, "runtime", component, plat, component)
	if exists(javaExeIn(dir)) {
		return javaExeIn(dir), nil
	}
	var all map[string]map[string][]struct {
		Manifest struct {
			URL string `json:"url"`
		} `json:"manifest"`
	}
	if err := getJSON(runtimeURL, &all); err != nil {
		return "", fmt.Errorf("Java-Liste nicht abrufbar: %w", err)
	}
	entries := all[plat][component]
	if len(entries) == 0 {
		return "", errors.New("Mojang bietet keine Java-Laufzeit \"" + component + "\" an")
	}
	var man struct {
		Files map[string]struct {
			Type      string `json:"type"`
			Downloads struct {
				Raw struct {
					URL  string `json:"url"`
					SHA1 string `json:"sha1"`
					Size int64  `json:"size"`
				} `json:"raw"`
			} `json:"downloads"`
		} `json:"files"`
	}
	if err := getJSON(entries[0].Manifest.URL, &man); err != nil {
		return "", err
	}
	root := filepath.Clean(dir) + string(os.PathSeparator)
	var tasks []task
	for p, f := range man.Files {
		dest := filepath.Join(dir, filepath.FromSlash(p))
		if !strings.HasPrefix(dest, root) {
			return "", errors.New("unsicherer Pfad in der Java-Liste")
		}
		switch f.Type {
		case "directory":
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return "", err
			}
		case "file":
			tasks = append(tasks, task{f.Downloads.Raw.URL, dest, f.Downloads.Raw.SHA1, f.Downloads.Raw.Size})
		}
	}
	logln(fmt.Sprintf("Lade Java-Laufzeit %s (%d Dateien)...", component, len(tasks)))
	if err := fetchAll(tasks, 8, "Java"); err != nil {
		return "", err
	}
	if !exists(javaExeIn(dir)) {
		return "", errors.New("Java wurde geladen, aber java.exe fehlt")
	}
	return javaExeIn(dir), nil
}
