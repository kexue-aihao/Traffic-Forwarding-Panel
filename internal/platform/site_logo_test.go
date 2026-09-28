package platform

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func logoFixture(t *testing.T, format string, width, height int) string {
	t.Helper()
	var out bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&out, picture, nil)
	} else {
		err = png.Encode(&out, picture)
	}
	if err != nil {
		t.Fatal(err)
	}
	return "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(out.Bytes())
}

func TestSiteLogoValidationAndPersistence(t *testing.T) {
	f := setup(t)
	site := read[contract.SiteSettings](t, f.req("GET", "/site", nil, ""), 200)
	if site.Logo != "" {
		t.Fatal("default site should use the bundled logo")
	}
	pngLogo := logoFixture(t, "png", 32, 32)
	for _, logo := range []string{pngLogo, logoFixture(t, "jpeg", 512, 512)} {
		site.Logo = logo
		stale := site
		site = read[contract.SiteSettings](t, f.req("PUT", "/site", site, ""), 200)
		if rr := f.req("PUT", "/site", stale, ""); rr.Code != 409 {
			t.Fatalf("stale update: %d", rr.Code)
		}
		cookie := f.cookie
		f.cookie = nil
		public := read[contract.SiteSettings](t, f.req("GET", "/site", nil, ""), 200)
		if public.Logo != logo {
			t.Fatal("public settings lost the saved logo")
		}
		if rr := f.req("PUT", "/site", public, ""); rr.Code != 401 {
			t.Fatalf("unauthenticated update: %d", rr.Code)
		}
		f.cookie = cookie
	}
	invalid := map[string]string{
		"remote":    "https://example.com/logo.png",
		"svg":       "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg onload="alert(1)"/>`)),
		"base64":    "data:image/png;base64,!!!",
		"content":   "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not a picture")),
		"mime":      strings.Replace(pngLogo, "image/png", "image/jpeg", 1),
		"truncated": pngLogo[:len(pngLogo)-12],
		"size":      "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, 32*1024+1)),
		"width":     logoFixture(t, "png", 513, 1),
		"height":    logoFixture(t, "png", 1, 513),
	}
	for name, logo := range invalid {
		t.Run(name, func(t *testing.T) {
			candidate := site
			candidate.Logo = logo
			if rr := f.req("PUT", "/site", candidate, ""); rr.Code != 400 {
				t.Fatalf("invalid logo accepted: %d", rr.Code)
			}
			current := read[contract.SiteSettings](t, f.req("GET", "/site", nil, ""), 200)
			if current.Logo != site.Logo || current.Version != site.Version {
				t.Fatal("invalid update changed saved settings")
			}
		})
	}
	site.Logo = ""
	site = read[contract.SiteSettings](t, f.req("PUT", "/site", site, ""), 200)
	if site.Logo != "" {
		t.Fatal("logo was not cleared")
	}
}

func TestSiteSettingsEncodedSizeLimit(t *testing.T) {
	f := setup(t)
	site := read[contract.SiteSettings](t, f.req("GET", "/site", nil, ""), 200)
	_, encoded, _ := strings.Cut(logoFixture(t, "png", 32, 32), ",")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	// Decoders permit trailing bytes; combined with escaped text this exceeds TEXT.
	data = append(data, make([]byte, 20000)...)
	site.Logo = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	site.Announcement = strings.Repeat("\x00", 8000)
	response := f.req("PUT", "/site", site, "")
	if response.Code != 400 || !strings.Contains(response.Body.String(), "exceed 64 KiB") {
		t.Fatalf("oversized settings: %d %s", response.Code, response.Body.String())
	}
}
