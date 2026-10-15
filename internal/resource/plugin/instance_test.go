package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

func TestPluginProcessHelper(t *testing.T) {
	if os.Getenv("ZENDE_PLUGIN_HELPER") != "1" {
		return
	}
	mode := os.Getenv("ZENDE_PLUGIN_MODE")
	if mode == "stall" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     string           `json:"id"`
			Method string           `json:"method"`
			Params InitializeParams `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.Method == "plugin.shutdown" {
			if mode == "shutdown-diagnostics" {
				fmt.Fprintln(os.Stderr, strings.Repeat("tail ", 20000)+"final diagnostic")
			}
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%q,\"result\":null}\n", req.ID)
			os.Exit(0)
		}
		if req.Method != "plugin.initialize" || req.Params.InstanceID == "" || req.Params.Host.ID != "zende" || len(req.Params.ProtocolVersions) != 1 {
			os.Exit(3)
		}
		result := InitializeResult{ProtocolVersion: ProtocolVersion, Plugin: req.Params.Plugin, Capabilities: req.Params.Capabilities}
		secret := os.Getenv("ZENDE_PLUGIN_TEST_CREDENTIAL")
		switch mode {
		case "version":
			result.ProtocolVersion++
		case "identity":
			result.Plugin.Version = "other"
		case "capabilities":
			result.Capabilities = []string{"events"}
		case "empty":
			result.Capabilities = nil
		case "error":
			encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32603, "message": secret, "data": secret}})
			fmt.Fprintln(os.Stdout, string(encoded))
			continue
		case "log":
			fmt.Fprintln(os.Stdout, "debug "+secret)
			continue
		case "oversized":
			fmt.Fprintln(os.Stdout, strings.Repeat("x", MaxFrameBytes+1))
			continue
		case "duplicate":
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%q,\"id\":%q,\"result\":null}\n", req.ID, req.ID)
			continue
		case "both":
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%q,\"result\":null,\"error\":{\"code\":1,\"message\":\"bad\"}}\n", req.ID)
			continue
		case "null-error":
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%q,\"error\":null}\n", req.ID)
			continue
		case "wrong-id":
			req.ID = "unsolicited"
		case "invalid-utf8":
			fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%q,\"result\":\"%s\"}\n", req.ID, []byte{255})
			continue
		case "child-exit":
			child := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 30")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if child.Start() != nil {
				os.Exit(4)
			}
			if os.WriteFile("child.pid", []byte(fmt.Sprint(child.Process.Pid)), 0600) != nil {
				os.Exit(5)
			}
			os.Exit(0)
		}
		if mode == "diagnostics" {
			fmt.Fprint(os.Stderr, strings.Repeat("diagnostic ", 10000))
			half := len(secret) / 2
			fmt.Fprint(os.Stderr, "\n"+secret[:half])
			time.Sleep(10 * time.Millisecond)
			fmt.Fprint(os.Stderr, secret[half:]+"\nuseful message\x1b[2J\n")
		}
		reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if mode == "unterminated" {
			os.Stdout.Write(reply)
			os.Exit(0)
		}
		fmt.Fprintln(os.Stdout, string(reply))
		if mode == "no-read" {
			time.Sleep(time.Minute)
			os.Exit(0)
		}
		if mode == "idle-log" {
			time.Sleep(30 * time.Millisecond)
			fmt.Fprintln(os.Stdout, "unexpected stdout log")
		}
	}
	os.Exit(0)
}

func helperManifest(t *testing.T, mode string) (Manifest, string) {
	t.Helper()
	t.Setenv("ZENDE_PLUGIN_HELPER", "1")
	t.Setenv("ZENDE_PLUGIN_MODE", mode)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Manifest{SchemaVersion: 1, ID: "fixture", Version: "1.2.3", ProtocolVersions: []int{1},
			Command: binary, Args: []string{"-test.run=^TestPluginProcessHelper$"}, Capabilities: []string{"tools", "commands"}},
		filepath.Join(t.TempDir(), "plugin.json")
}

func requireFailure(t *testing.T, err error, kinds ...string) {
	t.Helper()
	var failure *protocol.Error
	if !errors.As(err, &failure) {
		t.Fatal("expected classified failure", err)
	}
	for _, kind := range kinds {
		if kind == failure.Data.Kind {
			return
		}
	}
	t.Fatal("unexpected failure", err, kinds)
}

func TestPluginInitializationLifetimeAndIsolation(t *testing.T) {
	manifest, source := helperManifest(t, "good")
	ctx, cancel := context.WithCancel(t.Context())
	instance, err := Start(ctx, source, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	cancel()
	if got := instance.Info(); got.State != "ready" || got.ProtocolVersion != 1 || !got.Trusted || got.PID == os.Getpid() || got.Limits.FrameBytes != MaxFrameBytes {
		t.Fatal(got)
	}
	manifest.Capabilities[0] = "changed"
	info := instance.Info()
	info.Capabilities[0] = "changed"
	if instance.Info().Capabilities[0] != "tools" {
		t.Fatal("caller mutated the live instance")
	}
	bad, badSource := helperManifest(t, "version")
	failed, err := Start(t.Context(), badSource, bad, nil)
	requireFailure(t, err, "version_mismatch")
	if failed == nil || failed.Info().State != "failed" || len(failed.Info().Capabilities) != 0 || instance.Info().State != "ready" {
		t.Fatal("failed replacement affected the ready instance", failed, instance.Info())
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := instance.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if instance.Info().State != "closed" || syscall.Kill(info.PID, 0) != syscall.ESRCH {
		t.Fatal("owned process not reaped", instance.Info())
	}
}

func TestPluginInitializationRejectsProtocolFailures(t *testing.T) {
	for _, mode := range []string{"version", "identity", "capabilities", "empty", "error", "log", "oversized", "duplicate", "both", "null-error", "wrong-id", "invalid-utf8", "unterminated", "child-exit"} {
		t.Run(mode, func(t *testing.T) {
			manifest, source := helperManifest(t, mode)
			const secret = "fixture-credential-do-not-persist"
			t.Setenv("ZENDE_PLUGIN_TEST_CREDENTIAL", secret)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			instance, err := Start(ctx, source, manifest, nil)
			if err == nil || instance == nil {
				t.Fatal(instance, err)
			}
			kind := "plugin_protocol"
			if mode == "version" {
				kind = "version_mismatch"
			}
			if mode == "error" {
				kind = "plugin_initialization"
			}
			requireFailure(t, err, kind)
			<-instance.Closed()
			info := instance.Info()
			if info.State != "failed" || len(info.Capabilities) != 0 || syscall.Kill(info.PID, 0) != syscall.ESRCH || strings.Contains(err.Error(), secret) {
				t.Fatal(info, err)
			}
			if mode == "child-exit" {
				raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "child.pid"))
				if err != nil {
					t.Fatal(err)
				}
				// Delivery of SIGKILL is asynchronous. An orphan zombie may
				// await WSL PID 1, but cannot retain pipes or execute work.
				deadline := time.Now().Add(time.Second)
				for {
					stat, err := os.ReadFile(filepath.Join("/proc", string(raw), "stat"))
					if os.IsNotExist(err) {
						break
					}
					_, tail, ok := strings.Cut(string(stat), ") ")
					if err == nil && ok && strings.HasPrefix(tail, "Z ") {
						break
					}
					if err != nil || !ok || time.Now().After(deadline) {
						t.Fatal(string(stat), err)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		})
	}
}

func TestPluginTimeoutAndBlockedWriter(t *testing.T) {
	manifest, source := helperManifest(t, "stall")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	instance, err := Start(ctx, source, manifest, nil)
	if !errors.Is(err, context.DeadlineExceeded) || instance == nil || time.Since(started) > time.Second {
		t.Fatal(instance, err, time.Since(started))
	}
	manifest, source = helperManifest(t, "no-read")
	instance, err = Start(t.Context(), source, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started = time.Now()
	_, err = instance.exchange(ctx, "test.blocked-write", map[string]string{"large": strings.Repeat("x", 1<<20)})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatal(err, time.Since(started))
	}
	instance.stop(protocol.E("cancelled", "fixture finished"))
	<-instance.Closed()
}

func TestPluginDiagnosticsAreSeparateBoundedAndRedacted(t *testing.T) {
	manifest, source := helperManifest(t, "diagnostics")
	const secret = "known-credential-with-cross-chunk-boundary"
	manifest.Env = map[string]string{"ZENDE_PLUGIN_TEST_CREDENTIAL": "env:SOURCE_KEY"}
	instance, err := Start(t.Context(), source, manifest, map[string]string{"ZENDE_PLUGIN_TEST_CREDENTIAL": secret})
	if err != nil {
		t.Fatal(err)
	}
	instance.Close()
	diagnostics := instance.Diagnostics()
	raw, _ := json.Marshal(instance.Info())
	if strings.Contains(diagnostics.Text+string(raw), secret) || !strings.Contains(diagnostics.Text, "[REDACTED]") || !strings.Contains(diagnostics.Text, "useful message\\x1b[2J") || diagnostics.BytesDropped == 0 || diagnostics.BytesRead < 100000 {
		t.Fatal(diagnostics.BytesRead, diagnostics.BytesDropped, diagnostics.Text, string(raw))
	}
	if len(diagnostics.Text) > 4*MaxDiagnosticBytes {
		t.Fatal("unbounded display projection", len(diagnostics.Text))
	}
}

func TestPluginIdleStdoutPollutionClosesOnlyItsInstance(t *testing.T) {
	manifest, source := helperManifest(t, "idle-log")
	instance, err := Start(t.Context(), source, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-instance.Closed():
	case <-time.After(3 * time.Second):
		t.Fatal("idle stdout pollution ignored")
	}
	if instance.Info().State != "failed" {
		t.Fatal(instance.Info())
	}
}

func TestPluginShutdownDrainsDiagnosticTail(t *testing.T) {
	manifest, source := helperManifest(t, "shutdown-diagnostics")
	instance, err := Start(t.Context(), source, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	instance.Close()
	if !strings.HasSuffix(instance.Diagnostics().Text, "final diagnostic\n") {
		t.Fatal("shutdown discarded buffered diagnostics")
	}
	_, err = instance.exchange(t.Context(), "plugin.shutdown", struct{}{})
	if err == nil {
		t.Fatal("closed transport returned a successful empty response")
	}
}

func TestPluginCancellationInterruptsBlockedWriter(t *testing.T) {
	manifest, source := helperManifest(t, "no-read")
	instance, err := Start(t.Context(), source, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := instance.exchange(ctx, "test.blocked-write", map[string]string{"large": strings.Repeat("x", 1<<20)})
		result <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel left a blocked stdin writer")
	}
	instance.stop(protocol.E("cancelled", "fixture finished"))
	<-instance.Closed()
}

func TestPluginManifestValidationAndDigest(t *testing.T) {
	m := Manifest{SchemaVersion: 1, ID: "fixture", Version: "1", ProtocolVersions: []int{2, 1}, Command: "/bin/example", Capabilities: []string{"tools", "events"}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	other := m.clone()
	other.Capabilities[0], other.Capabilities[1] = other.Capabilities[1], other.Capabilities[0]
	other.ProtocolVersions[0], other.ProtocolVersions[1] = 1, 2
	if other.Digest() != m.Digest() {
		t.Fatal("ordering changed manifest binding")
	}
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.SchemaVersion++ }, func(m *Manifest) { m.ID = "../invalid" },
		func(m *Manifest) { m.Version = "" }, func(m *Manifest) { m.Command = "" },
		func(m *Manifest) { m.ProtocolVersions = []int{1, 1} }, func(m *Manifest) { m.Cwd = "relative" },
		func(m *Manifest) { m.Capabilities = []string{"unknown"} }, func(m *Manifest) { m.Capabilities = []string{"tools", "tools"} },
		func(m *Manifest) { m.Env = map[string]string{"KEY": "plain-value"} }, func(m *Manifest) { m.Env = map[string]string{"KEY": "file:relative"} },
	} {
		bad := m.clone()
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid manifest accepted", bad)
		}
	}
}

func TestDiagnosticRedactionAcrossEverySplit(t *testing.T) {
	raw := "prefix alpha-secret-beta suffix longer-alpha-secret-beta\n"
	want := "prefix [REDACTED] suffix [REDACTED]\n"
	for split := range len(raw) + 1 {
		d := newDiagnostics([]string{"alpha-secret-beta", "longer-alpha-secret-beta"})
		d.write([]byte(raw[:split]), false)
		d.write([]byte(raw[split:]), true)
		if got := d.snapshot().Text; got != want {
			t.Fatal(split, got)
		}
	}
}
