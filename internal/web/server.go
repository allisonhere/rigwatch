package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/allisonhere/rigwatch/internal"
)

//go:embed dashboard.html
var dashboardFS embed.FS

// HostSnapshot is the JSON shape for one host's data at a point in time.
type HostSnapshot struct {
	Name     string                 `json:"name"`
	Info     *internal.SystemInfo   `json:"info"`
	Error    string                 `json:"error,omitempty"`
	Updated  time.Time              `json:"updated"`
	History  *MetricHistory         `json:"history,omitempty"`
}

type MetricHistory struct {
	CPU     []float64 `json:"cpu"`
	GPU     []float64 `json:"gpu"`
	VRAM    []float64 `json:"vram"`
	RAM     []float64 `json:"ram"`
	Temp    []float64 `json:"temp"`
	Network []float64 `json:"network"`
}

const historyLimit = 60

type hostState struct {
	client  *internal.SSHClient
	info    *internal.SystemInfo
	err     error
	updated time.Time
	history metricHistory
}

type metricHistory struct {
	CPU     []float64
	GPU     []float64
	VRAM    []float64
	RAM     []float64
	Temp    []float64
	Network []float64
}

// Server manages data collection and HTTP serving.
type Server struct {
	hosts    []internal.SSHHost
	interval time.Duration
	port     int

	mu     sync.RWMutex
	states map[string]*hostState
}

// NewServer creates a web server that collects data from the given hosts.
// Call Start() to begin serving.
func NewServer(hosts []internal.SSHHost, interval time.Duration, port int) *Server {
	states := make(map[string]*hostState, len(hosts))
	for _, h := range hosts {
		states[h.Name] = &hostState{}
	}
	return &Server{
		hosts:    hosts,
		interval: interval,
		port:     port,
		states:   states,
	}
}

// Start begins data collection and HTTP serving. Blocks until the server stops.
func (s *Server) Start() error {
	// Connect to all hosts
	s.connectAll()

	// Start background collection
	go s.collectLoop()

	// HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/api/hosts", s.handleHosts)
	mux.HandleFunc("/", s.handleDashboard)

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("rigwatch web server starting on http://0.0.0.0%s", addr)
	return http.ListenAndServe(addr, corsMiddleware(mux))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) connectAll() {
	for _, host := range s.hosts {
		h := host
		client, err := internal.NewSSHClient(h)
		s.mu.Lock()
		st := s.states[h.Name]
		if err != nil {
			st.err = fmt.Errorf("connect: %w", err)
			st.client = nil
		} else {
			st.client = client
			st.err = nil
		}
		s.mu.Unlock()
	}
}

func (s *Server) collectLoop() {
	// Do first collection immediately
	s.collectAll()

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for range ticker.C {
		s.collectAll()
	}
}

func (s *Server) collectAll() {
	var wg sync.WaitGroup
	for _, host := range s.hosts {
		h := host
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.collectOne(h)
		}()
	}
	wg.Wait()
}

func (s *Server) collectOne(host internal.SSHHost) {
	s.mu.Lock()
	st := s.states[host.Name]
	s.mu.Unlock()

	if st == nil {
		return
	}

	// Ensure client exists
	s.mu.Lock()
	client := st.client
	s.mu.Unlock()

	if client == nil {
		// Try reconnect
		newClient, err := internal.NewSSHClient(host)
		s.mu.Lock()
		if err != nil {
			st.err = fmt.Errorf("reconnect: %w", err)
			st.client = nil
			s.mu.Unlock()
			return
		}
		st.client = newClient
		st.err = nil
		client = newClient
		s.mu.Unlock()
	}

	info, err := internal.GatherSystemInfo(client)
	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		st.err = fmt.Errorf("collect: %w", err)
		return
	}

	st.info = info
	st.err = nil
	st.updated = time.Now()

	// Update history
	if st.history.CPU == nil {
		st.history.CPU = make([]float64, 0, historyLimit)
		st.history.GPU = make([]float64, 0, historyLimit)
		st.history.VRAM = make([]float64, 0, historyLimit)
		st.history.RAM = make([]float64, 0, historyLimit)
		st.history.Temp = make([]float64, 0, historyLimit)
		st.history.Network = make([]float64, 0, historyLimit)
	}

	st.history.CPU = appendSample(st.history.CPU, info.CPU.UsagePercent)

	var gpuUtil, gpuCount int
	var vramUsed, vramTotal int
	for _, g := range info.GPUs {
		gpuUtil += g.Utilization
		gpuCount++
		vramUsed += g.VRAMUsed
		vramTotal += g.VRAMTotal
	}
	if gpuCount > 0 {
		st.history.GPU = appendSample(st.history.GPU, float64(gpuUtil)/float64(gpuCount))
	}
	if vramTotal > 0 {
		st.history.VRAM = appendSample(st.history.VRAM, (float64(vramUsed)/float64(vramTotal))*100)
	}

	st.history.RAM = appendSample(st.history.RAM, info.RAM.UsagePercent)

	var maxTemp float64
	for _, t := range info.Temps {
		if t.Celsius > maxTemp {
			maxTemp = t.Celsius
		}
	}
	for _, g := range info.GPUs {
		if float64(g.Temperature) > maxTemp {
			maxTemp = float64(g.Temperature)
		}
	}
	st.history.Temp = appendSample(st.history.Temp, maxTemp)

	var totalRate float64
	for _, iface := range info.Network {
		totalRate += float64(iface.RXBps + iface.TXBps)
	}
	netPercent := 0.0
	if totalRate > 0 {
		maxRate := 125.0 * 1024 * 1024
		pct := totalRate / maxRate * 100
		if pct > 100 {
			pct = 100
		}
		netPercent = pct
	}
	st.history.Network = appendSample(st.history.Network, netPercent)
}

func appendSample(samples []float64, sample float64) []float64 {
	samples = append(samples, sample)
	if len(samples) > historyLimit {
		return samples[len(samples)-historyLimit:]
	}
	return samples
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snapshots := make([]HostSnapshot, 0, len(s.hosts))
	for _, host := range s.hosts {
		st := s.states[host.Name]
		snap := HostSnapshot{
			Name:    host.Name,
			Updated: st.updated,
		}
		if st.err != nil {
			snap.Error = st.err.Error()
		}
		if st.info != nil {
			snap.Info = st.info
		}
		if st.history.CPU != nil {
			snap.History = &MetricHistory{
				CPU:     st.history.CPU,
				GPU:     st.history.GPU,
				VRAM:    st.history.VRAM,
				RAM:     st.history.RAM,
				Temp:    st.history.Temp,
				Network: st.history.Network,
			}
		}
		snapshots = append(snapshots, snap)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshots)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	htmlBytes, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusInternalServerError)
		return
	}

	tmpl, err := template.New("dashboard").Parse(string(htmlBytes))
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	data := struct {
		Version    string
		RefreshSec int
	}{
		Version:    internal.ShortVersion(),
		RefreshSec: int(s.interval.Seconds()) * 1000,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, data)
}
