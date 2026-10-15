# Local patch

Source: `charm.land/bubbletea/v2 v2.1.0`, MIT license in `LICENSE`.
Root `go.mod` uses this source copy; shared caches and research checkouts remain
unchanged. The library's production sources and tests are otherwise unchanged.

`tty.go: checkResize` no longer writes the startup `Program.width/height` fields
from worker goroutines. SIGWINCH, explicit size queries and `RestoreTerminal`
can run concurrently. They now only send `WindowSizeMsg`; the existing event
loop resizes the renderer and the app. Initial dimensions remain initialized
before handlers start.

Regression: `internal/render/TestRealTerminalHandoffTargetsOnePTYAndReplaysEvents`
under `-race -count=10`, including SIGWINCH during and after real tmux handoff.
Keep this replacement until an upstream version passes the same test.
