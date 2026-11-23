package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// Spielerkopf fuer die Kontoanzeige. Der Skin kommt direkt von Mojang
// (sessionserver.mojang.com -> textures.minecraft.net); es wird kein Drittdienst gefragt.

var (
	sessionBase = "https://sessionserver.mojang.com"
	skinHostOK  = func(h string) bool { return h == "textures.minecraft.net" }
	reUUIDHex   = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	headCache   = struct {
		sync.Mutex
		m map[string]cachedHead
	}{m: map[string]cachedHead{}}
)

type cachedHead struct {
	png []byte
	at  time.Time
}

// skinURL liest die Skin-Adresse aus dem Profil (Eigenschaft "textures", Base64-JSON).
func skinURL(uuid string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, sessionBase+"/session/minecraft/profile/"+uuid, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := authClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Profil nicht abrufbar (HTTP %d)", resp.StatusCode)
	}
	var prof struct {
		Properties []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"properties"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&prof); err != nil {
		return "", err
	}
	for _, p := range prof.Properties {
		if p.Name != "textures" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(p.Value)
		if err != nil {
			return "", err
		}
		var tex struct {
			Textures struct {
				Skin struct {
					URL string `json:"url"`
				} `json:"SKIN"`
			} `json:"textures"`
		}
		if err := json.Unmarshal(raw, &tex); err != nil {
			return "", err
		}
		if tex.Textures.Skin.URL != "" {
			return tex.Textures.Skin.URL, nil
		}
	}
	return "", errors.New("kein eigener Skin vorhanden")
}

// headFromSkin schneidet Gesicht (8,8) und Hut-Ebene (40,8) aus und vergroessert auf 64x64.
func headFromSkin(data []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() != 64 || (b.Dy() != 64 && b.Dy() != 32) {
		return nil, errors.New("unerwartetes Skin-Format")
	}
	face := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(face, face.Bounds(), img, image.Pt(b.Min.X+8, b.Min.Y+8), draw.Src)
	hat := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(hat, hat.Bounds(), img, image.Pt(b.Min.X+40, b.Min.Y+8), draw.Src)
	draw.Draw(face, face.Bounds(), hat, image.Point{}, draw.Over)
	const scale = 8
	out := image.NewNRGBA(image.Rect(0, 0, 8*scale, 8*scale))
	for y := 0; y < 8*scale; y++ {
		for x := 0; x < 8*scale; x++ {
			c := face.NRGBAAt(x/scale, y/scale)
			c.A = 255 // das Gesicht selbst ist nie durchsichtig
			out.SetNRGBA(x, y, color.NRGBA(c))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fetchHead(uuid string) ([]byte, error) {
	if !reUUIDHex.MatchString(uuid) {
		return nil, errors.New("ungueltige UUID")
	}
	headCache.Lock()
	if c, ok := headCache.m[uuid]; ok && time.Since(c.at) < 6*time.Hour {
		headCache.Unlock()
		return c.png, nil
	}
	headCache.Unlock()

	su, err := skinURL(uuid)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(su)
	if err != nil || !skinHostOK(u.Hostname()) || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("unerwartete Skin-Adresse")
	}
	req, err := http.NewRequest(http.MethodGet, su, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := authClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Skin nicht abrufbar (HTTP %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	head, err := headFromSkin(data)
	if err != nil {
		return nil, err
	}
	headCache.Lock()
	headCache.m[uuid] = cachedHead{png: head, at: time.Now()}
	headCache.Unlock()
	return head, nil
}

func (s *server) avatar(w http.ResponseWriter, r *http.Request) {
	info := auth.info()
	if !info.LoggedIn || info.UUID == "" {
		http.NotFound(w, r)
		return
	}
	head, err := fetchHead(info.UUID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(head)
}
