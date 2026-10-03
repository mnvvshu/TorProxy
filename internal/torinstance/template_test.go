package torinstance

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// TestTorrcTemplatesRender checks that both the built-in template and the
// shipped configs/torrc.template parse, render every required directive, and
// contain no non-ASCII bytes (Tor rejects some encodings on Windows).
func TestTorrcTemplatesRender(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "configs", "torrc.template"))
	if err != nil {
		t.Fatal(err)
	}
	data := TorrcData{
		InstanceID:  7,
		SocksPort:   9056,
		SocksPorts:  []int{9056, 9057, 9058},
		ControlPort: 30056,
		BindAddress: "127.0.0.1",
		DataDir:     `"C:/Program Files/TPM/data/instance_7"`,
		LogFile:     `"C:/Program Files/TPM/data/instance_7/tor.log"`,
	}
	for name, src := range map[string]string{"built-in": DefaultTorrcTemplate, "shipped": string(shipped)} {
		for i, b := range []byte(src) {
			if b > 0x7e || (b < 0x20 && b != '\n' && b != '\r' && b != '\t') {
				t.Errorf("%s template has non-ASCII byte 0x%02x at offset %d", name, b, i)
				break
			}
		}
		tmpl, err := template.New(name).Option("missingkey=error").Parse(src)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			t.Fatalf("%s: execute: %v", name, err)
		}
		out := buf.String()
		for _, want := range []string{
			"\nSocksPort 127.0.0.1:9056\n",
			"\nSocksPort 127.0.0.1:9058\n",
			"\nControlPort 127.0.0.1:30056\n",
			"\nCookieAuthentication 1\n",
			"\nDataDirectory " + data.DataDir + "\n",
			"\nLog notice file " + data.LogFile + "\n",
			"\nClientOnly 1\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s template output missing %q", name, strings.TrimSpace(want))
			}
		}
	}
}
