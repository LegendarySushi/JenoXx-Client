package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Modrinth-Abrufe ueber den Launcher selbst. Dient als Ausweg, wenn der Browser
// api.modrinth.com nicht erreicht (VPN, Firewall, Antivirus, DNS) - der Go-Client
// hat eigene Wiederholungen und einen HTTP/1.1-Modus.

var modrinthBase = "https://api.modrinth.com/v2"

// validModrinthCall prueft Methode und Pfad (nur Lesen/Suchen/Update-Pruefung).
func validModrinthCall(method, path string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "//") || strings.Contains(path, "..") ||
		strings.ContainsAny(path, "\\@ \r\n") || len(path) > 2000 {
		return "", errors.New("ungueltiger Modrinth-Pfad")
	}
	switch method {
	case "GET":
	case "POST":
		if path != "/version_files/update" && path != "/version_files" {
			return "", errors.New("diese Modrinth-Anfrage ist nicht erlaubt")
		}
	default:
		return "", errors.New("ungueltige Methode")
	}
	u, err := url.Parse(modrinthBase + path)
	base, _ := url.Parse(modrinthBase)
	if err != nil || base == nil || u.Host != base.Host {
		return "", errors.New("ungueltiger Modrinth-Pfad")
	}
	return u.String(), nil
}

// modrinthRequest sendet die Anfrage (bis zu 4 Versuche, abwechselnd HTTP/2 und HTTP/1.1).
// Rueckgabe: Statuscode und Antworttext von Modrinth.
func modrinthRequest(method, path string, body []byte) (int, []byte, error) {
	target, err := validModrinthCall(method, path)
	if err != nil {
		return 0, nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		c := httpClient
		if attempt%2 == 1 {
			c = httpClient11
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, target, rd)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err == nil {
			data, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			resp.Body.Close()
			if rerr == nil {
				return resp.StatusCode, data, nil
			}
			err = rerr
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 700 * time.Millisecond)
	}
	return 0, nil, fmt.Errorf("Modrinth nicht erreichbar: %w", lastErr)
}

func (s *server) modrinthProxy(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   string `json:"body"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	var body []byte
	if b.Body != "" {
		body = []byte(b.Body)
	}
	code, data, err := modrinthRequest(b.Method, b.Path, body)
	if err != nil {
		fail(w, 502, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}
