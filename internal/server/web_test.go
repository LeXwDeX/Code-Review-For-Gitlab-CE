package server

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

func TestWebConsoleSPAFallbackAndAssets(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	web, err := newWebFS()
	if err != nil {
		t.Fatalf("newWebFS: %v", err)
	}

	index := env.request(http.MethodGet, "/", "", nil, nil)
	if index.status != http.StatusOK {
		t.Fatalf("index status = %d", index.status)
	}
	if !strings.Contains(string(index.body), `<div id="root">`) {
		t.Fatalf("index body = %s", index.body)
	}
	if contentType := index.header.Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Errorf("index Content-Type = %q", contentType)
	}
	if index.header.Get("Cache-Control") != webIndexControl {
		t.Errorf("index Cache-Control = %q", index.header.Get("Cache-Control"))
	}

	fallback := env.request(http.MethodGet, "/reviews/0192f0c1", "", nil, nil)
	if fallback.status != http.StatusOK {
		t.Fatalf("SPA fallback status = %d", fallback.status)
	}
	if string(fallback.body) != string(index.body) {
		t.Error("SPA fallback did not return index.html")
	}

	entries, err := fs.ReadDir(web, "assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("read embedded assets: %v", err)
	}
	var asset string
	for _, entry := range entries {
		if !entry.IsDir() && (strings.HasSuffix(entry.Name(), ".js") || strings.HasSuffix(entry.Name(), ".css")) {
			asset = entry.Name()
			break
		}
	}
	if asset == "" {
		t.Fatal("embedded assets contain no JS or CSS")
	}
	assetResp := env.request(http.MethodGet, "/assets/"+asset, "", nil, nil)
	if assetResp.status != http.StatusOK {
		t.Fatalf("asset status = %d", assetResp.status)
	}
	if contentType := assetResp.header.Get("Content-Type"); !strings.Contains(contentType, "javascript") &&
		!strings.Contains(contentType, "css") {
		t.Errorf("asset Content-Type = %q", contentType)
	}
	if !strings.Contains(assetResp.header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset Cache-Control = %q", assetResp.header.Get("Cache-Control"))
	}

	missing := env.request(http.MethodGet, "/assets/does-not-exist.js", "", nil, nil)
	if missing.status != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, body=%s", missing.status, missing.body)
	}
	icon := env.request(http.MethodGet, "/favicon.ico", "", nil, nil)
	if icon.status != http.StatusNotFound {
		t.Fatalf("missing favicon status = %d", icon.status)
	}

	unknownAPI := env.request(http.MethodGet, "/api/v1/does-not-exist", "", nil, nil)
	if unknownAPI.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unknown API status = %d", unknownAPI.status)
	}
	unknownAPI = env.request(http.MethodGet, "/api/v1/does-not-exist", env.adminToken, nil, nil)
	if unknownAPI.status != http.StatusNotFound {
		t.Fatalf("authenticated unknown API status = %d", unknownAPI.status)
	}
	if strings.Contains(string(unknownAPI.body), `<div id="root">`) {
		t.Error("unknown API returned the SPA shell")
	}
	reserved := env.request(http.MethodGet, "/auth/does-not-exist", "", nil, nil)
	if reserved.status != http.StatusNotFound {
		t.Fatalf("reserved path status = %d", reserved.status)
	}
}
