package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/omidalighadr/media-job-api/internal/audio"
	"github.com/omidalighadr/media-job-api/internal/httpapi"
	"github.com/omidalighadr/media-job-api/internal/jobs"
)

func main() {
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	addr := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	data := flag.String("data", "./data", "private persistent data directory")
	workers := flag.Int("workers", 2, "number of verification workers (1-8)")
	maxUpload := flag.Int64("max-upload-mib", 32, "maximum bytes per upload in MiB (1-256)")
	queue := flag.Int("queue-limit", 32, "maximum queued and running jobs (1-1000)")
	timeout := flag.Duration("job-timeout", 2*time.Minute, "maximum verification time per attempt")
	flag.Parse()
	if *workers < 1 || *workers > 8 || *maxUpload < 1 || *maxUpload > 256 || *queue < 1 || *queue > 1000 || *timeout <= 0 {
		return errors.New("invalid resource limits")
	}
	token := os.Getenv("MEDIA_JOB_TOKEN")
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if token == "" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("MEDIA_JOB_TOKEN is required for a non-loopback listener")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("install ffmpeg: %w", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(*data)
	if err != nil {
		return err
	}
	store, err := jobs.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	// Private temporary files stay inside the data directory. Old files from a
	// crash are removed only after acquiring the exclusive service lock.
	tmp := filepath.Join(dir, "work")
	if err = os.RemoveAll(tmp); err != nil {
		return err
	}
	if err = os.Mkdir(tmp, 0700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	pool := jobs.NewPool(store, audio.Verifier{FFmpeg: ffmpeg, FFprobe: ffprobe, TempDir: tmp}, *timeout)
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	pool.Start(workCtx, *workers)
	server := &http.Server{Handler: httpapi.New(store, pool, token, *maxUpload<<20, *queue), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	slog.Info("listening", "address", listener.Addr().String(), "workers", *workers)
	select {
	case <-ctx.Done():
	case err = <-done:
	}
	shutdown, finish := context.WithTimeout(context.Background(), 10*time.Second)
	defer finish()
	if e := server.Shutdown(shutdown); e != nil {
		server.Close()
		if err == nil {
			err = e
		}
	}
	cancelWorkers()
	pool.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
