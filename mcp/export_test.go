package mcp

import mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

// Length limits are exposed so tests exercise the boundary the package
// actually enforces rather than restating the numbers.
const (
	MaxToolNameLengthForTest   = maxToolNameLength
	MaxServerNameLengthForTest = maxServerNameLength
)

// InputRequiredMessageForTest exposes inputRequiredMessage to the external test
// package. Reaching it through Execute requires a server request the SDK can
// neither fulfill nor reject outright, which the in-memory harness cannot
// stage, so it is covered directly instead.
func InputRequiredMessageForTest(result *mcpsdk.CallToolResult) string {
	return inputRequiredMessage(result)
}
