// Package logger provides a structured logging solution using Zap.
// The formatter subpackage provides human-friendly output formatting for installation/bootstrap phases.
package logger

import (
	"fmt"
	"regexp"
	"strings"
)

// StatusIcon represents a log status with visual indicator
type StatusIcon string

const (
	IconSuccess StatusIcon = "✓"
	IconError   StatusIcon = "✗"
	IconWarning StatusIcon = "⚠"
	IconInfo    StatusIcon = "➜"
	IconPending StatusIcon = "~"
)

// Color ANSI color codes (can be disabled for non-TTY)
type Color string

const (
	ColorReset   Color = "\033[0m"
	ColorGreen   Color = "\033[32m"
	ColorRed     Color = "\033[31m"
	ColorYellow  Color = "\033[33m"
	ColorBlue    Color = "\033[34m"
	ColorCyan    Color = "\033[36m"
	ColorBold    Color = "\033[1m"
)

// IsTerminal checks if output is a terminal (TTY)
// If not, colors should be disabled
func IsTerminal() bool {
	// This will be set at runtime based on stdout
	return true // Default; should be replaced with actual tty check if needed
}

// Formatter provides human-friendly log formatting
type Formatter struct {
	useColor bool
}

// NewFormatter creates a new formatter instance
func NewFormatter(useColor bool) *Formatter {
	return &Formatter{
		useColor: useColor,
	}
}

// Success formats a success log message
func (f *Formatter) Success(message string, details ...string) string {
	return f.formatLog(IconSuccess, ColorGreen, message, details...)
}

// Error formats an error log message
func (f *Formatter) Error(message string, details ...string) string {
	return f.formatLog(IconError, ColorRed, message, details...)
}

// Warning formats a warning log message
func (f *Formatter) Warning(message string, details ...string) string {
	return f.formatLog(IconWarning, ColorYellow, message, details...)
}

// Info formats an info/header log message
func (f *Formatter) Info(message string, details ...string) string {
	return f.formatLog(IconInfo, ColorCyan, message, details...)
}

// Pending formats a pending/loading log message
func (f *Formatter) Pending(message string, details ...string) string {
	return f.formatLog(IconPending, ColorBlue, message, details...)
}

// formatLog is the internal formatter
func (f *Formatter) formatLog(icon StatusIcon, color Color, message string, details ...string) string {
	var sb strings.Builder

	if f.useColor {
		sb.WriteString(string(color))
		sb.WriteString(string(ColorBold))
	}

	sb.WriteString("[")
	sb.WriteString(string(icon))
	sb.WriteString("] ")
	sb.WriteString(message)

	if f.useColor {
		sb.WriteString(string(ColorReset))
	}

	// Add details on same line if provided
	if len(details) > 0 {
		sb.WriteString(" ")
		for i, detail := range details {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(detail)
		}
	}

	return sb.String()
}

// Header formats a section header
func (f *Formatter) Header(title string) string {
	var sb strings.Builder

	if f.useColor {
		sb.WriteString(string(ColorCyan))
		sb.WriteString(string(ColorBold))
	}

	sb.WriteString("\n")
	sb.WriteString(title)
	sb.WriteString("\n")

	if f.useColor {
		sb.WriteString(string(ColorReset))
	}

	return sb.String()
}

// Indent indents a message (useful for nested info)
func (f *Formatter) Indent(message string, level int) string {
	indent := strings.Repeat("  ", level)
	return indent + message
}

// SanitizeLogs removes or masks sensitive information from log messages
type Sanitizer struct {
	maskIPs bool
}

// NewSanitizer creates a new log sanitizer
func NewSanitizer(maskIPs bool) *Sanitizer {
	return &Sanitizer{
		maskIPs: maskIPs,
	}
}

// Sanitize removes sensitive data from a log message
func (s *Sanitizer) Sanitize(message string) string {
	result := message

	if s.maskIPs {
		result = s.maskIPAddresses(result)
	}

	result = s.maskAPIKeys(result)
	result = s.maskAuthTokens(result)
	result = s.maskMACAddresses(result)

	return result
}

// maskIPAddresses masks IP addresses as xxx.xxx.xxx.xxx
func (s *Sanitizer) maskIPAddresses(message string) string {
	// IPv4 pattern
	ipv4Pattern := regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b`)
	message = ipv4Pattern.ReplaceAllString(message, "***.***.***.**")

	// IPv6 pattern (simplified)
	ipv6Pattern := regexp.MustCompile(`([0-9a-fA-F]{0,4}:){2,7}([0-9a-fA-F]{0,4})`)
	message = ipv6Pattern.ReplaceAllString(message, "****:****:****:****")

	return message
}

// maskAPIKeys masks API keys and tokens
func (s *Sanitizer) maskAPIKeys(message string) string {
	// api-key=xxx pattern
	apiKeyPattern := regexp.MustCompile(`(?i)(api[_-]?key|x-api-key|authorization|token)[=:\s]+([^\s,;"]+)`)
	message = apiKeyPattern.ReplaceAllString(message, "$1=[REDACTED]")

	return message
}

// maskAuthTokens masks authentication tokens
func (s *Sanitizer) maskAuthTokens(message string) string {
	// bearer token pattern
	bearerPattern := regexp.MustCompile(`(?i)(bearer|token)\s+([^\s]+)`)
	message = bearerPattern.ReplaceAllString(message, "$1 [REDACTED]")

	// JWT pattern (eyJ...)
	jwtPattern := regexp.MustCompile(`(eyJ[A-Za-z0-9_-]+)`)
	message = jwtPattern.ReplaceAllString(message, "[JWT_REDACTED]")

	return message
}

// maskMACAddresses masks MAC addresses
func (s *Sanitizer) maskMACAddresses(message string) string {
	// MAC address pattern
	macPattern := regexp.MustCompile(`([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})`)
	message = macPattern.ReplaceAllString(message, "**:**:**:**:**:**")

	return message
}

// FormatInstallationStep formats an installation step message
func FormatInstallationStep(stepNum int, title string, details string) string {
	formatter := NewFormatter(true)
	msg := formatter.Success(title)
	if details != "" {
		msg += fmt.Sprintf(" (%s)", details)
	}
	return msg
}

// FormatSystemCheck formats a system check result
func FormatSystemCheck(cpuCores int, ramGB int, gpuModel string) string {
	formatter := NewFormatter(true)
	details := fmt.Sprintf("%d Cores, %dGB RAM", cpuCores, ramGB)
	if gpuModel != "" {
		details += fmt.Sprintf(", %s", gpuModel)
	}
	return formatter.Success("System check passed", details)
}

// FormatNodeOnline formats the final "node online" message
func FormatNodeOnline(nodeName string, nodeID string) string {
	formatter := NewFormatter(true)
	msg := fmt.Sprintf("Node \"%s\" is now ONLINE", nodeName)
	return formatter.Info(msg)
}
