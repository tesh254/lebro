# Durable tool approval

This example pauses a model-requested transfer before its handler starts,
prints the immutable reviewed arguments, then resumes the same run with a
human decision. The in-memory store and fixture model need no credentials.

```sh
go run ./examples/tool-approval
```

Production callers should render `RunResult.ToolApproval`, retain its `RunID`
and `RequestID`, and submit only a `ToolApprovalDecision`. The runtime loads
the persisted tool identity and arguments; a decision cannot replace them.
