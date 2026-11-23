package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Spielen ueber den offiziellen Minecraft Launcher: JenoXx legt das installierte Profil in
// dessen launcher_profiles.json an und oeffnet ihn. Die Anmeldung erledigt dann Mojangs Launcher.

// writeOfficialProfile traegt (oder aktualisiert) das Profil im offiziellen Launcher ein.
func writeOfficialProfile(mcDir string, p Profile) error {
	if !p.Ready || p.VersionID == "" {
		return errors.New("Profil ist noch nicht installiert")
	}
	path := filepath.Join(mcDir, "launcher_profiles.json")
	root := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			return errors.New("launcher_profiles.json ist beschaedigt - bitte den offiziellen Launcher einmal starten")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	profs := map[string]json.RawMessage{}
	if raw, ok := root["profiles"]; ok {
		if err := json.Unmarshal(raw, &profs); err != nil {
			return errors.New("launcher_profiles.json hat ein unerwartetes Format")
		}
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	created := now
	if old, ok := profs[profileKeyOf(p)]; ok {
		var o struct {
			Created string `json:"created"`
		}
		if json.Unmarshal(old, &o) == nil && o.Created != "" {
			created = o.Created
		}
	}
	entry, _ := json.Marshal(map[string]any{
		"name":          "JenoXx - " + p.Name,
		"type":          "custom",
		"created":       created,
		"lastUsed":      now,
		"icon":          "Furnace",
		"lastVersionId": p.VersionID,
		"gameDir":       profileGameDir(mcDir, p),
		"javaArgs":      p.javaArgs(),
	})
	profs[profileKeyOf(p)] = entry
	root["profiles"], _ = json.Marshal(profs)
	if _, ok := root["version"]; !ok {
		root["version"] = json.RawMessage("3")
	}
	if _, ok := root["settings"]; !ok {
		root["settings"] = json.RawMessage("{}")
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(mcDir, 0o755); err != nil {
		return err
	}
	if exists(path) { // einmal sichern, falls etwas schiefgeht
		if _, err := os.Stat(path + ".jenoxx-backup"); err != nil {
			_ = copyFile(path, path+".jenoxx-backup")
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// officialLauncherExe sucht die klassische Installation des offiziellen Launchers.
func officialLauncherExe() string {
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		for _, rel := range []string{
			filepath.Join("Minecraft Launcher", "MinecraftLauncher.exe"),
			filepath.Join("Programs", "Minecraft Launcher", "MinecraftLauncher.exe"),
		} {
			if p := filepath.Join(base, rel); exists(p) {
				return p
			}
		}
	}
	return ""
}

// startOfficialLauncher oeffnet den offiziellen Launcher (klassisch oder Microsoft-Store-Version).
func startOfficialLauncher() error {
	if runtime.GOOS != "windows" {
		return errors.New("Der offizielle Launcher kann nur unter Windows geoeffnet werden")
	}
	if exe := officialLauncherExe(); exe != "" {
		return exec.Command(exe).Start()
	}
	// Store-Version (Paketfamilie des Minecraft Launchers)
	return exec.Command("explorer.exe", `shell:AppsFolder\Microsoft.4297127D64EC6_8wekyb3d8bbwe!Minecraft`).Start()
}

func (s *server) profileOfficial(w http.ResponseWriter, r *http.Request) {
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
	mcDir, err := minecraftDir()
	if err != nil {
		fail(w, 500, err)
		return
	}
	if err := writeOfficialProfile(mcDir, p); err != nil {
		fail(w, 400, err)
		return
	}
	if err := startOfficialLauncher(); err != nil {
		fail(w, 500, errors.New("Profil wurde eingetragen, aber der offizielle Launcher liess sich nicht oeffnen: "+err.Error()+" - bitte ihn selbst starten und das Profil \"JenoXx - "+p.Name+"\" waehlen."))
		return
	}
	writeJSON(w, 200, map[string]string{"profile": "JenoXx - " + p.Name})
}
