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

// Client is the gRPC client for communicating with the Orchestrator.
type Client struct {
	config *ClientConfig
	logger *zap.Logger
	conn   *grpc.ClientConn
	client pb.NodeServiceClient
	nodeID string

	mu     sync.RWMutex
	closed bool
}

// NewClient creates a new gRPC client.
func NewClient(config *ClientConfig, logger *zap.Logger) *Client {
	if config == nil {
		config = DefaultClientConfig()
	}
	return &Client{
		config: config,
		logger: logger,
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
		c.logger.Info("Connected to orchestrator",
			zap.String("address", c.config.OrchestratorAddress),
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

			default:
				c.logger.Warn("Unknown server event type received")
			}
		}
	}()

	// Send initial heartbeat
	initialHeartbeat := &pb.AgentEvent{
		NodeId: nodeID,
		Event:  &pb.AgentEvent_Heartbeat{Heartbeat: telemetryProvider()},
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
			heartbeat := &pb.AgentEvent{
				NodeId: nodeID,
				Event:  &pb.AgentEvent_Heartbeat{Heartbeat: telemetryProvider()},
			}
			select {
			case sendCh <- heartbeat:
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

// apiKeyStreamInterceptor returns a stream interceptor that adds the API key to streams.
func (c *Client) apiKeyStreamInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
		method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", c.config.APIKey)
		return streamer(ctx, desc, cc, method, opts...)
	}
}
