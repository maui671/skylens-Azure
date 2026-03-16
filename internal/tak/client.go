package tak

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// ClientConfig holds TAK server connection settings.
type ClientConfig struct {
	Address  string // host:port
	UseTLS   bool
	CertFile string // client cert PEM path
	KeyFile  string // client key PEM path
	CAFile   string // CA/truststore PEM path
}

// Client manages a persistent TCP/TLS connection to a TAK server.
type Client struct {
	cfg  ClientConfig
	mu   sync.Mutex
	conn net.Conn
	done chan struct{}

	// reconnectCh is signalled when the connection dies (Send fails)
	// or when config changes (UpdateConfig). This wakes the Start loop
	// from the "hold connection" select so it can reconnect.
	reconnectCh chan struct{}

	connected bool
	lastErr   string
}

// NewClient creates a new TAK client (does not connect yet).
func NewClient(cfg ClientConfig) *Client {
	return &Client{
		cfg:         cfg,
		done:        make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
	}
}

// Start begins the auto-reconnect loop. Blocks until ctx is cancelled.
// Always call this — when no address is configured, it waits for a config
// change signal instead of spinning.
func (c *Client) Start(ctx context.Context) {
	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			c.close()
			return
		case <-c.done:
			c.close()
			return
		default:
		}

		// Check if we have an address to connect to
		c.mu.Lock()
		addr := c.cfg.Address
		c.mu.Unlock()

		if addr == "" {
			// No address configured — wait for config change or shutdown
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-c.reconnectCh:
				// Config changed, loop back to check address
				backoff = time.Second
				continue
			}
		}

		if err := c.connect(); err != nil {
			c.mu.Lock()
			c.lastErr = err.Error()
			c.connected = false
			c.mu.Unlock()

			slog.Warn("TAK connection failed, will retry",
				"address", addr,
				"error", err,
				"backoff", backoff,
			)

			// Wait for backoff, but also wake on reconnect signal or shutdown
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-c.reconnectCh:
				// Config changed or connection died — retry immediately
				backoff = time.Second
				continue
			case <-time.After(backoff):
			}

			// Exponential backoff capped at 60s
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
			continue
		}

		// Connected — reset backoff
		backoff = time.Second
		slog.Info("TAK server connected", "address", addr)

		// Hold connection open — wake on:
		// - shutdown (ctx/done)
		// - connection died (reconnectCh from Send)
		// - config changed (reconnectCh from UpdateConfig)
		select {
		case <-ctx.Done():
			c.close()
			return
		case <-c.done:
			c.close()
			return
		case <-c.reconnectCh:
			// Drop current connection and reconnect (new config or dead conn)
			slog.Info("TAK reconnecting (config change or connection lost)")
			c.close()
			continue
		}
	}
}

// Stop signals the client to disconnect and stop reconnecting.
func (c *Client) Stop() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	c.close()
}

// Send writes CoT XML data to the TAK server. Returns error if not connected.
func (c *Client) Send(data []byte) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	// Set write deadline to avoid blocking forever
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}

	_, err := conn.Write(data)
	if err != nil {
		// Connection is dead — mark disconnected and signal reconnect
		c.mu.Lock()
		c.connected = false
		c.conn = nil
		c.lastErr = err.Error()
		c.mu.Unlock()
		conn.Close()

		// Wake the Start loop to reconnect
		c.signalReconnect()

		return fmt.Errorf("write failed: %w", err)
	}

	return nil
}

// IsConnected returns whether the client has an active connection.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// LastError returns the last connection error message.
func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// UpdateConfig updates the connection config and triggers a reconnect.
// The Start loop will drop the current connection (if any) and reconnect
// with the new config.
func (c *Client) UpdateConfig(cfg ClientConfig) {
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()

	// Signal the Start loop to reconnect with new config
	c.signalReconnect()
}

// GetConfig returns a copy of the current config.
func (c *Client) GetConfig() ClientConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// signalReconnect wakes the Start loop (non-blocking).
func (c *Client) signalReconnect() {
	select {
	case c.reconnectCh <- struct{}{}:
	default:
		// Already signalled, don't block
	}
}

func (c *Client) connect() error {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()

	if cfg.Address == "" {
		return fmt.Errorf("no address configured")
	}

	var conn net.Conn
	var err error

	if cfg.UseTLS {
		tlsCfg, tlsErr := buildTLSConfig(cfg)
		if tlsErr != nil {
			return fmt.Errorf("TLS config: %w", tlsErr)
		}
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", cfg.Address, tlsCfg)
	} else {
		conn, err = net.DialTimeout("tcp", cfg.Address, 10*time.Second)
	}

	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.lastErr = ""
	c.mu.Unlock()

	return nil
}

func (c *Client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.connected = false
}

func buildTLSConfig(cfg ClientConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load client certificate if provided
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate if provided
	if cfg.CAFile != "" {
		caCert, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsCfg.RootCAs = pool
	}

	return tlsCfg, nil
}

// TestConnection attempts a one-shot connect and close to validate connectivity.
func TestConnection(cfg ClientConfig) error {
	var conn net.Conn
	var err error

	if cfg.UseTLS {
		tlsCfg, tlsErr := buildTLSConfig(cfg)
		if tlsErr != nil {
			return fmt.Errorf("TLS config: %w", tlsErr)
		}
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", cfg.Address, tlsCfg)
	} else {
		conn, err = net.DialTimeout("tcp", cfg.Address, 10*time.Second)
	}

	if err != nil {
		return err
	}
	conn.Close()
	return nil
}
