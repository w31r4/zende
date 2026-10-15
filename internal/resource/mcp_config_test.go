package resource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestMCPConfigStrictAndNonsecret(t *testing.T) {
	base := "schema_version: 1\nname: local-test\ntransport: stdio\ncommand: mcp-server\n"
	for name, extra := range map[string]string{
		"unknown": "token: secret\n", "duplicate": "name: second\n", "null": "args: null\n",
		"alias": "args: &args [one]\nheaders: *args\n", "mixed": "url: https://example.invalid\n",
		"plain secret": "env: {KEY: secret}\n", "relative cwd": "cwd: relative\n", "multidoc": "---\nname: second\n",
		"invalid env": "env: {BAD-NAME: 'env:KEY'}\n", "credential": "credential: env:KEY\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseMCPConfig([]byte(base + extra)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	for _, input := range []string{
		base + "env: {KEY: 'env:SECRET', OTHER: 'file:/secrets/test'}\nargs: [one, two]\n",
		`{"schema_version":1,"name":"remote","transport":"streamable-http","url":"https://example.invalid/mcp","credential":"env:KEY","headers":{"X-Api-Key":"file:/secrets/test"}}`,
	} {
		c, err := ParseMCPConfig([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(c)
		again, err := ParseMCPConfig(b)
		if err != nil || !reflect.DeepEqual(c, again) || c.BindingVersion() != again.BindingVersion() {
			t.Fatal(c, again, err)
		}
	}
	httpBase := "schema_version: 1\nname: remote\ntransport: streamable-http\n"
	for _, tail := range []string{
		"url: file:///tmp/test\n", "url: https://user:pass@example.invalid\n", "url: https://example.invalid?key=secret\n", "url: https://example.invalid#fragment\n",
		"url: https://example.invalid\nheaders: {Authorization: secret}\n",
		"url: https://example.invalid\nheaders: {authorization: 'env:KEY', Authorization: 'env:OTHER'}\n",
		"url: https://example.invalid\nheaders: {Mcp-Session-Id: 'env:KEY'}\n",
		"url: https://example.invalid\nheaders: {Mcp-Method: 'env:KEY'}\n",
		"url: https://example.invalid\nheaders: {Mcp-Name: 'env:KEY'}\n",
		"url: https://example.invalid\nheaders: {mcp-param-tenant: 'env:KEY'}\n",
		"url: https://example.invalid\ncredential: env:KEY\nheaders: {Authorization: 'env:OTHER'}\n",
	} {
		if _, err := ParseMCPConfig([]byte(httpBase + tail)); err == nil {
			t.Fatal("accepted invalid HTTP config", tail)
		}
	}
	if _, err := ParseMCPConfig([]byte(strings.Repeat("x", MaxMCPConfigBytes+1))); err == nil {
		t.Fatal("unbounded config")
	}
}

func TestMCPConfigAndCredentialFIFOAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCredential("file:"+path, os.Getenv); err == nil {
		t.Fatal("FIFO credential accepted")
	}
	if _, err := readMCPFile(t.Context(), path); err == nil {
		t.Fatal("FIFO config accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readMCPFile(ctx, path); err == nil {
		t.Fatal("ignored cancellation")
	}
}
