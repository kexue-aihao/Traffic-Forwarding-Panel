package openapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestContractRouteCoverageAndReferences(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatal("unsupported contract version")
	}
	paths := doc["paths"].(map[string]any)
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	routes := map[string]bool{}
	pattern := regexp.MustCompile(`(?:HandleFunc|Handle)\("(GET|POST|PUT|PATCH|DELETE) /api/v1([^" ]+)"`)
	// app.go 是组合根，它自己也挂管理接口（支付通道配置）：文档里列了，就必须真的挂上。
	for _, path := range []string{"../platform/server.go", "../commerce/http.go", "../alerts/http.go", "../app/app.go", "handler.go"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pattern.FindAllStringSubmatch(string(raw), -1) {
			routes[strings.ToLower(m[1])+" "+m[2]] = true
		}
	}
	ids := map[string]bool{}
	for path, value := range paths {
		for method, value := range value.(map[string]any) {
			if !routes[method+" "+path] {
				t.Errorf("documented route missing: %s %s", method, path)
			}
			delete(routes, method+" "+path)
			op := value.(map[string]any)
			id := op["operationId"].(string)
			if ids[id] {
				t.Errorf("duplicate operation id %s", id)
			}
			ids[id] = true
			if len(op["responses"].(map[string]any)) == 0 {
				t.Error("missing responses")
			}
		}
	}
	for route := range routes {
		t.Error("undocumented route", route)
	}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if raw, ok := v["$ref"].(string); ok {
				prefix := "#/components/schemas/"
				if !strings.HasPrefix(raw, prefix) || schemas[strings.TrimPrefix(raw, prefix)] == nil {
					t.Error("unresolved schema", raw)
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	mux := http.NewServeMux()
	Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/openapi.json", nil))
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Type"), "application/json") || !json.Valid(rr.Body.Bytes()) {
		t.Fatal("document not served correctly")
	}
}
