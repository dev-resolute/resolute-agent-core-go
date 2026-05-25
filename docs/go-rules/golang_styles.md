---
paths: "**/*.go"
---

## Go Style Guide

Rules derived from Google's Go Style Guide. Follow these when writing Go code.

---

### Formatting

- **Always run `gofmt`** - Non-negotiable. All Go code must be formatted with gofmt.
- **MixedCaps only** - Use `MixedCaps` or `mixedCaps`, never `snake_case` or `ALL_CAPS`.
- **No fixed line length** - Prefer refactoring over line breaks. Don't artificially wrap.

---

### Naming

#### Packages
- Lowercase, single word when possible
- No underscores or mixedCaps
- Short, clear, evocative (`bufio`, `httputil`)
- Avoid generic names (`util`, `common`, `base`)

#### Variables
- Length proportional to scope, inversely proportional to usage frequency
- Single-letter names acceptable for: loops, readers/writers, short scopes
- Descriptive names for package-level and longer-lived variables

#### Functions & Methods
- No `Get` prefix for getters: `Counts()` not `GetCounts()`
- Setters use `Set` prefix: `SetCount(n int)`
- Receivers: 1-2 letter abbreviation of type: `(c *Client)`, `(req *Request)`

#### Constants
- MixedCaps: `MaxPacketSize` not `MAX_PACKET_SIZE`

#### Initialisms
- Consistent casing: `URL` or `url`, never `Url`
- `ID`, `HTTP`, `API`, `JSON`, `XML`, `HTML`

---

### Avoid Repetition

```go
// BAD
widget.NewWidget()
var numUsers int
func (c *Config) WriteConfigTo(w io.Writer)

// GOOD
widget.New()
var users int
func (c *Config) WriteTo(w io.Writer)
```

Context provides meaning - don't repeat package name, type, or obvious context in names.

---

### Comments & Documentation

#### Doc Comments
- Full sentences, starting with the name being declared
- Required for all exported names
- Explain "why", not "what"

```go
// Package math provides basic constants and mathematical functions.
package math

// Pi is the ratio of a circle's circumference to its diameter.
const Pi = 3.14159

// Add returns the sum of a and b.
func Add(a, b int) int { return a + b }
```

#### Comment Style
- Aim for ~80 character lines (not strict)
- Break at sentence/clause boundaries
- No trailing punctuation on single-line comments describing obvious behavior

---

### Imports

#### Grouping (in order)
1. Standard library
2. Third-party packages
3. Project packages
4. Protocol buffers (if applicable)

```go
import (
    "context"
    "fmt"

    "github.com/some/thirdparty"

    "myproject/internal/pkg"
)
```

#### Import Renaming
- Avoid unless necessary for collision
- If renaming proto packages: remove underscores, add `pb` suffix

---

### Error Handling

#### Return Pattern
- `error` is always the last return value
- Handle errors immediately, don't defer

```go
// GOOD - handle error immediately
result, err := doSomething()
if err != nil {
    return fmt.Errorf("failed to do something: %w", err)
}
// continue with result

// BAD - error handling in else
result, err := doSomething()
if err == nil {
    // happy path
} else {
    return err
}
```

#### Error Strings
- Not capitalized (unless proper noun)
- No ending punctuation
- Lowercase, no trailing period

```go
// GOOD
return errors.New("connection refused")
return fmt.Errorf("failed to load config: %w", err)

// BAD
return errors.New("Connection refused.")
return fmt.Errorf("Failed to load config: %w", err)
```

#### Wrapping Errors
- `%w` when callers need programmatic access to underlying error
- `%v` at system boundaries or when underlying error is implementation detail

#### No In-Band Errors
```go
// BAD
func Lookup(key string) string // returns "" on not found

// GOOD
func Lookup(key string) (string, error)
func Lookup(key string) (string, bool)
```

---

### Nil Slices

```go
// Prefer
var s []int

// Over
s := []string{}
s := make([]string, 0)
```

- Use `len(s) == 0` to check emptiness, not `s == nil`
- `nil` slices are valid and have length 0

---

### Interfaces

- Define interfaces in the **consuming** package, not the implementing package
- Accept interfaces, return concrete types
- Don't define interfaces before they're used
- Prefer small interfaces (1-3 methods)

```go
// GOOD - interface defined where it's used
package consumer

type Reader interface {
    Read(p []byte) (n int, err error)
}

func Process(r Reader) error { ... }
```

---

### Receiver Types

**Use pointer receiver when:**
- Method mutates the receiver
- Receiver contains sync.Mutex or similar
- Receiver is a large struct
- Consistency with other methods on the type

**Use value receiver when:**
- Receiver is a small, immutable struct
- Receiver is a map, func, or chan
- Receiver is a basic type (int, string)
- No mutation needed

---

### Goroutines

- Make goroutine lifetime crystal clear
- Use `context.Context` for cancellation
- Document ownership of cleanup
- Prevent leaks - every started goroutine must have a clear exit path

```go
// GOOD - clear ownership and cancellation
func (s *Server) Run(ctx context.Context) error {
    go func() {
        <-ctx.Done()
        s.shutdown()
    }()
    return s.serve()
}
```

---

### Context

- Always first parameter: `func DoSomething(ctx context.Context, ...)`
- Never store in structs
- Only `context.Background()` in main/init/tests
- Never create custom context types

---

### Panic vs Error

**Use `error` for:**
- Normal error conditions
- Invalid input
- Network failures
- File not found

**Use `panic` only for:**
- Truly unrecoverable conditions
- Programmer error (API misuse) that should never happen
- Internal implementation detail with corresponding recover

```go
// GOOD - use error
func ParseConfig(path string) (*Config, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("reading config: %w", err)
    }
    // ...
}

// Acceptable panic - programmer error
func MustCompileRegex(pattern string) *regexp.Regexp {
    r, err := regexp.Compile(pattern)
    if err != nil {
        panic(fmt.Sprintf("invalid regex %q: %v", pattern, err))
    }
    return r
}
```

---

### Testing

#### Test Failure Messages
Include: what failed, inputs used, actual result, expected result.

```go
// GOOD
t.Errorf("ParseInt(%q) = %d, want %d", input, got, want)

// BAD
t.Errorf("wrong result")
```

#### Table-Driven Tests
```go
func TestAdd(t *testing.T) {
    tests := []struct {
        name string
        a, b int
        want int
    }{
        {name: "zeros", a: 0, b: 0, want: 0},
        {name: "positive", a: 2, b: 3, want: 5},
        {name: "negative", a: -1, b: 1, want: 0},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := Add(tt.a, tt.b)
            if got != tt.want {
                t.Errorf("Add(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
            }
        })
    }
}
```

#### Test Helpers
```go
func setupTestDB(t *testing.T) *DB {
    t.Helper() // marks this as helper - errors report caller's line
    db, err := NewDB(":memory:")
    if err != nil {
        t.Fatalf("setupTestDB: %v", err)
    }
    t.Cleanup(func() { db.Close() })
    return db
}
```

- Use `t.Helper()` in test helpers
- Use `t.Cleanup()` for teardown
- Don't call `t.Fatal` from other goroutines

---

### Flags

- Flag names: `snake_case` (`--poll_interval`)
- Go variables: `camelCase` (`var pollInterval`)
- Define flags only in `package main`

---

### Global State

**Avoid:**
- Mutable global variables
- `init()` functions that modify shared state
- Service locators / registries

**Instead:**
- Pass dependencies explicitly
- Create instances at startup and inject them
- Use functional options for configuration

---

### Struct Literals

```go
// Use field names for clarity
server := &Server{
    Addr:    ":8080",
    Handler: mux,
    Timeout: 30 * time.Second,
}

// Omit zero-value fields
config := Config{
    Debug: true,
    // Port: 0,  -- omit, zero value
}
```

---

### Prefer Synchronous Functions

```go
// GOOD - synchronous, simple
func Fetch(ctx context.Context, url string) ([]byte, error) {
    // ...
}

// AVOID - unnecessary channel complexity
func Fetch(ctx context.Context, url string) <-chan Result {
    // ...
}
```

Keep goroutine management localized. Let callers decide on concurrency.

---

### Generics

**Use when:**
- Multiple types share identical behavior
- Operating on slices/maps of any element type
- Writing data structures (trees, queues)

**Don't use:**
- Just to avoid writing similar code twice
- To create mini-DSLs
- When interfaces suffice

---

### Crypto

- **Never** use `math/rand` for security-sensitive operations
- **Always** use `crypto/rand` for keys, tokens, secrets

```go
import "crypto/rand"

func generateToken() ([]byte, error) {
    token := make([]byte, 32)
    _, err := rand.Read(token)
    return token, err
}
```

---

### Quick Reference

| Topic | Rule |
|-------|------|
| Formatting | Always `gofmt` |
| Naming | MixedCaps, no snake_case |
| Getters | No `Get` prefix |
| Errors | Lowercase, no punctuation, last return value |
| Empty check | `len(s) == 0`, not `s == nil` |
| Nil slice | `var s []int` over `make([]int, 0)` |
| Context | First param, never in structs |
| Panic | Only for programmer errors |
| Tests | Table-driven, descriptive failures |
| Globals | Avoid mutable state |
