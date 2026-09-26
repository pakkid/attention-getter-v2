package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticHandlerHashesAssetLinks(t *testing.T) {
	root := fstest.MapFS{
		"index.html": {Data: []byte(`<link href="/style.css?v=2"><script src="/app.js?v=2"></script><img src="/gone.png?v=1">`)},
		"app.js":     {Data: []byte(`console.log(1)`)},
		"style.css":  {Data: []byte(`body{}`)},
	}
	h := staticHandler(root)
	get := func(path, inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	js := get("/app.js", "")
	jsTag := strings.Trim(js.Header().Get("ETag"), `"`)
	page := get("/", "")
	body := page.Body.String()
	if !strings.Contains(body, `src="/app.js?v=`+jsTag+`"`) || strings.Contains(body, "app.js?v=2") {
		t.Fatalf("app.js link not hashed: %s", body)
	}
	if strings.Contains(body, "style.css?v=2") || !strings.Contains(body, `src="/gone.png?v=1"`) {
		t.Fatalf("expected style.css hashed and unknown files left alone: %s", body)
	}
	if page.Header().Get("Cache-Control") != "no-cache" || page.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("page headers = %v", page.Header())
	}
	if rec := get("/", page.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Fatalf("revalidating the page = %d, want 304", rec.Code)
	}
	if rec := get("/index.html", ""); rec.Body.String() != body {
		t.Fatal("/index.html should serve the same page")
	}
	if rec := get("/app.js", js.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Fatalf("revalidating app.js = %d, want 304", rec.Code)
	}
}
