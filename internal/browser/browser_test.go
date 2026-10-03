package browser

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		`C:\Program Files\Mozilla Firefox\firefox.exe`:                          KindFirefox,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`:                 KindChromium,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`:          KindChromium,
		`C:\Users\x\AppData\Local\BraveSoftware\Brave-Browser\Application\brave.exe`: KindChromium,
		`C:\Users\x\Desktop\Tor Browser\Browser\firefox.exe`:                    KindTor,
		`C:\Windows\notepad.exe`:                                                KindUnknown,
	}
	for path, want := range cases {
		if got := Classify(path); got != want {
			t.Errorf("Classify(%q) = %s, want %s", path, got, want)
		}
	}
}

func TestFirefoxUserJS(t *testing.T) {
	js := FirefoxUserJS("127.0.0.1", 9077)
	for _, want := range []string{
		`"network.proxy.type", 1`,
		`"network.proxy.socks", "127.0.0.1"`,
		`"network.proxy.socks_port", 9077`,
		`"network.proxy.socks_remote_dns", true`,
		`"media.peerconnection.enabled", false`,
		`"network.proxy.failover_direct", false`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("user.js missing %s", want)
		}
	}
}

func TestChromiumArgs(t *testing.T) {
	args := strings.Join(ChromiumArgs("127.0.0.1", 9050, `C:\p`, "https://check.torproject.org"), " ")
	for _, want := range []string{
		"--proxy-server=socks5://127.0.0.1:9050",
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		`--user-data-dir=C:\p`,
		"disable_non_proxied_udp",
		"https://check.torproject.org",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
}

func TestLaunchRejectsMissingOrUnsupported(t *testing.T) {
	if _, err := Launch("", "127.0.0.1", 9050, t.TempDir(), ""); err == nil {
		t.Error("expected error for empty path")
	}
	if _, err := Launch(`C:\nope\firefox.exe`, "127.0.0.1", 9050, t.TempDir(), ""); err == nil {
		t.Error("expected error for missing browser")
	}
}
