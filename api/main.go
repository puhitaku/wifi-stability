package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

/* ---------- ping watchdog ---------- */

type pingCounter struct {
	failures    []pingFailure
	consecutive int
	failLock    sync.Mutex
}

type pingFailure struct {
	Timestamp        time.Time `json:"timestamp"`
	ConsecutiveCount int       `json:"consecutive_count"`
}

func (p *pingCounter) clearCount() {
	p.failLock.Lock()
	defer p.failLock.Unlock()

	p.consecutive = 0
}

func (p *pingCounter) recordFailure() {
	p.failLock.Lock()
	defer p.failLock.Unlock()

	p.consecutive++
	p.failures = append(p.failures, pingFailure{Timestamp: time.Now(), ConsecutiveCount: p.consecutive})
}

func (p *pingCounter) ListFailures() []pingFailure {
	p.failLock.Lock()
	defer p.failLock.Unlock()

	out := make([]pingFailure, len(p.failures))
	copy(out, p.failures)
	return out
}

func (p *pingCounter) PurgeFailures() {
	p.failLock.Lock()
	defer p.failLock.Unlock()

	p.failures = []pingFailure{}
}

func (p *pingCounter) Loop(ctx context.Context, dest string) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()

	p.PurgeFailures()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			args := []string{"-c", "1", "-w", "5", dest}
			if runtime.GOOS == "darwin" {
				args = []string{"-c", "1", "-t", "5", dest}
			}
			if err := exec.Command("ping", args...).Run(); err != nil {
				p.recordFailure()
			} else {
				p.clearCount()
			}
		}
	}
}

var p = &pingCounter{}

/* ---------- speed test schema ---------- */

type Latency struct {
	IQM    float64 `json:"iqm"`
	Low    float64 `json:"low,omitempty"`
	High   float64 `json:"high,omitempty"`
	Jitter float64 `json:"jitter,omitempty"`
}

type Transfer struct {
	Bandwidth int64   `json:"bandwidth"`
	Bytes     int64   `json:"bytes"`
	Elapsed   int64   `json:"elapsed"`
	Latency   Latency `json:"latency"`
	Progress  float64 `json:"progress,omitempty"`
}

type SpeedTestProgress struct {
	Type      string    `json:"type"` // "upload"
	Timestamp time.Time `json:"timestamp"`
	Upload    Transfer  `json:"upload"`
}

type SpeedTestResult struct {
	Type       string    `json:"type"` // "result"
	Timestamp  time.Time `json:"timestamp"`
	Ping       Latency   `json:"ping"`
	Download   Transfer  `json:"download"`
	Upload     Transfer  `json:"upload"`
	PacketLoss int       `json:"packetLoss"`
	ISP        string    `json:"isp"`
	Server     struct {
		ID       int    `json:"id"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Name     string `json:"name"`
		Location string `json:"location"`
		Country  string `json:"country"`
		IP       string `json:"ip"`
	} `json:"server"`
	Result struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		Persisted bool   `json:"persisted"`
	} `json:"result"`
}

type SpeedtestResponse struct {
	Progress []SpeedTestProgress `json:"progress,omitempty"`
	Result   *SpeedTestResult    `json:"result,omitempty"`
	Error    string              `json:"error"`
}

func runSpeedtest(serverID int) (SpeedtestResponse, error) {
	cmd := exec.Command("speedtest", "-s", strconv.Itoa(serverID), "-p", "no", "-f", "jsonl")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return SpeedtestResponse{}, err
	}
	if err = cmd.Start(); err != nil {
		return SpeedtestResponse{}, err
	}

	var (
		progress []SpeedTestProgress
		result   SpeedTestResult
	)
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		b := sc.Bytes()
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &head) != nil {
			continue
		}
		switch head.Type {
		case "upload":
			var p SpeedTestProgress
			if err = json.Unmarshal(b, &p); err == nil {
				progress = append(progress, p)
			}
		case "result":
			if err = json.Unmarshal(b, &result); err != nil {
				return SpeedtestResponse{}, err
			}
		}
	}
	if err = sc.Err(); err != nil {
		return SpeedtestResponse{}, err
	}
	if err = cmd.Wait(); err != nil {
		return SpeedtestResponse{}, err
	}
	if result.Type == "" {
		return SpeedtestResponse{}, errors.New("no result line")
	}
	return SpeedtestResponse{Progress: progress, Result: &result}, nil
}

/* ---------- HTTP handlers ---------- */

func failuresHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if err := json.NewEncoder(w).Encode(p.ListFailures()); err != nil {
			http.Error(w, mustMarshal(SpeedtestResponse{Error: err.Error()}), http.StatusInternalServerError)
		}
	case http.MethodDelete:
		p.PurgeFailures()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, mustMarshal(SpeedtestResponse{Error: "method not allowed"}), http.StatusInternalServerError)
	}
}

func speedtestHandler(w http.ResponseWriter, r *http.Request) {
	s := r.URL.Query().Get("server")
	if s == "" {
		s = "48463"
	}
	id, err := strconv.Atoi(s)
	if err != nil {
		http.Error(w, mustMarshal(SpeedtestResponse{Error: "invalid server ID"}), http.StatusInternalServerError)
		return
	}
	res, err := runSpeedtest(id)
	if err != nil {
		http.Error(w, mustMarshal(SpeedtestResponse{Error: err.Error()}), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(res); err != nil {
		http.Error(w, mustMarshal(SpeedtestResponse{Error: err.Error()}), http.StatusInternalServerError)
	}
}

func mustMarshal(s any) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func main() {
	addr := flag.String("addr", "0.0.0.0", "listen address")
	portFlag := flag.Int("port", 42839, "listen port")
	ping := flag.String("ping", "8.8.8.8", "listen address")
	flag.Parse()
	bind := fmt.Sprintf("%s:%d", *addr, *portFlag)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go p.Loop(ctx, *ping)

	mux := http.NewServeMux()
	mux.HandleFunc("/failures", failuresHandler)
	mux.HandleFunc("/speedtest", speedtestHandler)
	srv := &http.Server{Addr: bind, Handler: mux}

	srvErr := make(chan error, 1)
	go func() {
		log.Printf("Stability API listening on %s", bind)
		err := srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
		close(srvErr)
	}()

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		log.Fatalf("server error: %v", err)
	}

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("bye")
}
