package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Zusatzoptionen pro Profil: Direktbeitritt zu einem Server, Fenstergroesse/Vollbild, Duplizieren.

var reServer = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.\-]{0,251}[A-Za-z0-9])?(:\d{1,5})?$`)

// validateServer prueft "host" oder "host:port" (leer = kein Direktbeitritt).
func validateServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !reServer.MatchString(s) {
		return "", errors.New("Server bitte als Adresse oder Adresse:Port angeben, z. B. mc.example.net:25565")
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		if port, _ := strconv.Atoi(s[i+1:]); port < 1 || port > 65535 {
			return "", errors.New("Server-Port muss zwischen 1 und 65535 liegen")
		}
	}
	return s, nil
}

func validateWindow(w, h int) error {
	if w == 0 && h == 0 {
		return nil
	}
	if w < 320 || w > 7680 || h < 240 || h > 4320 {
		return errors.New("Fenstergroesse: Breite 320-7680, Hoehe 240-4320")
	}
	return nil
}

var reSnapshot = regexp.MustCompile(`^(\d{2})w\d{2}[a-z]`)

// mcMinor liefert die zweite Versionsnummer ("1.21.4" -> 21); Snapshots ab 2023 zaehlen als 20+.
func mcMinor(mc string) int {
	if m := reSnapshot.FindStringSubmatch(mc); m != nil {
		if y, _ := strconv.Atoi(m[1]); y >= 23 {
			return 20
		}
		return 0
	}
	parts := strings.Split(mc, ".")
	if len(parts) >= 2 && parts[0] == "1" {
		n, _ := strconv.Atoi(parts[1])
		return n
	}
	return 0
}

// extraGameArgs: Argumente, die nicht aus der Versionsdatei kommen (deren "features" sind aus).
func extraGameArgs(p Profile) []string {
	var args []string
	if p.Fullscreen {
		args = append(args, "--fullscreen")
	} else if p.Width > 0 && p.Height > 0 {
		args = append(args, "--width", strconv.Itoa(p.Width), "--height", strconv.Itoa(p.Height))
	}
	if p.Server != "" {
		if mcMinor(p.MC) >= 20 {
			args = append(args, "--quickPlayMultiplayer", p.Server)
		} else {
			host, port := p.Server, "25565"
			if i := strings.LastIndex(p.Server, ":"); i >= 0 {
				host, port = p.Server[:i], p.Server[i+1:]
			}
			args = append(args, "--server", host, "--port", port)
		}
	}
	return args
}

// ---------- Duplizieren ----------

const maxCopyBytes = 6 << 30

// copyTree kopiert Dateien rekursiv (ohne Symlinks); fehlende Quelle ist kein Fehler.
func copyTree(src, dst string, budget *int64) error {
	if _, err := os.Lstat(src); err != nil {
		return nil
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		*budget -= info.Size()
		if *budget < 0 {
			return errors.New("zu viele Daten zum Kopieren")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// duplicateProfile legt eine Kopie an: Einstellungen, Mods, Konfiguration, Resourcepacks,
// Shaderpacks, Optionen und Serverliste - aber keine Welten, Screenshots oder Logs.
func duplicateProfile(mcDir string, src Profile) (Profile, error) {
	name := src.Name + " (Kopie)"
	if r := []rune(name); len(r) > 40 {
		name = string(r[:36]) + " (K)"
	}
	p := src
	p.ID, p.Name, p.LastPlayed, p.PlaySeconds = newProfileID(name), name, "", 0
	srcDir, dstDir := profileGameDir(mcDir, src), profileGameDir(mcDir, p)
	budget := int64(maxCopyBytes)
	for _, item := range []string{"mods", "config", "resourcepacks", "shaderpacks", "options.txt", "servers.dat", overridesMarker} {
		if err := copyTree(filepath.Join(srcDir, item), filepath.Join(dstDir, item), &budget); err != nil {
			_ = os.RemoveAll(dstDir)
			return Profile{}, err
		}
	}
	if src.Pack != "" {
		if from, err := packFilePath(src.ID); err == nil {
			if to, err := packFilePath(p.ID); err == nil && exists(from) {
				_ = copyFile(from, to)
			}
		}
	}
	if err := profiles.add(p); err != nil {
		_ = os.RemoveAll(dstDir)
		return Profile{}, err
	}
	return p, nil
}
