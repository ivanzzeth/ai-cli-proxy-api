package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type shutdownObservedListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *shutdownObservedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func stopTestStream(t *testing.T) (*Server, *bufio.Reader, chan struct{}, *shutdownObservedListener) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &shutdownObservedListener{Listener: raw, closed: make(chan struct{})}
	server := NewServer(&config.Config{Host: "127.0.0.1", Port: raw.Addr().(*net.TCPAddr).Port}, nil, nil, "")
	release := make(chan struct{})
	server.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		<-release
		if _, err := io.WriteString(w, "event: response.completed\ndata: [DONE]\n\n"); err != nil {
			return
		}
		w.(http.Flusher).Flush()
	})
	served := make(chan error, 1)
	go func() { served <- server.server.Serve(ln) }()
	response, err := http.Get("http://" + ln.Addr().String() + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event %q %v", line, err)
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		if err := response.Body.Close(); err != nil {
			t.Log(err)
		}
		if err := server.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Log(err)
		}
		select {
		case err := <-served:
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server goroutine did not exit")
		}
	})
	return server, reader, release, ln
}

func TestServerStopDrainsAdmittedSSEBeforeReturning(t *testing.T) {
	server, reader, release, ln := stopTestStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Stop(ctx) }()
	<-ln.closed
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned before stream completed: %v", err)
	default:
	}
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err == nil {
		if e := conn.Close(); e != nil {
			t.Log(e)
		}
		t.Fatal("shutdown still accepts new connections")
	}
	close(release)
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "response.completed") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("terminal missing: %s", body)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestServerStopPreservesExpiredDeadlineError(t *testing.T) {
	server, reader, release, _ := stopTestStream(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := server.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop error=%v", err)
	}
	close(release)
	body, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("Stop deadline forcibly closed existing stream: %s %v", body, err)
	}
}
