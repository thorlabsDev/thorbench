package internal

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
)

// responseData captures full response details for debugging
type responseData struct {
	Status     string
	StatusCode int
	Headers    http.Header
	Body       string
	Latency    time.Duration
	URL        string
	Method     string
	ReqBody    string // Store request body for context
	Timestamp  time.Time
}

// debugHTTPClient wraps http.Client to provide detailed debug logging
type debugHTTPClient struct {
	client *http.Client
	mu     sync.Mutex
}

// HTTPDebugStats tracks various HTTP-related statistics
type HTTPDebugStats struct {
	mu            sync.RWMutex
	statusCounts  map[int]int          // Count of each status code
	errors        []string             // Detailed error messages
	errorsByType  map[string]int       // Count of each error type
	lastErrors    map[string]time.Time // Last occurrence of each error type
	rateLimits    int                  // Count of rate limit errors (429)
	totalRequests int                  // Total number of requests made
	latencies     []time.Duration      // All request latencies
	responses     []*responseData      // Store last N responses
	maxResponses  int                  // Maximum number of responses to keep
}

// Global instance of debug stats
var DebugStats = NewHTTPDebugStats()

// NewHTTPDebugStats creates a new HTTPDebugStats instance with initialized maps
func NewHTTPDebugStats() *HTTPDebugStats {
	return &HTTPDebugStats{
		statusCounts: make(map[int]int),
		errorsByType: make(map[string]int),
		lastErrors:   make(map[string]time.Time),
		maxResponses: 1000, // Keep last 1000 responses
		responses:    make([]*responseData, 0, 1000),
		latencies:    make([]time.Duration, 0, 1000),
	}
}

// InitializeRPCClient creates a properly configured RPC client
func InitializeRPCClient(url string) *rpc.Client {
	if !GlobalConfig.Debug {
		return rpc.New(url)
	}

	DebugLog("Creating debug HTTP client for RPC at %s", url)
	debugClient := &debugHTTPClient{
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 30 * time.Second,
		},
	}

	// Create RPC client with debug HTTP client
	rpcClient := rpc.New(url)

	// Use reflection to set the HTTP client
	clientVal := reflect.ValueOf(rpcClient).Elem()
	httpClientField := clientVal.FieldByName("httpClient")
	if httpClientField.IsValid() && httpClientField.CanSet() {
		httpClientField.Set(reflect.ValueOf(debugClient))
	}

	return rpcClient
}

// Do implements the HTTP client interface with detailed debug logging
func (d *debugHTTPClient) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	DebugStats.totalRequests++
	d.mu.Unlock()

	start := time.Now()

	// Clone and read the request body if it exists
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(reqBody))
	}

	// Execute the request
	resp, err := d.client.Do(req)
	duration := time.Since(start)

	// Handle connection errors
	if err != nil {
		errMsg := fmt.Sprintf("%.3fs - %s %s - Error: %s\nRequest Body: %s",
			duration.Seconds(),
			req.Method,
			req.URL.String(),
			err.Error(),
			string(reqBody),
		)
		DebugStats.recordError("CONNECTION", errMsg)
		return resp, err
	}

	// Create response data structure
	respData := &responseData{
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Latency:    duration,
		URL:        req.URL.String(),
		Method:     req.Method,
		ReqBody:    string(reqBody),
		Timestamp:  start,
	}

	// Read and restore the response body
	if resp.Body != nil {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		respData.Body = string(bodyBytes)
		resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}

	// Record response details
	DebugStats.recordResponse(respData)

	// Handle error status codes
	if resp.StatusCode >= 400 {
		errMsg := fmt.Sprintf("%.3fs - %s %s - %s\nHeaders: %v\nRequest Body: %s\nResponse Body: %s",
			duration.Seconds(),
			req.Method,
			req.URL.String(),
			resp.Status,
			respData.Headers,
			string(reqBody),
			respData.Body,
		)

		switch resp.StatusCode {
		case 429:
			DebugStats.mu.Lock()
			DebugStats.rateLimits++
			DebugStats.mu.Unlock()
			DebugStats.recordError("RATE_LIMIT", errMsg)
		case 403:
			DebugStats.recordError("FORBIDDEN", errMsg)
		case 404:
			DebugStats.recordError("NOT_FOUND", errMsg)
		case 500, 502, 503, 504:
			DebugStats.recordError("SERVER_ERROR", errMsg)
		default:
			DebugStats.recordError("HTTP_ERROR", errMsg)
		}
	}

	return resp, err
}

// recordResponse adds a response to the history with thread-safe access
func (s *HTTPDebugStats) recordResponse(resp *responseData) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update status counts
	s.statusCounts[resp.StatusCode]++

	// Record latency
	s.latencies = append(s.latencies, resp.Latency)

	// Maintain response history
	s.responses = append(s.responses, resp)
	if len(s.responses) > s.maxResponses {
		s.responses = s.responses[len(s.responses)-s.maxResponses:]
	}
}

// recordError logs an error with thread-safe access
func (s *HTTPDebugStats) recordError(errorType, details string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.errorsByType[errorType]++
	s.lastErrors[errorType] = time.Now()

	// Add timestamp to error message
	timestampedError := fmt.Sprintf("[%s] %s: %s",
		time.Now().Format("15:04:05.000"),
		errorType,
		details)

	s.errors = append(s.errors, timestampedError)

	// Only log error details in debug mode
	if GlobalConfig != nil && GlobalConfig.Debug {
		SimpleLogger.Printf("RPC Error: [%s] %s", errorType, details)
	}

	// Keep only last 1000 errors
	if len(s.errors) > 1000 {
		s.errors = s.errors[len(s.errors)-1000:]
	}
}

// PrintStats outputs comprehensive debug statistics
func (s *HTTPDebugStats) PrintStats() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	SimpleLogger.Print("\nHTTP Debug Statistics")
	SimpleLogger.Print(strings.Repeat("=", 50))

	SimpleLogger.Printf("Total Requests: %d", s.totalRequests)

	// Print latency statistics
	if len(s.latencies) > 0 {
		var total, max, min time.Duration
		min = s.latencies[0]
		for _, d := range s.latencies {
			total += d
			if d > max {
				max = d
			}
			if d < min {
				min = d
			}
		}
		avg := total / time.Duration(len(s.latencies))

		SimpleLogger.Print("\nLatency Statistics:")
		SimpleLogger.Printf("  Minimum: %.3fs", min.Seconds())
		SimpleLogger.Printf("  Maximum: %.3fs", max.Seconds())
		SimpleLogger.Printf("  Average: %.3fs", avg.Seconds())
	}

	// Print status code distribution
	SimpleLogger.Print("\nStatus Code Distribution:")
	for code, count := range s.statusCounts {
		percentage := float64(count) / float64(s.totalRequests) * 100
		SimpleLogger.Printf("  HTTP %d: %d (%.1f%%)", code, count, percentage)
	}

	// Print endpoint statistics
	if len(s.responses) > 0 {
		endpointStats := make(map[string]map[int]int)
		for _, resp := range s.responses {
			if _, exists := endpointStats[resp.URL]; !exists {
				endpointStats[resp.URL] = make(map[int]int)
			}
			endpointStats[resp.URL][resp.StatusCode]++
		}

		SimpleLogger.Print("\nEndpoint Statistics:")
		for url, stats := range endpointStats {
			SimpleLogger.Printf("  %s:", url)
			for code, count := range stats {
				percentage := float64(count) / float64(s.totalRequests) * 100
				SimpleLogger.Printf("    HTTP %d: %d (%.1f%%)", code, count, percentage)
			}
		}
	}

	// Print error distribution
	if len(s.errorsByType) > 0 {
		SimpleLogger.Print("\nError Type Distribution:")
		for errType, count := range s.errorsByType {
			lastTime := s.lastErrors[errType]
			SimpleLogger.Printf("  %s: %d (Last: %s)",
				errType,
				count,
				lastTime.Format("15:04:05.000"))
		}
	}

	// Print recent errors with full details
	if len(s.errors) > 0 {
		SimpleLogger.Print("\nRecent Errors (last 5):")
		start := len(s.errors) - 5
		if start < 0 {
			start = 0
		}
		for _, err := range s.errors[start:] {
			SimpleLogger.Printf("\n%s\n%s", strings.Repeat("-", 40), err)
		}
	}

	// Print rate limiting information if any occurred
	if s.rateLimits > 0 {
		SimpleLogger.Printf("\nRate Limit Hits: %d (%.1f%% of requests)",
			s.rateLimits,
			float64(s.rateLimits)/float64(s.totalRequests)*100)
	}
}
