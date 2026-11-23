package main

import (
	"archive/zip"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const vanillaJSON = `{
 "id": "1.21.4", "type": "release", "mainClass": "net.minecraft.client.main.Main", "assets": "19",
 "assetIndex": {"id": "19", "url": "http://x/19.json", "sha1": "", "size": 1},
 "downloads": {"client": {"url": "http://x/client.jar", "sha1": "", "size": 1}},
 "javaVersion": {"component": "java-runtime-delta", "majorVersion": 21},
 "arguments": {
  "game": ["--username", "${auth_player_name}", "--version", "${version_name}",
   {"rules": [{"action": "allow", "features": {"is_demo_user": true}}], "value": "--demo"},
   {"rules": [{"action": "allow", "features": {"has_custom_resolution": true}}], "value": ["--width", "${resolution_width}"]}],
  "jvm": [
   {"rules": [{"action": "allow", "os": {"name": "windows"}}], "value": "-XX:HeapDumpPath=x.heapdump"},
   {"rules": [{"action": "allow", "os": {"name": "osx"}}], "value": ["-XstartOnFirstThread"]},
   "-Djava.library.path=${natives_directory}", "-cp", "${classpath}"]
 },
 "libraries": [
  {"name": "com.mojang:logging:1.3.0", "downloads": {"artifact": {"path": "com/mojang/logging/1.3.0/logging-1.3.0.jar", "url": "http://x/logging.jar", "sha1": "aa", "size": 5}}},
  {"name": "org.lwjgl:lwjgl:3.3.3:natives-windows", "rules": [{"action": "allow", "os": {"name": "windows"}}],
   "downloads": {"artifact": {"path": "org/lwjgl/lwjgl/3.3.3/lwjgl-3.3.3-natives-windows.jar", "url": "http://x/n.jar", "sha1": "bb", "size": 6}}},
  {"name": "org.lwjgl:lwjgl:3.3.3:natives-windows-arm64", "rules": [{"action": "allow", "os": {"name": "windows", "arch": "arm64"}}],
   "downloads": {"artifact": {"path": "org/lwjgl/lwjgl/3.3.3/lwjgl-3.3.3-natives-windows-arm64.jar", "url": "http://x/a.jar", "sha1": "cc", "size": 7}}},
  {"name": "org.lwjgl:lwjgl:3.3.3:natives-macos", "rules": [{"action": "allow", "os": {"name": "osx"}}],
   "downloads": {"artifact": {"path": "org/lwjgl/lwjgl/3.3.3/lwjgl-3.3.3-natives-macos.jar", "url": "http://x/m.jar", "sha1": "dd", "size": 8}}},
  {"name": "org.ow2.asm:asm:9.6", "downloads": {"artifact": {"path": "org/ow2/asm/asm/9.6/asm-9.6.jar", "url": "http://x/asm96.jar", "sha1": "ee", "size": 9}}}
 ]
}`

const fabricJSON = `{
 "id": "fabric-loader-0.16.10-1.21.4", "inheritsFrom": "1.21.4", "type": "release",
 "mainClass": "net.fabricmc.loader.impl.launch.knot.KnotClient",
 "arguments": {"game": [], "jvm": ["-DFabricMcEmu= net.minecraft.client.main.Main "]},
 "libraries": [
  {"name": "net.fabricmc:fabric-loader:0.16.10", "url": "https://maven.fabricmc.net/"},
  {"name": "org.ow2.asm:asm:9.7.1", "url": "https://maven.fabricmc.net/"}
 ]
}`

func parse(t *testing.T, s string) *version {
	t.Helper()
	var v version
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func TestRulesAndArgs(t *testing.T) {
	v := parse(t, vanillaJSON)
	vars := map[string]string{"auth_player_name": "Steve", "version_name": "fabric-x", "natives_directory": "/n", "classpath": "CP"}
	jvm, game := buildArgs(v, vars)
	if strings.Join(game, " ") != "--username Steve --version fabric-x" {
		t.Errorf("game args: %v", game)
	}
	joined := strings.Join(jvm, " ")
	if !strings.Contains(joined, "-Djava.library.path=/n") || !strings.Contains(joined, "-cp CP") {
		t.Errorf("jvm args: %v", jvm)
	}
	if strings.Contains(joined, "XstartOnFirstThread") {
		t.Error("osx-Argument darf unter " + currentOS() + " nicht erscheinen")
	}
}

func TestLegacyMinecraftArguments(t *testing.T) {
	v := parse(t, `{"id":"1.8.9","mainClass":"net.minecraft.client.main.Main","minecraftArguments":"--username ${auth_player_name} --accessToken ${auth_access_token}"}`)
	jvm, game := buildArgs(v, map[string]string{"auth_player_name": "A", "auth_access_token": "T", "natives_directory": "/n", "classpath": "CP"})
	if strings.Join(game, " ") != "--username A --accessToken T" {
		t.Errorf("game: %v", game)
	}
	if strings.Join(jvm, " ") != "-Djava.library.path=/n -cp CP" {
		t.Errorf("jvm: %v", jvm)
	}
}

func TestMergeAndClasspath(t *testing.T) {
	m := mergeVersions(parse(t, vanillaJSON), parse(t, fabricJSON))
	if m.MainClass != "net.fabricmc.loader.impl.launch.knot.KnotClient" || m.ID != "fabric-loader-0.16.10-1.21.4" {
		t.Errorf("main/id: %s %s", m.MainClass, m.ID)
	}
	asm := 0
	for _, l := range m.Libraries {
		if strings.HasPrefix(l.Name, "org.ow2.asm:asm:") {
			asm++
			if l.Name != "org.ow2.asm:asm:9.7.1" {
				t.Errorf("Fabric-Version muss gewinnen: %s", l.Name)
			}
		}
	}
	if asm != 1 {
		t.Errorf("asm doppelt: %d", asm)
	}
	cp, err := classpathOf(m, "/mc", "1.21.4")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(cp, classpathSep())
	has := func(sub string) bool {
		for _, p := range parts {
			if strings.Contains(filepath.ToSlash(p), sub) {
				return true
			}
		}
		return false
	}
	if !has("fabric-loader/0.16.10/fabric-loader-0.16.10.jar") || !has("logging-1.3.0.jar") || !has("versions/1.21.4/1.21.4.jar") {
		t.Errorf("classpath: %v", parts)
	}
	if has("arm64") || has("macos") {
		t.Errorf("fremde Natives im Classpath: %v", parts)
	}
	if currentOS() == "windows" && !has("natives-windows.jar") {
		t.Error("windows-natives fehlen")
	}
}

func TestOldStyleNatives(t *testing.T) {
	l := library{Name: "org.lwjgl.lwjgl:lwjgl-platform:2.9.4-nightly-20150209",
		Natives: map[string]string{"windows": "natives-windows", "linux": "natives-linux", "osx": "natives-osx"},
		Extract: &struct {
			Exclude []string `json:"exclude"`
		}{Exclude: []string{"META-INF/"}}}
	fs, err := l.files()
	if err != nil || len(fs) != 1 || !fs[0].Natives || !strings.HasSuffix(fs[0].Path, "-natives-"+map[string]string{"windows": "windows", "linux": "linux", "osx": "osx"}[currentOS()]+".jar") {
		t.Fatalf("%+v %v", fs, err)
	}
	if fs[0].Exclude[0] != "META-INF/" || !strings.HasPrefix(fs[0].URL, mojangLibs) {
		t.Errorf("%+v", fs[0])
	}
}

func TestMavenPath(t *testing.T) {
	p, err := mavenPath("net.fabricmc:fabric-loader:0.16.10", "")
	if err != nil || p != "net/fabricmc/fabric-loader/0.16.10/fabric-loader-0.16.10.jar" {
		t.Errorf("%s %v", p, err)
	}
	p, _ = mavenPath("org.lwjgl:lwjgl:3.3.3:natives-windows", "")
	if p != "org/lwjgl/lwjgl/3.3.3/lwjgl-3.3.3-natives-windows.jar" {
		t.Error(p)
	}
	if _, err := mavenPath("kaputt", ""); err == nil {
		t.Error("Fehler erwartet")
	}
}

func TestCommandFor(t *testing.T) {
	v := mergeVersions(parse(t, vanillaJSON), parse(t, fabricJSON))
	p := Profile{ID: "x", RAM: 3072}
	vars := launchVars("/mc", "/mc/jenoxx-x", "fabric-loader-0.16.10-1.21.4", "1.21.4", v, session{"Steve", "uuid-1", "TOKEN"}, "CP")
	argv := commandFor("java", p, v, vars)
	if argv[0] != "java" || argv[1] != "-Xmx3072M" {
		t.Errorf("argv: %v", argv[:3])
	}
	idx := -1
	for i, a := range argv {
		if a == "net.fabricmc.loader.impl.launch.knot.KnotClient" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("Hauptklasse fehlt")
	}
	if strings.Join(argv[idx+1:], " ") != "--username Steve --version fabric-loader-0.16.10-1.21.4" {
		t.Errorf("game: %v", argv[idx+1:])
	}
	// Fabrics Argument mit Leerzeichen bleibt ein einzelnes Argument
	found := false
	for _, a := range argv {
		if a == "-DFabricMcEmu= net.minecraft.client.main.Main " {
			found = true
		}
	}
	if !found {
		t.Error("FabricMcEmu-Argument zerlegt")
	}
}

func TestFetchVerifiedAndFetchAll(t *testing.T) {
	body := []byte("hallo-datei")
	sum := sha1.Sum(body)
	good := hex.EncodeToString(sum[:])
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	dir := t.TempDir()
	dest := filepath.Join(dir, "a", "b.bin")
	if err := fetchVerified(srv.URL+"/f", dest, good, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	before := hits
	if err := fetchVerified(srv.URL+"/f", dest, good, int64(len(body))); err != nil || hits != before {
		t.Errorf("vorhandene Datei erneut geladen (hits %d->%d, err %v)", before, hits, err)
	}
	if err := fetchVerified(srv.URL+"/f", filepath.Join(dir, "bad.bin"), "0000", 0); err == nil {
		t.Error("falsche Pruefsumme akzeptiert")
	}
	if exists(filepath.Join(dir, "bad.bin")) {
		t.Error("Datei mit falscher Pruefsumme blieb liegen")
	}
	var tasks []task
	for i := 0; i < 25; i++ {
		tasks = append(tasks, task{srv.URL + "/f", filepath.Join(dir, "many", strings.Repeat("x", i+1)), good, 0})
	}
	if err := fetchAll(tasks, 4, "Test"); err != nil {
		t.Fatal(err)
	}
	if err := fetchAll([]task{{srv.URL + "/missing", filepath.Join(dir, "m"), "", 0}}, 2, "Test"); err == nil {
		t.Error("404 nicht gemeldet")
	}
}

func TestExtractNatives(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "n.jar")
	f, _ := os.Create(jar)
	zw := zip.NewWriter(f)
	for _, n := range []string{"lwjgl64.dll", "META-INF/MANIFEST.MF", "sub/x.dll"} {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte("x"))
	}
	_ = zw.Close()
	_ = f.Close()
	out := filepath.Join(dir, "out")
	if err := extractNatives(jar, out, []string{"META-INF/"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(out, "lwjgl64.dll")) || !exists(filepath.Join(out, "sub", "x.dll")) || exists(filepath.Join(out, "META-INF")) {
		t.Error("Natives falsch entpackt")
	}
}

func TestClientIDValidationAndAccountStorage(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	a := &authState{}
	if err := a.setClientID("nicht-gueltig"); err == nil {
		t.Error("ungueltige Client-ID akzeptiert")
	}
	id := "12345678-abcd-1234-abcd-1234567890ab"
	if err := a.setClientID(id); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.refresh, a.name, a.uuid = "REFRESH-TOKEN", "Steve", "u-1"
	err := a.saveAccountLocked()
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	b := &authState{}
	b.load()
	if b.clientID != id || b.refresh != "REFRESH-TOKEN" || b.name != "Steve" {
		t.Errorf("laden: %+v", b)
	}
	b.logout()
	c := &authState{}
	c.load()
	if c.refresh != "" {
		t.Error("Abmelden hat Token nicht entfernt")
	}
	if _, err := c.session(); err == nil {
		t.Error("session ohne Anmeldung sollte fehlschlagen")
	}
}

func TestXSTSErrors(t *testing.T) {
	if !strings.Contains(xstsError(2148916233, 401), "kein Xbox-Konto") || !strings.Contains(xstsError(2148916238, 401), "Kinderkonto") {
		t.Error("XSTS-Fehlertexte")
	}
}

func TestPKCEAndAuthorizeURL(t *testing.T) {
	v, c := newPKCE()
	sum := sha256.Sum256([]byte(v))
	if c != base64.RawURLEncoding.EncodeToString(sum[:]) || len(v) < 43 {
		t.Errorf("PKCE ungueltig: %s %s", v, c)
	}
	u := buildAuthorizeURL("cid-1", "http://localhost:5555", "st", c)
	for _, want := range []string{"client_id=cid-1", "response_type=code", "code_challenge_method=S256", "redirect_uri=http%3A%2F%2Flocalhost%3A5555", "state=st"} {
		if !strings.Contains(u, want) {
			t.Errorf("%s fehlt in %s", want, u)
		}
	}
}

func TestLoginCallback(t *testing.T) {
	a := &authState{}
	var calls int
	var gotOK bool
	var gotMsg string
	h := a.loginCallback("cid", "http://localhost:1", "STATE", "ver", func(ok bool, msg string) { calls++; gotOK, gotMsg = ok, msg })

	// falscher Status -> 400, kein Abschluss
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/?state=FALSCH&code=x", nil))
	if rec.Code != 400 || calls != 0 {
		t.Fatalf("code=%d calls=%d", rec.Code, calls)
	}
	// anderer Pfad (z. B. favicon) -> 404
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/favicon.ico", nil))
	if rec.Code != 404 || calls != 0 {
		t.Fatalf("favicon: code=%d calls=%d", rec.Code, calls)
	}
	// Abbruch durch den Nutzer
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/?state=STATE&error=access_denied&error_description=Abgebrochen", nil))
	if calls != 1 || gotOK || !strings.Contains(gotMsg, "Abgebrochen") || !strings.Contains(rec.Body.String(), "fehlgeschlagen") {
		t.Fatalf("calls=%d ok=%v msg=%q", calls, gotOK, gotMsg)
	}
	// zweiter Aufruf (Browser-Reload) loest keinen weiteren Abschluss aus
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/?state=STATE&error=access_denied", nil))
	if calls != 1 {
		t.Errorf("onDone mehrfach: %d", calls)
	}
	// ohne Code
	calls = 0
	h2 := a.loginCallback("cid", "r", "S2", "v", func(ok bool, msg string) { calls++; gotMsg = msg })
	h2(httptest.NewRecorder(), httptest.NewRequest("GET", "/?state=S2", nil))
	if calls != 1 || !strings.Contains(gotMsg, "keinen Code") {
		t.Errorf("ohne Code: calls=%d msg=%q", calls, gotMsg)
	}
}

func TestEmbeddedClientIDWins(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("JENOXX_CLIENT_ID", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	a := &authState{}
	a.load()
	info := a.info()
	if !info.ClientIDSet || !info.Fixed {
		t.Fatalf("%+v", info)
	}
	// feste ID laesst sich nicht ueberschreiben
	if err := a.setClientID("11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatal(err)
	}
	if a.clientID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("feste ID ueberschrieben: %s", a.clientID)
	}
}
