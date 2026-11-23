package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Zustandspruefung eines Profils und Diagnosebericht zum Weitergeben.

type healthItem struct {
	Level  string `json:"level"` // ok | warn | error
	Text   string `json:"text"`
	Action string `json:"action,omitempty"` // repair | settings | mods
}

func healthCheck(mcDir string, p Profile) []healthItem {
	var items []healthItem
	add := func(level, text, action string) { items = append(items, healthItem{level, text, action}) }

	if p.VersionID == "" || !p.Ready {
		return []healthItem{{"error", "Das Profil ist noch nicht (vollstaendig) installiert.", "repair"}}
	}
	v, vanillaID, err := loadMerged(mcDir, p.VersionID)
	if err != nil || v == nil {
		return []healthItem{{"error", "Versionsdaten fehlen oder sind beschaedigt.", "repair"}}
	}
	if st, err := os.Stat(filepath.Join(mcDir, "versions", vanillaID, vanillaID+".jar")); err != nil || st.Size() == 0 {
		add("error", "Die Minecraft-Datei "+vanillaID+".jar fehlt oder ist leer.", "repair")
	}
	if _, err := classpathOf(v, mcDir, vanillaID); err != nil {
		add("error", "Bibliotheken fehlen: "+shorten(err.Error(), 160), "repair")
	}
	if jp := strings.TrimSpace(p.JavaPath); jp != "" {
		if !exists(jp) {
			add("error", "Der eingestellte Java-Pfad existiert nicht: "+jp, "settings")
		}
	} else if plat, perr := runtimePlatform(); perr == nil {
		comp := orDefault(v.JavaVersion.Component, "jre-legacy")
		if !exists(javaExeIn(filepath.Join(mcDir, "runtime", comp, plat, comp))) {
			add("error", "Die Java-Laufzeit "+comp+" fehlt.", "repair")
		}
	}

	modsDir := filepath.Join(profileGameDir(mcDir, p), "mods")
	modItems, enabled := modHealth(modsDir)
	items = append(items, modItems...)
	if total := totalRAMMB(); total > 0 && p.RAM > total*85/100 {
		add("warn", fmt.Sprintf("Der eingestellte RAM (%d MB) ist fast so gross wie dein Arbeitsspeicher (%d MB).", p.RAM, total), "settings")
	}
	if enabled > 40 && p.RAM > 0 && p.RAM < 4096 {
		add("warn", fmt.Sprintf("%d Mods mit nur %d MB RAM - empfohlen sind mindestens 4 GB.", enabled, p.RAM), "settings")
	}
	if len(items) == 0 {
		add("ok", fmt.Sprintf("Alles in Ordnung (%d aktive Mods).", enabled), "")
	}
	rank := map[string]int{"error": 0, "warn": 1, "ok": 2}
	sort.SliceStable(items, func(i, j int) bool { return rank[items[i].Level] < rank[items[j].Level] })
	return items
}

// modHealth prueft den Mods-Ordner: doppelte Mods, fehlende Fabric API, unvollstaendige Downloads.
func modHealth(modsDir string) (items []healthItem, enabled int) {
	add := func(level, text, action string) { items = append(items, healthItem{level, text, action}) }
	mods := listMods(modsDir)
	fillModMeta(modsDir, mods)
	byID := map[string][]string{}
	for _, m := range mods {
		if !m.Enabled {
			continue
		}
		enabled++
		if m.ID != "" {
			byID[m.ID] = append(byID[m.ID], m.File)
		}
	}
	var ids []string
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if files := byID[id]; len(files) > 1 {
			add("error", fmt.Sprintf("Doppelte Mod \"%s\": %s. Fabric startet so nicht - eine Datei entfernen.", id, strings.Join(files, ", ")), "mods")
		}
	}
	if len(byID["fabric-api"]) == 0 {
		add("warn", "Die Fabric API fehlt oder ist deaktiviert. Die meisten Mods brauchen sie.", "mods")
	}
	if parts, _ := filepath.Glob(filepath.Join(modsDir, "*.part")); len(parts) > 0 {
		add("warn", "Unvollstaendiger Download im Mods-Ordner: "+filepath.Base(parts[0]), "mods")
	}
	return items, enabled
}

// buildDiagnostics erzeugt einen Textbericht (ohne Token), den man weitergeben kann.
func buildDiagnostics(mcDir string, p Profile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s - Diagnosebericht\n", appName)
	fmt.Fprintf(&b, "System: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	if t := totalRAMMB(); t > 0 {
		fmt.Fprintf(&b, "Arbeitsspeicher: %d MB\n", t)
	}
	fmt.Fprintf(&b, "Profil: %s (Minecraft %s, Fabric %s, Version-ID %s)\n", p.Name, p.MC, orDefault(p.Loader, "-"), orDefault(p.VersionID, "-"))
	if p.Pack != "" {
		fmt.Fprintf(&b, "Modpack: %s\n", p.Pack)
	}
	fmt.Fprintf(&b, "RAM: %d MB | Java: %s\n", p.RAM, orDefault(strings.TrimSpace(p.JavaPath), "automatisch"))
	if a := strings.TrimSpace(p.JVMArgs); a != "" {
		fmt.Fprintf(&b, "JVM-Argumente: %s\n", a)
	}

	b.WriteString("\nZustand:\n")
	for _, it := range healthCheck(mcDir, p) {
		fmt.Fprintf(&b, "  [%s] %s\n", it.Level, it.Text)
	}

	modsDir := filepath.Join(profileGameDir(mcDir, p), "mods")
	mods := listMods(modsDir)
	fillModMeta(modsDir, mods)
	fmt.Fprintf(&b, "\nMods (%d):\n", len(mods))
	for _, m := range mods {
		state := "an"
		if !m.Enabled {
			state = "aus"
		}
		fmt.Fprintf(&b, "  [%s] %s %s (%s) - %s\n", state, orDefault(m.Title, m.Name), m.Version, orDefault(m.ID, "?"), m.File)
	}

	if d, ok := crashOf(p.ID); ok {
		fmt.Fprintf(&b, "\nLetzter Absturz: %s (Code %d)\n  %s\n", d.Title, d.Code, d.Hint)
		if d.Detail != "" {
			fmt.Fprintf(&b, "  Detail: %s\n", d.Detail)
		}
		if len(d.Suspects) > 0 {
			fmt.Fprintf(&b, "  Verdaechtige Mods: %s\n", strings.Join(d.Suspects, ", "))
		}
		if d.Report != "" {
			fmt.Fprintf(&b, "  Absturzbericht: %s\n", d.Report)
		}
	}
	lines := lastGameLog(p.ID)
	if len(lines) > 80 {
		lines = lines[len(lines)-80:]
	}
	if len(lines) > 0 {
		b.WriteString("\nLetzte Ausgabe des Spiels:\n")
		for _, l := range lines {
			b.WriteString("  " + l + "\n")
		}
	}
	out := b.String()
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = strings.ReplaceAll(out, home, "~")
	}
	return out
}
