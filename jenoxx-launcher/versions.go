package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Minecraft-/Fabric-Versionsbeschreibungen (JSON des offiziellen Launchers).

type osRule struct {
	Name    string `json:"name"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

type rule struct {
	Action   string          `json:"action"`
	OS       *osRule         `json:"os"`
	Features map[string]bool `json:"features"`
}

func currentOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "osx"
	}
	return "linux"
}

// matches: Regeln mit "arch" (x86/arm64) treffen auf 64-Bit-Intel/AMD nicht zu;
// Feature-Regeln (Demo, eigene Aufloesung, QuickPlay) sind bei uns immer aus.
func (r rule) matches() bool {
	if r.OS != nil {
		if r.OS.Name != "" && r.OS.Name != currentOS() {
			return false
		}
		if r.OS.Arch != "" {
			return false
		}
	}
	for _, want := range r.Features {
		if want {
			return false
		}
	}
	return true
}

func rulesAllow(rs []rule) bool {
	if len(rs) == 0 {
		return true
	}
	allowed := false
	for _, r := range rs {
		if r.matches() {
			allowed = r.Action == "allow"
		}
	}
	return allowed
}

type arg struct {
	Values []string
	Rules  []rule
}

func (a *arg) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		a.Values = []string{s}
		return nil
	}
	var o struct {
		Rules []rule          `json:"rules"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	a.Rules = o.Rules
	var one string
	if json.Unmarshal(o.Value, &one) == nil {
		a.Values = []string{one}
		return nil
	}
	return json.Unmarshal(o.Value, &a.Values)
}

type artifact struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	SHA1 string `json:"sha1"`
	Size int64  `json:"size"`
}

type library struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Rules   []rule            `json:"rules"`
	Natives map[string]string `json:"natives"`
	Extract *struct {
		Exclude []string `json:"exclude"`
	} `json:"extract"`
	Downloads *struct {
		Artifact    *artifact            `json:"artifact"`
		Classifiers map[string]*artifact `json:"classifiers"`
	} `json:"downloads"`
}

type version struct {
	ID                 string `json:"id"`
	InheritsFrom       string `json:"inheritsFrom"`
	MainClass          string `json:"mainClass"`
	Type               string `json:"type"`
	Assets             string `json:"assets"`
	MinecraftArguments string `json:"minecraftArguments"`
	Arguments          struct {
		Game []arg `json:"game"`
		JVM  []arg `json:"jvm"`
	} `json:"arguments"`
	Libraries  []library `json:"libraries"`
	AssetIndex struct {
		ID   string `json:"id"`
		URL  string `json:"url"`
		SHA1 string `json:"sha1"`
		Size int64  `json:"size"`
	} `json:"assetIndex"`
	Downloads struct {
		Client artifact `json:"client"`
	} `json:"downloads"`
	JavaVersion struct {
		Component    string `json:"component"`
		MajorVersion int    `json:"majorVersion"`
	} `json:"javaVersion"`
}

// libFile ist eine konkret herunterzuladende Bibliotheksdatei.
type libFile struct {
	Path    string // relativ zu libraries/
	URL     string
	SHA1    string
	Size    int64
	Natives bool // alte Natives: entpacken statt in den Classpath
	Exclude []string
}

const mojangLibs = "https://libraries.minecraft.net/"

// mavenPath erzeugt group/artifact/version/artifact-version[-classifier].jar aus "g:a:v[:c][@ext]".
func mavenPath(name, classifier string) (string, error) {
	ext := "jar"
	if i := strings.Index(name, "@"); i >= 0 {
		ext, name = name[i+1:], name[:i]
	}
	p := strings.Split(name, ":")
	if len(p) < 3 {
		return "", fmt.Errorf("ungueltiger Bibliotheksname: %s", name)
	}
	file := p[1] + "-" + p[2]
	cl := classifier
	if cl == "" && len(p) > 3 {
		cl = p[3]
	}
	if cl != "" {
		file += "-" + cl
	}
	return strings.ReplaceAll(p[0], ".", "/") + "/" + p[1] + "/" + p[2] + "/" + file + "." + ext, nil
}

// libKey identifiziert eine Bibliothek ohne Version (zum Entfernen von Duplikaten).
func libKey(name string) string {
	p := strings.Split(strings.SplitN(name, "@", 2)[0], ":")
	if len(p) < 2 {
		return name
	}
	k := p[0] + ":" + p[1]
	if len(p) > 3 {
		k += ":" + p[3]
	}
	return k
}

func (l library) files() ([]libFile, error) {
	if !rulesAllow(l.Rules) {
		return nil, nil
	}
	if len(l.Natives) > 0 { // alte Natives (Klassifizierer)
		cls := strings.ReplaceAll(l.Natives[currentOS()], "${arch}", "64")
		if cls == "" {
			return nil, nil
		}
		lf := libFile{Natives: true}
		if l.Extract != nil {
			lf.Exclude = l.Extract.Exclude
		}
		if l.Downloads != nil && l.Downloads.Classifiers[cls] != nil {
			a := l.Downloads.Classifiers[cls]
			lf.Path, lf.URL, lf.SHA1, lf.Size = a.Path, a.URL, a.SHA1, a.Size
			return []libFile{lf}, nil
		}
		p, err := mavenPath(l.Name, cls)
		if err != nil {
			return nil, err
		}
		lf.Path, lf.URL = p, mojangLibs+p
		return []libFile{lf}, nil
	}
	if l.Downloads != nil && l.Downloads.Artifact != nil && l.Downloads.Artifact.URL != "" {
		a := l.Downloads.Artifact
		p := a.Path
		if p == "" {
			var err error
			if p, err = mavenPath(l.Name, ""); err != nil {
				return nil, err
			}
		}
		return []libFile{{Path: p, URL: a.URL, SHA1: a.SHA1, Size: a.Size}}, nil
	}
	p, err := mavenPath(l.Name, "")
	if err != nil {
		return nil, err
	}
	base := l.URL
	if base == "" {
		base = mojangLibs
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return []libFile{{Path: p, URL: base + p}}, nil
}

// mergeVersions verbindet das Fabric-Profil (child) mit der Vanilla-Version (parent).
func mergeVersions(parent, child *version) *version {
	m := *parent
	m.ID = child.ID
	if child.MainClass != "" {
		m.MainClass = child.MainClass
	}
	if child.MinecraftArguments != "" {
		m.MinecraftArguments = child.MinecraftArguments
	}
	m.Arguments.Game = append(append([]arg(nil), parent.Arguments.Game...), child.Arguments.Game...)
	m.Arguments.JVM = append(append([]arg(nil), parent.Arguments.JVM...), child.Arguments.JVM...)
	seen := map[string]bool{}
	m.Libraries = nil
	for _, l := range append(append([]library(nil), child.Libraries...), parent.Libraries...) {
		k := libKey(l.Name)
		if seen[k] {
			continue
		}
		seen[k] = true
		m.Libraries = append(m.Libraries, l)
	}
	return &m
}

func readVersion(mcDir, id string) (*version, error) {
	data, err := os.ReadFile(filepath.Join(mcDir, "versions", id, id+".json"))
	if err != nil {
		return nil, err
	}
	var v version
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("%s.json: %w", id, err)
	}
	return &v, nil
}

// loadMerged liest die (ggf. mit Vanilla verbundene) Version. vanillaID ist die ID der Client-Jar.
func loadMerged(mcDir, id string) (v *version, vanillaID string, err error) {
	c, err := readVersion(mcDir, id)
	if err != nil {
		return nil, "", err
	}
	if c.InheritsFrom == "" {
		return c, c.ID, nil
	}
	p, err := readVersion(mcDir, c.InheritsFrom)
	if err != nil {
		return nil, "", err
	}
	return mergeVersions(p, c), c.InheritsFrom, nil
}

func substitute(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}

// buildArgs erzeugt JVM- und Spielargumente (ohne Java-Pfad, ohne Hauptklasse).
func buildArgs(v *version, vars map[string]string) (jvm, game []string) {
	for _, a := range v.Arguments.JVM {
		if rulesAllow(a.Rules) {
			for _, s := range a.Values {
				jvm = append(jvm, substitute(s, vars))
			}
		}
	}
	if len(v.Arguments.JVM) == 0 { // alte Versionen (< 1.13)
		for _, s := range []string{"-Djava.library.path=${natives_directory}", "-cp", "${classpath}"} {
			jvm = append(jvm, substitute(s, vars))
		}
	}
	if v.MinecraftArguments != "" {
		for _, s := range strings.Fields(v.MinecraftArguments) {
			game = append(game, substitute(s, vars))
		}
		return
	}
	for _, a := range v.Arguments.Game {
		if rulesAllow(a.Rules) {
			for _, s := range a.Values {
				game = append(game, substitute(s, vars))
			}
		}
	}
	return
}

func classpathSep() string {
	if runtime.GOOS == "windows" {
		return ";"
	}
	return ":"
}

// classpathOf: alle Nicht-Native-Bibliotheken + Client-Jar.
func classpathOf(v *version, mcDir, vanillaID string) (string, error) {
	var parts []string
	for _, l := range v.Libraries {
		fs, err := l.files()
		if err != nil {
			return "", err
		}
		for _, f := range fs {
			if !f.Natives {
				parts = append(parts, filepath.Join(mcDir, "libraries", filepath.FromSlash(f.Path)))
			}
		}
	}
	parts = append(parts, filepath.Join(mcDir, "versions", vanillaID, vanillaID+".jar"))
	return strings.Join(parts, classpathSep()), nil
}

var errNoVersion = errors.New("Minecraft-Version nicht in der Mojang-Liste gefunden")
