package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Direkter Start von Minecraft (ohne den offiziellen Launcher).

type runningGame struct {
	cmd     *exec.Cmd
	started time.Time
	stopped bool // vom Nutzer beendet (kein Absturz)
}

var games = struct {
	sync.Mutex
	m map[string]*runningGame
}{m: map[string]*runningGame{}}

func isRunning(id string) bool {
	games.Lock()
	defer games.Unlock()
	_, ok := games.m[id]
	return ok
}

func runningIDs() []string {
	games.Lock()
	defer games.Unlock()
	var out []string
	for id := range games.m {
		out = append(out, id)
	}
	return out
}

func stopGame(id string) error {
	games.Lock()
	g := games.m[id]
	if g != nil {
		g.stopped = true
	}
	games.Unlock()
	if g == nil || g.cmd.Process == nil {
		return errors.New("Spiel laeuft nicht")
	}
	return g.cmd.Process.Kill()
}

// prefixWriter schreibt Prozessausgabe zeilenweise ins Protokoll (ohne Token).
type prefixWriter struct {
	prefix string
	secret string
	buf    []byte
	sink   func(string) // optional: bekommt jede (geschwaerzte) Zeile ohne Praefix
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if w.secret != "" {
			line = strings.ReplaceAll(line, w.secret, "***")
		}
		if strings.TrimSpace(line) != "" {
			if w.sink != nil {
				w.sink(line)
			}
			logln(w.prefix + line)
		}
	}
	return len(p), nil
}

// launchVars baut die Platzhalter fuer die Startargumente.
func launchVars(mcDir, gameDir, versionID, vanillaID string, v *version, s session, classpath string) map[string]string {
	return map[string]string{
		"auth_player_name":    s.Name,
		"version_name":        versionID,
		"game_directory":      gameDir,
		"assets_root":         filepath.Join(mcDir, "assets"),
		"game_assets":         filepath.Join(mcDir, "assets"),
		"assets_index_name":   v.Assets,
		"auth_uuid":           s.UUID,
		"auth_access_token":   s.Token,
		"auth_session":        "token:" + s.Token + ":" + s.UUID,
		"clientid":            "",
		"auth_xuid":           "",
		"user_type":           "msa",
		"user_properties":     "{}",
		"version_type":        orDefault(v.Type, "release"),
		"natives_directory":   filepath.Join(mcDir, "versions", versionID, "natives"),
		"launcher_name":       "JenoXx",
		"launcher_version":    "1.0",
		"classpath":           classpath,
		"library_directory":   filepath.Join(mcDir, "libraries"),
		"classpath_separator": classpathSep(),
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// commandFor setzt Java-Pfad, Profil-JVM-Argumente, Version-JVM-Argumente, Hauptklasse und Spielargumente zusammen.
func commandFor(javaExe string, p Profile, v *version, vars map[string]string) []string {
	jvm, game := buildArgs(v, vars)
	args := append([]string(nil), strings.Fields(p.javaArgs())...)
	args = append(args, jvm...)
	args = append(args, v.MainClass)
	args = append(args, game...)
	args = append(args, extraGameArgs(p)...)
	return append([]string{javaExe}, args...)
}

func launchGame(p Profile) error {
	if isRunning(p.ID) {
		return errors.New("Dieses Profil laeuft bereits")
	}
	s, err := auth.session()
	if err != nil {
		return err
	}
	mcDir, err := minecraftDir()
	if err != nil {
		return err
	}
	if p.VersionID == "" {
		return errors.New("Profil ist noch nicht installiert")
	}
	v, vanillaID, err := loadMerged(mcDir, p.VersionID)
	if err != nil {
		return fmt.Errorf("Versionsdaten fehlen - bitte Profil installieren/reparieren: %w", err)
	}
	cp, err := classpathOf(v, mcDir, vanillaID)
	if err != nil {
		return err
	}
	gameDir := profileGameDir(mcDir, p)
	if err := os.MkdirAll(gameDir, 0o755); err != nil {
		return err
	}
	if err := validateJVMArgs(p.JVMArgs); err != nil {
		return err
	}
	if err := validateJavaPath(p.JavaPath); err != nil {
		return err
	}
	javaExe := strings.TrimSpace(p.JavaPath)
	if javaExe == "" {
		if javaExe, err = ensureRuntime(mcDir, v.JavaVersion.Component); err != nil {
			return err
		}
	}
	vars := launchVars(mcDir, gameDir, p.VersionID, vanillaID, v, s, cp)
	argv := commandFor(javaExe, p, v, vars)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = gameDir
	ring := newRing(300)
	w := &prefixWriter{prefix: "[MC] ", secret: s.Token, sink: ring.add}
	cmd.Stdout, cmd.Stderr = w, w
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("Java konnte nicht gestartet werden: %w", err)
	}
	g := &runningGame{cmd: cmd, started: time.Now()}
	games.Lock()
	games.m[p.ID] = g
	games.Unlock()
	beginGameLog(p.ID, ring)
	profiles.update(p.ID, func(q *Profile) { q.LastPlayed = time.Now().Format(time.RFC3339) })
	logln("Minecraft " + p.MC + " gestartet als " + s.Name)
	ui.emit(event{Type: "game", State: "started", Text: p.ID})

	go func() {
		err := cmd.Wait()
		elapsed := time.Since(g.started)
		games.Lock()
		stopped := g.stopped
		delete(games.m, p.ID)
		games.Unlock()
		profiles.update(p.ID, func(q *Profile) { q.PlaySeconds += int64(elapsed.Seconds()) })
		code := 0
		if cmd.ProcessState != nil {
			code = normExit(cmd.ProcessState.ExitCode())
		}
		crashed := err != nil && code != 0 && !stopped
		if crashed {
			report := findCrashReport(gameDir, g.started)
			d := analyzeCrash(ring.snapshot(), readHead(report, 96<<10), code)
			if report != "" {
				d.Report = filepath.Base(report)
			}
			d.At = time.Now().Format(time.RFC3339)
			setCrash(p.ID, d)
			logln(fmt.Sprintf("Minecraft ist abgestuerzt (Code %d): %s", code, d.Title))
		} else {
			logln("Minecraft wurde beendet.")
		}
		ui.emit(event{Type: "game", State: "stopped", Text: p.ID, OK: !crashed})
	}()
	return nil
}
