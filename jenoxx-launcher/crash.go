package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Absturzanalyse: erkennt haeufige Ursachen aus Spielausgabe und Absturzbericht
// und gibt dem Nutzer eine verstaendliche Anleitung statt eines Java-Stacktraces.

type crashDiag struct {
	Title    string   `json:"title"`
	Hint     string   `json:"hint"`
	Action   string   `json:"action"` // "" | repair | settings | mods | login
	Detail   string   `json:"detail,omitempty"`
	Suspects []string `json:"suspects,omitempty"`
	Report   string   `json:"report,omitempty"`
	Code     int      `json:"code"`
	At       string   `json:"at,omitempty"`
}

// ---------- Ringpuffer fuer die letzte Spielausgabe ----------

type ringLog struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRing(max int) *ringLog { return &ringLog{max: max} }

func (r *ringLog) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, s)
	if len(r.lines) > r.max {
		r.lines = append([]string(nil), r.lines[len(r.lines)-r.max:]...)
	}
}

func (r *ringLog) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// gameState merkt sich pro Profil die letzte Ausgabe und den letzten Absturz.
var gameState = struct {
	sync.Mutex
	logs   map[string]*ringLog
	crashs map[string]crashDiag
}{logs: map[string]*ringLog{}, crashs: map[string]crashDiag{}}

func beginGameLog(id string, r *ringLog) {
	gameState.Lock()
	defer gameState.Unlock()
	gameState.logs[id] = r
	delete(gameState.crashs, id)
}

func lastGameLog(id string) []string {
	gameState.Lock()
	r := gameState.logs[id]
	gameState.Unlock()
	if r == nil {
		return nil
	}
	return r.snapshot()
}

func setCrash(id string, d crashDiag) {
	gameState.Lock()
	defer gameState.Unlock()
	gameState.crashs[id] = d
}

func crashOf(id string) (crashDiag, bool) {
	gameState.Lock()
	defer gameState.Unlock()
	d, ok := gameState.crashs[id]
	return d, ok
}

func clearCrash(id string) {
	gameState.Lock()
	defer gameState.Unlock()
	delete(gameState.crashs, id)
}

// ---------- Regeln ----------

type crashRule struct {
	re     *regexp.Regexp
	detail *regexp.Regexp // genauere Zeile fuer die Anzeige (optional)
	title  string
	hint   string
	action string
}

func crule(pattern, detail, title, hint, action string) crashRule {
	r := crashRule{re: regexp.MustCompile(pattern), title: title, hint: hint, action: action}
	if detail != "" {
		r.detail = regexp.MustCompile(detail)
	}
	return r
}

var crashRules = []crashRule{
	crule(`(?i)java\.lang\.OutOfMemoryError|Java heap space|GC overhead limit exceeded`, "",
		"Nicht genug Arbeitsspeicher",
		"Minecraft hatte zu wenig RAM. Erhoehe den Arbeitsspeicher in den Einstellungen (z. B. auf 6 GB) oder entferne speicherhungrige Mods.",
		"settings"),
	crule(`(?i)Could not reserve enough space|Invalid maximum heap size|Too small initial heap|Could not create the Java Virtual Machine|Error occurred during initialization of VM`, "",
		"Java konnte nicht starten (RAM / JVM-Argumente)",
		"Der eingestellte RAM ist groesser als dein Rechner frei hat, oder die JVM-Argumente sind ungueltig. Senke den RAM oder setze die JVM-Argumente auf den Standard zurueck.",
		"settings"),
	crule(`UnsupportedClassVersionError|compiled by a more recent version of the Java Runtime|Unsupported major\.minor version`, `UnsupportedClassVersionError|compiled by a more recent|Unsupported major`,
		"Falsche Java-Version",
		"Die verwendete Java-Version ist zu alt fuer diese Minecraft-Version. Leere den Java-Pfad in den Einstellungen, dann nutzt JenoXx automatisch die passende Java-Version.",
		"settings"),
	crule(`(?i)Could not find or load main class|Unable to access jarfile|zip END header not found|invalid LOC header|invalid CEN header|Error opening zip file|java\.util\.zip\.ZipException`, "",
		"Beschaedigte oder fehlende Dateien",
		"Eine Minecraft- oder Bibliotheksdatei ist kaputt (oft nach einem abgebrochenen Download). \"Reparieren\" prueft alle Dateien und laedt fehlerhafte neu.",
		"repair"),
	crule(`(?i)Some of your mods are incompatible|Incompatible mod set|Mod resolution failed|Could not resolve mod|Missing or unsupported mandatory dependencies|Duplicate mod|duplicate mod ids?|Found duplicate mod`,
		`(?i)Mod '[^']+' \([^)]+\)|requires (any )?version|requires |conflicts with|Duplicate|duplicate`,
		"Mods passen nicht zusammen",
		"Fabric hat beim Start Mods gefunden, die fehlen, doppelt sind oder nicht zur Minecraft-Version passen. Oeffne den Tab \"Mods\", deaktiviere zuletzt hinzugefuegte Mods oder suche nach Updates.",
		"mods"),
	crule(`(?i)Mixin apply (for mod \S+ )?failed|MixinApplyError|InvalidInjectionException|Mixin transformation of .* failed|Critical injection failure|InjectionError`, `(?i)Mixin apply|Mixin transformation|injection`,
		"Eine Mod ist inkompatibel (Mixin-Fehler)",
		"Eine Mod greift ins Spiel ein und passt nicht zu dieser Version oder zu einer anderen Mod. Suche nach Mod-Updates oder deaktiviere die verdaechtige Mod.",
		"mods"),
	crule(`(?i)GLFW error|Pixel format not accepted|WGL: The driver does not appear to support OpenGL|failed to create window|GLFW_API_UNAVAILABLE|OpenGL.*not supported|No OpenGL context found`, `(?i)GLFW|Pixel format|WGL|OpenGL|create window`,
		"Grafikproblem (OpenGL)",
		"Das Spiel konnte kein Fenster mit Grafikbeschleunigung oeffnen. Aktualisiere den Grafiktreiber (NVIDIA/AMD/Intel) und teste ohne Grafik-Mods wie Sodium oder Iris.",
		"mods"),
	crule(`(?i)Invalid session|Failed to verify authentication`, "",
		"Anmeldung abgelaufen",
		"Die Microsoft-Anmeldung ist nicht mehr gueltig. Melde dich ab und neu an.",
		"login"),
	crule(`EXCEPTION_ACCESS_VIOLATION|A fatal error has been detected by the Java Runtime Environment|SIGSEGV`, `EXCEPTION_ACCESS_VIOLATION|fatal error|SIGSEGV`,
		"Absturz der Java-Laufzeit",
		"Java selbst ist abgestuerzt, meist wegen eines Grafiktreibers oder einer defekten Datei. Aktualisiere den Grafiktreiber, klicke auf \"Reparieren\" und teste ohne Mods.",
		"repair"),
	crule(`(?i)NoClassDefFoundError|NoSuchMethodError|NoSuchFieldError|ClassNotFoundException|AbstractMethodError`, `(?i)NoClassDefFoundError|NoSuchMethodError|NoSuchFieldError|ClassNotFoundException|AbstractMethodError`,
		"Eine Mod passt nicht zur Version",
		"Ein Programmteil wurde nicht gefunden. Meist ist eine Mod fuer eine andere Minecraft- oder Fabric-API-Version gebaut. Suche nach Mod-Updates oder entferne die Mod.",
		"mods"),
}

var (
	reMixinMod = regexp.MustCompile(`(?i)Mixin apply for mod ([A-Za-z0-9_\-]+) failed`)
)

func pickDetail(text string, r crashRule) string {
	re := r.detail
	if re == nil {
		re = r.re
	}
	for _, line := range strings.Split(text, "\n") {
		if re.MatchString(line) {
			return shorten(strings.TrimSpace(line), 220)
		}
	}
	return ""
}

func shorten(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "..."
}

// suspectedMods liest den Block "Suspected Mods:" aus einem Fabric-Absturzbericht.
func suspectedMods(report string) []string {
	lines := strings.Split(strings.ReplaceAll(report, "\r", ""), "\n")
	indent := func(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }
	var out []string
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "Suspected Mods:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(t, "Suspected Mods:"))
		if rest != "" && !strings.EqualFold(rest, "none") {
			out = append(out, shorten(rest, 80))
		}
		base := indent(l)
		for _, nx := range lines[i+1:] {
			if strings.TrimSpace(nx) == "" || indent(nx) <= base {
				break
			}
			name := strings.TrimSpace(nx)
			if k := strings.Index(name, ":"); k > 0 {
				name = name[:k]
			}
			out = append(out, shorten(name, 80))
			if len(out) >= 6 {
				break
			}
		}
		break
	}
	return out
}

func addUnique(list []string, s string) []string {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return list
		}
	}
	return append(list, s)
}

// analyzeCrash wertet Spielausgabe und (falls vorhanden) den Absturzbericht aus.
func analyzeCrash(lines []string, report string, code int) crashDiag {
	text := strings.Join(lines, "\n") + "\n" + report
	d := crashDiag{Code: code, Suspects: suspectedMods(report)}
	if m := reMixinMod.FindStringSubmatch(text); m != nil {
		d.Suspects = addUnique(d.Suspects, m[1])
	}
	for _, r := range crashRules {
		if r.re.MatchString(text) {
			d.Title, d.Hint, d.Action = r.title, r.hint, r.action
			d.Detail = pickDetail(text, r)
			return d
		}
	}
	d.Title = "Minecraft wurde unerwartet beendet"
	if len(d.Suspects) > 0 {
		d.Hint = "Der Absturzbericht nennt verdaechtige Mods. Aktualisiere oder deaktiviere sie im Tab \"Mods\" und starte erneut."
		d.Action = "mods"
	} else {
		d.Hint = "Eine genaue Ursache wurde nicht erkannt. Teste, ob das Spiel ohne zusaetzliche Mods startet, und nutze \"Reparieren\". Mit \"Diagnose kopieren\" bekommst du einen Bericht zum Weitergeben."
		d.Action = "repair"
	}
	return d
}

// normExit rechnet Windows-Exitcodes wie 4294967295 in -1 um.
func normExit(code int) int {
	if code > 1<<31-1 && code <= 1<<32-1 {
		return code - 1<<32
	}
	return code
}

// ---------- Absturzbericht finden ----------

// findCrashReport liefert den neuesten Minecraft-Absturzbericht bzw. hs_err-Log, der nach since entstand.
func findCrashReport(gameDir string, since time.Time) string {
	var best string
	var bestT time.Time
	pick := func(pattern string) {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			st, err := os.Stat(m)
			if err != nil || st.IsDir() || st.ModTime().Before(since.Add(-2*time.Second)) {
				continue
			}
			if best == "" || st.ModTime().After(bestT) {
				best, bestT = m, st.ModTime()
			}
		}
	}
	pick(filepath.Join(gameDir, "crash-reports", "crash-*.txt"))
	pick(filepath.Join(gameDir, "hs_err_pid*.log"))
	return best
}

func readHead(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return string(b)
}
