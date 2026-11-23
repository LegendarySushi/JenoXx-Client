// JenoXx Client Launcher
//
// Richtet Fabric, die Fabric API und den JenoXx-Client-Mod in einem eigenen
// Profil ein und startet danach den offiziellen Minecraft Launcher. Der Login
// laeuft ueber den offiziellen Launcher (keine Zugangsdaten in diesem Programm).
package main

import (
	"crypto/tls"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Optional eingebettete Mod-Jar (siehe build-exe.sh / build-exe.bat).
//
//go:embed all:payload
var payload embed.FS

const (
	appName    = "JenoXx Client"
	modJarGlob = "jenoxx-client*.jar"
	mcVersion  = "1.21.4"
	userAgent  = "JenoXxClientLauncher/1.0"
)

var httpClient = &http.Client{Timeout: 2 * time.Minute, CheckRedirect: secureRedirect}

func main() {
	noOpen := flag.Bool("no-open", false, "Fenster nicht automatisch oeffnen (URL wird ausgegeben)")
	flag.Parse()
	if err := startGUI(*noOpen); err != nil {
		reportFatal(err)
		os.Exit(1)
	}
}

// forceRebuild erzwingt einen neuen Gradle-Build der Mod-Jar.
var forceRebuild bool

func stepFail(id int, err error) error {
	ui.step(id, "error", err.Error())
	return err
}

// ownModSupported: der eingebaute JenoXx-Mod ist nur fuer diese Minecraft-Version gebaut.
func ownModSupported(mc string) bool { return mc == mcVersion }

// runInstall richtet ein Profil komplett ein (Schritte 1-5 im GUI).
func runInstall(profileID string, force bool) error {
	forceRebuild = force
	deepVerify = force
	defer func() { deepVerify = false }()
	p, ok := profiles.get(profileID)
	if !ok {
		return stepFail(1, errors.New("Profil nicht gefunden"))
	}
	ui.emit(event{Type: "reset", Text: p.Name})
	profiles.update(p.ID, func(q *Profile) { q.Ready = false })
	clearCrash(p.ID)
	mcDir, err := minecraftDir()
	if err != nil {
		return stepFail(1, err)
	}
	modsDir := filepath.Join(profileGameDir(mcDir, p), "mods")
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		return stepFail(1, err)
	}

	ui.step(1, "run", "Minecraft "+p.MC+" und Fabric laden...")
	if err := installMinecraft(mcDir, p.MC); err != nil {
		return stepFail(1, err)
	}
	loader, err := latestLoader(p.MC)
	if err != nil {
		return stepFail(1, fmt.Errorf("Fabric-Version nicht abrufbar (Internet?): %w", err))
	}
	versionID, err := installVersion(mcDir, p.MC, loader)
	if err != nil {
		return stepFail(1, err)
	}
	p, _ = profiles.update(p.ID, func(q *Profile) { q.VersionID = versionID; q.Loader = loader })
	ui.step(1, "done", versionID)

	ui.step(2, "run", "Bibliotheken und Assets laden...")
	merged, _, err := loadMerged(mcDir, versionID)
	if err != nil {
		return stepFail(2, err)
	}
	if force { // Natives und Reste abgebrochener Downloads entfernen, damit alles frisch entsteht
		_ = os.RemoveAll(filepath.Join(mcDir, "versions", versionID, "natives"))
		logln("Reparatur: pruefe alle Dateien auf Beschaedigungen (kann etwas dauern)...")
	}
	if err := installLibraries(mcDir, merged, versionID); err != nil {
		return stepFail(2, err)
	}
	ui.progress(-2, "")
	ui.step(2, "done", "Bibliotheken und Assets bereit")

	ui.step(3, "run", "Java-Laufzeit pruefen...")
	if strings.TrimSpace(p.JavaPath) == "" {
		if _, err := ensureRuntime(mcDir, merged.JavaVersion.Component); err != nil {
			return stepFail(3, err)
		}
	}
	ui.progress(-2, "")
	ui.step(3, "done", orDefault(merged.JavaVersion.Component, "Java bereit"))

	if p.Pack != "" {
		ui.step(4, "run", "Modpack-Dateien laden...")
		n, err := installPackMods(p, mcDir)
		if err != nil {
			return stepFail(4, err)
		}
		if err := installFabricAPI(modsDir, p.MC); err != nil { // nur falls das Modpack sie nicht mitbringt
			return stepFail(4, fmt.Errorf("Fabric API: %w", err))
		}
		ui.progress(-2, "")
		ui.step(4, "done", fmt.Sprintf("%d Dateien bereit", n))

		ui.step(5, "run", "Modpack-Konfiguration kopieren...")
		cnt, skipped, err := installPackOverrides(p, mcDir)
		if err != nil {
			return stepFail(5, err)
		}
		if skipped {
			ui.step(5, "done", "Konfiguration bleibt unveraendert")
		} else {
			ui.step(5, "done", fmt.Sprintf("%d Konfigurationsdateien kopiert", cnt))
		}
		profiles.update(p.ID, func(q *Profile) { q.Ready = true })
		return nil
	}

	ui.step(4, "run", "Fabric API laden...")
	if err := installFabricAPI(modsDir, p.MC); err != nil {
		return stepFail(4, fmt.Errorf("Fabric API: %w", err))
	}
	ui.step(4, "done", "Fabric API bereit")

	if ownModSupported(p.MC) {
		ui.step(5, "run", appName+" Mod einrichten...")
		if err := installOwnMod(modsDir); err != nil {
			return stepFail(5, err)
		}
		ui.progress(-2, "")
		ui.step(5, "done", "Mod installiert")
	} else {
		ui.step(5, "done", "JenoXx-Mod gibt es nur fuer "+mcVersion+" - eigene Mods im Mod-Browser waehlen")
	}
	profiles.update(p.ID, func(q *Profile) { q.Ready = true })
	return nil
}

func minecraftDir() (string, error) {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, ".minecraft"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".minecraft"), nil
}

// ---------- Netzwerk ----------

// httpClient11 nutzt nur HTTP/1.1 (hilft bei Proxys/Antivirus, die HTTP/2 abbrechen).
var httpClient11 = &http.Client{
	Timeout:       2 * time.Minute,
	CheckRedirect: secureRedirect,
	Transport: &http.Transport{
		Proxy:        http.ProxyFromEnvironment,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	},
}

func httpGetOnce(c *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody+1))
	if err == nil && len(data) > maxHTTPBody {
		return nil, fmt.Errorf("%s: Antwort zu gross", u)
	}
	return data, err
}

// httpGet versucht es bis zu 4x, abwechselnd mit Standard-Client und HTTP/1.1.
func httpGet(u string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		c := httpClient
		if attempt%2 == 1 {
			c = httpClient11
		}
		data, err := httpGetOnce(c, u)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "HTTP 4") { // z.B. 404: Wiederholen bringt nichts
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return nil, lastErr
}

func getJSON(u string, v any) error {
	data, err := httpGet(u)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func download(u, dest string) error {
	data, err := httpGet(u)
	if err != nil {
		return err
	}
	tmp := dest + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// ---------- Fabric ----------

func latestLoader(mc string) (string, error) {
	var list []struct {
		Loader struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		} `json:"loader"`
	}
	if err := getJSON("https://meta.fabricmc.net/v2/versions/loader/"+mc, &list); err != nil {
		return "", err
	}
	for _, e := range list {
		if e.Loader.Stable {
			return e.Loader.Version, nil
		}
	}
	if len(list) > 0 {
		return list[0].Loader.Version, nil
	}
	return "", errors.New("keine Fabric-Loader-Version fuer " + mc + " gefunden")
}

// installVersion legt versions/<id>/<id>.json (+ leere Dummy-Jar wie der Fabric-Installer) an.
func installVersion(mcDir, mc, loader string) (string, error) {
	u := fmt.Sprintf("https://meta.fabricmc.net/v2/versions/loader/%s/%s/profile/json", mc, loader)
	raw, err := httpGet(u)
	if err != nil {
		return "", err
	}
	var meta struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil || meta.ID == "" {
		return "", errors.New("ungueltiges Fabric-Profil erhalten")
	}
	dir := filepath.Join(mcDir, "versions", meta.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, meta.ID+".json"), raw, 0o644); err != nil {
		return "", err
	}
	jar := filepath.Join(dir, meta.ID+".jar")
	if _, err := os.Stat(jar); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(jar, nil, 0o644); err != nil {
			return "", err
		}
	}
	return meta.ID, nil
}

func installFabricAPI(modsDir, mc string) error {
	if m, _ := filepath.Glob(filepath.Join(modsDir, "fabric-api-*.jar")); len(m) > 0 {
		logln("      bereits vorhanden: " + filepath.Base(m[0]))
		return nil
	}
	err := installFabricAPIModrinth(modsDir, mc)
	if err == nil {
		return nil
	}
	logln("      Modrinth nicht erreichbar (" + err.Error() + ")")
	logln("      Versuche Fabric-Maven als Ersatz...")
	if err2 := installFabricAPIMaven(modsDir, mc); err2 != nil {
		return fmt.Errorf("Modrinth: %v; Maven: %w", err, err2)
	}
	return nil
}

const fabricMavenBase = "https://maven.fabricmc.net/net/fabricmc/fabric-api/fabric-api/"

// pickFabricAPIVersion waehlt aus maven-metadata.xml die neueste Version fuer die MC-Version.
func pickFabricAPIVersion(metadata []byte, mc string) (string, error) {
	var meta struct {
		Versions []string `xml:"versioning>versions>version"`
	}
	if err := xml.Unmarshal(metadata, &meta); err != nil {
		return "", err
	}
	best := ""
	for _, v := range meta.Versions { // aufsteigend sortiert
		if strings.HasSuffix(v, "+"+mc) {
			best = v
		}
	}
	if best == "" {
		return "", errors.New("keine Fabric API fuer " + mc + " gefunden")
	}
	return best, nil
}

func installFabricAPIMaven(modsDir, mc string) error {
	raw, err := httpGet(fabricMavenBase + "maven-metadata.xml")
	if err != nil {
		return err
	}
	ver, err := pickFabricAPIVersion(raw, mc)
	if err != nil {
		return err
	}
	name := "fabric-api-" + ver + ".jar"
	logln("      lade " + name)
	return download(fabricMavenBase+ver+"/"+name, filepath.Join(modsDir, name))
}

func installFabricAPIModrinth(modsDir, mc string) error {
	q := url.Values{}
	q.Set("loaders", `["fabric"]`)
	q.Set("game_versions", `["`+mc+`"]`)

	var versions []struct {
		Files []struct {
			URL      string `json:"url"`
			Filename string `json:"filename"`
			Primary  bool   `json:"primary"`
		} `json:"files"`
	}
	if err := getJSON("https://api.modrinth.com/v2/project/fabric-api/version?"+q.Encode(), &versions); err != nil {
		return err
	}
	if len(versions) == 0 || len(versions[0].Files) == 0 {
		return errors.New("keine Fabric API fuer " + mc + " gefunden")
	}
	file := versions[0].Files[0]
	for _, f := range versions[0].Files {
		if f.Primary {
			file = f
			break
		}
	}
	name := filepath.Base(file.Filename)
	if !strings.HasSuffix(name, ".jar") {
		return errors.New("unerwarteter Dateiname: " + name)
	}
	logln("      lade " + name)
	return download(file.URL, filepath.Join(modsDir, name))
}

// ---------- Eigener Mod ----------

func isModJar(name string) bool {
	ok, _ := filepath.Match(modJarGlob, name)
	return ok && !strings.HasSuffix(name, "-sources.jar") && !strings.HasSuffix(name, "-dev.jar")
}

var errNoJar = errors.New("keine jenoxx-client-*.jar gefunden")

// findModJar sucht zuerst die eingebettete Jar, dann eine Jar neben der .exe.
func findModJar() (name string, data []byte, err error) {
	if entries, rerr := fs.ReadDir(payload, "payload"); rerr == nil {
		for _, e := range entries {
			if !e.IsDir() && isModJar(e.Name()) {
				b, err := payload.ReadFile("payload/" + e.Name())
				return e.Name(), b, err
			}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	dirs := []string{filepath.Dir(exe)}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if !e.IsDir() && isModJar(e.Name()) {
				b, err := os.ReadFile(filepath.Join(d, e.Name()))
				return e.Name(), b, err
			}
		}
	}
	return "", nil, errNoJar
}

func installOwnMod(modsDir string) error {
	name, data, err := findModJar()
	if errors.Is(err, errNoJar) {
		logln("      Keine fertige Jar gefunden - baue den Mod aus dem eingebetteten Quellcode...")
		name, data, err = buildModFromSource()
	}
	if err != nil {
		if old, _ := filepath.Glob(filepath.Join(modsDir, modJarGlob)); len(old) > 0 {
			logln("      Hinweis: " + err.Error() + " - vorhandene Version bleibt.")
			return nil
		}
		return err
	}
	if old, _ := filepath.Glob(filepath.Join(modsDir, modJarGlob)); len(old) > 0 {
		for _, f := range old {
			_ = os.Remove(f)
		}
	}
	if err := os.WriteFile(filepath.Join(modsDir, name), data, 0o644); err != nil {
		return err
	}
	logln("      installiert: " + name)
	return nil
}
