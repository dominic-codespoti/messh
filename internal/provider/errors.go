package provider

import (
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrorFrom preserves an operation's stable error code for agent consumers.
// Older providers without a code retain their ordinary readable tool error.
func ErrorFrom(err error, prefix string) *mcp.CallToolResult {
	message := fmt.Sprintf("%s: %v", prefix, err)
	result := ErrorResult("%s", message)
	var coded interface {
		error
		ErrorCode() string
	}
	if errors.As(err, &coded) {
		result.StructuredContent = map[string]any{"code": coded.ErrorCode(), "error": message}
	}
	return result
}
