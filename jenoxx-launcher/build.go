package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Quellcode des Mods (wird von build-exe.sh / build-exe.bat aus ../jenoxx-client kopiert).
//
//go:embed all:modsrc
var modsrc embed.FS

const (
	gradleVersion = "8.12"
	toolsSubdir   = "JenoXxClient"
)

var jdkURLs = []string{
	"https://aka.ms/download-jdk/microsoft-jdk-21-windows-x64.zip",
	"https://api.adoptium.net/v3/binary/latest/21/ga/windows/x64/jdk/hotspot/normal/eclipse",
}

var gradleURLs = []string{
	"https://services.gradle.org/distributions/gradle-" + gradleVersion + "-bin.zip",
	"https://downloads.gradle.org/distributions/gradle-" + gradleVersion + "-bin.zip",
}

// bigClient: HTTP/1.1, kein Gesamt-Timeout (fuer grosse Downloads).
var bigClient = &http.Client{
	CheckRedirect: secureRedirect,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
	},
}

func workDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return "", err
		}
	}
	d := filepath.Join(base, toolsSubdir)
	return d, os.MkdirAll(d, 0o755)
}

// sourceHash bildet einen Hash ueber alle eingebetteten Quelldateien.
func sourceHash() (string, int, error) {
	h := sha256.New()
	var paths []string
	err := fs.WalkDir(modsrc, "modsrc", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() != ".keep" {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		b, err := modsrc.ReadFile(p)
		if err != nil {
			return "", 0, err
		}
		h.Write([]byte(p))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), len(paths), nil
}

func extractSource(dst string) error {
	if err := os.RemoveAll(filepath.Join(dst, "src")); err != nil {
		return err
	}
	return fs.WalkDir(modsrc, "modsrc", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "modsrc"), "/")
		if rel == "" || d.Name() == ".keep" {
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := modsrc.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func findBuiltJar(srcDir string) (string, []byte, error) {
	matches, _ := filepath.Glob(filepath.Join(srcDir, "build", "libs", modJarGlob))
	for _, m := range matches {
		if isModJar(filepath.Base(m)) {
			b, err := os.ReadFile(m)
			return filepath.Base(m), b, err
		}
	}
	return "", nil, errors.New("Build ist durchgelaufen, aber keine Jar gefunden")
}

// buildModFromSource baut den Mod mit einem automatisch geladenen JDK und Gradle.
func buildModFromSource() (string, []byte, error) {
	if runtime.GOOS != "windows" {
		return "", nil, errors.New("Auto-Build gibt es nur unter Windows - bitte die Jar selbst bauen und neben das Programm legen")
	}
	hash, n, err := sourceHash()
	if err != nil {
		return "", nil, err
	}
	if n == 0 {
		return "", nil, errors.New("kein Quellcode eingebettet - Jar neben die .exe legen")
	}
	wd, err := workDir()
	if err != nil {
		return "", nil, err
	}
	srcDir := filepath.Join(wd, "modsrc")
	stamp := filepath.Join(wd, "build.stamp")

	if st, err := os.ReadFile(stamp); err == nil && string(st) == hash && !forceRebuild {
		if name, data, err := findBuiltJar(srcDir); err == nil {
			logln("      Bereits gebaute Jar wiederverwendet.")
			return name, data, nil
		}
	}

	logln("      Arbeitsordner: " + wd)
	logln("      Der erste Build dauert einige Minuten und laedt ca. 1 GB (JDK, Gradle, Minecraft).")

	jdkHome, err := ensureJDK(wd)
	if err != nil {
		return "", nil, fmt.Errorf("JDK: %w", err)
	}
	gradleHome, err := ensureGradle(wd)
	if err != nil {
		return "", nil, fmt.Errorf("Gradle: %w", err)
	}

	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return "", nil, err
	}
	if err := extractSource(srcDir); err != nil {
		return "", nil, err
	}

	logln("      Starte Gradle-Build (Ausgabe folgt)...")
	cmd := exec.Command(filepath.Join(gradleHome, "bin", "gradle.bat"), "build", "--no-daemon", "--console=plain")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(),
		"JAVA_HOME="+jdkHome,
		"PATH="+filepath.Join(jdkHome, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	lw := &lineWriter{}
	cmd.Stdout = lw
	cmd.Stderr = lw
	hideWindow(cmd)
	ui.progress(-1, "Baue Mod mit Gradle (dauert beim ersten Mal einige Minuten)...")
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("Gradle-Build fehlgeschlagen: %w (Ausgabe oben kopieren und weitergeben)", err)
	}

	name, data, err := findBuiltJar(srcDir)
	if err != nil {
		return "", nil, err
	}
	_ = os.WriteFile(stamp, []byte(hash), 0o644)
	return name, data, nil
}

func ensureJDK(wd string) (string, error) {
	dir := filepath.Join(wd, "jdk")
	if home := findJDK(dir); home != "" {
		return home, nil
	}
	logln("      Lade JDK 21 (ca. 200 MB)...")
	zipPath := filepath.Join(wd, "jdk.zip")
	if err := downloadAny(jdkURLs, zipPath); err != nil {
		return "", err
	}
	defer os.Remove(zipPath)
	_ = os.RemoveAll(dir)
	if err := unzip(zipPath, dir); err != nil {
		return "", err
	}
	if home := findJDK(dir); home != "" {
		return home, nil
	}
	return "", errors.New("javac.exe nach dem Entpacken nicht gefunden")
}

func findJDK(dir string) string {
	m, _ := filepath.Glob(filepath.Join(dir, "*", "bin", "javac.exe"))
	if len(m) > 0 {
		return filepath.Dir(filepath.Dir(m[0]))
	}
	return ""
}

func ensureGradle(wd string) (string, error) {
	home := filepath.Join(wd, "gradle", "gradle-"+gradleVersion)
	if _, err := os.Stat(filepath.Join(home, "bin", "gradle.bat")); err == nil {
		return home, nil
	}
	logln("      Lade Gradle " + gradleVersion + " (ca. 130 MB)...")
	zipPath := filepath.Join(wd, "gradle.zip")
	if err := downloadAny(gradleURLs, zipPath); err != nil {
		return "", err
	}
	defer os.Remove(zipPath)
	_ = os.RemoveAll(filepath.Join(wd, "gradle"))
	if err := unzip(zipPath, filepath.Join(wd, "gradle")); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(home, "bin", "gradle.bat")); err != nil {
		return "", errors.New("gradle.bat nach dem Entpacken nicht gefunden")
	}
	return home, nil
}

func downloadAny(urls []string, dest string) error {
	var errs []string
	for _, u := range urls {
		for attempt := 1; attempt <= 2; attempt++ {
			err := downloadLarge(u, dest)
			if err == nil {
				return nil
			}
			errs = append(errs, err.Error())
			time.Sleep(2 * time.Second)
		}
	}
	return errors.New(strings.Join(errs, "; "))
}

func downloadLarge(u, dest string) error {
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
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	var done int64
	total := resp.ContentLength
	lastPrint := time.Now()
	buf := make([]byte, 256*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if time.Since(lastPrint) > 300*time.Millisecond {
				if total > 0 {
					ui.progress(float64(done)*100/float64(total), fmt.Sprintf("Download: %d / %d MB", done/(1024*1024), total/(1024*1024)))
				} else {
					ui.progress(-1, fmt.Sprintf("Download: %d MB", done/(1024*1024)))
				}
				lastPrint = time.Now()
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// unzip entpackt mit Schutz vor Pfad-Ausbruechen (zip slip).
func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	root := filepath.Clean(dst) + string(os.PathSeparator)
	for _, f := range r.File {
		target := filepath.Join(dst, f.Name)
		if !strings.HasPrefix(target, root) {
			return fmt.Errorf("unsicherer Pfad im Archiv: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
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

func extractFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

// lineWriter leitet Prozessausgabe zeilenweise ins GUI-Log.
type lineWriter struct{ buf []byte }

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(l.buf[:i]), "\r")
		l.buf = l.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			logln(line)
		}
	}
	return len(p), nil
}
