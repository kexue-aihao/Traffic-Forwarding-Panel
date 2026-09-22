package agentdist

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 这两个路由是免登录的，因此每个用例都必须走一遍「没有任何凭据」的路径。
func get(t *testing.T, dir, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux, dir)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
	return w
}

func stage(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerScriptIsAlwaysAvailable(t *testing.T) {
	// 脚本内嵌，因此即使没有任何产物目录也必须能取到 —— 否则交给运营方的
	// 那条命令会指向一个不存在的东西。
	w := get(t, filepath.Join(t.TempDir(), "absent"), "/download/agent-install.sh")
	if w.Code != http.StatusOK {
		t.Fatalf("installer status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Fatalf("installer content type = %q", ct)
	}
	body := w.Body.String()
	for _, want := range []string{"#!/usr/bin/env bash", "TFP_ENROLLMENT_TOKEN", "/download/agent/linux/", "systemctl"} {
		if !strings.Contains(body, want) {
			t.Errorf("installer script is missing %q", want)
		}
	}
	// 脚本绝不能把令牌拼进命令行或 unit 文件：同机其他用户能读到进程参数。
	if strings.Contains(body, "-t $TOKEN") || strings.Contains(body, "--token") {
		t.Error("installer passes the token on the command line")
	}
}

func TestAgentBinaryByPlatform(t *testing.T) {
	dir := t.TempDir()
	stage(t, dir, "agent-linux-arm64", "ARM64-AGENT")
	w := get(t, dir, "/download/agent/linux/arm64")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.String() != "ARM64-AGENT" {
		t.Fatalf("body = %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content disposition = %q", cd)
	}
}

// 镜像里只放了一个不带平台后缀的 /agent，它是给面板自身平台用的。
func TestNativeFallbackServesBareAgent(t *testing.T) {
	dir := t.TempDir()
	stage(t, dir, "agent", "NATIVE-AGENT")
	w := get(t, dir, "/download/agent")
	if w.Code != http.StatusOK || w.Body.String() != "NATIVE-AGENT" {
		t.Fatalf("native fallback = %d %q", w.Code, w.Body.String())
	}
	// 带平台后缀的文件优先于裸文件
	stage(t, dir, "agent-"+runtime.GOOS+"-"+runtime.GOARCH, "EXPLICIT")
	if w := get(t, dir, "/download/agent"); w.Body.String() != "EXPLICIT" {
		t.Fatalf("explicit platform file must win, got %q", w.Body.String())
	}
}

// 没有对应产物时要给出能读懂的原因，而不是一个空 body 的 404：
// 这条响应会原样出现在运营方的终端里。
func TestMissingPlatformExplainsItself(t *testing.T) {
	w := get(t, t.TempDir(), "/download/agent/plan9/sparc")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "plan9/sparc") {
		t.Fatalf("body should name the platform, got %q", w.Body.String())
	}
}

func TestPlatformSegmentsAreLockedDown(t *testing.T) {
	dir := t.TempDir()
	stage(t, dir, "agent", "NATIVE-AGENT")
	// 在产物目录外放一个「不该被取到」的文件，验证穿越不会成功。
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, target := range []string{
		"/download/agent/UPPER/amd64",
		"/download/agent/linux/AMD64",
		"/download/agent/" + strings.Repeat("a", 64) + "/amd64",
		"/download/agent/linux/amd64%2f..",
		"/download/agent/..%2f..%2fsecret.txt/x",
	} {
		w := get(t, dir, target)
		if w.Code == http.StatusOK {
			t.Errorf("%s must not be served, got 200 with %q", target, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("%s escaped the agent directory", target)
		}
	}
}

func TestLookupIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "agent-linux-amd64"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := Lookup(dir, "linux", "amd64"); ok {
		t.Fatal("a directory must not be published as an Agent binary")
	}
}
