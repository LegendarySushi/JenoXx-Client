package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsModJar(t *testing.T) {
	cases := map[string]bool{
		"jenoxx-client-1.0.0.jar":         true,
		"jenoxx-client-1.0.0-sources.jar": false,
		"fabric-api-1.jar":                false,
	}
	for n, want := range cases {
		if isModJar(n) != want {
			t.Errorf("%s: erwartet %v", n, want)
		}
	}
}

func TestPickFabricAPIVersion(t *testing.T) {
	xml := `<metadata><versioning><versions>
<version>0.100.0+1.21</version>
<version>0.119.2+1.21.4</version>
<version>0.118.0+1.21.4</version>
<version>0.120.0+1.21.5</version>
</versions></versioning></metadata>`
	got, err := pickFabricAPIVersion([]byte(xml), "1.21.4")
	if err != nil || got != "0.118.0+1.21.4" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := pickFabricAPIVersion([]byte(xml), "1.99"); err == nil {
		t.Error("Fehler erwartet")
	}
}

func TestUnzipRejectsZipSlip(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "evil.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../evil.txt")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	_ = f.Close()
	if err := unzip(zp, filepath.Join(dir, "out")); err == nil {
		t.Fatal("Zip-Slip nicht erkannt")
	}
}

func TestUnzipOK(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "ok.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("a/b.txt")
	_, _ = w.Write([]byte("hi"))
	_ = zw.Close()
	_ = f.Close()
	out := filepath.Join(dir, "out")
	if err := unzip(zp, out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "a", "b.txt")); string(b) != "hi" {
		t.Fatal("Inhalt falsch")
	}
}

func TestEmbeddedSource(t *testing.T) {
	_, n, err := sourceHash()
	if err != nil || n < 5 {
		t.Fatalf("Quellcode nicht eingebettet: n=%d err=%v", n, err)
	}
	dst := t.TempDir()
	if err := extractSource(dst); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"build.gradle", "settings.gradle", "gradle.properties", "src/main/resources/fabric.mod.json", "src/main/java/dev/jenoxx/client/JenoXxClient.java"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(p))); err != nil {
			t.Errorf("fehlt: %s", p)
		}
	}
}

func TestProfileJavaArgsAndKey(t *testing.T) {
	p := Profile{ID: "abc", RAM: 6144}
	if profileKeyOf(p) != "jenoxx-abc" {
		t.Error("Schluessel falsch")
	}
	if !strings.HasPrefix(p.javaArgs(), "-Xmx6144M ") || !strings.Contains(p.javaArgs(), "UseG1GC") {
		t.Errorf("javaArgs: %s", p.javaArgs())
	}
	p.JVMArgs = "-Dfoo=1"
	if p.javaArgs() != "-Xmx6144M -Dfoo=1" {
		t.Errorf("javaArgs: %s", p.javaArgs())
	}
}

func TestSafeModFile(t *testing.T) {
	good := []string{"sodium-fabric-0.6.0+mc1.21.4.jar", "a.jar", "x.jar.disabled"}
	bad := []string{"", "../evil.jar", "a/b.jar", `a\b.jar`, "evil.exe", "x.jar.disabled.exe", "con:.jar", "a..b.jar"}
	for _, n := range good {
		if _, err := safeModFile(n, true); err != nil {
			t.Errorf("%q sollte gueltig sein: %v", n, err)
		}
	}
	for _, n := range bad {
		if _, err := safeModFile(n, true); err == nil {
			t.Errorf("%q sollte abgelehnt werden", n)
		}
	}
	if _, err := safeModFile("x.jar.disabled", false); err == nil {
		t.Error(".disabled ohne Erlaubnis akzeptiert")
	}
}

func TestInstallModFromURLRejectsHosts(t *testing.T) {
	dir := t.TempDir()
	for _, u := range []string{"http://cdn.modrinth.com/x.jar", "https://evil.com/x.jar", "https://cdn.modrinth.com.evil.com/x.jar", "file:///etc/passwd"} {
		if _, err := installModFromURL(dir, u, "x.jar"); err == nil {
			t.Errorf("%s sollte abgelehnt werden", u)
		}
	}
}

func TestModsToggleDeleteUpload(t *testing.T) {
	dir := t.TempDir()
	if _, err := saveUploadedMod(dir, "m.jar", strings.NewReader("PK\x03\x04zip")); err != nil {
		t.Fatal(err)
	}
	if _, err := saveUploadedMod(dir, "bad.jar", strings.NewReader("nope")); err == nil {
		t.Error("Nicht-Jar akzeptiert")
	}
	if err := toggleMod(dir, "m.jar"); err != nil {
		t.Fatal(err)
	}
	ms := listMods(dir)
	if len(ms) != 1 || ms[0].Enabled || ms[0].Name != "m" {
		t.Fatalf("listMods: %+v", ms)
	}
	if err := toggleMod(dir, "m.jar.disabled"); err != nil {
		t.Fatal(err)
	}
	if err := deleteMod(dir, "m.jar"); err != nil {
		t.Fatal(err)
	}
	if len(listMods(dir)) != 0 {
		t.Error("nicht geloescht")
	}
}

func TestFabricAPIDuplicateSkipped(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "fabric-api-0.1+1.21.4.jar"), []byte("PK"), 0o644)
	skipped, err := saveUploadedMod(dir, "fabric-api-0.2+1.21.4.jar", strings.NewReader("PKxx"))
	if err != nil || !skipped {
		t.Fatalf("skipped=%v err=%v", skipped, err)
	}
}

func TestDeleteProfileFilesSafety(t *testing.T) {
	mc := t.TempDir()
	p := Profile{ID: "t1"}
	dir := profileGameDir(mc, p)
	_ = os.MkdirAll(filepath.Join(dir, "mods"), 0o755)
	if err := deleteProfileFiles(mc, p); err != nil || exists(dir) {
		t.Fatalf("nicht geloescht: %v", err)
	}
	if err := deleteProfileFiles(mc, Profile{ID: ""}); err == nil {
		t.Error("leere ID akzeptiert")
	}
}

func TestProfileStoreLifecycle(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	s := &store{}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	if len(s.all()) != 1 || s.all()[0].ID != "client" {
		t.Fatalf("Standardprofil fehlt: %+v", s.all())
	}
	id := newProfileID("Mein Profil!")
	if !strings.HasPrefix(id, "mein-profil-") {
		t.Errorf("id: %s", id)
	}
	if err := s.add(Profile{ID: id, Name: "Mein Profil!", MC: "1.20.1", RAM: 2048}); err != nil {
		t.Fatal(err)
	}
	s.update(id, func(p *Profile) { p.RAM = 8192 })
	s2 := &store{}
	if err := s2.load(); err != nil {
		t.Fatal(err)
	}
	p, ok := s2.get(id)
	if !ok || p.RAM != 8192 {
		t.Fatalf("nicht persistent: %+v ok=%v", p, ok)
	}
	if _, ok := s2.remove(id); !ok || len(s2.all()) != 1 {
		t.Error("remove fehlgeschlagen")
	}
}
