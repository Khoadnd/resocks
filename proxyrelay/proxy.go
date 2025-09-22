package proxyrelay

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/sync/errgroup"
)

var globalTrafficMonitor *TrafficMonitor

// SetTrafficMonitor sets the global traffic monitor
func SetTrafficMonitor(monitor *TrafficMonitor) {
	globalTrafficMonitor = monitor
}

// RunProxy starts a SOCKS server on socks5listenAddr that tunnels all incoming
// connections through relayConn. The opposite site of the relayConn connection
// should be handled by RunRelay.
func RunProxy(ctx context.Context, relayConn net.Conn, socks5listenAddr string) (err error) {
	return RunProxyWithEventCallback(ctx, relayConn, socks5listenAddr, DefaultEventCallback)
}

// RunProxyWithEventCallback is like RunProxy but it allows to specify a custom
// event callback instead of DefaultEventCallback. If callback is nil, events
// are ignored.
func RunProxyWithEventCallback(
	ctx context.Context, relayConn net.Conn, socks5ListenAddr string, callback func(Event),
) error {
	if callback != nil {
		callback(Event{Type: TypeRelayConnected, Data: relayConn.RemoteAddr().String()})
	}

	// Set tunnel as active when relay connection is established
	if globalTrafficMonitor != nil {
		globalTrafficMonitor.SetTunnelActive(true)
	}

	err := handleRelayConnection(ctx, relayConn, socks5ListenAddr, callback)
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}

	// Set tunnel as inactive when relay connection ends
	if globalTrafficMonitor != nil {
		globalTrafficMonitor.SetTunnelActive(false)
	}

	if callback != nil {
		data := ""
		if err != nil {
			data = err.Error()
		}

		callback(Event{Type: TypeRelayDisconnected, Data: data})
	}

	return err
}

func handleRelayConnection(ctx context.Context, relayConn net.Conn, proxyAddr string, callback func(Event)) error {
	go func() {
		<-ctx.Done()

		_ = relayConn.Close()
	}()

	client, err := yamux.Client(relayConn, yamuxCfg())
	if err != nil {
		// Set tunnel as inactive on connection failure
		if globalTrafficMonitor != nil {
			globalTrafficMonitor.SetTunnelActive(false)
		}
		return fmt.Errorf("initialize multiplexer: %w", err)
	}

	var tlsErr *tls.CertificateVerificationError

	// we use the first connection to receive socks-related errors from the relay
	errConn, err := client.Open()
	if err != nil {
		if errors.Is(err, yamux.ErrSessionShutdown) || errors.As(err, &tlsErr) {
			// Set tunnel as inactive on invalid connection key
			if globalTrafficMonitor != nil {
				globalTrafficMonitor.SetTunnelActive(false)
			}
			return fmt.Errorf("invalid connection key")
		}

		// Set tunnel as inactive on connection failure
		if globalTrafficMonitor != nil {
			globalTrafficMonitor.SetTunnelActive(false)
		}
		return fmt.Errorf("open error notification connection: %w", err)
	}

	// display the errors in the UI
	go handleErrorNotificationConnection(errConn, callback)

	err = startLocalProxyServer(proxyAddr, client, callback)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	return nil
}

func handleErrorNotificationConnection(conn net.Conn, callback func(Event)) {
	for {
		lengthBytes := make([]byte, 4)

		_, err := conn.Read(lengthBytes)
		if errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			if callback != nil {
				callback(Event{
					Type: TypeError,
					Data: fmt.Sprintf("read message length from error notification connection: %v", err),
				})
			}

			return
		}

		msg := make([]byte, binary.BigEndian.Uint32(lengthBytes))

		_, err = conn.Read(msg)
		if err != nil {
			if callback != nil {
				callback(Event{Type: TypeError, Data: fmt.Sprintf("read message from error notification connection: %v", err)})
			}

			return
		}

		if callback != nil {
			callback(Event{Type: TypeError, Data: string(msg)})
		}
	}
}

func startLocalProxyServer(proxyAddr string, sess *yamux.Session, callback func(Event)) error {
	proxyListener, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		return fmt.Errorf("listen for relay connection: %w", err)
	}

	defer proxyListener.Close() //nolint:errcheck

	if callback != nil {
		callback(Event{Type: TypeSOCKS5Active})
		defer callback(Event{Type: TypeSOCKS5Inactive})
	}

	var closedBecausePayloadDisconnected bool

	go func() {
		<-sess.CloseChan()

		closedBecausePayloadDisconnected = true

		err := proxyListener.Close()
		if err != nil && callback != nil {
			callback(Event{Type: TypeError, Data: fmt.Sprintf("socks5 close: %v", err)})
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := proxyListener.Accept()
		if err != nil {
			if closedBecausePayloadDisconnected {
				return nil
			}

			return fmt.Errorf("accept socks5 connection: %w", err)
		}

		if callback != nil {
			callback(Event{Type: TypeSOCKS5ConnectionOpened, Data: formatAddr(conn.RemoteAddr())})
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			err := handleLocalProxyConn(conn, sess)
			if err != nil && callback != nil {
				callback(Event{Type: TypeError, Data: fmt.Sprintf("handling socks5 connection: %v", err)})
			}

			if callback != nil {
				callback(Event{Type: TypeSOCKS5ConnectionClosed, Data: formatAddr(conn.RemoteAddr())})
			}
		}()
	}
}

func handleLocalProxyConn(conn net.Conn, sess *yamux.Session) error {
	connectStart := time.Now()
	yamuxConn, err := sess.Open()
	connectLatency := time.Since(connectStart)

	if err != nil {
		return fmt.Errorf("open multiplexed connection: %w", err)
	}

	// Generate connection ID and start tracking
	connID := ConnID(conn.LocalAddr(), conn.RemoteAddr())

	// Wrap connections with traffic and latency monitoring if monitor is available
	var clientConn, relayConn net.Conn = conn, yamuxConn
	if globalTrafficMonitor != nil {
		// Start tracking this connection
		globalTrafficMonitor.StartConnection(connID)
		globalTrafficMonitor.RecordConnectionLatency(connectLatency)

		// Wrap connections with tracking
		clientConn = NewTrackedConn(conn, globalTrafficMonitor, true, connID+"_client")     // client->relay is upload
		relayConn = NewTrackedConn(yamuxConn, globalTrafficMonitor, false, connID+"_relay") // relay->client is download
	}

	var eg errgroup.Group

	eg.Go(func() error {
		defer conn.Close()      //nolint:errcheck
		defer yamuxConn.Close() //nolint:errcheck

		_, err := io.Copy(relayConn, clientConn)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("proxy->relay: %w", err)
		}

		return nil
	})

	eg.Go(func() error {
		defer conn.Close()      //nolint:errcheck
		defer yamuxConn.Close() //nolint:errcheck

		_, err := io.Copy(clientConn, relayConn)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("relay->proxy: %w", err)
		}

		return nil
	})

	result := eg.Wait()

	// End connection tracking
	if globalTrafficMonitor != nil {
		globalTrafficMonitor.EndConnection(connID)
	}

	return result
}
