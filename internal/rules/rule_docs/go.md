#### Correctness and Boundary Conditions
- Check nil handling, zero values, empty slices and maps, integer conversions, index boundaries, and partial-result behavior
- Verify range-loop variable capture, pointer aliasing, slice capacity reuse, and map mutation do not produce unintended shared state
- Confirm type assertions, channel receives, map lookups, and parsing operations handle their boolean or error results when failure is possible
- Report a defect only when the changed code and its reachable call path provide concrete evidence; do not report formatting or preference-only issues

#### Error Handling
- Errors must not be silently discarded, replaced by misleading success values, or logged and then incorrectly treated as handled
- Wrapped errors should preserve the cause with `%w` when callers may need `errors.Is` or `errors.As`
- Cleanup errors should be handled when they can affect correctness, durability, or resource state
- Panic and `log.Fatal` should not be used for ordinary recoverable failures in libraries, servers, workers, or request paths

#### Context and Cancellation
- Pass `context.Context` through blocking I/O and request boundaries; do not store it in long-lived structs without a justified lifecycle
- Goroutines, retries, polling loops, and channel operations must have bounded termination and observe cancellation where required
- Do not replace a caller context with `context.Background()` when doing so loses deadlines, cancellation, tracing, or request metadata
- Ensure timeout and cancel functions are invoked on every path and are not deferred inside unbounded loops

#### Concurrency
- Identify unsynchronized concurrent access to maps, slices, counters, caches, and mutable struct fields
- Check for goroutine leaks, send-on-closed-channel panics, double close, blocked unbuffered channels, WaitGroup misuse, and lock-order deadlocks
- Do not flag local variables or immutable data as races without evidence that the value escapes to concurrent callers
- Verify locks are not held across slow I/O, callbacks, or channel operations unless the invariant requires it

#### Resource and Lifecycle Management
- Response bodies, files, database rows, transactions, timers, tickers, and other owned resources must be closed, stopped, committed, or rolled back on all relevant paths
- Check defer placement in loops and long-lived functions because deferred cleanup may retain resources longer than intended
- Verify ownership when returning slices, byte buffers, pooled objects, readers, or references backed by mutable storage
- Background workers must have an explicit shutdown path and must not outlive the service lifecycle unintentionally

#### API and Data Semantics
- Preserve distinctions between nil and empty values when serialization, patch semantics, database writes, or public APIs depend on them
- Check JSON and YAML tags, exported field changes, interface implementations, and method receiver choices for compatibility impact
- Avoid returning internal mutable collections when callers can mutate protected state; copy only when ownership requires it
- Validate filesystem paths, URLs, commands, SQL inputs, credentials, and authorization decisions at trust boundaries

#### Performance and Tests
- Flag allocations, reflection, serialization, database calls, or network requests inside hot loops only when the surrounding path indicates material impact
- Check for N+1 queries, unbounded reads, missing pagination, repeated regexp compilation, and unnecessary whole-buffer copies
- New concurrency, retry, parsing, and boundary logic should have focused tests covering failure and cancellation paths
- Do not request tests for trivial wiring unless the change introduces a meaningful regression risk
