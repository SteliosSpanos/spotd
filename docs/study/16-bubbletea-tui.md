# 16 — Terminal UI with Bubble Tea v2, Bubbles, Lip Gloss

> **Version note:** Bubble Tea, Bubbles and Lip Gloss v2 moved to vanity import paths:
> `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`.
> Many tutorials still show v1 (`github.com/charmbracelet/...`, `View() string`, `tea.KeyMsg`).
> Read the official v2 upgrade guides before starting.

## Concepts: The Elm Architecture

```
          ┌──────────── Msg ◄──────────────┐
          ▼                                │
   Update(model, msg) ──► (model', Cmd) ── Cmd runs in a goroutine, returns a Msg
          │
          ▼
       View(model) ──► rendered frame
```

- **Model**: all UI state, a plain value.
- **Update**: pure-ish function: handle a message, return new model + optional command. **Never block here.**
- **View**: render model to a `tea.View`. No side effects.
- **Cmd** (`func() tea.Msg`): where I/O happens (gRPC calls, timers). Bubble Tea runs it off the main loop and feeds the result back as a Msg.

## v2 essentials

```go
import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type model struct{ /* ... */ }

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg: // v1: tea.KeyMsg
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "space": // v1: " "
			return m, m.togglePlayCmd()
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	}
	return m, nil
}

func (m model) View() tea.View { // v1: string
	v := tea.NewView(m.render())
	v.AltScreen = true // v1: tea.WithAltScreen() program option
	return v
}

func main() {
	if _, err := tea.NewProgram(model{}).Run(); err != nil {
		log.Fatal(err)
	}
}
```

## Talking to the daemon: commands for unary calls

```go
type pausedMsg struct{}
type errMsg struct{ err error }

func pauseCmd(c pb.PlayerServiceClient) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.Pause(ctx, &pb.PauseRequest{}); err != nil {
			return errMsg{err}
		}
		return pausedMsg{}
	}
}
```

Fetching stats when the Stats tab opens works the same way: return a `Cmd` that calls `TopArtists` and returns `topArtistsMsg{rows}`.

## Consuming a gRPC stream

Pattern: a goroutine reads the stream into a channel; a `Cmd` waits for **one** message and re-arms itself after each one.

```go
type eventMsg struct{ ev *pb.WatchEventsResponse }
type streamClosedMsg struct{ err error }

func startStream(ctx context.Context, c pb.PlayerServiceClient) (<-chan tea.Msg, error) {
	stream, err := c.WatchEvents(ctx, &pb.WatchEventsRequest{})
	if err != nil {
		return nil, err
	}
	out := make(chan tea.Msg, 8)
	go func() {
		defer close(out)
		for {
			ev, err := stream.Recv()
			if err != nil {
				out <- streamClosedMsg{err}
				return
			}
			select {
			case out <- eventMsg{ev}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func waitFor(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return streamClosedMsg{}
		}
		return msg
	}
}

// in Update:
case eventMsg:
	m.nowPlaying = msg.ev.GetNowPlaying()
	return m, waitFor(m.events) // re-arm
case streamClosedMsg:
	m.status = "disconnected, retrying…"
	return m, tea.Tick(m.backoff.Next(), func(time.Time) tea.Msg { return reconnectMsg{} })
```

Alternative: pass `*tea.Program` to the goroutine and call `p.Send(msg)`. Simpler but couples your stream code to the program; the channel + `Cmd` pattern is easier to test.

Cancel the stream context when the program exits.

## Smooth progress bar without extra RPCs

Daemon sends `progress_ms` + `fetched_at` only on change. TUI ticks every second and interpolates:

```go
type tickMsg time.Time

func tick() tea.Cmd { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }

case tickMsg:
	return m, tick()

func (m model) progress() time.Duration {
	np := m.nowPlaying
	if np == nil { return 0 }
	p := time.Duration(np.ProgressMs) * time.Millisecond
	if np.IsPlaying {
		p += time.Since(np.FetchedAt.AsTime())
	}
	return min(p, time.Duration(np.DurationMs)*time.Millisecond)
}
```

## Tabs: composing sub-models

```go
type tab int

const (
	tabNow tab = iota
	tabStats
	tabPlaylists
)

type model struct {
	active    tab
	now       nowModel
	stats     statsModel
	playlists playlistsModel
	width, height int
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "tab":
			m.active = (m.active + 1) % 3
			return m, m.onEnterTab()
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	}
	// Route: background messages (stream events, job progress) go to their
	// owner regardless of active tab; keys go to the active tab only.
	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.now, cmd = m.now.Update(msg)
	cmds = append(cmds, cmd)
	m.stats, cmd = m.stats.Update(msg)
	cmds = append(cmds, cmd)
	m.playlists, cmd = m.playlists.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}
```

Sub-models return their concrete type (`func (n nowModel) Update(tea.Msg) (nowModel, tea.Cmd)`), like Bubbles components do.

## Bubbles components you'll use

| Component | Use in spotd |
|-----------|--------------|
| `table` | top artists / tracks |
| `list` | playlists picker (filtering built in) |
| `progress` | track progress, job progress |
| `spinner` | loading states |
| `help` + `key` | keybinding footer generated from `key.Binding`s |
| `viewport` | scrollable job log |

Define key bindings once with `key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "play/pause"))` and match with `key.Matches(msg, m.keys.Play)`. `help` renders them automatically.

## Lip Gloss styling

```go
var (
	accent    = lipgloss.Color("#1DB954")
	tabStyle  = lipgloss.NewStyle().Padding(0, 2)
	activeTab = tabStyle.Foreground(accent).Bold(true).Underline(true)
	box       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2)
)

header := lipgloss.JoinHorizontal(lipgloss.Top, renderTabs(m.active)...)
body := box.Width(m.width - 4).Render(content)
return lipgloss.JoinVertical(lipgloss.Left, header, body, m.help.View(m.keys))
```

- Recompute layout from `tea.WindowSizeMsg`; never hard-code terminal size.
- Use `lipgloss.Width()` (cell width) not `len()` for alignment — emoji/CJK in track names are double width.
- Keep styles in one `styles.go`.

## "Listening by day" chart

A simple horizontal bar chart in text is enough:

```go
bar := strings.Repeat("█", int(float64(maxWidth)*float64(v)/float64(maxV)))
```

`github.com/NimbleMarkets/ntcharts` offers sparklines/bar charts for Bubble Tea if you want more (check v2 compatibility).

## Testing TUIs

- Unit-test `Update`: feed messages, assert on the returned model and that the right `Cmd` type comes back (call the cmd, inspect the Msg).
- Golden-file test `View()` output for fixed sizes.
- `teatest` (in `github.com/charmbracelet/x/exp/teatest`, check for the v2 variant) drives a full program and snapshots the final output.
- Fake the gRPC clients with small interfaces (`type PlayerClient interface{ Pause(ctx, *pb.PauseRequest, ...grpc.CallOption) (*pb.PauseResponse, error) }`) — generated client interfaces already exist.

## Pitfalls

- Blocking in `Update` (calling gRPC directly) → frozen UI. Always use `Cmd`s.
- Mutating the model from a goroutine → data race. Only `Update` changes state.
- Forgetting to re-arm `waitFor` → you get exactly one event.
- Logging to stdout → corrupted screen. Use `tea.LogToFile` or a file `slog` handler.
- Mixing v1 and v2 imports → type mismatches that are confusing to debug.

## Further reading

- https://github.com/charmbracelet/bubbletea (README, examples/, UPGRADE_GUIDE_V2.md)
- https://github.com/charmbracelet/bubbles (UPGRADE_GUIDE_V2.md)
- https://github.com/charmbracelet/lipgloss
- https://guide.elm-lang.org/architecture/
