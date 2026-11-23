package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// ---------- Absturzanalyse ----------

func TestAnalyzeCrashRules(t *testing.T) {
	cases := []struct {
		name, line, action string
	}{
		{"ram", "Exception in thread \"main\" java.lang.OutOfMemoryError: Java heap space", "settings"},
		{"jvm", "Error occurred during initialization of VM\nCould not reserve enough space for object heap", "settings"},
		{"java", "java.lang.UnsupportedClassVersionError: x has been compiled by a more recent version", "settings"},
		{"zip", "Error: Unable to access jarfile foo.jar", "repair"},
		{"fabric", "net.fabricmc.loader.impl.FormattedException: Some of your mods are incompatible with the game or each other!\n\t - Mod 'Sodium' (sodium) 0.6.0 requires version 1.21.4 of minecraft", "mods"},
		{"mixin", "Mixin apply for mod lithium failed lithium.mixins.json:foo", "mods"},
		{"gl", "GLFW error 65543: WGL: The driver does not appear to support OpenGL", "mods"},
		{"auth", "Failed to verify authentication", "login"},
		{"native", "# A fatal error has been detected by the Java Runtime Environment:", "repair"},
		{"class", "java.lang.NoSuchMethodError: net.minecraft.Foo.bar()V", "mods"},
		{"unknown", "irgendetwas ist passiert", "repair"},
	}
	for _, c := range cases {
		d := analyzeCrash(strings.Split(c.line, "\n"), "", 1)
		if d.Action != c.action || d.Title == "" || d.Hint == "" {
			t.Errorf("%s: Action=%q Title=%q", c.name, d.Action, d.Title)
		}
	}
	d := analyzeCrash([]string{"net.fabricmc.loader.impl.FormattedException: Some of your mods are incompatible", "\t - Mod 'Sodium' (sodium) 0.6.0 requires version 1.21.4 of minecraft"}, "", 1)
	if !strings.Contains(d.Detail, "Sodium") {
		t.Errorf("Detailzeile: %q", d.Detail)
	}
	d = analyzeCrash([]string{"Mixin apply for mod lithium failed x"}, "", 1)
	if len(d.Suspects) != 1 || d.Suspects[0] != "lithium" {
		t.Errorf("Mixin-Verdaechtiger: %v", d.Suspects)
	}
}

func TestSuspectedModsAndFallback(t *testing.T) {
	report := "---- Minecraft Crash Report ----\n\n-- System Details --\nDetails:\n\tSuspected Mods: \n\t\tSodium (sodium): Sodium 0.6.0\n\t\tIris (iris): Iris 1.8\n\tFabric Mods: \n\t\tsodium: Sodium 0.6.0\n"
	got := suspectedMods(report)
	if len(got) != 2 || got[0] != "Sodium (sodium)" || got[1] != "Iris (iris)" {
		t.Fatalf("suspectedMods: %v", got)
	}
	if got := suspectedMods("\tSuspected Mods: None\n\tFabric Mods:\n"); len(got) != 0 {
		t.Errorf("None: %v", got)
	}
	d := analyzeCrash([]string{"nichts erkennbares"}, report, 255)
	if d.Action != "mods" || len(d.Suspects) != 2 {
		t.Errorf("Fallback mit Verdacht: %+v", d)
	}
}

func TestNormExitAndRing(t *testing.T) {
	if normExit(4294967295) != -1 || normExit(1) != 1 || normExit(-1) != -1 || normExit(0) != 0 {
		t.Error("normExit")
	}
	r := newRing(3)
	for _, s := range []string{"a", "b", "c", "d"} {
		r.add(s)
	}
	if got := strings.Join(r.snapshot(), ""); got != "bcd" {
		t.Errorf("ring: %s", got)
	}
}

func TestFindCrashReport(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	old := filepath.Join(dir, "crash-reports", "crash-old.txt")
	_ = os.MkdirAll(filepath.Dir(old), 0o755)
	_ = os.WriteFile(old, []byte("x"), 0o644)
	_ = os.Chtimes(old, start.Add(-time.Hour), start.Add(-time.Hour))
	if got := findCrashReport(dir, start); got != "" {
		t.Errorf("alter Bericht gefunden: %s", got)
	}
	fresh := filepath.Join(dir, "crash-reports", "crash-new.txt")
	_ = os.WriteFile(fresh, []byte("Suspected Mods: None"), 0o644)
	if got := findCrashReport(dir, start); got != fresh {
		t.Errorf("neuer Bericht: %q", got)
	}
}

func TestPrefixWriterSinkRedacts(t *testing.T) {
	var got []string
	w := &prefixWriter{prefix: "[MC] ", secret: "GEHEIM", sink: func(s string) { got = append(got, s) }}
	_, _ = w.Write([]byte("token=GEHEIM ok\n\nzweite\n"))
	if len(got) != 2 || strings.Contains(got[0], "GEHEIM") || got[1] != "zweite" {
		t.Errorf("sink: %v", got)
	}
}

// ---------- Mod-Metadaten, Updates, Gesundheit ----------

func fakeMod(t *testing.T, dir, file, id, ver string) {
	t.Helper()
	makeZip(t, filepath.Join(dir, file), map[string]string{
		"fabric.mod.json": `{"schemaVersion":1,"id":"` + id + `","name":"Mod ` + id + `","version":"` + ver + `"}`,
	})
}

func TestModMetaAndHealth(t *testing.T) {
	dir := t.TempDir()
	fakeMod(t, dir, "fabric-api-1.jar", "fabric-api", "1.0")
	fakeMod(t, dir, "sodium-a.jar", "sodium", "0.6")
	fakeMod(t, dir, "sodium-b.jar", "sodium", "0.5")
	_ = os.WriteFile(filepath.Join(dir, "x.jar.part"), []byte("x"), 0o644)

	mods := listMods(dir)
	fillModMeta(dir, mods)
	var found bool
	for _, m := range mods {
		if m.File == "sodium-a.jar" && m.ID == "sodium" && m.Version == "0.6" && m.Title == "Mod sodium" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Metadaten: %+v", mods)
	}
	items, enabled := modHealth(dir)
	if enabled != 3 {
		t.Errorf("enabled=%d", enabled)
	}
	var dup, part bool
	for _, it := range items {
		if it.Level == "error" && strings.Contains(it.Text, "Doppelte Mod \"sodium\"") {
			dup = true
		}
		if it.Level == "warn" && strings.Contains(it.Text, ".part") {
			part = true
		}
	}
	if !dup || !part {
		t.Errorf("Pruefung: %+v", items)
	}
	// ohne Fabric API
	empty := t.TempDir()
	fakeMod(t, empty, "a.jar", "a", "1")
	items, _ = modHealth(empty)
	if len(items) != 1 || !strings.Contains(items[0].Text, "Fabric API") {
		t.Errorf("Fabric-API-Hinweis: %+v", items)
	}
}

func TestHashMods(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.jar"), []byte("PKabc"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "jenoxx-client-1.0.jar"), []byte("PKown"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.jar.disabled"), []byte("PKabc"), 0o644)
	h := hashMods(dir)
	if len(h) != 2 {
		t.Fatalf("hashes: %+v", h)
	}
	if h[0].SHA1 != sha1Hex([]byte("PKabc")) || h[0].SHA1 != h[1].SHA1 {
		t.Errorf("sha1: %+v", h)
	}
	for _, x := range h {
		if strings.HasPrefix(x.File, "jenoxx-client") {
			t.Error("eigener Mod darf nicht aktualisiert werden")
		}
	}
}

func TestReplaceMod(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "m-1.jar"), []byte("PKold"), 0o644)
	if _, err := saveUploadedModReplace(dir, "m-2.jar", "m-1.jar", strings.NewReader("PKnew")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "m-1.jar")) || !exists(filepath.Join(dir, "m-2.jar")) {
		t.Error("Update ersetzt die alte Datei nicht")
	}
	// deaktivierte Mod bleibt deaktiviert
	_ = os.WriteFile(filepath.Join(dir, "d-1.jar.disabled"), []byte("PKold"), 0o644)
	if _, err := saveUploadedModReplace(dir, "d-2.jar", "d-1.jar.disabled", strings.NewReader("PKnew")); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "d-2.jar.disabled")) || exists(filepath.Join(dir, "d-2.jar")) || exists(filepath.Join(dir, "d-1.jar.disabled")) {
		t.Errorf("deaktivierter Zustand ging verloren: %v", listMods(dir))
	}
	// gleicher Dateiname: Inhalt wird ersetzt
	if _, err := saveUploadedModReplace(dir, "m-2.jar", "m-2.jar", strings.NewReader("PKnewer")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "m-2.jar")); string(b) != "PKnewer" {
		t.Errorf("Inhalt: %s", b)
	}
	// Fabric API darf beim Update trotz vorhandener Datei ersetzt werden
	_ = os.WriteFile(filepath.Join(dir, "fabric-api-1.jar"), []byte("PKold"), 0o644)
	skipped, err := saveUploadedModReplace(dir, "fabric-api-2.jar", "fabric-api-1.jar", strings.NewReader("PKnew"))
	if err != nil || skipped || exists(filepath.Join(dir, "fabric-api-1.jar")) {
		t.Errorf("Fabric-API-Update: skipped=%v err=%v", skipped, err)
	}
	// unsicherer replace-Name
	if _, err := saveUploadedModReplace(dir, "x.jar", "../evil.jar", strings.NewReader("PKx")); err == nil {
		t.Error("replace mit Pfad akzeptiert")
	}
	if _, err := installModReplace(dir, "https://evil.example/x.jar", "x.jar", "m-2.jar"); err == nil {
		t.Error("fremder Host akzeptiert")
	}
}

func TestDeepVerify(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(f, []byte("abcd"), 0o644)
	good := sha1Hex([]byte("abcd"))
	bad := sha1Hex([]byte("wxyz"))
	if !upToDate(f, 4, bad) { // normal: nur Groesse
		t.Error("normaler Modus soll nur die Groesse pruefen")
	}
	deepVerify = true
	defer func() { deepVerify = false }()
	if upToDate(f, 4, bad) || !upToDate(f, 4, good) {
		t.Error("Tiefenpruefung")
	}
}

// ---------- Modpacks ----------

func fabricIndex(extra string) string {
	return `{"formatVersion":1,"game":"minecraft","name":"Mein Pack","versionId":"1.0","dependencies":{"minecraft":"1.21.4","fabric-loader":"0.16.10"},"files":[` + extra + `]}`
}

func TestSafeRelPath(t *testing.T) {
	for _, bad := range []string{"", "/etc/x", "../x", "mods/../../x", `mods\x.jar`, "C:/x", "a//b", "mods/x.jar.", "mods/", "mods/ ./x"} {
		if _, err := safeRelPath(bad); err == nil {
			t.Errorf("%q akzeptiert", bad)
		}
	}
	if _, err := safeRelPath("mods/setup.exe"); err != errBlockedExt {
		t.Errorf("exe: %v", err)
	}
	if p, err := safeRelPath("config/sodium-options.json"); err != nil || p != filepath.FromSlash("config/sodium-options.json") {
		t.Errorf("gueltiger Pfad: %q %v", p, err)
	}
}

func TestReadMrpackIndex(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.mrpack")
	makeZip(t, ok, map[string]string{"modrinth.index.json": fabricIndex("")})
	idx, err := readMrpackIndex(ok)
	if err != nil || idx.Dependencies["minecraft"] != "1.21.4" {
		t.Fatalf("gueltiges Pack: %v", err)
	}
	forge := filepath.Join(dir, "forge.mrpack")
	makeZip(t, forge, map[string]string{"modrinth.index.json": `{"formatVersion":1,"game":"minecraft","name":"x","dependencies":{"minecraft":"1.20.1","forge":"47.1"},"files":[]}`})
	if _, err := readMrpackIndex(forge); err == nil || !strings.Contains(err.Error(), "forge") {
		t.Errorf("Forge-Pack: %v", err)
	}
	none := filepath.Join(dir, "none.mrpack")
	makeZip(t, none, map[string]string{"a.txt": "x"})
	if _, err := readMrpackIndex(none); err == nil {
		t.Error("Archiv ohne Index akzeptiert")
	}
	notzip := filepath.Join(dir, "no.mrpack")
	_ = os.WriteFile(notzip, []byte("kein zip"), 0o644)
	if _, err := readMrpackIndex(notzip); err == nil {
		t.Error("Nicht-ZIP akzeptiert")
	}
}

func TestPackTasks(t *testing.T) {
	game := t.TempDir()
	sum := sha1Hex([]byte("x"))
	good := `{"path":"mods/a.jar","hashes":{"sha1":"` + sum + `"},"env":{"client":"required"},"downloads":["https://cdn.modrinth.com/data/a.jar"],"fileSize":1}`
	serverOnly := `{"path":"mods/s.jar","hashes":{"sha1":"` + sum + `"},"env":{"client":"unsupported"},"downloads":["https://cdn.modrinth.com/data/s.jar"],"fileSize":1}`
	exe := `{"path":"mods/evil.exe","hashes":{"sha1":"` + sum + `"},"downloads":["https://cdn.modrinth.com/data/e"],"fileSize":1}`
	path := filepath.Join(t.TempDir(), "p.mrpack")
	makeZip(t, path, map[string]string{"modrinth.index.json": fabricIndex(good + "," + serverOnly + "," + exe)})
	idx, err := readMrpackIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := packTasks(game, idx)
	if err != nil || len(tasks) != 1 || tasks[0].dest != filepath.Join(game, "mods", "a.jar") || tasks[0].sha1 != sum {
		t.Fatalf("tasks: %+v err=%v", tasks, err)
	}
	for _, bad := range []string{
		`{"path":"mods/a.jar","hashes":{"sha1":"` + sum + `"},"downloads":["https://evil.example/a.jar"]}`,
		`{"path":"../a.jar","hashes":{"sha1":"` + sum + `"},"downloads":["https://cdn.modrinth.com/a.jar"]}`,
		`{"path":"mods/a.jar","hashes":{},"downloads":["https://cdn.modrinth.com/a.jar"]}`,
		`{"path":"mods/a.jar","hashes":{"sha1":"` + sum + `"},"downloads":["http://cdn.modrinth.com/a.jar"]}`,
	} {
		p2 := filepath.Join(t.TempDir(), "q.mrpack")
		makeZip(t, p2, map[string]string{"modrinth.index.json": fabricIndex(bad)})
		idx2, err := readMrpackIndex(p2)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := packTasks(game, idx2); err == nil {
			t.Errorf("unsicherer Eintrag akzeptiert: %s", bad)
		}
	}
}

func TestExtractOverrides(t *testing.T) {
	game := t.TempDir()
	pack := filepath.Join(t.TempDir(), "p.mrpack")
	makeZip(t, pack, map[string]string{
		"modrinth.index.json":                 fabricIndex(""),
		"overrides/config/a.toml":             "base",
		"overrides/config/b.toml":             "b",
		"client-overrides/config/a.toml":      "client",
		"overrides/mods/tool.exe":             "MZ",
		"server-overrides/config/server.toml": "nein",
	})
	n, err := extractOverrides(pack, game)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(game, "config", "a.toml")); string(b) != "client" {
		t.Errorf("client-overrides muss gewinnen: %s", b)
	}
	if exists(filepath.Join(game, "mods", "tool.exe")) || exists(filepath.Join(game, "config", "server.toml")) {
		t.Error("exe oder server-overrides wurden geschrieben")
	}
	// Zip-Slip
	evil := filepath.Join(t.TempDir(), "e.mrpack")
	makeZip(t, evil, map[string]string{"modrinth.index.json": fabricIndex(""), "overrides/../../escape.txt": "x"})
	if _, err := extractOverrides(evil, game); err == nil {
		t.Error("Pfad-Ausbruch nicht abgelehnt")
	}
}

func TestImportPackFileAndOverridesOnce(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	saved := profiles
	profiles = &store{}
	defer func() { profiles = saved }()
	if err := profiles.load(); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "pack.mrpack")
	makeZip(t, src, map[string]string{"modrinth.index.json": fabricIndex(""), "overrides/config/x.toml": "v1"})
	p, err := importPackFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Pack != "Mein Pack" || p.MC != "1.21.4" || p.RAM < 4096 {
		t.Errorf("Profil: %+v", p)
	}
	if got, ok := profiles.get(p.ID); !ok || got.Pack == "" {
		t.Error("Profil nicht gespeichert")
	}
	mc := t.TempDir()
	cnt, skipped, err := installPackOverrides(p, mc)
	if err != nil || skipped || cnt != 1 {
		t.Fatalf("erstes Kopieren: %d %v %v", cnt, skipped, err)
	}
	// Nutzer aendert die Konfiguration; Reparatur darf sie nicht ueberschreiben
	cfg := filepath.Join(profileGameDir(mc, p), "config", "x.toml")
	_ = os.WriteFile(cfg, []byte("meine-aenderung"), 0o644)
	_, skipped, err = installPackOverrides(p, mc)
	if err != nil || !skipped {
		t.Fatalf("zweites Kopieren: %v %v", skipped, err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "meine-aenderung" {
		t.Errorf("Konfiguration ueberschrieben: %s", b)
	}
	if _, err := importPackFile(filepath.Join(t.TempDir(), "fehlt.mrpack")); err == nil {
		t.Error("fehlende Datei akzeptiert")
	}
}

// ---------- Modrinth ueber den Launcher ----------

func TestValidModrinthCall(t *testing.T) {
	for _, c := range []struct{ m, p string }{
		{"GET", "/search?query=sodium&limit=10"}, {"GET", "/project/AANobbMI/version"}, {"POST", "/version_files/update"},
	} {
		if _, err := validModrinthCall(c.m, c.p); err != nil {
			t.Errorf("%s %s abgelehnt: %v", c.m, c.p, err)
		}
	}
	for _, c := range []struct{ m, p string }{
		{"GET", "search"}, {"GET", "//evil.example/x"}, {"GET", "/../x"}, {"GET", "/a@evil.example"}, {"GET", "/a b"},
		{"POST", "/user"}, {"DELETE", "/search"}, {"PUT", "/search"},
	} {
		if _, err := validModrinthCall(c.m, c.p); err == nil {
			t.Errorf("%s %s akzeptiert", c.m, c.p)
		}
	}
}

func TestModrinthRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			w.Write([]byte(`{"echo":` + string(b) + `}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v2/missing") {
			http.Error(w, `{"error":"nf"}`, 404)
			return
		}
		w.Write([]byte(`{"ok":true,"path":"` + r.URL.RequestURI() + `"}`))
	}))
	defer srv.Close()
	old := modrinthBase
	modrinthBase = srv.URL + "/v2"
	defer func() { modrinthBase = old }()

	code, data, err := modrinthRequest("GET", "/search?query=a", nil)
	if err != nil || code != 200 || !strings.Contains(string(data), "/v2/search?query=a") {
		t.Fatalf("GET: %d %s %v", code, data, err)
	}
	code, data, err = modrinthRequest("POST", "/version_files/update", []byte(`{"x":1}`))
	if err != nil || code != 200 || string(data) != `{"echo":{"x":1}}` {
		t.Fatalf("POST: %d %s %v", code, data, err)
	}
	if code, _, err = modrinthRequest("GET", "/missing", nil); err != nil || code != 404 {
		t.Errorf("404 muss durchgereicht werden: %d %v", code, err)
	}
}

// ---------- CurseForge ----------

func TestCFPickFileAndKey(t *testing.T) {
	f, ok := cfPickFile([]cfFile{
		{ID: 1, IsAvailable: true, ReleaseType: 1}, {ID: 5, IsAvailable: true, ReleaseType: 2},
		{ID: 3, IsAvailable: true, ReleaseType: 1}, {ID: 9, IsAvailable: false, ReleaseType: 1},
	})
	if !ok || f.ID != 3 {
		t.Errorf("neueste verfuegbare Release erwartet, bekam %+v", f)
	}
	if f, ok = cfPickFile([]cfFile{{ID: 2, IsAvailable: true, ReleaseType: 2}, {ID: 4, IsAvailable: true, ReleaseType: 3}}); !ok || f.ID != 4 {
		t.Errorf("ohne Release die neueste: %+v", f)
	}
	if _, ok = cfPickFile(nil); ok {
		t.Error("leere Liste")
	}

	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("JENOXX_CF_KEY", "")
	if loadCFKey() != "" {
		t.Fatal("Schluessel ohne Eingabe")
	}
	key := "$2a$10$abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQ"
	if err := saveCFKey(key); err != nil {
		t.Fatal(err)
	}
	if loadCFKey() != key {
		t.Error("Schluessel nicht wiedergefunden")
	}
	if err := saveCFKey(""); err != nil || loadCFKey() != "" {
		t.Error("Schluessel loeschen")
	}
	if _, _, err := cfRequestKey(key, "/mods/search/../../x", nil); err == nil {
		t.Error("unerlaubter Pfad akzeptiert")
	}
	if _, _, err := cfRequest("/mods/search", nil); err != errNoCFKey {
		t.Errorf("ohne Schluessel: %v", err)
	}
}

func TestCFInstall(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	key := "$2a$10$abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQ"
	t.Setenv("JENOXX_CF_KEY", key)
	mkjar := func(id string) []byte {
		var b strings.Builder
		zw := zip.NewWriter(&b)
		w, _ := zw.Create("fabric.mod.json")
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"` + id + `","version":"1"}`))
		_ = zw.Close()
		return []byte(b.String())
	}
	mainJar, libJar := mkjar("coolmod"), mkjar("libdup")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != key {
			http.Error(w, "no key", 403)
			return
		}
		switch {
		case r.URL.Path == "/v1/mods/101/files":
			if r.URL.Query().Get("modLoaderType") != "4" || r.URL.Query().Get("gameVersion") != "1.21.4" {
				http.Error(w, "filter", 400)
				return
			}
			fmt.Fprintf(w, `{"data":[{"id":11,"fileName":"coolmod-1.0.jar","releaseType":1,"isAvailable":true,"downloadUrl":"%s/cdn/coolmod-1.0.jar","hashes":[{"value":"%s","algo":1}],"dependencies":[{"modId":102,"relationType":3},{"modId":306612,"relationType":3},{"modId":103,"relationType":2}]}]}`, srv.URL, sha1Hex(mainJar))
		case r.URL.Path == "/v1/mods/102/files":
			w.Write([]byte(`{"data":[{"id":7,"fileName":"libdup-2.jar","releaseType":1,"isAvailable":true,"downloadUrl":null,"hashes":[],"dependencies":[]}]}`))
		case r.URL.Path == "/v1/mods/102/files/7/download-url":
			fmt.Fprintf(w, `{"data":"%s/cdn/libdup-2.jar"}`, srv.URL)
		case r.URL.Path == "/v1/mods/200/files":
			w.Write([]byte(`{"data":[{"id":1,"fileName":"closed.jar","releaseType":1,"isAvailable":true,"downloadUrl":null,"hashes":[],"dependencies":[]}]}`))
		case r.URL.Path == "/v1/mods/200/files/1/download-url":
			http.Error(w, "no", 403)
		case r.URL.Path == "/cdn/coolmod-1.0.jar":
			w.Write(mainJar)
		case r.URL.Path == "/cdn/libdup-2.jar":
			w.Write(libJar)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldBase, oldOK, oldHTTP := cfBase, cfCDNHostOK, cfAllowHTTP
	cfBase, cfCDNHostOK, cfAllowHTTP = srv.URL+"/v1", func(string) bool { return true }, true
	defer func() { cfBase, cfCDNHostOK, cfAllowHTTP = oldBase, oldOK, oldHTTP }()

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "fabric-api-1.jar"), []byte("PK"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "libdup-1.jar"), mkjar("libdup"), 0o644) // gleiche Mod-ID schon vorhanden
	res := &cfResult{}
	if err := cfInstall(dir, "1.21.4", 101, map[int]bool{}, 0, res); err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 1 || res.Installed[0] != "coolmod-1.0.jar" {
		t.Errorf("installiert: %+v", res)
	}
	if !exists(filepath.Join(dir, "coolmod-1.0.jar")) {
		t.Error("Hauptmod fehlt")
	}
	if exists(filepath.Join(dir, "libdup-2.jar")) {
		t.Error("Duplikat (gleiche Mod-ID) darf nicht liegen bleiben")
	}
	joined := strings.Join(res.Skipped, " | ")
	if !strings.Contains(joined, "Fabric API") || !strings.Contains(joined, "gleiche Mod wie libdup-1.jar") {
		t.Errorf("uebersprungen: %s", joined)
	}
	if err := cfInstall(dir, "1.21.4", 200, map[int]bool{}, 0, &cfResult{}); err != errCFDistrib {
		t.Errorf("Download-Sperre: %v", err)
	}
	// falsche Pruefsumme
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("anderer inhalt")) }))
	defer srvBad.Close()
	if err := cfDownload(srvBad.URL+"/x.jar", filepath.Join(dir, "x.jar"), sha1Hex([]byte("erwartet"))); err == nil {
		t.Error("falsche SHA-1 akzeptiert")
	}
	// fremder Host
	cfCDNHostOK = func(h string) bool { return false }
	if err := cfDownload(srv.URL+"/cdn/coolmod-1.0.jar", filepath.Join(dir, "y.jar"), ""); err == nil {
		t.Error("fremder Host akzeptiert")
	}
}

// ---------- Server, Fenster, Duplizieren ----------

func TestServerWindowAndArgs(t *testing.T) {
	for _, ok := range []string{"", "mc.example.net", "127.0.0.1:25565", "a-b.example.com:1"} {
		if _, err := validateServer(ok); err != nil {
			t.Errorf("%q abgelehnt: %v", ok, err)
		}
	}
	for _, bad := range []string{"mc example", "host:0", "host:70000", "http://x", "-bad.net", "a/b", "x;y"} {
		if _, err := validateServer(bad); err == nil {
			t.Errorf("%q akzeptiert", bad)
		}
	}
	if validateWindow(0, 0) != nil || validateWindow(1280, 720) != nil || validateWindow(100, 100) == nil || validateWindow(1280, 0) == nil {
		t.Error("validateWindow")
	}
	if mcMinor("1.21.4") != 21 || mcMinor("1.19.2") != 19 || mcMinor("24w14a") < 20 || mcMinor("22w11a") >= 20 || mcMinor("x") != 0 {
		t.Error("mcMinor")
	}
	got := strings.Join(extraGameArgs(Profile{MC: "1.21.4", Server: "mc.example.net:25570", Width: 1280, Height: 720}), " ")
	if got != "--width 1280 --height 720 --quickPlayMultiplayer mc.example.net:25570" {
		t.Errorf("neue Version: %s", got)
	}
	got = strings.Join(extraGameArgs(Profile{MC: "1.19.2", Server: "mc.example.net", Fullscreen: true, Width: 800, Height: 600}), " ")
	if got != "--fullscreen --server mc.example.net --port 25565" {
		t.Errorf("alte Version: %s", got)
	}
	if len(extraGameArgs(Profile{MC: "1.21.4"})) != 0 {
		t.Error("ohne Optionen keine Argumente")
	}
	// im Startbefehl am Ende
	v := &version{MainClass: "net.minecraft.client.main.Main"}
	argv := commandFor("java", Profile{MC: "1.21.4", RAM: 2048, Server: "a.b"}, v, map[string]string{})
	if argv[len(argv)-2] != "--quickPlayMultiplayer" || argv[len(argv)-1] != "a.b" {
		t.Errorf("Startbefehl: %v", argv)
	}
}

func TestDuplicateProfile(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	saved := profiles
	profiles = &store{}
	defer func() { profiles = saved }()
	if err := profiles.load(); err != nil {
		t.Fatal(err)
	}
	mc := t.TempDir()
	src := Profile{ID: "orig", Name: "Mein Profil", MC: "1.21.4", RAM: 6144, Server: "a.b", Ready: true, VersionID: "fabric-1", PlaySeconds: 99, LastPlayed: "x"}
	if err := profiles.add(src); err != nil {
		t.Fatal(err)
	}
	sd := profileGameDir(mc, src)
	_ = os.MkdirAll(filepath.Join(sd, "mods"), 0o755)
	_ = os.MkdirAll(filepath.Join(sd, "saves", "Welt1"), 0o755)
	_ = os.MkdirAll(filepath.Join(sd, "config"), 0o755)
	_ = os.WriteFile(filepath.Join(sd, "mods", "a.jar"), []byte("PKa"), 0o644)
	_ = os.WriteFile(filepath.Join(sd, "config", "x.toml"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(sd, "options.txt"), []byte("fov:90"), 0o644)
	_ = os.WriteFile(filepath.Join(sd, "saves", "Welt1", "level.dat"), []byte("welt"), 0o644)

	p, err := duplicateProfile(mc, src)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == src.ID || p.Name != "Mein Profil (Kopie)" || p.RAM != 6144 || p.Server != "a.b" || p.PlaySeconds != 0 || p.LastPlayed != "" || !p.Ready {
		t.Errorf("Kopie: %+v", p)
	}
	dd := profileGameDir(mc, p)
	for _, f := range []string{"mods/a.jar", "config/x.toml", "options.txt"} {
		if !exists(filepath.Join(dd, filepath.FromSlash(f))) {
			t.Errorf("%s fehlt in der Kopie", f)
		}
	}
	if exists(filepath.Join(dd, "saves")) {
		t.Error("Welten duerfen nicht kopiert werden")
	}
	if got, ok := profiles.get(p.ID); !ok || got.Name != p.Name {
		t.Error("Kopie nicht gespeichert")
	}
	if !exists(filepath.Join(sd, "mods", "a.jar")) {
		t.Error("Original veraendert")
	}
}

func TestHeadFromSkin(t *testing.T) {
	skin := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 8; y < 16; y++ {
		for x := 8; x < 16; x++ {
			skin.SetNRGBA(x, y, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	// Hut-Ebene: ein deckendes Pixel links oben
	skin.SetNRGBA(40, 8, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, skin); err != nil {
		t.Fatal(err)
	}
	out, err := headFromSkin(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 64 || img.Bounds().Dy() != 64 {
		t.Fatalf("Groesse %v", img.Bounds())
	}
	r, g, b, _ := img.At(0, 0).RGBA()
	if r>>8 != 1 || g>>8 != 2 || b>>8 != 3 {
		t.Errorf("Hut nicht aufgelegt: %d %d %d", r>>8, g>>8, b>>8)
	}
	r, g, b, _ = img.At(63, 63).RGBA()
	if r>>8 != 200 || g>>8 != 100 || b>>8 != 50 {
		t.Errorf("Gesicht falsch: %d %d %d", r>>8, g>>8, b>>8)
	}
	if _, err := headFromSkin([]byte("kein png")); err == nil {
		t.Error("Muell muss scheitern")
	}
	if _, err := fetchHead("../../etc"); err == nil {
		t.Error("ungueltige UUID muss scheitern")
	}
}

func TestFetchHeadViaMojangMock(t *testing.T) {
	skin := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	var sbuf bytes.Buffer
	_ = png.Encode(&sbuf, skin)
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/session/minecraft/profile/") {
			tex := `{"textures":{"SKIN":{"url":"` + ts.URL + `/skin.png"}}}`
			fmt.Fprintf(w, `{"properties":[{"name":"textures","value":"%s"}]}`, base64.StdEncoding.EncodeToString([]byte(tex)))
			return
		}
		w.Write(sbuf.Bytes())
	}))
	defer ts.Close()
	oldBase, oldOK := sessionBase, skinHostOK
	sessionBase = ts.URL
	skinHostOK = func(h string) bool { return h == "127.0.0.1" }
	defer func() { sessionBase, skinHostOK = oldBase, oldOK }()
	out, err := fetchHead("0123456789abcdef0123456789abcdef")
	if err != nil || len(out) == 0 {
		t.Fatalf("fetchHead: %v", err)
	}
	// ein fremder Host fuer den Skin wird abgelehnt
	skinHostOK = func(h string) bool { return false }
	headCache.Lock()
	headCache.m = map[string]cachedHead{}
	headCache.Unlock()
	if _, err := fetchHead("0123456789abcdef0123456789abcdef"); err == nil {
		t.Error("fremder Skin-Host muss abgelehnt werden")
	}
}

func TestWriteOfficialProfile(t *testing.T) {
	dir := t.TempDir()
	p := Profile{ID: "abc", Name: "PvP", MC: "1.21.4", RAM: 4096, Ready: true, VersionID: "fabric-loader-0.16.10-1.21.4"}
	if err := writeOfficialProfile(dir, Profile{ID: "x"}); err == nil {
		t.Error("nicht installiertes Profil muss abgelehnt werden")
	}
	// bestehende Datei mit fremdem Profil und eigenen Einstellungen bleibt erhalten
	orig := `{"profiles":{"fremd":{"name":"Mein Profil","lastVersionId":"1.20.1"}},"settings":{"crashAssistance":true},"version":3}`
	if err := os.WriteFile(filepath.Join(dir, "launcher_profiles.json"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // zweimal: aktualisiert statt zu verdoppeln
		if err := writeOfficialProfile(dir, p); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "launcher_profiles.json"))
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var profs map[string]map[string]any
	_ = json.Unmarshal(root["profiles"], &profs)
	if len(profs) != 2 || profs["fremd"]["name"] != "Mein Profil" {
		t.Fatalf("Profile: %v", profs)
	}
	e := profs["jenoxx-abc"]
	if e["lastVersionId"] != p.VersionID || e["type"] != "custom" || e["name"] != "JenoXx - PvP" ||
		e["gameDir"] != filepath.Join(dir, "jenoxx-abc") || !strings.Contains(e["javaArgs"].(string), "-Xmx4096M") {
		t.Errorf("Eintrag: %v", e)
	}
	if !strings.Contains(string(root["settings"]), "crashAssistance") {
		t.Error("Einstellungen gingen verloren")
	}
	if !exists(filepath.Join(dir, "launcher_profiles.json.jenoxx-backup")) {
		t.Error("Sicherung fehlt")
	}
	// beschaedigte Datei wird nicht ueberschrieben
	_ = os.WriteFile(filepath.Join(dir, "launcher_profiles.json"), []byte("{kaputt"), 0o644)
	if err := writeOfficialProfile(dir, p); err == nil {
		t.Error("beschaedigte Datei darf nicht ueberschrieben werden")
	}
	// ohne vorhandene Datei wird eine neue angelegt
	dir2 := t.TempDir()
	if err := writeOfficialProfile(dir2, p); err != nil || !exists(filepath.Join(dir2, "launcher_profiles.json")) {
		t.Errorf("neue Datei: %v", err)
	}
}
