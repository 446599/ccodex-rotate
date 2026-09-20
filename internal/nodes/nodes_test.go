package nodes

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestParseSS(t *testing.T) {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	p, err := Parse("ss://" + userinfo + "@1.2.3.4:8388#NodeA")
	if err != nil {
		t.Fatal(err)
	}
	if p["type"] != "ss" || p["server"] != "1.2.3.4" || p["port"] != "8388" ||
		p["cipher"] != "aes-256-gcm" || p["password"] != "secret" || p["name"] != "NodeA" {
		t.Fatalf("bad ss: %+v", p)
	}
}

func TestParseVmess(t *testing.T) {
	jsonBody := `{"v":"2","ps":"VNode","add":"5.6.7.8","port":"443","id":"uuid-1","aid":"0","scy":"auto","net":"ws","host":"h.example","path":"/p","tls":"tls","sni":"h.example"}`
	uri := "vmess://" + base64.RawStdEncoding.EncodeToString([]byte(jsonBody))
	p, err := Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if p["type"] != "vmess" || p["server"] != "5.6.7.8" || p["uuid"] != "uuid-1" || p["network"] != "ws" || p["tls"] != true {
		t.Fatalf("bad vmess: %+v", p)
	}
	if _, ok := p["ws-opts"]; !ok {
		t.Fatalf("vmess ws-opts missing: %+v", p)
	}
}

func TestParseVless(t *testing.T) {
	p, err := Parse("vless://uuid-2@1.1.1.1:443?security=tls&type=ws&path=%2Fws&host=ex.com&sni=ex.com#VNode")
	if err != nil {
		t.Fatal(err)
	}
	if p["type"] != "vless" || p["uuid"] != "uuid-2" || p["tls"] != true || p["network"] != "ws" || p["name"] != "VNode" {
		t.Fatalf("bad vless: %+v", p)
	}
}

func TestParseTrojan(t *testing.T) {
	p, err := Parse("trojan://pw@2.2.2.2:443?security=tls&sni=t.com#TNode")
	if err != nil {
		t.Fatal(err)
	}
	if p["type"] != "trojan" || p["password"] != "pw" || p["sni"] != "t.com" || p["name"] != "TNode" {
		t.Fatalf("bad trojan: %+v", p)
	}
}

func TestParseHysteria2(t *testing.T) {
	p, err := Parse("hysteria2://pw@3.3.3.3:443?sni=h.com&insecure=1#HNode")
	if err != nil {
		t.Fatal(err)
	}
	if p["type"] != "hysteria2" || p["password"] != "pw" || p["skip-cert-verify"] != true {
		t.Fatalf("bad hysteria2: %+v", p)
	}
}

func TestParseSimpleProxy(t *testing.T) {
	p, err := Parse("socks5://127.0.0.1:7897")
	if err != nil {
		t.Fatal(err)
	}
	if p["type"] != "socks5" || p["server"] != "127.0.0.1" || p["port"] != "7897" {
		t.Fatalf("bad socks5: %+v", p)
	}
}

func TestParseUnsupported(t *testing.T) {
	if _, err := Parse("ftp://x"); err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

func TestWriteProviderFlow(t *testing.T) {
	proxies, errs := ParseAll([]string{
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:p")) + "@1.1.1.1:80#A",
		"bogus://x",
	})
	if len(proxies) != 1 || len(errs) != 1 {
		t.Fatalf("ParseAll: proxies=%d errs=%d", len(proxies), len(errs))
	}
	dir := t.TempDir()
	path := dir + "/custom.yaml"
	if err := WriteProvider(path, proxies); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "proxies:") || !strings.Contains(string(data), "'type': 'ss'") {
		t.Fatalf("bad provider file:\n%s", data)
	}
}
