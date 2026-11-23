package main

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ---------- Netzwerk ----------

// maxHTTPBody begrenzt, wie viel httpGet im Speicher haelt.
const maxHTTPBody = 300 << 20

// secureRedirect erlaubt nur wenige Weiterleitungen, nur per HTTPS, und entfernt geheime
// Header, wenn die Weiterleitung zu einem anderen Server fuehrt (Go entfernt nur Authorization).
func secureRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("zu viele Weiterleitungen")
	}
	if req.URL.Scheme != "https" {
		return errors.New("Weiterleitung auf unverschluesselte Adresse blockiert")
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		req.Header.Del("X-Api-Key")
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
	}
	return nil
}

// ---------- Einstellungen ----------

// validateJavaPath: nur eine vorhandene, lokale java/javaw-Datei (kein Netzwerkpfad, keine andere Programmdatei).
func validateJavaPath(jp string) error {
	jp = strings.TrimSpace(jp)
	if jp == "" {
		return nil
	}
	if strings.HasPrefix(jp, `\\`) || strings.HasPrefix(jp, "//") {
		return errors.New("Netzwerkpfade sind als Java-Pfad nicht erlaubt")
	}
	if !filepath.IsAbs(jp) {
		return errors.New("Java-Pfad muss ein vollstaendiger Pfad sein")
	}
	switch strings.ToLower(filepath.Base(jp)) {
	case "java", "java.exe", "javaw.exe":
	default:
		return errors.New("Java-Pfad muss auf java.exe oder javaw.exe zeigen")
	}
	st, err := os.Stat(jp)
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("Java-Pfad existiert nicht: " + jp)
	}
	return nil
}

var (
	// JVM-Optionen, mit denen sich fremder Code laden oder Befehle ausfuehren liessen.
	badJVMPrefixes = []string{
		"-xbootclasspath", "-xrun", "-xdebug", "-xnoagent",
		"-xx:onerror", "-xx:onoutofmemoryerror", "-xx:flags", "-xx:vmoptionsfile", "-xx:errorfile",
		"-xx:heapdumppath", "-xx:sharedarchivefile", "-xx:compilecommandfile", "-xx:replaydatafile",
		"-xx:logfile", "-xx:+unlockdiagnosticvmoptions", "-xx:+unlockcommercialfeatures",
	}
	badProps = []string{
		"java.library.path", "java.class.path", "java.system.class.loader", "java.security.manager",
		"java.security.policy", "java.security.properties", "java.ext.dirs", "java.endorsed.dirs", "java.home",
		"jdk.module", "jdk.attach", "sun.boot", "jna.boot.library.path", "jna.library.path", "jna.nosys",
		"org.lwjgl.librarypath", "org.lwjgl.system.sharedlibraryextractpath", "user.dir",
		"fabric.addmods", "fabric.gamejarpath", "fabric.classpathgroups", "fabric.remapclasspathfile",
		"log4j.configurationfile", "log4j2.configurationfile", "log4j.configurationfactory", "log4j2.configurationfactory",
		"com.sun.management.jmxremote", "javax.net.ssl.truststore", "javax.net.ssl.keystore",
	}
)

// validateJVMArgs erlaubt nur Speicher-/GC-/-D-Optionen und sperrt Agenten, Klassenpfad-Eingriffe u. Ae.
func validateJVMArgs(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if len(s) > 1000 {
		return errors.New("JVM-Argumente sind zu lang")
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return errors.New("JVM-Argumente enthalten Steuerzeichen")
		}
	}
	for _, tok := range strings.Fields(s) {
		low := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(low, "-x"), strings.HasPrefix(low, "-d"), low == "-ea", low == "-da", low == "-esa", low == "-dsa":
		default:
			return errors.New("JVM-Argument nicht erlaubt: " + tok)
		}
		for _, b := range badJVMPrefixes {
			if strings.HasPrefix(low, b) {
				return errors.New("JVM-Argument nicht erlaubt: " + tok)
			}
		}
		if strings.HasPrefix(low, "-d") {
			key := strings.SplitN(low[2:], "=", 2)[0]
			for _, b := range badProps {
				if strings.HasPrefix(key, b) {
					return errors.New("JVM-Argument nicht erlaubt: " + tok)
				}
			}
		}
	}
	return nil
}

// ---------- Lokaler Webserver ----------

// tokenOK vergleicht den Pfad-Token in konstanter Zeit.
func tokenOK(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

const csp = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
	"img-src 'self' data: https://cdn.modrinth.com https://media.forgecdn.net https://*.forgecdn.net; " +
	"connect-src 'self' https://api.modrinth.com https://cdn.modrinth.com; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
}

// sameOrigin: Browser-Anfragen von fremden Seiten (CSRF, DNS-Rebinding) werden abgewiesen.
func sameOrigin(r *http.Request, addr string) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+addr {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}
