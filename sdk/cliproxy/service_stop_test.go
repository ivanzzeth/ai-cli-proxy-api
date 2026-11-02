package cliproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type shutdownContextProvider struct{ loaded chan context.Context }

func (p *shutdownContextProvider) Load(ctx context.Context, _ *sdkconfig.Config) (*APIKeyClientResult, error) {
	p.loaded <- ctx
	return &APIKeyClientResult{}, nil
}

func waitServiceListener(t *testing.T, address string, accept bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if conn != nil {
			if e := conn.Close(); e != nil {
				t.Log(e)
			}
		}
		if (err == nil) == accept {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("listener accept=%v not observed", accept)
		case <-ticker.C:
		}
	}
}

func startShutdownService(t *testing.T) (*Service, context.CancelFunc, <-chan error, context.Context, chan struct{}, string) {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := raw.Addr().String()
	port := raw.Addr().(*net.TCPAddr).Port
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err = os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Host: "127.0.0.1", Port: port, AuthDir: dir}
	provider := &shutdownContextProvider{loaded: make(chan context.Context, 1)}
	release := make(chan struct{})
	var runtimeCtx context.Context
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath(path).WithAPIKeyClientProvider(provider).WithServerOptions(api.WithEngineConfigurator(func(engine *gin.Engine) {
		engine.GET("/fixture-stream", func(c *gin.Context) {
			c.Header("Content-Type", "text/event-stream")
			if _, err := io.WriteString(c.Writer, "data: first\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
			select {
			case <-release:
			case <-runtimeCtx.Done():
				return
			}
			if _, err := io.WriteString(c.Writer, "event: response.completed\ndata: [DONE]\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		})
	})).Build()
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	select {
	case runtimeCtx = <-provider.loaded:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runtime provider was not loaded")
	}
	waitServiceListener(t, address, true)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		cancel()
	})
	return service, cancel, done, runtimeCtx, release, address
}

func TestServiceRunSignalDrainsHTTPBeforeCancellingRuntime(t *testing.T) {
	t.Setenv("AI_ROUTER_SHUTDOWN_TIMEOUT", "5s")
	_, cancel, done, runtimeCtx, release, address := startShutdownService(t)
	response, err := http.Get("http://" + address + "/fixture-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Log(err)
		}
	}()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first event %q %v", line, err)
	}
	cancel()
	waitServiceListener(t, address, false)
	select {
	case <-runtimeCtx.Done():
		t.Fatal("signal cancelled runtime before admitted HTTP drained")
	default:
	}
	select {
	case err := <-done:
		t.Fatalf("Run exited before terminal: %v", err)
	default:
	}
	close(release)
	body, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(body), "response.completed") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("terminal lost %s %v", body, err)
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish")
	}
	select {
	case <-runtimeCtx.Done():
	default:
		t.Fatal("runtime not cancelled after drain")
	}
}

func TestServiceRunConfiguredDrainBudgetExpiresWithoutFalseTerminal(t *testing.T) {
	t.Setenv("AI_ROUTER_SHUTDOWN_TIMEOUT", "1ms")
	_, cancel, done, _, _, address := startShutdownService(t)
	response, err := http.Get("http://" + address + "/fixture-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Log(err)
		}
	}()
	reader := bufio.NewReader(response.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("configured drain budget was ignored")
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Logf("expected expired-drain stream read: %v", err)
	}
	if strings.Contains(string(body), "response.completed") || strings.Contains(string(body), "[DONE]") {
		t.Fatalf("timed-out drain invented completion: %s", body)
	}
}

func TestServiceRunInternalCancellationStillExits(t *testing.T) {
	t.Setenv("AI_ROUTER_SHUTDOWN_TIMEOUT", "15m")
	service, _, done, runtimeCtx, _, address := startShutdownService(t)
	service.cancelServiceRun()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("internal runtime cancellation no longer stops Run")
	}
	select {
	case <-runtimeCtx.Done():
	default:
		t.Fatal("internal cancellation not propagated")
	}
	waitServiceListener(t, address, false)
}

func TestServiceRunInvalidShutdownBudgetFailsBeforeStartup(t *testing.T) {
	for _, value := range []string{"0", "-1s", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AI_ROUTER_SHUTDOWN_TIMEOUT", value)
			service := &Service{}
			if err := service.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "AI_ROUTER_SHUTDOWN_TIMEOUT") {
				t.Fatalf("invalid budget startup error: %v", err)
			}
		})
	}
}

func TestServiceShutdownPropagatesCallerDeadline(t *testing.T) {
	t.Setenv("AI_ROUTER_SHUTDOWN_TIMEOUT", "5s")
	service, cancelRun, done, _, release, address := startShutdownService(t)
	response, err := http.Get("http://" + address + "/fixture-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Log(err)
		}
	}()
	reader := bufio.NewReader(response.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := service.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active HTTP deadline error lost: %v", err)
	}
	close(release)
	body, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("deadline force-closed stream: %s %v", body, err)
	}
	cancelRun()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run failed to exit after explicit cancellation")
	}
}
