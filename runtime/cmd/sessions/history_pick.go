package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
)

// The picker closes the distance between finding a conversation and being back
// inside it. `sessions history` already prints, against every row, the exact
// command that reopens that row; until now the user retyped or copied it. This
// is that same command, run for them.
//
// It is deliberately not a full-screen TUI. Nothing else in this CLI takes over
// the screen, none of the alternate-screen machinery is in go.mod, and the one
// thing a TUI would buy here — reading a candidate and then going back to the
// list — is a numbered list, a prompt, and a loop. So that is what this is: the
// rows the command already printed, numbered, and a line-oriented prompt over
// them. It composes with a scrollback buffer instead of erasing it.
const (
	// A pick preview is a deliberate look at one candidate rather than a glance
	// across a page of them, so it reaches further back than --preview's
	// default. It is still a tail, not a transcript: `sessions cat` prints the
	// whole conversation and this must not grow into it.
	historyPickPreviewCount = 10
	// Retyped nonsense is cheap to answer, but an endless stream of it from a
	// mis-piped stdin is not. The loop gives up after this many unusable lines
	// rather than spinning forever against something that will never answer.
	historyPickMaxConfusion = 20
)

// conversationPicker holds the one browse being picked from. Previews read in
// the loop are cached here rather than written back onto the rows, so that
// redrawing the list reprints the list the user first saw instead of a list
// that has silently grown the paragraphs they looked at.
type conversationPicker struct {
	app     *app
	rows    []conversationRow
	targets []fleetTarget
	redraw  func() error

	previews      map[int][]conversationPreviewMessage
	previewErrors map[int]string
}

// pickConversation runs the selection loop over a browse that has already been
// printed. It returns the error of whatever the chosen row's own command
// returned, or nil when nothing was chosen — quitting a picker is not a
// failure.
func (a *app) pickConversation(rows []conversationRow, targets []fleetTarget, redraw func() error) error {
	if len(rows) == 0 {
		return nil
	}
	picker := &conversationPicker{
		app: a, rows: rows, targets: targets, redraw: redraw,
		previews:      make(map[int][]conversationPreviewMessage, 4),
		previewErrors: make(map[int]string, 4),
	}
	return picker.run()
}

func (p *conversationPicker) run() error {
	// One reader for the whole loop: a fresh bufio.Reader per prompt would
	// discard whatever it had already buffered, which silently eats the second
	// line of any scripted or pasted input.
	reader := bufio.NewReader(p.app.stdin)
	confused := 0
	for {
		fmt.Fprintf(p.app.stderr,
			"Reopen which conversation? 1-%d · p N to preview · l to list again · q to quit: ", len(p.rows))
		line, err := reader.ReadString('\n')
		if line == "" {
			// Nothing left to read. A caller whose input ran out chose nothing,
			// which is the same outcome as quitting.
			fmt.Fprintln(p.app.stderr)
			if err != nil && err != io.EOF {
				return fail(2, "read selection: %s", err)
			}
			return nil
		}
		choice := strings.TrimSpace(line)
		switch strings.ToLower(choice) {
		case "", "q", "quit", "exit":
			return nil
		case "l", "list":
			if _, err := fmt.Fprintln(p.app.stdout); err != nil {
				return err
			}
			if err := p.redraw(); err != nil {
				return err
			}
			continue
		}
		if rest, previewing := pickPreviewRequest(choice); previewing {
			index, ok := p.resolveIndex(rest)
			if !ok {
				confused++
				if confused >= historyPickMaxConfusion {
					return fail(1, "no conversation was selected")
				}
				continue
			}
			confused = 0
			if err := p.showPreview(index); err != nil {
				return err
			}
			continue
		}
		index, ok := p.resolveIndex(choice)
		if !ok {
			confused++
			if confused >= historyPickMaxConfusion {
				return fail(1, "no conversation was selected")
			}
			continue
		}
		confused = 0
		row := p.rows[index]
		// Eligibility is not re-derived here. conversationRecovery already
		// decided what this row can do and the printed command is its answer, so
		// a row it refused to give a command to is a row the picker refuses to
		// act on — with that row's own reason, rather than by running something
		// the daemon would reject.
		if len(row.Resume) == 0 {
			fmt.Fprintf(p.app.stderr, "sessions: %s cannot be reopened: %s\n",
				conversationLabel(row), row.Reason)
			continue
		}
		confirmed, err := p.confirm(reader, row)
		if err != nil {
			return err
		}
		if !confirmed {
			continue
		}
		return p.app.runConversationCommand(row.Resume)
	}
}

// confirm asks before acting. Reopening is not destructive, but it starts a
// provider process and hands the terminal to it, and the only thing standing
// between "row 4" and "row 14" is one keystroke. The confirmation is where the
// name and the exact command are shown together, so a mistyped number is caught
// while it is still free to catch. It is one keypress, and it is the only
// moment in this loop that has any consequence at all.
func (p *conversationPicker) confirm(reader *bufio.Reader, row conversationRow) (bool, error) {
	verb := "Resume"
	if row.Status == historyStatusLive {
		// A live conversation attaches; resume would be refused by the daemon.
		verb = "Attach to"
	}
	fmt.Fprintf(p.app.stderr, "%s %s?\n  %s\n[y/N] ", verb, conversationLabel(row), shellRecipe(row.Resume))
	line, err := reader.ReadString('\n')
	if line == "" {
		fmt.Fprintln(p.app.stderr)
		if err != nil && err != io.EOF {
			return false, fail(2, "read confirmation: %s", err)
		}
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// showPreview reads the tail of one candidate and prints it, then returns to the
// prompt. This is the reason a picker is worth having over a printed command:
// neither provider lets you look inside a conversation before committing to it.
// It reuses --preview's reader, so previewing here is the same read it is
// there — no session is created, nothing is marked.
func (p *conversationPicker) showPreview(index int) error {
	row := p.rows[index]
	label := fmt.Sprintf("%d. %s", index+1, conversationName(row))
	if len(row.Resume) == 0 {
		fmt.Fprintf(p.app.stderr, "sessions: no preview for %s: %s\n", label, row.Reason)
		return nil
	}
	messages, cached := p.previews[index]
	failure, failed := p.previewErrors[index]
	if !cached && !failed {
		read, err := readConversationTail(p.targets[row.target], row.ID, historyPickPreviewCount)
		if err != nil {
			failure, failed = err.Error(), true
			p.previewErrors[index] = failure
		} else {
			messages, cached = read, true
			p.previews[index] = read
		}
	}
	if failed {
		fmt.Fprintf(p.app.stderr, "sessions: preview unavailable for %s: %s\n", label, failure)
		return nil
	}
	if _, err := fmt.Fprintf(p.app.stdout, "\n%s\n  %s\n", label, p.app.conversationMetaLine(row)); err != nil {
		return err
	}
	if len(messages) == 0 {
		if _, err := fmt.Fprintf(p.app.stdout, "  (no user or assistant messages to show)\n"); err != nil {
			return err
		}
	}
	for _, message := range messages {
		if _, err := fmt.Fprintf(p.app.stdout, "  %-10s %s\n",
			message.Role, truncateRunes(message.Text, historySnippetRunes)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(p.app.stdout, "  %s\n\n", shellRecipe(row.Resume))
	return err
}

func (p *conversationPicker) resolveIndex(value string) (int, bool) {
	value = strings.TrimSpace(value)
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 || number > len(p.rows) {
		fmt.Fprintf(p.app.stderr,
			"sessions: %q is not one of 1-%d; type a row number, p N to preview it, l to list again, or q to quit\n",
			value, len(p.rows))
		return 0, false
	}
	return number - 1, true
}

// pickPreviewRequest recognises the ways someone asks to look before leaping.
func pickPreviewRequest(choice string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(choice))
	if rest, ok := strings.CutPrefix(lower, "preview"); ok {
		return strings.TrimSpace(rest), true
	}
	if rest, ok := strings.CutPrefix(lower, "p"); ok {
		return strings.TrimSpace(rest), true
	}
	return "", false
}

// runConversationCommand runs the argv a row printed, in this process, through
// the same command table `sessions` itself dispatches through. Deriving the
// action from the row's own printed command rather than from the row's status
// is the point: the picker cannot drift from what the browse advertised,
// because there is only one thing to drift from.
func (a *app) runConversationCommand(argv []string) error {
	if len(argv) < 2 || argv[0] != "sessions" {
		return fail(2, "this conversation's command could not be run: %s", shellRecipe(argv))
	}
	// Resolved against this app's own command table rather than the package
	// one: reaching for the global here would make the command table's
	// initializer depend on the picker that the table itself points at.
	for _, command := range a.commands {
		if command.name != argv[1] {
			continue
		}
		fmt.Fprintf(a.stderr, "sessions: %s\n", shellRecipe(argv))
		return command.run(a, append([]string(nil), argv[2:]...))
	}
	return fail(2, "this build has no `sessions %s` command", argv[1])
}

func conversationName(row conversationRow) string {
	if name := strings.TrimSpace(row.Name); name != "" {
		return name
	}
	return "(unnamed)"
}

func conversationLabel(row conversationRow) string {
	return conversationName(row) + " (" + row.Reference + ")"
}

// attachedToTerminal reports whether a person is on both ends of this
// invocation. It governs advice only — never behaviour — so that what the
// command does is decided by its arguments and nothing else.
func (a *app) attachedToTerminal() bool {
	input, ok := a.stdin.(*os.File)
	if !ok || !term.IsTerminal(input.Fd()) {
		return false
	}
	output, ok := terminalFile(a.stdout)
	return ok && term.IsTerminal(output.Fd())
}

func terminalFile(writer io.Writer) (*os.File, bool) {
	switch typed := writer.(type) {
	case *os.File:
		return typed, true
	case *countingWriter:
		return terminalFile(typed.inner)
	default:
		return nil, false
	}
}
