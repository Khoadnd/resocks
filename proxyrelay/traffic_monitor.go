package proxyrelay

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// TrafficMetrics represents the traffic and latency data structure
type TrafficMetrics struct {
	UploadSpeed   string `json:"up"`
	DownloadSpeed string `json:"down"`
	TotalUp       string `json:"total_up,omitempty"`
	TotalDown     string `json:"total_down,omitempty"`

	// Latency metrics
	AvgLatency     string `json:"avg_latency"`
	ConnLatency    string `json:"connection_latency"`
	ProcessLatency string `json:"processing_latency"`
	MinLatency     string `json:"min_latency"`
	MaxLatency     string `json:"max_latency"`

	// Connection statistics
	ActiveConnections int   `json:"active_connections"`
	TotalConnections  int64 `json:"total_connections"`
	TunnelActive      bool  `json:"tunnel_active"`
}

// LatencyStats holds latency measurement data
type LatencyStats struct {
	sum   int64 // nanoseconds
	count int64
	min   int64 // nanoseconds
	max   int64 // nanoseconds
}

// ConnectionInfo tracks individual connection metrics
type ConnectionInfo struct {
	StartTime      time.Time
	LastActivity   time.Time
	BytesUp        int64
	BytesDown      int64
	ConnectLatency time.Duration
}

// TrafficMonitor tracks and reports network traffic and latency
type TrafficMonitor struct {
	uploadBytes   int64
	downloadBytes int64
	totalUp       int64
	totalDown     int64

	lastUpload   int64
	lastDownload int64
	lastUpdate   time.Time

	// Latency tracking
	connectionLatency LatencyStats // Time to establish connections
	processingLatency LatencyStats // Time spent processing data
	totalLatency      LatencyStats // End-to-end latency

	// Connection tracking
	activeConnections map[string]*ConnectionInfo
	totalConnections  int64
	tunnelActive      int32 // Using int32 for atomic operations (0=false, 1=true)

	metricsFile string
	interval    time.Duration
	mu          sync.RWMutex
	stopChan    chan struct{}
}

// NewTrafficMonitor creates a new traffic monitor with latency tracking
func NewTrafficMonitor(metricsFile string, interval time.Duration) *TrafficMonitor {
	return &TrafficMonitor{
		metricsFile:       metricsFile,
		interval:          interval,
		lastUpdate:        time.Now(),
		activeConnections: make(map[string]*ConnectionInfo),
		stopChan:          make(chan struct{}),
		connectionLatency: LatencyStats{min: int64(^uint64(0) >> 1)}, // Max int64
		processingLatency: LatencyStats{min: int64(^uint64(0) >> 1)},
		totalLatency:      LatencyStats{min: int64(^uint64(0) >> 1)},
	}
}

// Start begins monitoring and writing metrics
func (tm *TrafficMonitor) Start() {
	go tm.monitorLoop()
}

// Stop stops the monitoring
func (tm *TrafficMonitor) Stop() {
	close(tm.stopChan)
}

// AddUploadBytes adds uploaded bytes to the counter
func (tm *TrafficMonitor) AddUploadBytes(bytes int64) {
	atomic.AddInt64(&tm.uploadBytes, bytes)
	atomic.AddInt64(&tm.totalUp, bytes)
}

// AddDownloadBytes adds downloaded bytes to the counter
func (tm *TrafficMonitor) AddDownloadBytes(bytes int64) {
	atomic.AddInt64(&tm.downloadBytes, bytes)
	atomic.AddInt64(&tm.totalDown, bytes)
}

// RecordConnectionLatency records the time taken to establish a connection
func (tm *TrafficMonitor) RecordConnectionLatency(latency time.Duration) {
	nanos := latency.Nanoseconds()

	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.connectionLatency.sum += nanos
	tm.connectionLatency.count++

	if nanos < tm.connectionLatency.min {
		tm.connectionLatency.min = nanos
	}
	if nanos > tm.connectionLatency.max {
		tm.connectionLatency.max = nanos
	}
}

// RecordProcessingLatency records processing latency for data operations
func (tm *TrafficMonitor) RecordProcessingLatency(latency time.Duration) {
	nanos := latency.Nanoseconds()

	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.processingLatency.sum += nanos
	tm.processingLatency.count++

	if nanos < tm.processingLatency.min {
		tm.processingLatency.min = nanos
	}
	if nanos > tm.processingLatency.max {
		tm.processingLatency.max = nanos
	}
}

// RecordTotalLatency records end-to-end latency
func (tm *TrafficMonitor) RecordTotalLatency(latency time.Duration) {
	nanos := latency.Nanoseconds()

	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.totalLatency.sum += nanos
	tm.totalLatency.count++

	if nanos < tm.totalLatency.min {
		tm.totalLatency.min = nanos
	}
	if nanos > tm.totalLatency.max {
		tm.totalLatency.max = nanos
	}
}

// StartConnection starts tracking a new connection
func (tm *TrafficMonitor) StartConnection(connID string) *ConnectionInfo {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	info := &ConnectionInfo{
		StartTime:    time.Now(),
		LastActivity: time.Now(),
	}
	tm.activeConnections[connID] = info
	atomic.AddInt64(&tm.totalConnections, 1)

	return info
}

// EndConnection stops tracking a connection
func (tm *TrafficMonitor) EndConnection(connID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	delete(tm.activeConnections, connID)
}

// UpdateConnectionLatency updates the connection establishment latency for a specific connection
func (tm *TrafficMonitor) UpdateConnectionLatency(connID string, latency time.Duration) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if info, exists := tm.activeConnections[connID]; exists {
		info.ConnectLatency = latency
	}
	tm.RecordConnectionLatency(latency)
}

// SetTunnelActive sets the tunnel active status (when resocks client connects to server)
func (tm *TrafficMonitor) SetTunnelActive(active bool) {
	if active {
		atomic.StoreInt32(&tm.tunnelActive, 1)
	} else {
		atomic.StoreInt32(&tm.tunnelActive, 0)
	}
}

// IsTunnelActive returns whether the tunnel is currently active
func (tm *TrafficMonitor) IsTunnelActive() bool {
	return atomic.LoadInt32(&tm.tunnelActive) == 1
}

func (tm *TrafficMonitor) monitorLoop() {
	ticker := time.NewTicker(tm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			tm.updateMetrics()
		case <-tm.stopChan:
			return
		}
	}
}

func (tm *TrafficMonitor) updateMetrics() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tm.lastUpdate).Seconds()

	currentUp := atomic.LoadInt64(&tm.uploadBytes)
	currentDown := atomic.LoadInt64(&tm.downloadBytes)

	upDiff := currentUp - tm.lastUpload
	downDiff := currentDown - tm.lastDownload

	upSpeed := float64(upDiff) / elapsed
	downSpeed := float64(downDiff) / elapsed

	metrics := TrafficMetrics{
		UploadSpeed:       formatSpeed(upSpeed),
		DownloadSpeed:     formatSpeed(downSpeed),
		TotalUp:           formatBytes(atomic.LoadInt64(&tm.totalUp)),
		TotalDown:         formatBytes(atomic.LoadInt64(&tm.totalDown)),
		ActiveConnections: len(tm.activeConnections),
		TotalConnections:  atomic.LoadInt64(&tm.totalConnections),
		TunnelActive:      tm.IsTunnelActive(),
	}

	// Calculate latency metrics
	if tm.totalLatency.count > 0 {
		avgNanos := tm.totalLatency.sum / tm.totalLatency.count
		metrics.AvgLatency = formatDuration(time.Duration(avgNanos))
		metrics.MinLatency = formatDuration(time.Duration(tm.totalLatency.min))
		metrics.MaxLatency = formatDuration(time.Duration(tm.totalLatency.max))
	} else {
		metrics.AvgLatency = "0ms"
		metrics.MinLatency = "0ms"
		metrics.MaxLatency = "0ms"
	}

	if tm.connectionLatency.count > 0 {
		avgConnNanos := tm.connectionLatency.sum / tm.connectionLatency.count
		metrics.ConnLatency = formatDuration(time.Duration(avgConnNanos))
	} else {
		metrics.ConnLatency = "0ms"
	}

	if tm.processingLatency.count > 0 {
		avgProcNanos := tm.processingLatency.sum / tm.processingLatency.count
		metrics.ProcessLatency = formatDuration(time.Duration(avgProcNanos))
	} else {
		metrics.ProcessLatency = "0ms"
	}

	tm.writeMetrics(metrics)

	tm.lastUpload = currentUp
	tm.lastDownload = currentDown
	tm.lastUpdate = now
}

func (tm *TrafficMonitor) writeMetrics(metrics TrafficMetrics) {
	data, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		return
	}

	// Write to temporary file first, then rename for atomic update
	tempFile := tm.metricsFile + ".tmp"
	err = os.WriteFile(tempFile, data, 0644)
	if err != nil {
		return
	}

	err = os.Rename(tempFile, tm.metricsFile)
	if err != nil {
		os.Remove(tempFile) // Clean up temp file on error
	}
}

func formatSpeed(bytesPerSec float64) string {
	if bytesPerSec >= 1024*1024*1024 {
		return fmt.Sprintf("%.1fGB/s", bytesPerSec/(1024*1024*1024))
	} else if bytesPerSec >= 1024*1024 {
		return fmt.Sprintf("%.1fMB/s", bytesPerSec/(1024*1024))
	} else if bytesPerSec >= 1024 {
		return fmt.Sprintf("%.1fKB/s", bytesPerSec/1024)
	}
	return fmt.Sprintf("%.0fB/s", bytesPerSec)
}

func formatBytes(bytes int64) string {
	if bytes >= 1024*1024*1024 {
		return fmt.Sprintf("%.1fGB", float64(bytes)/(1024*1024*1024))
	} else if bytes >= 1024*1024 {
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	} else if bytes >= 1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%dB", bytes)
}

func formatDuration(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.2fs", d.Seconds())
	} else if d >= time.Millisecond {
		return fmt.Sprintf("%.1fms", float64(d.Nanoseconds())/1e6)
	} else if d >= time.Microsecond {
		return fmt.Sprintf("%.1fμs", float64(d.Nanoseconds())/1e3)
	}
	return fmt.Sprintf("%dns", d.Nanoseconds())
}
