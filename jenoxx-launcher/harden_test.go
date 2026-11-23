package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateJVMArgs(t *testing.T) {
	good := []string{
		"", defaultJVMArgs, "-Xms512M -Xmx4G", "-XX:+UseZGC -Dfoo.bar=1", "-ea",
	}
	for _, g := range good {
		if err := validateJVMArgs(g); err != nil {
			t.Errorf("%q sollte erlaubt sein: %v", g, err)
		}
	}
	bad := []string{
		"-javaagent:C:\\evil.jar", "-agentpath:evil.dll", "-agentlib:jdwp=transport=dt_socket",
		"-cp evil.jar", "-jar evil.jar", "@args.txt", "--class-path x", "-Xbootclasspath/a:evil.jar",
		"-XX:OnError=calc.exe", "-XX:OnOutOfMemoryError=calc.exe", "-XX:Flags=x", "-XX:VMOptionsFile=x",
		"-Djava.library.path=C:\\evil", "-DJAVA.CLASS.PATH=x", "-Dfabric.addMods=C:\\evil",
		"-Dlog4j2.configurationFile=http://x/y.xml", "-Xmx1G\n-javaagent:x", strings.Repeat("-Xa ", 400),
		"-XX:+UnlockDiagnosticVMOptions",
	}
	for _, b := range bad {
		if err := validateJVMArgs(b); err == nil {
			t.Errorf("%q muss abgelehnt werden", b)
		}
	}
}

func TestValidateJavaPath(t *testing.T) {
	dir := t.TempDir()
	java := filepath.Join(dir, "java")
	other := filepath.Join(dir, "calc.exe")
	for _, f := range []string{java, other} {
		if err := os.WriteFile(f, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateJavaPath(""); err != nil {
		t.Error("leer = automatisch muss erlaubt sein")
	}
	if err := validateJavaPath(java); err != nil {
		t.Errorf("java muss erlaubt sein: %v", err)
	}
	for _, bad := range []string{other, dir, filepath.Join(dir, "nope", "java"), "java", `\\server\share\java.exe`, "//server/share/java"} {
		if err := validateJavaPath(bad); err == nil {
			t.Errorf("%q muss abgelehnt werden", bad)
		}
	}
}

func TestSecureRedirect(t *testing.T) {
	first, _ := http.NewRequest("GET", "https://a.example/x", nil)
	next, _ := http.NewRequest("GET", "https://b.example/y", nil)
	next.Header.Set("X-Api-Key", "geheim")
	next.Header.Set("Authorization", "Bearer x")
	if err := secureRedirect(next, []*http.Request{first}); err != nil {
		t.Fatal(err)
	}
	if next.Header.Get("X-Api-Key") != "" || next.Header.Get("Authorization") != "" {
		t.Error("geheime Header muessen bei Serverwechsel entfernt werden")
	}
	same, _ := http.NewRequest("GET", "https://a.example/z", nil)
	same.Header.Set("X-Api-Key", "geheim")
	_ = secureRedirect(same, []*http.Request{first})
	if same.Header.Get("X-Api-Key") == "" {
		t.Error("derselbe Server darf den Header behalten")
	}
	plain, _ := http.NewRequest("GET", "http://a.example/z", nil)
	if err := secureRedirect(plain, []*http.Request{first}); err == nil {
		t.Error("Weiterleitung auf http muss blockiert werden")
	}
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = first
	}
	if err := secureRedirect(next, via); err == nil {
		t.Error("zu viele Weiterleitungen muessen blockiert werden")
	}
}

func TestHandlerHardening(t *testing.T) {
	s := &server{token: strings.Repeat("ab", 32), addr: "127.0.0.1:5555"}
	h := s.handler()
	do := func(method, path, host string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://"+host+path, nil)
		req.Host = host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	base := "/" + s.token + "/"

	rec := do("GET", base, s.addr, nil)
	if rec.Code != 200 {
		t.Fatalf("Startseite: %d", rec.Code)
	}
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(k) == "" {
			t.Errorf("Header %s fehlt", k)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("CSP ohne frame-ancestors")
	}
	if c := do("GET", "/"+strings.Repeat("00", 32)+"/", s.addr, nil).Code; c != 404 {
		t.Errorf("falscher Token: %d", c)
	}
	if c := do("GET", "/", s.addr, nil).Code; c != 404 {
		t.Errorf("ohne Token: %d", c)
	}
	if c := do("GET", base, "evil.example:5555", nil).Code; c != 404 {
		t.Errorf("fremder Host (DNS-Rebinding): %d", c)
	}
	if c := do("POST", base+"api/quit", s.addr, map[string]string{"Origin": "https://evil.example"}).Code; c != 403 {
		t.Errorf("fremder Origin: %d", c)
	}
	if c := do("POST", base+"api/quit", s.addr, map[string]string{"Sec-Fetch-Site": "cross-site"}).Code; c != 403 {
		t.Errorf("cross-site: %d", c)
	}
	if c := do("GET", base, s.addr, map[string]string{"Sec-Fetch-Site": "none", "Origin": "http://" + s.addr}).Code; c != 200 {
		t.Errorf("eigene Herkunft muss erlaubt sein: %d", c)
	}
	u, _ := url.Parse(base)
	_ = u
}

func TestNewTokenLength(t *testing.T) {
	a, b := newToken(), newToken()
	if len(a) != 64 || a == b {
		t.Errorf("Token unbrauchbar: %q %q", a, b)
	}
}
