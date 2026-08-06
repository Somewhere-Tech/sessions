package main

import (
	"fmt"
	"io"
	"strings"
)

type commandSpec struct {
	name      string
	usage     string
	summary   string
	longHelp  string
	examples  []string
	group     string
	aliases   []string
	localJSON bool
	run       func(*app, []string) error
}

const (
	dailyCommandGroup = "Daily workflows"
	modelCommandGroup = "Models and interactive"
	adminCommandGroup = "Admin/operational"
)

// commandTable is the single source of truth for command discovery, dispatch,
// top-level help, and per-command help. Keep the daily path first.
var commandTable = []commandSpec{
	{
		name: "new", usage: "new [--tool claude|codex|shell] [--permissions inherit|constrained|full] [--lifecycle task|session] [--profile NAME] [--cwd P] [--name L] [--model M] [--effort LEVEL] [--fast] [--structured] [--wait-ready] [--on-idle CMD] [--owner ID [--detach]] [--force] [--worktree [--base REF]] [--cmd PATH] [options] [args...]",
		summary: "create an interactive session", group: dailyCommandGroup, localJSON: true,
		longHelp: "Create a session. --tool selects Claude, Codex, or shell. For Claude and Codex, positional text after the options is sent immediately as the first request. Agent-created children inherit their manager's resolved permission policy by default; when the user has explicitly enabled autonomous delegated work, those children use full access instead. A child cannot escalate itself. --permissions makes the policy explicit, while --full-access remains an alias for --permissions full. Agent-created children default to the task lifecycle and close after a successful final response; --lifecycle session or --keep-alive creates a long-lived manager. User-created sessions remain long-lived. Approval questions become needs-input state and are never blindly accepted. Existing sessions keep their original runtime and permission mode. Claude defaults to its native interactive runtime; Codex defaults to its sandboxed terminal mode unless full access selects app-server. Remote Control remains separately consent-gated. --profile selects a private provider login. --description and repeated --tag values record purpose and dimensions. --worktree creates a Sessions-owned worktree, and --base picks its starting ref.\n\nAgent controls: --model chooses the provider model for the new session and --effort its reasoning effort; both are validated by the provider and are only valid for Claude or Codex. --fast requests the Codex priority service tier and is refused for Claude, which has no service tier. Explicit provider arguments you pass yourself always win over these controls.\n\nRuntime and lifecycle options: --structured creates a Rich structured Claude session instead of the interactive terminal, --pty-claude keeps the terminal explicitly, and --codex-appserver or --pty-codex select the Codex runtime; app-server currently requires full access. --wait-ready holds the create call until the new agent runtime has produced its first structured event or a short settle timeout expires, so an immediately following send is not lost. --on-idle registers a shell command the daemon runs in the session's working directory every time the session becomes idle. --force overrides the live or moved conversation guard. --no-skip-perms is an accepted no-op kept for scripts written before constrained execution became the default, and it cannot be combined with full access. --cmd runs an explicit executable instead of a tool preset.\n\nOwnership: --owner records an external principal as the creator instead of the inherited Sessions ancestry, and --detach is required with it when this process already belongs to a session, creating an external root rather than a child.",
		examples: []string{"sessions new --tool claude --cwd ~/work", "sessions new --tool codex --permissions inherit --name focused-worker", "sessions new --tool codex --permissions full --lifecycle task 'Review this repository'", "sessions new --tool claude --keep-alive --name manager", "sessions new --cmd /bin/zsh"},
		run:      (*app).cmdNew,
	},
	{
		name: "profiles", usage: "profiles",
		summary: "list Claude and Codex login profiles", group: dailyCommandGroup, localJSON: true,
		longHelp: "List profile names, private config paths, active sessions, and last-use times. Sessions never reads or copies credentials and has no profile delete command; remove a profile manually only after reviewing the printed path.",
		examples: []string{"sessions profiles", "sessions --json profiles"}, run: (*app).cmdProfiles,
	},
	{
		name: "onboarding", usage: "onboarding",
		summary: "inspect user consent and delegated access", group: dailyCommandGroup, localJSON: true,
		longHelp: "Show whether this machine has completed Sessions onboarding, whether Claude Remote Control is enabled, and whether agent-created task workers inherit or receive autonomous access. This command is deliberately read-only: an agent can explain the state but cannot grant user consent. Open Sessions.app to make or change either choice.",
		examples: []string{"sessions onboarding", "sessions --json onboarding"}, run: (*app).cmdOnboarding,
	},
	{
		name: "providers", usage: "providers [update claude|codex]",
		summary: "inspect or update agent CLIs", group: dailyCommandGroup, localJSON: true,
		longHelp: "Show the locally installed Claude Code and Codex versions using only local provider metadata. `providers update` explicitly invokes that provider's own updater; it may access the internet and change the provider executable. Existing sessions keep their running process, while new sessions use the updated binary.",
		examples: []string{"sessions providers", "sessions providers update codex", "sessions --json providers"}, run: (*app).cmdProviders,
	},
	{
		name: "run", usage: "run [--name N] [--description PURPOSE] [--tag KEY=VALUE ...] [--cwd D] [--worktree [--base REF]] [--spec FILE] [--owner ID [--detach]] [--wait [--output]] -- <cmd args...>",
		summary: "run a command in a headless lane", group: dailyCommandGroup, localJSON: true,
		longHelp: "Create a headless lane for the command following the first -- separator. --description (alias --desc) records why the lane exists. --worktree creates an isolated Sessions-owned worktree; it does not symlink node_modules. Every child argument after the separator is passed unchanged. Without --wait, print the lane id and return. --wait blocks for completion and propagates the child exit code; --output prints the captured output tail. --owner records an external principal as the creator instead of the inherited Sessions ancestry, and --detach is required with it when this process already belongs to a session, creating an external root rather than a child.",
		examples: []string{"sessions run -- make test", "sessions run --name lint --worktree --wait --output -- npm run lint", "sessions --json run --wait -- sh -c 'exit 3'"},
		run:      (*app).cmdRun,
	},
	{
		name: "tags", usage: "tags <session> [key=value ...] [--remove key ...] [--clear]",
		summary: "view or edit session tags", group: dailyCommandGroup, localJSON: true,
		longHelp: "With no edits, print a session's tags. key=value adds or replaces a tag, --remove deletes one key, and --clear removes all tags. Tags are durable daemon-owned dimensions used by usage reports and the Sessions dashboard.",
		examples: []string{"sessions tags 0123abcd", "sessions tags 0123abcd product=Sessions client=Acme", "sessions tags 0123abcd --remove client", "sessions --json tags 0123abcd"}, run: (*app).cmdTags,
	},
	{
		name: "rename", usage: "rename <session> <name>",
		summary: "rename a session everywhere in Sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "Set the durable Sessions title used by the app, CLI, Fleet, search, and later continuations. Claude /rename titles are imported when no Sessions title has been chosen. Sessions does not rewrite Claude or Codex private history files, so unsupported provider-native renames remain unchanged.",
		examples: []string{"sessions rename 0123abcd DB", "sessions --json rename 0123abcd 'Database migration'"}, run: (*app).cmdRename,
	},
	{
		name: "worktrees", usage: "worktrees [clean [--dry-run]]",
		summary: "list or safely clean Sessions-created worktrees", group: dailyCommandGroup, localJSON: true,
		longHelp: "List worktrees recorded in the Sessions ledger with dirty, merge, and session state. clean removes only worktrees whose session has exited, whose tree is clean, and whose branch is fully merged into its recorded base; every other worktree is skipped with a reason. --dry-run shows the plan without mutation. There is no force option, and killing a session never cleans its worktree automatically.",
		examples: []string{"sessions worktrees", "sessions --json worktrees", "sessions worktrees clean --dry-run", "sessions worktrees clean"}, run: (*app).cmdWorktrees,
	},
	{
		name: "gc", usage: "gc [--older-than DURATION] [--apply | --dry-run]",
		summary: "archive old closed records safely", group: dailyCommandGroup, localJSON: true,
		longHelp: "Preview or archive sessions and lanes that have been closed longer than the retention age (30d by default). The default is a dry run; --apply records an append-only archive fact. --dry-run only states that default explicitly and changes nothing; it cannot be combined with --apply. Live runners and ancestors with retained descendants are never archived. Recovery history, transcripts, and worktrees are preserved.",
		examples: []string{"sessions gc", "sessions gc --older-than 7d", "sessions gc --older-than 7d --dry-run", "sessions gc --older-than 30d --apply", "sessions --json gc"}, run: (*app).cmdGC,
	},
	{
		name: "archive", usage: "archive <session> [session...]",
		summary: "hide selected closed sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "Archive one or more explicitly selected closed Sessions records. Archive hides them from normal lists but preserves provider history, transcripts, recovery facts, and worktrees. Live sessions and ancestors with retained descendants are refused.",
		examples: []string{"sessions archive 0123abcd", "sessions archive 0123abcd 89abcdef", "sessions --json archive 0123abcd"}, run: (*app).cmdArchive,
	},
	{
		name: "aside", usage: "aside <session> [session...] [--clear]",
		summary: "set live sessions aside or bring them back", group: dailyCommandGroup, localJSON: true,
		longHelp: "Set aside removes selected live sessions from the default native working set without ending them, suppressing attention, changing notifications, or hiding them from the CLI. --clear brings them back. Ended records use archive instead.",
		examples: []string{"sessions aside 0123abcd", "sessions aside 0123abcd 89abcdef", "sessions aside 0123abcd --clear", "sessions --json aside 0123abcd"}, run: (*app).cmdAside,
	},
	{
		name: "ls", usage: "ls [--mine | --all] [-a | --include-exited] [--aside | --not-aside]",
		summary: "list sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "List agent sessions known to the daemon. --mine follows SESSIONS_OWNER_ID, then the SESSIONS_SESSION_ID descendant subtree, then the daemon OS user. The OS-user fallback is user-wide, not invocation-scoped. Exited sessions are hidden by default; -a and --include-exited include them. Set-aside sessions remain listed and marked; --aside selects only them, while --not-aside reproduces the native default working set.",
		examples: []string{"sessions ls", "sessions ls --mine", "sessions ls --aside", "sessions ls --not-aside", "sessions --json ls"}, run: (*app).cmdLSDispatch,
	},
	{
		name: "list", usage: "list [--mine | --owner ID | --all] [--include-closed]",
		summary: "list agent sessions and headless lanes", group: dailyCommandGroup, localJSON: true,
		longHelp: "List agent sessions and headless lanes together. --mine follows SESSIONS_OWNER_ID, then the SESSIONS_SESSION_ID descendant subtree, then the daemon OS user. The OS-user fallback is user-wide, not invocation-scoped. Closed records are hidden unless --include-closed is supplied.",
		examples: []string{"sessions", "sessions list --mine", "sessions list --mine --include-closed", "sessions list --owner team:mine"}, run: (*app).cmdSessions,
	},
	{
		name: "lanes", usage: "lanes [--all | --mine [--owner ID] | --subtree ID] [--direct] [--detach]",
		summary: "list headless lanes", group: dailyCommandGroup, localJSON: true,
		longHelp: "List retained headless lanes. --mine follows SESSIONS_OWNER_ID, then the SESSIONS_SESSION_ID descendant subtree, then the daemon OS user. The OS-user fallback is user-wide, not invocation-scoped. --subtree selects session ancestry; --direct limits ancestry to immediate children.",
		examples: []string{"sessions lanes", "sessions lanes --mine", "sessions lanes --subtree 0123abcd --direct"}, run: (*app).cmdLanes,
	},
	{
		name: "send", usage: "send <id> [--from SESSION] [--timeout D] [--no-wait] [--file PATH] [--] <text...>",
		summary: "send text and Enter to a session", group: dailyCommandGroup, localJSON: true,
		longHelp: "Send a message and Enter. Claude and Codex sessions wait for receipt confirmation by default; --no-wait uses fire-and-forget behavior and --file reads the message body from a UTF-8 file. --from records a durable, content-free source-lane attribution; agents running inside Sessions inherit their source lane automatically. An unrecognized option in front of the message is refused rather than typed into the session; put -- before a message that must begin with dashes.",
		examples: []string{"sessions send 0123abcd 'Run the focused tests.'", "sessions send 0123abcd --from 89abcdef 'Please review this result.'", "sessions send 0123abcd --file prompt.md", "sessions send 0123abcd -- --json is a flag, not output"}, run: (*app).cmdSend,
	},
	{
		name: "ask", usage: "ask <id> [--timeout D] [--idle D] [--wait-timeout D] <text...>",
		summary: "send, wait, and print the reply", group: dailyCommandGroup, localJSON: true,
		longHelp: "Send a confirmed message to a Claude or Codex session, wait for the reply to finish, and print the last assistant message.",
		examples: []string{"sessions ask 0123abcd 'Summarize the failing test.'", "sessions --json ask 0123abcd --wait-timeout 2m 'Report status.'"}, run: (*app).cmdAsk,
	},
	{
		name: "wait", usage: "wait <id> [<id>... --any] [--idle D] [--timeout D] [--summary] [condition]",
		summary: "wait for session idle or lane exit", group: dailyCommandGroup, localJSON: true,
		longHelp: "Wait for a session to become idle or a lane to exit. --summary reports which target changed and its last useful assistant/output summary. Lane waits propagate the lane exit code. Conditions include --until commit, --until-file-contains FILE STRING, and --until-idle-stable D.\n\nWaiting on a session always answers with the same JSON object: ok, reason, session, working, idleMs, and the optional idleReason, detail, and summary. reason is one of idle, needs-input, failed, gone, or timeout, and ok is true only when the caller can stop waiting and act — idle and needs-input. --summary adds prose; it never changes the shape.\n\nExit codes: 0 the condition was satisfied, 1 usage, 2 the daemon could not be reached, 3 timed out without observing the condition, 4 the target is gone or failed so waiting longer cannot help. A vanished target reports ok:false and exit 4; treat exit 0 alone as success only for commands that do not wait.",
		examples: []string{"sessions wait 0123abcd --timeout 2m --summary", "sessions wait lane-a lane-b --any --summary", "sessions wait 0123abcd --until commit --timeout 10m"}, run: (*app).cmdWaitDispatch,
	},
	{
		name: "last", usage: "last <id> [--role user|assistant] [-n N]",
		summary: "print recent conversation or lane output", group: dailyCommandGroup, localJSON: true,
		longHelp: "For sessions, print recent user and assistant messages from the event log. For completed lanes, print the captured output tail.",
		examples: []string{"sessions last 0123abcd", "sessions last 0123abcd --role assistant -n 1", "sessions --json last 0123abcd"}, run: (*app).cmdLastDispatch,
	},
	{
		name: "grep", usage: "grep [options] <query>",
		summary: "search every approved machine", group: dailyCommandGroup, localJSON: true,
		longHelp: "Search normalized Claude and Codex history across this machine and every machine approved in Sessions.app or with `sessions machines connect`. Familiar -i and -C N flags are accepted; matching is already case-insensitive. Results carry durable machine::history-id references, duplicate copies of the same provider message are collapsed, and an offline machine produces a partial-result warning instead of hiding reachable history. Use --machine before the command to scope one machine.",
		examples: []string{"sessions grep -i -C 3 'Google Ads'", "sessions grep --tool claude --role user bolo", "sessions --json grep 'release decision'"}, run: (*app).cmdGrep,
	},
	{
		name: "search", usage: "search <query> [--session ID[,ID...]] [--role user|assistant|tool] [--tool claude|codex|shell] [--name GLOB | --lane GLOB] [--cwd PATH] [--since DATE] [--until DATE] [--context N] [--timeline] [-n N] [--exact | --regex | --ranked] [--json]",
		summary: "search normalized session chat history", group: dailyCommandGroup, localJSON: true,
		longHelp: "Search chat history across every live and persisted session on this machine and every approved machine by default. Ranked token recall is the default: bare words are alternatives, quoted phrases stay exact, boolean AND/OR/NOT and near(a,b,N) are supported, and results include a stable content-derived message bookmark plus optional surrounding turns. --exact uses a case-insensitive contiguous substring; --regex uses a Go regular expression. Filter to real user requests, agent replies, or typed delegation/handoff/automation/status operations with --role; scope by sessions, lane-name glob, workspace, provider, and date. --lane is an accepted alias of --name; supplying both is refused. --timeline merges matching moments chronologically. Use global --machine or --host before the command to search only one daemon.",
		examples: []string{"sessions search 'drafts rollout' --role user --since 2026-07-23", "sessions search 'hello world' --role user --context 3", `sessions search 'near(draft,egress,8) OR "stable session"' --timeline`, "sessions search '{{first_name}}' --exact --session 0123abcd --json"}, run: (*app).cmdSearch,
	},
	{
		name: "usage", usage: "usage [daily|weekly|monthly|session|tag|provider|model] [--mode auto|calculate|display] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--provider claude|codex] [--dimension KEY] [--json]",
		summary: "report local Claude and Codex token usage", group: dailyCommandGroup, localJSON: true,
		longHelp: "Incrementally index the local Claude Code and Codex JSONL stores, then report token usage and estimated cost by day, week, month, session, provider, model, or one session-tag dimension. Reasoning tokens are reported separately but remain a subset of output tokens. auto uses a recorded cost when present and otherwise calculates with pinned ccusage pricing semantics; calculate always prices tokens; display shows recorded costs only. No usage data leaves the daemon.",
		examples: []string{"sessions usage", "sessions usage weekly --since 2026-07-01", "sessions usage session --mode calculate", "sessions usage tag --dimension product", "sessions usage model", "sessions --json usage monthly"}, run: (*app).cmdUsage,
	},
	{
		name: "status", usage: "status <id>",
		summary: "show a compact session status card", group: dailyCommandGroup, localJSON: true,
		longHelp: "Show session state, tool, working directory, git state, activity timestamps, and the latest explicit verdict when present.",
		examples: []string{"sessions status 0123abcd", "sessions --json status 0123abcd"}, run: (*app).cmdStatus,
	},
	{
		name: "kill", usage: "kill <id> [<id>...] [--reason <text>] [--force]",
		summary: "terminate sessions or lanes", group: dailyCommandGroup, localJSON: true,
		longHelp: "Resolve every id or unique prefix before requesting any termination. Sessions durably records whether the caller was another Sessions runtime, a paired device or external owner, or a local user client. --reason adds an optional literal human explanation; Sessions never invents one. Multi-target calls use one guarded daemon batch and share an operation id. More than three targets are refused unless --force is explicit.\n\nResults are reported per target from what the daemon confirmed, never assumed. Each target is killed, already-exited for a lane that had already finished, failed when the daemon refused or did not confirm it, or unconfirmed when the daemon accepted the request without saying which sessions ended. The command exits 1 when any target failed and 2 when any target could not be confirmed, so a partially refused batch is never reported as success. --json prints {\"items\":[{\"id\",\"status\",\"reason\"}],\"operation_id\"} on stdout with the same statuses, matching the per-target shape used by archive and aside.",
		examples: []string{"sessions kill 0123abcd", `sessions kill 0123abcd 89abcdef --reason "completed rollout batch"`, "sessions --json kill 0123abcd", "sessions kill --json 0123abcd 89abcdef"}, run: (*app).cmdKill,
	},
	{
		name: "recover", usage: "recover [--all | --reopen [--force]]",
		summary: "inspect or reopen recoverable sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "List actionable recovery recipes. --all also shows blocked and unresumable lost records with reasons. --reopen creates replacement sessions for eligible records; --force overrides the live or moved conversation guard.",
		examples: []string{"sessions recover", "sessions recover --all", "sessions recover --reopen", "sessions --json recover --reopen --force"}, run: (*app).cmdRecover,
	},
	{
		name: "recall", usage: "recall [<full-session-id> [--raw]]",
		summary: "inspect integration recall data", group: dailyCommandGroup, localJSON: true,
		longHelp: "Show the integration-backed recall view, optionally for one full session id. --raw prints the source payload.",
		examples: []string{"sessions recall", "sessions recall 00000000-0000-4000-8000-000000000001 --raw"}, run: (*app).cmdRecall,
	},
	{
		name: "source", usage: "source <[machine::]name-or-id> [--text | --raw]",
		summary: "locate or read a saved conversation", group: dailyCommandGroup, localJSON: true,
		longHelp: "Resolve a durable Sessions title, full id, id prefix, or machine::history-id across every approved machine. With no read flag, show the authoritative provider-owned JSONL path, provider, machine, workspace, size, and exact follow-up commands. --text streams the normalized user and agent conversation; --raw streams the untouched provider source. Ambiguous titles are never guessed. Sessions does not create or modify a transcript copy.",
		examples: []string{"sessions source PM", "sessions source db-final-review-sol --text", "sessions source 'mini::provider-history:claude:00000000-0000-4000-8000-000000000001' --raw", "sessions --json source PM"}, run: (*app).cmdSource,
	},
	{
		name: "snap", usage: "snap <id> [--raw]",
		summary: "print the current terminal buffer", group: dailyCommandGroup,
		longHelp: "Print the current terminal snapshot. The default cleans terminal control sequences; --raw preserves the daemon response.",
		examples: []string{"sessions snap 0123abcd", "sessions snap 0123abcd --raw"}, run: (*app).cmdSnap,
	},
	{
		name: "tail", usage: "tail <id> [-f | --follow] [-n N | --lines N]",
		summary: "print or follow recent terminal lines", group: dailyCommandGroup,
		longHelp: "Print the last N terminal lines, defaulting to 50. -n and --lines are the same option and take a positive integer. -f and --follow are the same option and keep streaming new output until interrupted.",
		examples: []string{"sessions tail 0123abcd", "sessions tail 0123abcd -n 200 -f", "sessions tail 0123abcd --lines 200 --follow"}, run: (*app).cmdTail,
	},
	{
		name: "cat", usage: "cat <session-id | machine::history-id>",
		summary: "print one durable conversation", group: dailyCommandGroup, localJSON: true,
		longHelp: "Print the complete normalized conversation identified by a live session id, a unique session-id prefix, or a fleet-search reference. An unqualified argument that resolves to a session on this daemon prints that session's transcript; otherwise it is treated as a history reference. The machine qualifier selects the approved per-device credential without putting a token in argv. The conversation is read from its source machine; Sessions does not create a second transcript copy merely for search.",
		examples: []string{"sessions cat 0123abcd", "sessions cat 'mini::provider-history:claude:00000000-0000-4000-8000-000000000001'", "sessions --json cat 'local::provider:codex:00000000-0000-4000-8000-000000000001'"}, run: (*app).cmdCat,
	},
	{
		name: "transcript", usage: "transcript <id>",
		summary: "print the full conversation transcript", group: dailyCommandGroup, localJSON: true,
		longHelp: "Print all user and assistant turns decoded from the session event log. Use the global --json flag for structured turns.",
		examples: []string{"sessions transcript 0123abcd", "sessions --json transcript 0123abcd"}, run: (*app).cmdTranscript,
	},
	{
		name: "input", usage: "input <id> [--from SESSION] [--timeout D] [--no-wait] [--file PATH] [--] <text...>",
		summary: "alias for send", group: dailyCommandGroup, localJSON: true,
		longHelp: "Send text and Enter using the same confirmation behavior, options, JSON result, and unknown-option refusal as sessions send.",
		examples: []string{"sessions input 0123abcd 'Continue.'", "sessions --json input 0123abcd 'Continue.'", "sessions input 0123abcd --json 'Continue.'"}, run: (*app).cmdSend,
	},
	{
		name: "keys", usage: "keys <id> <esc|up|down|left|right|^c|^d|enter|tab>",
		summary: "send a named key to a session", group: dailyCommandGroup,
		longHelp: "Translate a supported key name to terminal bytes and send it to the session.",
		examples: []string{"sessions keys 0123abcd esc", "sessions keys 0123abcd ^c"}, run: (*app).cmdKeys,
	},
	{
		name: "resize", usage: "resize <id> <cols> <rows>",
		summary: "resize a session PTY", group: dailyCommandGroup, localJSON: true,
		longHelp: "Resize the terminal associated with a session to the requested columns and rows.",
		examples: []string{"sessions resize 0123abcd 160 48"}, run: (*app).cmdResize,
	},
	{
		name: "verdict", usage: "verdict <id> | verdict emit <id> [JSON]",
		summary: "read or emit an explicit producer verdict", group: dailyCommandGroup, localJSON: true,
		longHelp: "Print the latest verdict for a session or lane. verdict emit appends a schemaVersion 1 verdict, reading JSON from the argument or standard input.",
		examples: []string{"sessions verdict 0123abcd", "sessions --json verdict emit 0123abcd '{\"schemaVersion\":1,\"verdict\":\"pass\",\"findings\":[]}'"}, run: (*app).cmdVerdict,
	},
	{
		name: "move", usage: "move <ended-session> (--machine NAME | --to ENDPOINT [--token T]) [--terminal] [--dry-run] [--allow-dirty]",
		summary: "continue an ended conversation on another machine", group: dailyCommandGroup, localJSON: true,
		longHelp: "Continue an ended Claude or Codex conversation on another Sessions machine while preserving the source history. Put a global --machine before move when the source is another approved computer; the --machine after the session selects the destination. The client reads each saved per-device credential from its private file and sends neither credential to the other daemon. Run --dry-run first to verify workspace and conversation identity. Claude continues in its native interactive runtime, with Remote Control determined by the destination machine's explicit onboarding/Settings choice. Codex continues in its Rich app-server runtime. The target refuses to overwrite different provider history and records both sides of the continuation link. --terminal explicitly selects the provider terminal and is retained for Codex compatibility. --to/--token remains a low-level endpoint escape hatch. --allow-dirty creates a Git checkpoint without deleting or changing the source worktree.",
		examples: []string{"sessions move 0123abcd --machine mini --dry-run", "sessions move 0123abcd --machine mini", "sessions --machine mini move 0123abcd --machine macbook --dry-run", "sessions --machine mini move 0123abcd --machine macbook", "sessions move 0123abcd --machine mini --terminal", "sessions move 0123abcd --to https://mini.tailnet.ts.net --dry-run"}, run: (*app).cmdMove,
	},
	{
		name: "adopt", usage: "adopt <path-or-uuid> [--force] [--source SESSION] [--repair LIVE-SUCCESSOR]",
		summary: "bind an existing conversation into Sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "Adopt an existing conversation path or UUID as a Sessions session. --source links an ended Sessions record. --force overrides the live or moved conversation guard. If the runtime starts but a post-launch ledger annotation fails, rerun the emitted --repair command: repair only completes missing records and never starts another runtime.",
		examples: []string{"sessions adopt 00000000-0000-4000-8000-000000000001", "sessions adopt ~/.claude/projects/example/session.jsonl --force", "sessions adopt 00000000-0000-4000-8000-000000000001 --repair 0123abcd --source 4567cdef"}, run: (*app).cmdAdopt,
	},
	{
		name: "resume", aliases: []string{"continue", "resurrect"}, usage: "resume <[machine::]name-or-id> [--with claude|codex] [--terminal [--remote-control] | --structured] [--force] [--source SESSION] [--repair LIVE-SUCCESSOR]",
		summary: "resume one saved conversation", group: dailyCommandGroup, localJSON: true,
		longHelp: "Resume a conversation by its durable Sessions title, full id, id prefix, or exact machine::history-id across the approved fleet. Sessions first recovers a missing Codex identity from the provider's session_meta, then uses the native provider resume. If the provider handle is truly gone but the authored transcript remains, Sessions creates one linked same-provider successor from that transcript instead of losing the conversation. `continue` and `resurrect` remain compatibility aliases. Claude resumes in its native interactive runtime by default; Codex resumes in its Rich app-server runtime. --with creates a linked copy in the other provider. --source links the ended Sessions runtime, and --repair only completes missing records for an already-live successor.",
		examples: []string{"sessions resume db-final-review-sol", "sessions resume PM", "sessions resume 'mini::provider-history:claude:00000000-0000-4000-8000-000000000001'", "sessions resume db-final-review-sol --with claude", "sessions --json resume provider:codex:00000000-0000-4000-8000-000000000001"}, run: (*app).cmdContinue,
	},
	{
		name: "fork", usage: "fork <live-session> [--with claude|codex] [--at MESSAGE_INDEX [--message-id ID]]",
		summary: "copy a live conversation without stopping it", group: dailyCommandGroup, localJSON: true,
		longHelp: "Create a new Rich conversation from a stable authored-history snapshot while the original session remains live and unchanged. Omit --with to fork into the same provider, or select Claude/Codex to open a copy in the other provider. --at forks at one non-negative authored-message index, copying that user or agent message and everything before it instead of the whole history; --message-id is only valid with --at and pins the expected message identity, so a conversation that moved on is refused instead of forked from the wrong point. Sessions copies user and assistant messages only; tool output, credentials, attachments, provider internals, and the source runtime are never modified. Wait for the current turn to finish before forking.",
		examples: []string{"sessions fork 0123abcd", "sessions fork 0123abcd --with codex", "sessions fork 0123abcd --at 42 --message-id a1b2c3", "sessions --json fork 0123abcd --with claude"}, run: (*app).cmdFork,
	},
	{
		name: "model", usage: "model <session> <model> [--effort LEVEL]",
		summary: "set the next-turn model for a Rich session", group: modelCommandGroup, localJSON: true,
		longHelp: "Set the model, and optionally effort, used by the next turn of an idle Rich Claude or Rich Codex session. The daemon validates the choice and updates the durable runner; Terminal sessions keep their provider's own model controls. Omitting --effort preserves the current effort.",
		examples: []string{"sessions model 0123abcd sonnet", "sessions model 0123abcd gpt-5.6-sol --effort high", "sessions --json model 0123abcd opus"}, run: (*app).cmdModel,
	},
	{
		name: "models", usage: "models",
		summary: "list the live Codex model catalog", group: modelCommandGroup, localJSON: true,
		longHelp: "Query the Codex app-server model catalog, including supported efforts and service tiers. Use the global --json flag for the full structured catalog.",
		examples: []string{"sessions models", "sessions --json models"}, run: (*app).cmdModels,
	},
	{
		name: "attach", usage: "attach <id>",
		summary: "attach a raw two-way terminal stream", group: modelCommandGroup,
		longHelp: "Attach the local terminal to a session. Press Ctrl+Q to detach without terminating the session.",
		examples: []string{"sessions attach 0123abcd"}, run: (*app).cmdAttach,
	},
	{
		name: "install", usage: "install",
		summary: "install and start the development daemon", group: adminCommandGroup,
		longHelp: "Register the development sessionsd macOS LaunchAgent and start it.",
		examples: []string{"sessions install"}, run: (*app).cmdInstall,
	},
	{
		name: "uninstall", usage: "uninstall",
		summary: "stop and remove the development daemon", group: adminCommandGroup,
		longHelp: "Stop and remove the development sessionsd macOS LaunchAgent.",
		examples: []string{"sessions uninstall"}, run: (*app).cmdUninstall,
	},
	{
		name: "deploy", usage: "deploy",
		summary: "explain the retired Node deploy path", group: adminCommandGroup,
		longHelp: "The mutating Node-daemon deploy path is retired. Sessions.app is the macOS release and update vehicle; this command exits without changing files, services, or sessions and points operators to the current release documentation.",
		examples: []string{"sessions deploy"}, run: (*app).cmdDeploy,
	},
	{
		name: "update", usage: "update [--check]",
		summary: "securely update Sessions.app", group: adminCommandGroup, localJSON: true,
		longHelp: "Check or install the latest macOS Sessions release. The updater accepts no URL or key overrides: it fetches only the public Somewhere release manifest, requires the pinned Minisign key, validates the exact immutable GitHub artifact path, and verifies the Developer ID and notarization before an atomic app swap. Only the Sessions UI is restarted; sessionsd and runners are never stopped.",
		examples: []string{"sessions update", "sessions update --check", "sessions --json update --check"}, run: (*app).cmdUpdate,
	},
	{
		name: "pair", usage: "pair [--name NAME]",
		summary: "pair a device on the same LAN", group: adminCommandGroup, localJSON: true,
		longHelp: "Mint a five-minute, single-use pairing ticket for the explicit same-network LAN listener. This is the fallback for devices without Tailscale: Sessions apps on the same tailnet discover each other and use Request access instead. The claiming device receives its own revocable token; the master daemon token is never embedded in the link.",
		examples: []string{"sessions pair", "sessions pair --name 'Uzair phone'", "sessions --json pair"}, run: (*app).cmdPair,
	},
	{
		name: "devices", usage: "devices [revoke <id-or-prefix>]",
		summary: "list or revoke paired devices", group: adminCommandGroup, localJSON: true,
		longHelp: "List per-device credentials by id prefix, name, creation time, and last use. Revoke resolves an exact id or unique prefix, reports the matched device, and invalidates its token immediately.",
		examples: []string{"sessions devices", "sessions --json devices", "sessions devices revoke 0123abcd"}, run: (*app).cmdDevices,
	},
	{
		name: "machines", usage: "machines <discover [--timeout D] | connect ENDPOINT [--name ALIAS] [--timeout D] | list | forget ALIAS>",
		summary: "discover, approve, and save Sessions machines", group: adminCommandGroup, localJSON: true,
		longHelp: "Discover Sessions hosts announced with Bonjour on the nearby network, request host approval, and save the issued per-device credential in a mode-0600 file. `sessions --machine ALIAS <command>` then runs any daemon-backed CLI command against that saved machine. Discovery reveals no credentials or session data. Nearby HTTP traffic is not encrypted, so connect only on a private network you trust; use Tailscale HTTPS on untrusted networks. Forget removes the local credential but does not revoke it on the host.",
		examples: []string{"sessions machines discover", "sessions machines connect http://192.168.1.20:8787 --name mini", "sessions machines", "sessions --machine mini ls", "sessions machines forget mini"}, run: (*app).cmdMachines,
	},
	{
		name: "access", usage: "access <requests | accept ID | deny ID>",
		summary: "review and decide machine access requests", group: adminCommandGroup, localJSON: true,
		longHelp: "List pending nearby and Tailscale access requests, or accept or deny one by id. Requests show the transport and verified Tailscale identity or observed private-LAN source address before approval. This is the CLI equivalent of the native access inbox, so an authorized agent can complete the same workflow without GUI automation.",
		examples: []string{"sessions access requests", "sessions --json access requests", "sessions access accept 0123abcd", "sessions access deny 0123abcd"}, run: (*app).cmdAccess,
	},
	{
		name: "lan", usage: "lan <enable|disable|status>",
		summary: "manage same-network access", group: adminCommandGroup, localJSON: true,
		longHelp: "Enable, disable, or inspect explicit HTTP access from other devices on the same Wi-Fi or Ethernet network. Enabling LAN access also advertises a low-sensitivity Bonjour record for native discovery. Protected routes still require a revocable device or daemon token. LAN HTTP traffic is unencrypted; use it only on a private network you trust.",
		examples: []string{"sessions lan enable", "sessions lan status", "sessions lan disable"}, run: (*app).cmdLan,
	},
	{
		name: "notify", usage: "notify <status|on|off> [done|waiting|lost]",
		summary: "configure session push notifications", group: adminCommandGroup, localJSON: true,
		longHelp: "Inspect or toggle encrypted push notifications for structured turn completion, sustained waiting, and unexpectedly lost sessions. Omitting the kind from on or off changes all three kinds; delivery begins only after subscribing in the web UI.",
		examples: []string{"sessions notify status", "sessions notify off waiting", "sessions notify on done", "sessions --json notify status"}, run: (*app).cmdNotify,
	},
	{
		name: "remote", usage: "remote <enable|disable|status>",
		summary: "manage tailnet-only remote access", group: adminCommandGroup, localJSON: true,
		longHelp: "Enable, disable, or inspect the Tailscale Serve HTTPS endpoint used for Sessions remote access. Once enabled, other Sessions apps in the same tailnet can discover this Mac and request access; the host must accept before a revocable device credential is issued.",
		examples: []string{"sessions remote enable", "sessions remote status", "sessions remote disable"}, run: (*app).cmdRemote,
	},
	{
		name: "token", usage: "token",
		summary: "print the daemon authentication token", group: adminCommandGroup,
		longHelp: "Read and print the local daemon token for use by an authorized Sessions client.",
		examples: []string{"sessions token"}, run: func(a *app, _ []string) error { return a.cmdToken() },
	},
	{
		name: "backup", usage: "backup <enable|now|status|decrypt> [options]",
		summary: "configure and run session backups", group: adminCommandGroup, localJSON: true,
		longHelp: "Enable scheduled backup storage, push a backup immediately, show backup status, or decrypt an encrypted backup. Enable requires --project and accepts --interval and --encrypt.",
		examples: []string{"sessions backup enable --project my-project --interval 15m --encrypt", "sessions backup now", "sessions backup decrypt transcript.jsonl.enc", "sessions --json backup status"}, run: (*app).cmdBackup,
	},
	{
		name: "doctor", usage: "doctor",
		summary: "diagnose daemon and session health", group: adminCommandGroup, localJSON: true,
		longHelp: "Report per-session health, spawn path, QoS state, and sessions which should be recreated.",
		examples: []string{"sessions doctor", "sessions --json doctor"}, run: func(a *app, _ []string) error { return a.cmdDoctor() },
	},
	{
		name: "support", usage: "support [--diagnostics | --bundle PATH | --attach --ticket tsk_ID --project somewhere-project]",
		summary: "leave feedback or open a support ticket", group: adminCommandGroup, localJSON: true,
		longHelp: "Print the official feedback, bug-ticket, and private security-report links. Agents use `sessions --json support --diagnostics` for a stable machine-readable support contract and local diagnostic preview, add the sanitized failing command shape/action and error, then ask the user before opening or submitting a ticket. `--bundle PATH` writes that same redacted JSON locally without overwriting an existing file. Only the explicit `--attach --ticket ... --project ...` form uploads: it uses the user's existing Somewhere CLI login to attach one private bundle to one named ticket, with one bounded attempt and no project environment access. The bundle contains only versions, platform, daemon readiness, and a session count. It excludes transcripts, terminal output, prompts, responses, titles, tags, session command content, IDs, process details, usernames, hostnames, paths, credentials, environment, logs, and crash files. Nothing is uploaded automatically.",
		examples: []string{"sessions support", "sessions support --diagnostics", "sessions --json support --diagnostics", "sessions support --bundle ./sessions-support.json", "sessions --json support --attach --ticket tsk_1234abcd --project sessions"}, run: (*app).cmdSupport,
	},
	{
		name: "docs", usage: "docs",
		summary: "print the complete offline CLI reference", group: adminCommandGroup,
		longHelp: "Print the complete Sessions CLI reference as Markdown. The output is generated directly from the same command registry as sessions help, needs no daemon or network connection, and can be saved or passed to a coding agent.",
		examples: []string{"sessions docs", "sessions docs > sessions-cli.md"}, run: (*app).cmdDocs,
	},
	{
		name: "help", usage: "help [command]",
		summary: "show top-level or command help", group: adminCommandGroup,
		longHelp: "Show complete command help, or detailed help for one command. sessions <command> --help is equivalent.",
		examples: []string{"sessions help", "sessions help run", "sessions recover --help"}, run: func(_ *app, _ []string) error { return nil },
	},
	{
		name: "version", usage: "version",
		summary: "print the CLI version", group: adminCommandGroup, aliases: []string{"--version", "-v"},
		longHelp: "Print the Sessions CLI version and exit.",
		examples: []string{"sessions version", "sessions --version"}, run: func(a *app, _ []string) error { _, err := fmt.Fprintln(a.stdout, version); return err },
	},
}

func lookupCommand(name string) (commandSpec, bool) {
	for _, command := range commandTable {
		if command.name == name {
			return command, true
		}
		for _, alias := range command.aliases {
			if alias == name {
				return command, true
			}
		}
	}
	return commandSpec{}, false
}

func helpRequested(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if argument == "--help" || argument == "-h" {
			return true
		}
	}
	return false
}

func writeTopLevelHelp(writer io.Writer) error {
	return writeTopLevelHelpFor(writer, commandTable)
}

func writeTopLevelHelpFor(writer io.Writer, commands []commandSpec) error {
	if _, err := io.WriteString(writer, "sessions — local session fleet CLI\n\nUsage:\n  sessions [global flags]\n  sessions [global flags] <command> [args]\n  sessions help <command>\n\nWith no command, Sessions lists agent sessions and headless lanes. Session ids may be full ids or unique prefixes from `sessions ls`.\n"); err != nil {
		return err
	}
	groups := []string{dailyCommandGroup, modelCommandGroup, adminCommandGroup}
	for _, group := range groups {
		if _, err := fmt.Fprintf(writer, "\n%s:\n", group); err != nil {
			return err
		}
		for _, command := range commands {
			if command.group != group {
				continue
			}
			name := command.name
			if len(command.aliases) > 0 {
				name += " (" + strings.Join(command.aliases, ", ") + ")"
			}
			if _, err := fmt.Fprintf(writer, "  %-24s %s\n", name, command.summary); err != nil {
				return err
			}
		}
	}
	_, err := io.WriteString(writer, "\nGlobal flags:\n  --json           machine-friendly output; may also appear among command options\n  --machine NAME   use a saved Sessions machine and its device credential\n  --host HOST      low-level sessionsd host; local token stays on loopback\n  --port PORT      sessionsd port (default 8787)\n\nConnection flags must precede the command. Arguments after `sessions run --` always belong to the child command.\n\nRun `sessions help <command>` for one command or `sessions docs` for the complete offline reference.\n")
	return err
}

func writeCommandHelp(writer io.Writer, name string) error {
	command, ok := lookupCommand(name)
	if !ok {
		return fail(1, "unknown help topic: %s\n\nrun 'sessions help' for commands", name)
	}
	return writeCommandSpecHelp(writer, command)
}

func writeCommandSpecHelp(writer io.Writer, command commandSpec) error {
	if _, err := fmt.Fprintf(writer, "Usage:\n  sessions %s\n\n%s\n\n%s\n", command.usage, command.summary, command.longHelp); err != nil {
		return err
	}
	if len(command.examples) > 0 {
		if _, err := io.WriteString(writer, "\nExamples:\n"); err != nil {
			return err
		}
		for _, example := range command.examples {
			if _, err := fmt.Fprintf(writer, "  %s\n", example); err != nil {
				return err
			}
		}
	}
	_, err := io.WriteString(writer, "\n--json may appear before the command or among its options. --machine, --host, and --port must appear before the command. Arguments after `sessions run --` always belong to the child command.\n")
	return err
}

func (a *app) cmdDocs(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions docs")
	}
	return writeFullDocs(a.stdout, a.commands)
}

func writeFullDocs(writer io.Writer, commands []commandSpec) error {
	if _, err := io.WriteString(writer, "<!-- GENERATED by `sessions docs` from runtime/cmd/sessions/help.go — do not edit -->\n\n# Sessions CLI reference\n\nThis complete offline reference is generated from the built `sessions` command registry.\n\n## Top-level help\n\n```text\n"); err != nil {
		return err
	}
	if err := writeTopLevelHelpFor(writer, commands); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, "```\n"); err != nil {
		return err
	}
	for _, command := range commands {
		if _, err := fmt.Fprintf(writer, "\n## `sessions %s`\n\n```text\n", command.name); err != nil {
			return err
		}
		if err := writeCommandSpecHelp(writer, command); err != nil {
			return err
		}
		if _, err := io.WriteString(writer, "```\n"); err != nil {
			return err
		}
	}
	return nil
}
