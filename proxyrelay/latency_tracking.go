package proxyrelay

import (
	"fmt"
	"net"
	"time"
)

// TrackedConn wraps a net.Conn to track bytes transferred and latency
type TrackedConn struct {
	net.Conn
	monitor       *TrafficMonitor
	isUpload      bool // true for upload (client->relay), false for download (relay->client)
	connID        string
	lastReadTime  time.Time
	lastWriteTime time.Time
}

// NewTrackedConn creates a new tracked connection with latency monitoring
func NewTrackedConn(conn net.Conn, monitor *TrafficMonitor, isUpload bool, connID string) *TrackedConn {
	now := time.Now()
	return &TrackedConn{
		Conn:          conn,
		monitor:       monitor,
		isUpload:      isUpload,
		connID:        connID,
		lastReadTime:  now,
		lastWriteTime: now,
	}
}

func (tc *TrackedConn) Read(b []byte) (n int, err error) {
	start := time.Now()
	n, err = tc.Conn.Read(b)
	readLatency := time.Since(start)

	if n > 0 && tc.monitor != nil {
		// Record processing latency (time spent in read operation)
		tc.monitor.RecordProcessingLatency(readLatency)

		// Track data flow based on connection type:
		// - Client connection (isUpload=true): Reading means data coming from client (upload)
		// - Relay connection (isUpload=false): Reading means data coming from target (download)
		if tc.isUpload {
			tc.monitor.AddUploadBytes(int64(n))
		} else {
			tc.monitor.AddDownloadBytes(int64(n))
		}

		// RTT calculation for download responses
		if !tc.lastWriteTime.IsZero() && !tc.isUpload {
			rtt := start.Sub(tc.lastWriteTime)
			if rtt > 0 && rtt < 10*time.Second { // Reasonable RTT threshold
				tc.monitor.RecordTotalLatency(rtt)
			}
		}

		tc.lastReadTime = time.Now()
	}

	return n, err
}

func (tc *TrackedConn) Write(b []byte) (n int, err error) {
	start := time.Now()
	n, err = tc.Conn.Write(b)
	writeLatency := time.Since(start)

	if n > 0 && tc.monitor != nil {
		// Record processing latency (time spent in write operation)
		tc.monitor.RecordProcessingLatency(writeLatency)

		// Track data flow based on connection type:
		// - Client connection (isUpload=true): Writing means data going to client (download)
		// - Relay connection (isUpload=false): Writing means data going to target (upload)
		if tc.isUpload {
			tc.monitor.AddDownloadBytes(int64(n))
		} else {
			tc.monitor.AddUploadBytes(int64(n))
		}

		tc.lastWriteTime = time.Now()
	}

	return n, err
}

// Close closes the connection and updates connection tracking
func (tc *TrackedConn) Close() error {
	if tc.monitor != nil {
		tc.monitor.EndConnection(tc.connID)
	}
	return tc.Conn.Close()
}

// LatencyMeasuredConn wraps a connection to measure connection establishment latency
type LatencyMeasuredConn struct {
	net.Conn
	establishLatency time.Duration
}

// NewLatencyMeasuredConn creates a connection with measured establishment latency
func NewLatencyMeasuredConn(network, address string) (*LatencyMeasuredConn, error) {
	start := time.Now()
	conn, err := net.Dial(network, address)
	latency := time.Since(start)

	if err != nil {
		return nil, err
	}

	return &LatencyMeasuredConn{
		Conn:             conn,
		establishLatency: latency,
	}, nil
}

// GetEstablishLatency returns the connection establishment latency
func (lmc *LatencyMeasuredConn) GetEstablishLatency() time.Duration {
	return lmc.establishLatency
}

// ConnID generates a unique connection identifier
func ConnID(local, remote net.Addr) string {
	return fmt.Sprintf("%s->%s", local.String(), remote.String())
}

// MeasureLatency is a helper function to measure the latency of any operation
func MeasureLatency(operation func() error) (time.Duration, error) {
	start := time.Now()
	err := operation()
	latency := time.Since(start)
	return latency, err
}

// PingMeasurement can be used to measure ping-like latency
type PingMeasurement struct {
	conn   net.Conn
	buffer []byte
}

// NewPingMeasurement creates a new ping measurement helper
func NewPingMeasurement(conn net.Conn) *PingMeasurement {
	return &PingMeasurement{
		conn:   conn,
		buffer: make([]byte, 1),
	}
}

// MeasurePing sends a single byte and measures the round-trip time
func (pm *PingMeasurement) MeasurePing() (time.Duration, error) {
	start := time.Now()

	// Send a ping byte
	_, err := pm.conn.Write([]byte{0x00})
	if err != nil {
		return 0, fmt.Errorf("ping write: %w", err)
	}

	// Read the response
	_, err = pm.conn.Read(pm.buffer)
	if err != nil {
		return 0, fmt.Errorf("ping read: %w", err)
	}

	return time.Since(start), nil
}

// LatencyStats provides aggregated latency statistics
type LatencyAggregator struct {
	measurements []time.Duration
	maxSize      int
}

// NewLatencyAggregator creates a new latency aggregator with a maximum size
func NewLatencyAggregator(maxSize int) *LatencyAggregator {
	return &LatencyAggregator{
		measurements: make([]time.Duration, 0, maxSize),
		maxSize:      maxSize,
	}
}

// Add adds a latency measurement
func (la *LatencyAggregator) Add(latency time.Duration) {
	if len(la.measurements) >= la.maxSize {
		// Remove oldest measurement
		copy(la.measurements, la.measurements[1:])
		la.measurements[len(la.measurements)-1] = latency
	} else {
		la.measurements = append(la.measurements, latency)
	}
}

// GetStats returns aggregated statistics
func (la *LatencyAggregator) GetStats() (avg, min, max time.Duration, count int) {
	if len(la.measurements) == 0 {
		return 0, 0, 0, 0
	}

	var total time.Duration
	min = la.measurements[0]
	max = la.measurements[0]

	for _, latency := range la.measurements {
		total += latency
		if latency < min {
			min = latency
		}
		if latency > max {
			max = latency
		}
	}

	avg = total / time.Duration(len(la.measurements))
	count = len(la.measurements)

	return avg, min, max, count
}
