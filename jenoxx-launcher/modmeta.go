package main

import (
	"archive/zip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Liest Name/ID/Version aus der fabric.mod.json einer Mod-Jar (mit Cache).

type modMeta struct {
	ID      string
	Name    string
	Version string
}

var metaCache = struct {
	sync.Mutex
	m map[string]modMeta
}{m: map[string]modMeta{}}

func readModMeta(path string) modMeta {
	st, err := os.Stat(path)
	if err != nil {
		return modMeta{}
	}
	key := fmt.Sprintf("%s|%d|%d", path, st.Size(), st.ModTime().UnixNano())
	metaCache.Lock()
	if m, ok := metaCache.m[key]; ok {
		metaCache.Unlock()
		return m
	}
	metaCache.Unlock()

	var meta modMeta
	if zr, err := zip.OpenReader(path); err == nil {
		for _, f := range zr.File {
			if f.Name != "fabric.mod.json" {
				continue
			}
			if rc, err := f.Open(); err == nil {
				data, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
				rc.Close()
				var j struct {
					ID      string `json:"id"`
					Name    string `json:"name"`
					Version string `json:"version"`
				}
				if json.Unmarshal(data, &j) == nil {
					meta = modMeta{ID: j.ID, Name: j.Name, Version: j.Version}
				}
			}
			break
		}
		zr.Close()
	}
	metaCache.Lock()
	metaCache.m[key] = meta
	metaCache.Unlock()
	return meta
}

// fillModMeta ergaenzt Titel, ID und Version aus den Jar-Dateien.
func fillModMeta(dir string, mods []modInfo) {
	for i := range mods {
		m := readModMeta(filepath.Join(dir, mods[i].File))
		mods[i].ID, mods[i].Title, mods[i].Version = m.ID, m.Name, m.Version
	}
}

// ---------- Hashes fuer die Update-Pruefung ----------

type modHash struct {
	File    string `json:"file"`
	SHA1    string `json:"sha1"`
	Enabled bool   `json:"enabled"`
}

// hashMods berechnet SHA-1 aller Mods (ohne den eingebauten JenoXx-Mod).
func hashMods(dir string) []modHash {
	out := []modHash{}
	for _, m := range listMods(dir) {
		if strings.HasPrefix(m.File, "jenoxx-client") {
			continue
		}
		sum, err := fileSHA1(filepath.Join(dir, m.File))
		if err != nil {
			continue
		}
		out = append(out, modHash{File: m.File, SHA1: sum, Enabled: m.Enabled})
	}
	return out
}

func sha1Hex(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}

// finishReplace entfernt nach einem Update die alte Datei und behaelt den Aktiv/Inaktiv-Zustand.
func finishReplace(dir, newName, oldFile string) error {
	if oldFile == "" {
		return nil
	}
	old, err := safeModFile(oldFile, true)
	if err != nil {
		return err
	}
	oldPath := filepath.Join(dir, old)
	if _, err := os.Stat(oldPath); err != nil {
		return nil // alte Datei existiert nicht mehr
	}
	wasDisabled := strings.HasSuffix(old, ".disabled")
	if old == newName || old == newName+".disabled" {
		// gleicher Dateiname: neue Datei liegt unter newName
		if wasDisabled {
			return os.Rename(filepath.Join(dir, newName), oldPath)
		}
		return nil
	}
	if err := os.Remove(oldPath); err != nil {
		return err
	}
	if wasDisabled {
		return os.Rename(filepath.Join(dir, newName), filepath.Join(dir, newName+".disabled"))
	}
	return nil
}
