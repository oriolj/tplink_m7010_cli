package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testAddr(ts *httptest.Server) string {
	return strings.TrimPrefix(ts.URL, "http://")
}

func TestProbeM7010(t *testing.T) {
	// A real M7010 answers the step-1 hello with base64-wrapped JSON
	// carrying the nonce.
	m7010 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != authPath {
			http.NotFound(w, r)
			return
		}
		body := `{"nonce":"abc","rsaPubKey":"010001","rsaMod":"F09","seqNum":1,"result":0}`
		w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(body))))
	}))
	defer m7010.Close()
	if !probeM7010(testAddr(m7010), time.Second) {
		t.Error("real M7010 hello not recognised")
	}

	// A home router at 192.168.0.1 answers HTTP but not the protocol —
	// the exact false positive the probe exists to reject.
	homeRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>router admin</html>"))
	}))
	defer homeRouter.Close()
	if probeM7010(testAddr(homeRouter), time.Second) {
		t.Error("home router misidentified as M7010")
	}

	if probeM7010("127.0.0.1:1", 100*time.Millisecond) {
		t.Error("unreachable address probed true")
	}
}

func TestProbeMudi(t *testing.T) {
	mudi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != mudiRPCPath {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":1,"jsonrpc":"2.0","result":{"alg":5,"salt":"s","nonce":"n"}}`))
	}))
	defer mudi.Close()
	if !probeMudi(testAddr(mudi), time.Second) {
		t.Error("real Mudi challenge not recognised")
	}

	notMudi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not json-rpc</html>"))
	}))
	defer notMudi.Close()
	if probeMudi(testAddr(notMudi), time.Second) {
		t.Error("non-Mudi HTTP server misidentified as Mudi")
	}
}

func TestRefineDeviceFromReportedModel(t *testing.T) {
	m7010 := findDeviceByID("m7010")
	m7450 := findDeviceByID("m7450")
	mudi := findDeviceByID("mudi")

	if got := refineDevice(m7010, &Status{Model: "M7450"}); got != m7450 {
		t.Errorf("M7450 status refined to %v, want m7450", got)
	}
	if got := refineDevice(m7450, &Status{Model: "m7010"}); got != m7010 {
		t.Errorf("M7010 status refined to %v, want m7010", got)
	}
	if got := refineDevice(mudi, &Status{Model: "M7450"}); got != mudi {
		t.Errorf("unrelated protocol changed from mudi to %v", got)
	}
	if got := refineDevice(m7010, &Status{Model: "unknown"}); got != m7010 {
		t.Errorf("unknown model changed device to %v", got)
	}
}

func TestM7450PasswordFallsBackToSharedTPLinkFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("M7450_PASS", "")
	t.Setenv("TPLINK_PASS", "")
	t.Setenv("M7010_PASS", "")

	path := filepath.Join(dir, "tplink-m7010", "password")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("shared-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolvePassword(findDeviceByID("m7450"), ""); got != "shared-secret" {
		t.Errorf("M7450 fallback password = %q, want shared-secret", got)
	}
}
