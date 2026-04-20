// Package client provides a gRPC client for connecting to the DePIN Orchestrator.
// It handles connection management, retry logic, node registration, and event streaming.
package client

import (
	"context"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/depin-agent/agent/internal/limits"
	"github.com/depin-agent/agent/pkg/logger"
	pb "github.com/depin-agent/agent/proto"
)

// ClientConfig holds configuration for the gRPC client.
type ClientConfig struct {
	// OrchestratorAddress is the address of the orchestrator (e.g., "localhost:50051")
	OrchestratorAddress string

	// MaxRetries is the maximum number of connection/call retry attempts
	MaxRetries int

	// InitialBackoff is the initial backoff duration for retries
	InitialBackoff time.Duration

	// MaxBackoff is the maximum backoff duration between retries
	MaxBackoff time.Duration

	// ConnectionTimeout is the timeout for establishing connection
	ConnectionTimeout time.Duration

	// CallTimeout is the timeout for RPC calls
	CallTimeout time.Duration

	// APIKey is the authentication key sent with every request
	APIKey string
}

// DefaultClientConfig returns a ClientConfig with sensible defaults.
func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		OrchestratorAddress: "localhost:50051",
		MaxRetries:          5,
		InitialBackoff:      1 * time.Second,
		MaxBackoff:          30 * time.Second,
		ConnectionTimeout:   10 * time.Second,
		CallTimeout:         10 * time.Second,
	}
}

// JobHandler is a callback function for handling incoming job requests.
// It receives a JobRequest and a logSender for streaming logs,
// and returns a JobResult with the execution outcome.
// The logSender should be called with each log line to stream to the orchestrator.
type JobHandler func(ctx context.Context, req *pb.JobRequest, logSender func(string)) *pb.JobResult

// StopServiceHandler is a callback for handling stop service requests.
type StopServiceHandler func(jobID string, gracePeriodSeconds int32)

// Client is the gRPC client for communicating with the Orchestrator.
type Client struct {
	config *ClientConfig
	logger *zap.Logger
	conn   *grpc.ClientConn
	client pb.NodeServiceClient
	nodeID string

	// RTT tracking
	rttMu           sync.RWMutex
	lastRTT         time.Duration
	avgRTT          time.Duration
	rttSamples      []time.Duration
	maxRTTSamples   int
	heartbeatSentAt map[int64]time.Time // sent_timestamp_ms -> send time
	formatter       *logger.Formatter
	rttLogCounter   int // Counter to throttle RTT logging (every Nth measurement)

	mu     sync.RWMutex
	closed bool
}

// NewClient creates a new gRPC client.
func NewClient(config *ClientConfig, logger *zap.Logger) *Client {
	if config == nil {
		config = DefaultClientConfig()
	}
	return &Client{
		config:          config,
		logger:          logger,
		heartbeatSentAt: make(map[int64]time.Time),
		maxRTTSamples:   10, // Keep last 10 samples for averaging
		rttSamples:      make([]time.Duration, 0, 10),
	}
}

// Connect establishes a connection to the Orchestrator with retry logic.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("client is closed")
	}

	var lastErr error
	backoff := c.config.InitialBackoff

	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("connection cancelled: %w", ctx.Err())
		default:
		}

		if attempt > 0 {
			c.logger.Info("Retrying connection",
				zap.Int("attempt", attempt),
				zap.Duration("backoff", backoff),
			)
			time.Sleep(backoff)
			// Exponential backoff with cap
			backoff = time.Duration(math.Min(
				float64(backoff)*2,
				float64(c.config.MaxBackoff),
			))
		}

		// Create connection with timeout
		connCtx, cancel := context.WithTimeout(ctx, c.config.ConnectionTimeout)

		// Setup dial options
		dialOpts := []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
			grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(50 * 1024 * 1024)), // 50MB for artifact uploads
		}

		// Add API key interceptors if configured
		if c.config.APIKey != "" {
			dialOpts = append(dialOpts,
				grpc.WithUnaryInterceptor(c.apiKeyUnaryInterceptor()),
				grpc.WithStreamInterceptor(c.apiKeyStreamInterceptor()),
			)
			c.logger.Debug("API key authentication enabled")
		}

		conn, err := grpc.DialContext(connCtx, c.config.OrchestratorAddress, dialOpts...)
		cancel()

		if err != nil {
			lastErr = err
			c.logger.Warn("Connection attempt failed",
				zap.Int("attempt", attempt),
				zap.Error(err),
			)
			continue
		}

		c.conn = conn
		c.client = pb.NewNodeServiceClient(conn)

		// Sanitize address for logging (mask IP)
		sanitizer := logger.NewSanitizer(true)
		safeAddress := sanitizer.Sanitize(c.config.OrchestratorAddress)

		c.logger.Info("Connected to orchestrator",
			zap.String("address", safeAddress),
		)
		return nil
	}

	return fmt.Errorf("failed to connect after %d attempts: %w", c.config.MaxRetries, lastErr)
}

// Register sends node information to the Orchestrator.
// It uses the discovered GPU data to create a NodeInfo message.
func (c *Client) Register(ctx context.Context, nodeInfo *pb.NodeInfo) (*pb.RegistrationResponse, error) {
	c.mu.RLock()
	if c.client == nil {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client not connected")
	}
	client := c.client
	c.mu.RUnlock()

	var lastErr error
	backoff := c.config.InitialBackoff

	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("register cancelled: %w", ctx.Err())
		default:
		}

		if attempt > 0 {
			c.logger.Info("Retrying registration",
				zap.Int("attempt", attempt),
				zap.Duration("backoff", backoff),
			)
			time.Sleep(backoff)
			backoff = time.Duration(math.Min(
				float64(backoff)*2,
				float64(c.config.MaxBackoff),
			))
		}

		callCtx, cancel := context.WithTimeout(ctx, c.config.CallTimeout)
		resp, err := client.Register(callCtx, nodeInfo)
		cancel()

		if err != nil {
			lastErr = err
			// Check if error is retryable
			if !isRetryable(err) {
				return nil, fmt.Errorf("registration failed (non-retryable): %w", err)
			}
			c.logger.Warn("Registration attempt failed",
				zap.Int("attempt", attempt),
				zap.Error(err),
			)
			continue
		}

		// Store the node ID assigned by orchestrator
		c.mu.Lock()
		c.nodeID = nodeInfo.Id
		c.mu.Unlock()

		c.logger.Info("Successfully registered with orchestrator",
			zap.String("node_id", nodeInfo.Id),
			zap.String("status", resp.Status),
			zap.String("message", resp.Message),
		)
		return resp, nil
	}

	return nil, fmt.Errorf("registration failed after %d attempts: %w", c.config.MaxRetries, lastErr)
}

// StreamEvents establishes a bidirectional stream with the orchestrator.
// It sends periodic heartbeats and handles incoming job requests.
//
// The handler callback is invoked for each incoming JobRequest.
// Results are automatically sent back to the orchestrator.
//
// If limitsStore is provided, resource limit updates are applied to it.
//
// This function blocks until the context is cancelled or an error occurs.
// ServiceStatusSender sends service status updates to the orchestrator.
// It is exposed via StreamEvents so the job handler can report service health.
type ServiceStatusSender func(status *pb.ServiceStatus)

func (c *Client) StreamEvents(ctx context.Context, nodeInfo *pb.NodeInfo, handler JobHandler, telemetryProvider func() *pb.Heartbeat, heartbeatInterval time.Duration, limitsStore *limits.Store) error {
	c.mu.RLock()
	if c.client == nil {
		c.mu.RUnlock()
		return fmt.Errorf("client not connected")
	}
	client := c.client
	nodeID := nodeInfo.Id
	apiKey := c.config.APIKey
	c.mu.RUnlock()

	// Add API key to context for stream authentication
	if apiKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", apiKey)
	}

	// Establish the bidirectional stream
	stream, err := client.StreamEvents(ctx)
	if err != nil {
		return fmt.Errorf("failed to establish stream: %w", err)
	}

	c.logger.Info("Stream established with orchestrator",
		zap.String("node_id", nodeID),
	)

	// Channel for sending events to orchestrator
	sendCh := make(chan *pb.AgentEvent, 10)
	defer close(sendCh)

	// Error channel for goroutines
	errCh := make(chan error, 2)

	// Goroutine: Send events to orchestrator
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-sendCh:
				if !ok {
					return
				}
				if err := stream.Send(event); err != nil {
					errCh <- fmt.Errorf("failed to send event: %w", err)
					return
				}
			}
		}
	}()

	// Goroutine: Receive events from orchestrator
	go func() {
		for {
			serverEvent, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					errCh <- fmt.Errorf("stream closed by server")
				} else {
					errCh <- fmt.Errorf("failed to receive event: %w", err)
				}
				return
			}

			// Handle the event based on its type
			switch event := serverEvent.GetEvent().(type) {
			case *pb.ServerEvent_JobRequest:
				jobReq := event.JobRequest
				c.logger.Info("Received job request",
					zap.String("job_id", jobReq.JobId),
					zap.String("image", jobReq.Image),
					zap.Strings("command", jobReq.Command),
				)

				// Execute the job in a goroutine to not block receiving
				go func(req *pb.JobRequest) {
					// Create a log sender that streams logs to the orchestrator
					logSender := func(line string) {
						jobLog := &pb.JobLog{
							JobId: req.JobId,
							Line:  line,
						}
						agentLogEvent := &pb.AgentEvent{
							NodeId: nodeID,
							Event:  &pb.AgentEvent_JobLog{JobLog: jobLog},
						}
						select {
						case sendCh <- agentLogEvent:
							// Log sent successfully
						case <-ctx.Done():
							// Context cancelled, skip sending
						default:
							c.logger.Warn("Log channel full, dropping log line",
								zap.String("job_id", req.JobId),
							)
						}
					}

					// Call the handler with the log sender
					result := handler(ctx, req, logSender)
					result.JobId = req.JobId // Ensure job ID is set

					// Send result back
					agentEvent := &pb.AgentEvent{
						NodeId: nodeID,
						Event:  &pb.AgentEvent_JobResult{JobResult: result},
					}

					select {
					case sendCh <- agentEvent:
						c.logger.Info("Job result sent",
							zap.String("job_id", result.JobId),
							zap.String("status", result.Status),
						)
					case <-ctx.Done():
						c.logger.Warn("Failed to send job result - context cancelled",
							zap.String("job_id", result.JobId),
						)
					}
				}(jobReq)

			case *pb.ServerEvent_ResourceLimitsUpdate:
				update := event.ResourceLimitsUpdate
				if limitsStore != nil && update != nil && update.Limits != nil {
					limitsStore.Update(update.Limits)
					c.logger.Info("Resource limits updated",
						zap.String("updated_at", update.UpdatedAt),
						zap.Float64("max_cpu", update.Limits.MaxCpu),
						zap.Float64("max_ram_gb", update.Limits.MaxRamGb),
						zap.Int64("max_volume_gb", update.Limits.MaxVolumeGb),
					)
				}

			case *pb.ServerEvent_HeartbeatAck:
				ack := event.HeartbeatAck
				c.handleHeartbeatAck(ack)

			case *pb.ServerEvent_StopService:
				stop := event.StopService
				c.logger.Info("Received stop service request",
					zap.String("job_id", stop.JobId),
					zap.Int32("grace_period_seconds", stop.GracePeriodSeconds),
				)
				// StopService handling is delegated to the job handler via context cancellation
				// The main.go service manager will handle this

			default:
				c.logger.Warn("Unknown server event type received")
			}
		}
	}()

	// Send initial heartbeat
	initialHeartbeatData := telemetryProvider()

	// Track send time for RTT calculation
	c.rttMu.Lock()
	c.heartbeatSentAt[initialHeartbeatData.SentTimestampMs] = time.Now()
	c.rttMu.Unlock()

	initialHeartbeat := &pb.AgentEvent{
		NodeId: nodeID,
		Event:  &pb.AgentEvent_Heartbeat{Heartbeat: initialHeartbeatData},
	}
	if err := stream.Send(initialHeartbeat); err != nil {
		return fmt.Errorf("failed to send initial heartbeat: %w", err)
	}
	c.logger.Debug("Initial heartbeat sent")

	// Heartbeat ticker
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Main loop: send heartbeats and wait for errors/cancellation
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("Stream context cancelled, closing stream")
			return ctx.Err()

		case err := <-errCh:
			return err

		case <-ticker.C:
			heartbeat := telemetryProvider()

			// Track send time for RTT calculation
			c.rttMu.Lock()
			c.heartbeatSentAt[heartbeat.SentTimestampMs] = time.Now()
			c.rttMu.Unlock()

			agentEvent := &pb.AgentEvent{
				NodeId: nodeID,
				Event:  &pb.AgentEvent_Heartbeat{Heartbeat: heartbeat},
			}
			select {
			case sendCh <- agentEvent:
				c.logger.Debug("Heartbeat sent")
			default:
				c.logger.Warn("Heartbeat channel full, skipping")
			}
		}
	}
}

// GetNodeID returns the registered node ID.
func (c *Client) GetNodeID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nodeID
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		c.client = nil
		return err
	}
	return nil
}

// isRetryable checks if a gRPC error is retryable.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	st, ok := status.FromError(err)
	if !ok {
		return true // Unknown errors are retryable
	}

	switch st.Code() {
	case codes.Unavailable,
		codes.DeadlineExceeded,
		codes.ResourceExhausted,
		codes.Aborted:
		return true
	default:
		return false
	}
}

// apiKeyUnaryInterceptor returns a unary interceptor that adds the API key to requests.
func (c *Client) apiKeyUnaryInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", c.config.APIKey)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// handleHeartbeatAck processes HeartbeatAck from orchestrator and calculates RTT.
func (c *Client) handleHeartbeatAck(ack *pb.HeartbeatAck) {
	if ack == nil {
		return
	}

	c.rttMu.Lock()
	defer c.rttMu.Unlock()

	// Find the send time for this heartbeat
	sendTime, found := c.heartbeatSentAt[ack.SentTimestampMs]
	if !found {
		c.logger.Debug("Received HeartbeatAck for unknown timestamp",
			zap.Int64("sent_timestamp_ms", ack.SentTimestampMs),
		)
		return
	}

	// Calculate RTT
	now := time.Now()
	rtt := now.Sub(sendTime)
	c.lastRTT = rtt

	// Add to samples for averaging
	c.rttSamples = append(c.rttSamples, rtt)
	if len(c.rttSamples) > c.maxRTTSamples {
		c.rttSamples = c.rttSamples[1:] // Keep last N samples
	}

	// Calculate average RTT
	var sum time.Duration
	for _, sample := range c.rttSamples {
		sum += sample
	}
	c.avgRTT = sum / time.Duration(len(c.rttSamples))

	// Clean up old sent timestamps (keep last 20 to handle out-of-order acks)
	if len(c.heartbeatSentAt) > 20 {
		oldest := int64(math.MaxInt64)
		for ts := range c.heartbeatSentAt {
			if ts < oldest {
				oldest = ts
			}
		}
		delete(c.heartbeatSentAt, oldest)
	}

	// Delete current timestamp
	delete(c.heartbeatSentAt, ack.SentTimestampMs)

	// Log RTT periodically (every 20 measurements = ~1 minute at 3s interval)
	c.rttLogCounter++
	if c.rttLogCounter >= 20 {
		c.rttLogCounter = 0
		if c.formatter != nil {
			logMsg := c.formatter.Info(
				"Network latency",
				fmt.Sprintf("RTT: %dms (avg: %dms)", rtt.Milliseconds(), c.avgRTT.Milliseconds()),
			)
			c.logger.Debug(logMsg)
		}
	}

	// Always log with debug level for monitoring
	c.logger.Debug("Heartbeat RTT measured",
		zap.Duration("rtt", rtt),
		zap.Duration("avg_rtt", c.avgRTT),
	)
}

// GetRTT returns the last measured RTT and average RTT.
func (c *Client) GetRTT() (last, avg time.Duration) {
	c.rttMu.RLock()
	defer c.rttMu.RUnlock()
	return c.lastRTT, c.avgRTT
}

// SetFormatter sets the user-friendly formatter for RTT logging.
func (c *Client) SetFormatter(formatter *logger.Formatter) {
	c.rttMu.Lock()
	defer c.rttMu.Unlock()
	c.formatter = formatter
}

// apiKeyStreamInterceptor returns a stream interceptor that adds the API key to streams.
func (c *Client) apiKeyStreamInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
		method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", c.config.APIKey)
		return streamer(ctx, desc, cc, method, opts...)
	}
}
