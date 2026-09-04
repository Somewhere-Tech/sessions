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
		name: "new", usage: "new [--tool claude|codex|shell] [--permissions inherit|constrained|full] [--lifecycle task|session] [--profile NAME] [--cwd P] [--name L] [--model M] [--effort LEVEL] [--fast] [--structured] [--wait-ready] [--on-idle CMD] [--owner ID [--detach]] [--force] [--worktree [--base REF] | --no-worktree] [--cmd PATH] [options] [args...]",
		summary: "create an interactive session", group: dailyCommandGroup, localJSON: true,
		longHelp: "Create a session. --tool selects Claude, Codex, or shell. For Claude and Codex, positional text after the options is sent immediately as the first request. Agent-created children run with autonomous full access by default so delegated work finishes in the background; when the user has chosen inheritance in onboarding or Settings, children inherit their manager's resolved permission policy instead. A child cannot escalate itself past what the machine allows. --permissions makes the policy explicit, while --full-access remains an alias for --permissions full. Every new conversation, including an agent-created child, stays session-lifecycle by default. Use --lifecycle task only when the caller deliberately wants a bounded worker; provider completion alone never authorizes Sessions to end a runtime. Approval questions become needs-input state and are never blindly accepted. Existing sessions keep their original runtime and permission mode. Top-level Claude sessions default to the native interactive runtime; a Claude child created from inside another Sessions session defaults to the provider-native structured runtime so its manager can drive exact events without screen parsing. Use --pty-claude when that child specifically needs the interactive terminal. Codex defaults to its sandboxed terminal mode unless full access selects app-server; --codex-appserver explicitly selects the Rich app-server runtime for constrained or full sessions. Remote Control remains separately consent-gated. --profile selects a private provider login. --description and repeated --tag values record purpose and dimensions. --worktree creates a Sessions-owned worktree, and --base picks its starting ref. A lane created from inside another session gets its own worktree by default when its folder is a usable Git checkout, so autonomous work lands on a branch the manager can diff or merge; --no-worktree makes it share the folder instead, and a folder that cannot host a worktree is shared automatically.\n\nAgent controls: --model chooses the provider model for the new session and --effort its reasoning effort; both are validated by the provider and are only valid for Claude or Codex. --fast requests the Codex priority service tier and is refused for Claude, which has no service tier. Explicit provider arguments you pass yourself always win over these controls.\n\nRuntime and lifecycle options: --structured creates a Rich structured Claude session instead of the interactive terminal, --pty-claude keeps the terminal explicitly, and --codex-appserver or --pty-codex select the Codex runtime. In a constrained Rich session, Sessions presents provider approval requests through `sessions approve`. --wait-ready holds the create call until the new agent runtime has produced its first structured event or a short settle timeout expires, so an immediately following send is not lost. --on-idle registers a shell command the daemon runs in the session's working directory every time the session becomes idle. --force overrides the live or moved conversation guard. --no-skip-perms is an accepted no-op kept for scripts written before constrained execution became the default, and it cannot be combined with full access. --cmd runs an explicit executable instead of a tool preset.\n\nLong-running child processes: a server started inside Claude or Codex belongs to that provider terminal and may end when the provider exits. If the server must remain inspectable, start it as its own Sessions command, for example `sessions new --name preview --cwd ~/work --cmd npm run dev`. That gives the server a first-class session; explicit End then terminates its complete runner-owned process tree on every supported desktop platform. A process that deliberately detaches itself into a different process group is outside Sessions' lifecycle.\n\nOwnership: --owner records an external principal as the creator instead of the inherited Sessions ancestry, and --detach is required with it when this process already belongs to a session, creating an external root rather than a child.",
		examples: []string{"sessions new --tool claude --cwd ~/work", "sessions new --tool codex --permissions inherit --name focused-worker", "sessions new --tool codex --permissions full --lifecycle task 'Review this repository'", "sessions new --tool claude --keep-alive --name manager", "sessions new --name preview --cwd ~/work --cmd npm run dev", "sessions new --cmd /bin/zsh"},
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
		name: "defaults", usage: "defaults [--permissions settings|ask|accept-edits|auto|plan|dont-ask|full]",
		summary: "inspect or change new-session defaults", group: dailyCommandGroup, localJSON: true,
		longHelp: "Inspect the Claude launch defaults stored by Sessions on the selected machine. --permissions changes the mode for future Claude sessions while preserving the other Claude settings. Full access maps to Claude's exact skip-permissions launch mode. Existing sessions keep their current provider mode; for a blocked live Terminal session, use `sessions keys SESSION shift-tab` to cycle Claude's own mode without replacing the session.",
		examples: []string{"sessions defaults", "sessions defaults --permissions full", "sessions --machine mini defaults --permissions full", "sessions --json defaults"}, run: (*app).cmdDefaults,
	},
	{
		name: "providers", usage: "providers [update claude|codex]",
		summary: "inspect or update agent CLIs", group: dailyCommandGroup, localJSON: true,
		longHelp: "Show the locally installed Claude Code and Codex versions using only local provider metadata. `providers update` explicitly invokes that provider's own updater; it may access the internet and change the provider executable. Existing sessions keep their running process, while new sessions use the updated binary.",
		examples: []string{"sessions providers", "sessions providers update codex", "sessions --json providers"}, run: (*app).cmdProviders,
	},
	{
		name: "run", usage: "run [--name N] [--description PURPOSE] [--tag KEY=VALUE ...] [--cwd D] [--worktree [--base REF]] [--spec FILE] [--owner ID [--detach]] [--wait [--timeout D] [--output]] -- <cmd args...>",
		summary: "run a command in a headless lane", group: dailyCommandGroup, localJSON: true,
		longHelp: "Create a headless lane for the command following the first -- separator. --description (alias --desc) records why the lane exists. --worktree creates an isolated Sessions-owned worktree; it does not symlink node_modules. Every child argument after the separator is passed unchanged. Without --wait, print the lane id and return. --wait blocks for completion and propagates the child exit code; --output prints the captured output tail. --timeout raises the 30-second default the wait would otherwise be capped at, which is shorter than most delegated work; the lane keeps running past a timeout, so `sessions wait <lane>` can still collect it. --owner records an external principal as the creator instead of the inherited Sessions ancestry, and --detach is required with it when this process already belongs to a session, creating an external root rather than a child.\n\nUnder --wait the completion is reported in the shared wait envelope: ok, kind:\"lane\", reason, session, and a nested lane object with exit_code, signal, duration_ms, and last_output_tail.\n\nFinding the lane again: a lane is not an interactive session and never appears in `sessions ls`, and it drops out of the default `sessions list` view once it exits. Use `sessions lanes`, `sessions ls --kind lane`, or `sessions list -a` to see it in any state, and `sessions wait <lane>` to collect a lane whose --wait timed out.",
		examples: []string{"sessions run -- make test", "sessions run --name lint --worktree --wait --output -- npm run lint", "sessions run --wait --timeout 30m -- ./slow-migration.sh", "sessions --json run --wait -- sh -c 'exit 3'"},
		run:      (*app).cmdRun,
	},
	{
		name: "tags", usage: "tags <session> [key=value ...] [--remove key ...] [--clear]",
		summary: "view or edit session tags", group: dailyCommandGroup, localJSON: true,
		longHelp: "With no edits, print a session's tags. key=value adds or replaces a tag, --remove deletes one key, and --clear removes all tags. Tags are durable daemon-owned dimensions used by usage reports and the Sessions dashboard.",
		examples: []string{"sessions tags 0123abcd", "sessions tags 0123abcd product=Sessions client=Acme", "sessions tags 0123abcd --remove client", "sessions --json tags 0123abcd"}, run: (*app).cmdTags,
	},
	{
		name: "rename", usage: "rename <session> <name> | rename <session> --auto",
		summary: "rename a session everywhere in Sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "Set the durable Sessions title used by the app, CLI, Fleet, search, and later continuations.\n\nA session you have not renamed follows the provider's own conversation title automatically. Claude titles its conversations, so a Claude session is named whatever Claude calls it and keeps following later title changes; searching Sessions for the name you can see in Claude finds the session. Codex writes no title to its rollout files, so a Codex session keeps the name it was created with.\n\nRenaming makes the title yours, and Sessions stops following the provider for that session. --auto reverses that and hands the name back: it takes no name of its own, adopts the provider's current title immediately when there is one, and otherwise keeps the present name until the next title arrives.\n\nSessions does not rewrite Claude or Codex private history files, so unsupported provider-native renames remain unchanged.",
		examples: []string{"sessions rename 0123abcd DB", "sessions rename 0123abcd --auto", "sessions --json rename 0123abcd 'Database migration'"}, run: (*app).cmdRename,
	},
	{
		name: "worktrees", usage: "worktrees [--all | clean [--dry-run]]",
		summary: "list or safely clean Sessions-created worktrees", group: dailyCommandGroup, localJSON: true,
		longHelp: "List worktrees recorded in the Sessions ledger with dirty, merge, and session state. Worktrees successfully removed by clean are omitted by default; --all includes their durable cleaned records. clean removes only worktrees whose session has exited, whose tree is clean, and whose branch is fully merged into its recorded base; every other worktree is skipped with a reason. --dry-run shows the plan without mutation. There is no force option, and killing a session never cleans its worktree automatically.",
		examples: []string{"sessions worktrees", "sessions worktrees --all", "sessions --json worktrees", "sessions worktrees clean --dry-run", "sessions worktrees clean"}, run: (*app).cmdWorktrees,
	},
	{
		name: "transcripts", usage: "transcripts [--apply | --dry-run]",
		summary: "keep a durable copy of provider conversations", group: dailyCommandGroup, localJSON: true,
		longHelp: "Copy conversations into storage Sessions owns, so they survive the provider deleting its own transcript. Claude Code prunes ~/.claude/projects on a retention timer, and once a transcript is gone nothing can recover that conversation unless Sessions already copied it. A session Sessions is actively watching is copied continuously and needs nothing here; this exists for ended sessions whose provider transcript is still on disk. The default is a dry run that reports what would be copied; --apply performs the copy. Copying is additive and idempotent, never moves or modifies the provider's files, and can be run repeatedly. A conversation Sessions already holds a copy of is reported as already kept: the provider deleting its transcript no longer loses it, and `sessions resume <id>` replays Sessions' copy, which is the only way back for it because a native provider resume would be refused. Only a conversation with no provider transcript and no Sessions copy is reported as unrecoverable, because that one really is.",
		examples: []string{"sessions transcripts", "sessions transcripts --apply", "sessions --json transcripts"}, run: (*app).cmdTranscripts,
	},
	{
		name: "gc", usage: "gc [--older-than DURATION] [--apply | --dry-run]",
		summary: "archive old closed records safely", group: dailyCommandGroup, localJSON: true,
		longHelp: "Preview or archive sessions and lanes that have been closed longer than the retention age (30d by default). The default is a dry run; --apply records an append-only archive fact. --dry-run only states that default explicitly and changes nothing; it cannot be combined with --apply. Live runners are never archived. Finished parents and children may be archived independently because lineage remains in the append-only ledger. Recovery history, transcripts, and worktrees are preserved.",
		examples: []string{"sessions gc", "sessions gc --older-than 7d", "sessions gc --older-than 7d --dry-run", "sessions gc --older-than 30d --apply", "sessions --json gc"}, run: (*app).cmdGC,
	},
	{
		name: "archive", usage: "archive <session> [session...]",
		summary: "hide selected closed sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "Archive one or more explicitly selected closed Sessions records. Archive hides them from normal lists but preserves provider history, transcripts, recovery facts, lineage, and worktrees. Live sessions are refused; finished parents and children may be archived independently.",
		examples: []string{"sessions archive 0123abcd", "sessions archive 0123abcd 89abcdef", "sessions --json archive 0123abcd"}, run: (*app).cmdArchive,
	},
	{
		name: "aside", usage: "aside <session> [session...] [--clear]",
		summary: "set live sessions aside or bring them back", group: dailyCommandGroup, localJSON: true,
		longHelp: "Set aside removes selected live sessions from the default native working set without ending them, suppressing attention, changing notifications, or hiding them from the CLI. --clear brings them back. Ended records use archive instead.",
		examples: []string{"sessions aside 0123abcd", "sessions aside 0123abcd 89abcdef", "sessions aside 0123abcd --clear", "sessions --json aside 0123abcd"}, run: (*app).cmdAside,
	},
	{
		name: "pin", usage: "pin <session>",
		summary: "keep a user-driven session at the top", group: dailyCommandGroup, localJSON: true,
		longHelp: "Mark a live session as a user-driven workbench. A pinned session sorts first in `sessions ls`, `sessions list`, and the app's session list, is marked PINNED in `sessions history`, and is excluded from delegated-work review suggestions. Sessions never ends a session because it is old or inactive. A pin never stops, starts, or otherwise touches the running process, and it never protects a session from an explicit choice: `sessions kill`, ending it in the app, and archiving it all still work.\n\nThe mark is daemon-owned and persisted in runner metadata, so it survives daemon restarts and runner re-adoption exactly as a name or a tag does. Ended records are refused because a pin organizes a live workbench; use archive to organize ended records.\n\nUnder --json the answer is {ok, code, id, name, pinned}, where pinned is the state the daemon actually stored rather than the one that was requested.",
		examples: []string{"sessions pin 0123abcd", "sessions pin bolo", "sessions --json pin 0123abcd"}, run: (*app).cmdPin,
	},
	{
		name: "unpin", usage: "unpin <session>",
		summary: "remove the workbench mark from a session", group: dailyCommandGroup, localJSON: true,
		longHelp: "Clear a pin. The session keeps running and keeps everything else about it; it returns to its ordinary place in the listings and becomes eligible again for the automatic policies a pin exempted it from. The answer shape matches pin.",
		examples: []string{"sessions unpin 0123abcd", "sessions --json unpin 0123abcd"}, run: (*app).cmdUnpin,
	},
	{
		name: "ls", usage: "ls [--mine | --all-owners] [-a | --include-exited] [--aside | --not-aside] [--kind lane]",
		summary: "list interactive sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "List agent sessions known to the daemon. --mine follows SESSIONS_OWNER_ID, then the SESSIONS_SESSION_ID descendant subtree, then the daemon OS user. The OS-user fallback is user-wide, not invocation-scoped. Set-aside sessions remain listed and marked; --aside selects only them, while --not-aside reproduces the native default working set.\n\nSessions you pinned with `sessions pin` come first, in both the table and --json, and a PIN column appears when any listed session carries the mark. Everything below the pinned ones keeps the order it already had.\n\nTwo columns report recency and they are not the same fact. LAST-USER is the provider transcript's idea of a user turn, and a provider writes its own scheduled prompts into that transcript as user turns, so a lane driven entirely by its own cron shows recent LAST-USER activity nobody caused. LAST-HUMAN is stamped by the daemon when a message actually arrives through Sessions carrying no source-session attribution — a person, rather than another session relaying. It appears when any listed session has one, following the same rule as PIN and PROFILE. A row where the two disagree is a session whose recent user activity was machinery. --json always carries both as lastUserMessageAt, lastHumanMessageAt and lastAgentMessageAt.\n\nTwo independent axes decide what comes back, and they are easy to confuse. State: ended sessions are hidden by default, and -a (long form --include-exited, alias --include-closed) includes them. Owner: --all-owners (alias --all) returns every owner's sessions and changes nothing about which states are shown. The same two spellings mean the same two things on ls, list, and lanes.\n\n--json selects a format, not a working set. It returns exactly the sessions the plain table would show, so `sessions --json ls` answers \"what is running?\" and needs -a to see ended records. This is a deliberate change: --json used to force every ended session into the answer and ignore -a entirely.\n\nls lists interactive sessions only. A lane created by `sessions run` never appears here, in any state. Reach it with `sessions lanes`, `sessions ls --kind lane`, or `sessions list -a`, which is the single view of every session and lane in every state.\n\nls lists sessions Sessions created, and only those. A conversation you started by running plain `claude` or `codex` yourself is recorded but never appears here, so this is not the place to look for one. `sessions history` browses every Claude and Codex conversation on the machine, whoever started it.",
		examples: []string{"sessions ls", "sessions ls -a", "sessions ls --mine", "sessions ls --aside", "sessions ls --not-aside", "sessions ls --kind lane", "sessions --json ls"}, run: (*app).cmdLSDispatch,
	},
	{
		name: "list", usage: "list [--mine | --owner ID | --all-owners] [-a | --include-exited]",
		summary: "list agent sessions and headless lanes", group: dailyCommandGroup, localJSON: true,
		longHelp: "List agent sessions and headless lanes together. --mine follows SESSIONS_OWNER_ID, then the SESSIONS_SESSION_ID descendant subtree, then the daemon OS user. The OS-user fallback is user-wide, not invocation-scoped. Pinned sessions come first here too, with a PIN column when any row carries the mark.\n\nState: ended sessions and exited lanes are hidden by default, and -a (long form --include-exited, alias --include-closed) includes them. Owner: --all-owners (alias --all) returns every owner's records and changes nothing about which states are shown. A runtime whose process is proven gone reads as lost and carries its recovery action: resume a provider conversation that can continue, or kill a headless lane to close its retained record.\n\n`sessions list -a` is the one command that answers \"show me every session Sessions created\": every agent session and every retained lane, live or ended, in a single table with a TYPE column. It does not reach conversations Sessions did not create — for those, and for reopening any past conversation, use `sessions history`. Use it when a lane you dispatched with `sessions run` is not where you expected to find it — ls never lists lanes, and a lane drops out of the default list view as soon as it exits.",
		examples: []string{"sessions", "sessions list -a", "sessions list --mine", "sessions list --mine -a", "sessions list --owner team:mine"}, run: (*app).cmdSessions,
	},
	{
		name: "lanes", usage: "lanes [--all-owners | --mine [--owner ID] | --subtree ID] [--direct] [--detach]",
		summary: "list headless lanes", group: dailyCommandGroup, localJSON: true,
		longHelp: "List retained headless lanes, including the ones `sessions run` created that `sessions ls` never shows. --mine follows SESSIONS_OWNER_ID, then the SESSIONS_SESSION_ID descendant subtree, then the daemon OS user. The OS-user fallback is user-wide, not invocation-scoped. --subtree selects session ancestry; --direct limits ancestry to immediate children. --all-owners (alias --all) returns every owner's lanes; like everywhere else it selects owners, not states.\n\nA lane with no completion manifest is not necessarily running. When the daemon's process probe proves its runner is gone, the lane reads as lost with the reason and exact `sessions kill <id>` command that closes the retained record. JSON carries the same answer in lane_status without changing exited: a vanished runner supplies no exit status.\n\nLanes are retained after they exit and are always listed here, so -a (--include-exited, --include-closed) is accepted for spelling parity with ls and list and changes nothing.",
		examples: []string{"sessions lanes", "sessions lanes --mine", "sessions lanes --subtree 0123abcd --direct"}, run: (*app).cmdLanes,
	},
	{
		name: "team", usage: "team [lane-id] | team --all",
		summary: "show the lanes a manager delegated and their state", group: dailyCommandGroup, localJSON: true,
		longHelp: "Show the lanes one manager is responsible for: its own parent, if any, and its delegated descendants, each with a compact state and the last line of work. Visibility follows responsibility — a lane sees only its parent and its own descendants, never other projects — and every row carries a short summary rather than a transcript, so a manager can watch its workers without pulling their conversations into context. The calling lane is SESSIONS_SESSION_ID; pass a lane id to inspect any lane's team. Rows waiting on a decision and lanes whose runners are lost are called out with the command that resolves them.\n\n--all is the view from the top: every session that has delegated lanes, with how many are working, lost, or waiting on you, so a person sees across all their managers without opening any of them.",
		examples: []string{"sessions team", "sessions team 0123abcd", "sessions --json team 0123abcd", "sessions team --all"}, run: (*app).cmdTeam,
	},
	{
		name: "fanout", usage: "fanout [--with claude,codex] [--name N] [--cwd D] [--timeout D] [--idle D] [--no-wait] [--no-worktree] -- <request...>",
		summary: "give one request to a lane per provider and join them", group: dailyCommandGroup, localJSON: true,
		longHelp: "Start one lane per installed provider with the same request and wait for all of them, so a change can be checked by an agent from each provider in one step. --with picks the providers; the default is every installed one. Run from inside a lane, the new lanes are its delegated children (autonomous, each in its own worktree unless --no-worktree); from a shell they are your own sessions. --no-wait prints the lanes and returns; otherwise the command joins them like `sessions wait --all --summary` and reports each lane's last line. Every lane keeps running afterwards and can be opened, questioned, or ended like any other.",
		examples: []string{"sessions fanout -- review the diff on this branch and list any bug you are sure about", "sessions fanout --with codex --no-wait -- run the test suite and report failures", "sessions --json fanout --timeout 20m -- summarize what changed in src/"}, run: (*app).cmdFanout,
	},
	{
		name: "approve", usage: "approve <session-id> [--deny | --for-session]",
		summary: "answer the permission a Rich lane is waiting on", group: dailyCommandGroup, localJSON: true,
		longHelp: "A lane that inherits your permissions instead of running on its own asks before it runs a command, changes files, or takes more access. The request shows as the lane's needs-you line (`sessions ls`, `sessions team`, the app) and the lane waits until it is answered. `approve` allows it once; --for-session allows the same kind of request for the rest of the lane's session; --deny refuses and lets the lane continue without it. Run from inside a manager lane, the decision is attributed to that lane in the worker's transcript.",
		examples: []string{"sessions approve 0123abcd", "sessions approve 0123abcd --for-session", "sessions approve 0123abcd --deny"}, run: (*app).cmdApprove,
	},
	{
		name: "retry", usage: "retry <session-id> [--stop]",
		summary: "retry or stop retrying a failed Rich provider turn", group: dailyCommandGroup, localJSON: true,
		longHelp: "Run a failed Rich Claude or Codex turn again immediately. While Sessions is waiting through the automatic outage backoff, this uses the pending attempt now; after retries are exhausted, it retries the retained failed turn. During that schedule `sessions ls` shows the attempt and countdown, and `sessions wait` keeps treating the session as working. --stop cancels only the automatic schedule and leaves the provider fault visible. PTY sessions cannot retain a structured failed turn and are refused with the reason.",
		examples: []string{"sessions retry 0123abcd", "sessions retry 0123abcd --stop", "sessions --json retry 0123abcd"}, run: (*app).cmdRetry,
	},
	{
		name: "projects", usage: "projects [name <folder> <name> | forget <project-id>]",
		summary: "list or name the projects sessions are grouped under", group: dailyCommandGroup, localJSON: true,
		longHelp: "A project is the work a session belongs to: a folder, a git checkout together with every worktree of it, or a Somewhere project. Sessions find their project by working directory, so every folder shows up here on day one as an implicit project named after itself; `name` claims a folder under a name of your choosing (a GitHub origin suggests owner/repo), and `forget` drops a stored project so its sessions return to their folder's implicit one. The inbox groups sessions by these projects.",
		examples: []string{"sessions projects", "sessions projects name ~/Sessions Sessions", "sessions --json projects", "sessions projects forget p_0123"}, run: (*app).cmdProjects,
	},
	{
		name: "send", usage: "send <id> [--from SESSION] [--timeout D] [--no-wait] [--file PATH] [--operation-id UUID] [--] <text...>",
		summary: "send text and Enter to a session", group: dailyCommandGroup, localJSON: true,
		longHelp: "Send a message and Enter. Every send records a durable operation before runner input and returns its operation_id. If the caller disconnects after Sessions may have delivered the message, the result is unknown with retry:false; inspect it with `sessions send-status <operation-id>` instead of creating a duplicate writer. --operation-id lets an automated caller supply a UUID so retrying the same request is idempotent, including across a daemon restart. Reusing it for different content or a different target is refused.\n\nClaude and Codex sessions return success only after Sessions observes the provider's user event; --no-wait is retained for script compatibility but never disables that delivery check. When Codex is already working, its native runtime accepts ordinary follow-up input for submission after the next tool call. Sessions records that provider-owned state and does not create a second hidden prompt queue. A Codex refusal remains recoverable and is never reported as delivered. --file reads the complete message body from a UTF-8 file before delivery begins. --from records a durable, content-free source-lane attribution, so a delegate can see which session asked and reply to it by id; agents running inside Sessions inherit their source lane automatically, and the target may be running the other provider. An unrecognized option in front of the message is refused rather than typed into the session; put -- before a message that must begin with dashes.\n\nSend confirms delivery and returns; it does not wait for the reply. Follow it with `sessions wait <id>` for one delegate or `sessions wait <id>... --all` for a fan-out, or use `sessions ask` for a single request and answer. A terminal-only tool that cannot expose provider events is explicitly reported as unconfirmed rather than silently treated as delivered.",
		examples: []string{"sessions send 0123abcd 'Run the focused tests.'", "sessions send 0123abcd --from 89abcdef 'Please review this result.'", "sessions send 0123abcd --file prompt.md", "sessions send 0123abcd -- --json is a flag, not output"}, run: (*app).cmdSend,
	},
	{
		name: "send-status", usage: "send-status <operation-id>",
		summary: "inspect a durable message-delivery receipt", group: dailyCommandGroup, localJSON: true,
		longHelp: "Read the durable receipt for a send operation. accepted means Sessions delivered the message and Enter to the runner. not-delivered with retry:true is the only result that authorizes an automatic retry. unknown or text-delivered means the message may already be visible to the provider and must not be resent automatically. Receipts survive daemon restarts and contain no message text.",
		examples: []string{"sessions send-status 11111111-2222-4333-8444-555555555555", "sessions --json send-status 11111111-2222-4333-8444-555555555555"}, run: (*app).cmdSendStatus,
	},
	{
		name: "ask", usage: "ask <id> [--timeout D] [--idle D] [--wait-timeout D] <text...>",
		summary: "send, wait, and print the reply", group: dailyCommandGroup, localJSON: true,
		longHelp: "Send a confirmed message to a Claude or Codex session, wait for the reply to finish, and print the last assistant message. This is the request-and-answer form of delegation, including to a session running the other provider; use send plus wait when the reply should not be waited for inline.\n\nask and send share one delivery path and report it identically: while the message is still being confirmed, ask answers with send's document — submitted, confidence, and on failure reason, sessionState, textStillInComposer, and composerTail — and exits with send's status, 1 when the text is still sitting in the composer and 2 when it left the composer with nothing acknowledging it. Once delivery is confirmed, the answer is {submitted, confidence, reply} with reply null when no assistant message followed. A target whose tool cannot confirm submission exits 1 in both plain and --json mode; use send plus wait for those.\n\nExit codes: 0 a reply was printed, 1 or 2 the message was not confirmed delivered, 3 the message was delivered but no reply arrived within --wait-timeout — the session may still be working, so poll with `sessions wait`.",
		examples: []string{"sessions ask 0123abcd 'Summarize the failing test.'", "sessions --json ask 0123abcd --wait-timeout 2m 'Report status.'"}, run: (*app).cmdAsk,
	},
	{
		name: "wait", usage: "wait <id> [<id>... --any | --all] [--idle D] [--timeout D] [--summary] [condition]",
		summary: "wait for session idle, lane exit, or a fan-out join", group: dailyCommandGroup, localJSON: true,
		longHelp: "Wait for a session to become idle or a lane to exit. --summary reports which target changed and its last useful assistant/output summary. A single lane wait propagates the lane exit code. Conditions include --until commit, --until-file-contains FILE STRING, and --until-idle-stable D.\n\nEvery wait answers with the same JSON object: ok, kind, reason, session, working, idleMs, and the optional elapsedMs, idleReason, detail, summary, and a nested lane or condition object carrying what only that kind of target can report — a lane's exit_code, signal, duration_ms, and last_output_tail, or a condition's commit, file, or idle_stable_ms. kind is session, lane, commit, file-contains, or idle-stable, and the target id is always in session. reason is idle, needs-input, exited, satisfied, failed, gone, or timeout, and ok is true only when the caller can stop waiting and act. A lane that exits non-zero reports failed with its status in lane.exit_code. --summary adds prose; it never changes the shape.\n\nFanning out to several delegates: --any returns the first target to finish, for a race. --all waits for every target and returns {ok, kind:\"all\", reason, waited, results:[...]} where results holds one envelope per target in the order they were named, ok is true only if every target is ok, and reason carries the worst outcome — so a delegator can join N delegates in one call instead of re-waiting them one at a time and losing the ones that died in between. Sessions and lanes may be mixed. --idle describes a settling session and governs only the session targets; it is refused when every target is a lane, whose wait ends when the process exits.\n\n--until-file-contains resolves a relative path against your own working directory, not the delegate's; an absolute path is used unchanged. The delegate's cwd is rarely what a caller expects, since `sessions new` defaults it to $HOME while `sessions run` inherits the caller's.\n\nExit codes: 0 the condition was satisfied, 1 usage, 2 the daemon could not be reached, 3 timed out without observing the condition, 4 the target is gone or failed so waiting longer cannot help. With --all the worst per-target outcome decides. A vanished target reports ok:false and exit 4; treat exit 0 alone as success only for commands that do not wait.",
		examples: []string{"sessions wait 0123abcd --timeout 2m --summary", "sessions wait lane-a lane-b --any --summary", "sessions --json wait 0123abcd 89abcdef lane-c --all --timeout 30m", "sessions wait 0123abcd --until commit --timeout 10m"}, run: (*app).cmdWaitDispatch,
	},
	{
		name: "last", usage: "last <id> [--role user|assistant] [-n N]",
		summary: "print recent conversation or lane output", group: dailyCommandGroup, localJSON: true,
		longHelp: "For sessions, print recent user and assistant messages from the event log. For completed lanes, print the captured output tail.",
		examples: []string{"sessions last 0123abcd", "sessions last 0123abcd --role assistant -n 1", "sessions --json last 0123abcd"}, run: (*app).cmdLastDispatch,
	},
	{
		name: "history", aliases: []string{"conversations"}, usage: "history [QUERY] [--since WHEN] [--until WHEN] [--tool claude|codex|shell] [--surface SURFACE] [--actor user|automation|agent] [--cwd PATH] [--name GLOB] [--session ID[,ID...]] [--touched] [--preview [N]] [--pick] [-n N] [--all] [--wait-for-peers] [--json]",
		summary: "browse and preview every past conversation", group: dailyCommandGroup, localJSON: true,
		longHelp: "Browse every Claude and Codex conversation recorded on this machine and every approved machine, newest first, without having to remember which directory you started it in. This is the view neither provider has: `claude --resume` only offers conversations belonging to the directory you are standing in and its git worktrees, and Codex's own picker filters by working directory too, so a conversation you started somewhere else is effectively lost. Sessions resolves conversation ids fleet-wide, so every row here can be reopened from anywhere.\n\nBrowsing needs no search term. `sessions history --since today --tool codex` answers \"what did I have open today\", and any single narrowing option is enough on its own. WHEN accepts today, yesterday, a span like 3d, 6h or 2w, YYYY-MM-DD, or RFC3339. Give a QUERY as the first argument to keep only conversations whose text matched it; matching uses the same ranked engine as `sessions search`, but the unit here is the conversation rather than the message, and rows stay newest-first.\n\nEvery row carries what it takes to recognise a conversation a week later — when it was last active, where it was started from, the working directory, its name derived from the opening message, and how many messages are in it — followed by the exact command that brings it back, runnable from any directory. That command is the one that actually works for that row, following the same discipline as `sessions recover`: `sessions resume` for a conversation Sessions can reopen, including one whose provider deleted its own transcript and which comes back from Sessions' copy; `sessions attach` for a conversation that is still running, which resume would refuse; and no command at all, with the reason, for one that neither the provider nor Sessions still holds.\n\nWhere it was started from is the other thing neither provider's picker will tell you. Both providers record it and neither shows it, so a row says \"Codex Desktop\", \"Codex CLI\" or \"Claude Desktop\" rather than just the provider name, and a conversation Sessions itself started says so. Select on it with --surface: codex-cli, codex-desktop, codex-exec, claude-cli, claude-desktop, claude-sdk, sessions, or the raw value a provider recorded — an unrecognised value is accepted, and an empty answer lists the surfaces this machine actually has. --actor separates work you did from work something else did: user, automation, or agent. A row is annotated only when it was not you, because a history reads as yours until it says otherwise. A provider that never recorded the answer leaves it blank rather than being guessed at, so --actor user selects only conversations that recorded a person, and a machine running a Sessions too old to report any of this is named rather than silently dropped from a filtered answer. A last-active time marked \"(file time)\" is dated by the transcript file rather than by the conversation's own last record, which is what a history copied without preserving timestamps looks like.\n\n--touched keeps only conversations a person actually spoke into, and orders them by when. This is a different question from --actor user, which reports what the provider recorded about who started a conversation; --touched is the daemon's own record of a message arriving through Sessions with no source-session attribution, which is what separates a person from one agent driving another. It also separates a person from the provider itself: a lane on a self-scheduled cadence writes its own prompts into its transcript as user turns, so it looks recently active and recently \"used\" while nobody has said a word to it. Only a conversation with a live session can answer, because the stamp lives with the session, so --touched narrows to the running fleet by construction.\n\n--preview prints the last few exchanges of each row so a candidate can be read before it is reopened. It reads the same stored conversation `sessions cat` prints, creates nothing, and marks nothing. It also narrows the page to five rows unless -n says otherwise, because previews are long.\n\n--pick numbers the rows and reopens the one you choose, so the command a row carries no longer has to be copied by hand. It is the only thing that makes this command interactive and it is never implied: without it the output is exactly what it has always been, so pipelines, scripts, and agents reading the plain listing are unaffected, and combining it with --json is refused rather than silently ignored. At the prompt, a row number reopens that conversation, `p N` prints the last ten exchanges of row N and returns to the prompt, `l` reprints the list after a preview has scrolled it away, and `q`, an empty line, or end of input leaves without doing anything. Selection runs that row's own printed command — resume for a saved conversation, attach for one that is still running — and a row that carries no command because nothing can bring it back is refused with its reason rather than reopened into a failure. The choice is confirmed with the name and the exact command before anything runs, because reopening starts a provider process and hands it the terminal.\n\nThe default view is conversations you could plausibly return to: Claude and Codex, with messages in them, still readable. --all adds empty, shell, and unrecoverable records. The number that matched and the number recorded are always printed, so a narrowed view is never mistaken for an empty history.\n\nAn approved machine that is not in the answer never disappears quietly. The count in the footer is scoped to the machines that answered, and a line beside it on stdout names the machine that is missing and how many conversations it held the last time this one reached it, so a browse showing a fraction of the fleet cannot be read as the whole of it, and a redirected browse keeps that line.\n\nA browse waits two seconds for the fleet and no longer. A machine whose history is large enough that listing it costs more than that is left out of later browses rather than waited for and dropped again, so the cost of missing it is nothing instead of the whole budget; it is re-checked every ten minutes, and a machine that is powered off fails at connect and is skipped for a few minutes. --wait-for-peers is the complete answer: it ignores those skips, waits for every approved machine as long as this one is allowed to take, and is what the shortfall line points at. Running it once is also what teaches a browse how much a slow machine holds.",
		examples: []string{"sessions history", "sessions history --since today --tool codex", "sessions history --surface codex-desktop", "sessions history --actor automation --since 1w", "sessions history --touched", "sessions history --preview -n 3", "sessions history --pick", "sessions history --since today --pick", "sessions history 'sessions hardening' --tool codex", "sessions history --cwd . --since 1w", "sessions --json history --since yesterday"},
		run:      (*app).cmdHistory,
	},
	{
		name: "grep", usage: "grep [options] <query>",
		summary: "search every approved machine", group: dailyCommandGroup, localJSON: true,
		longHelp: "Search normalized Claude and Codex history across this machine and every machine approved in Sessions.app or with `sessions machines connect`. Familiar -i and -C N flags are accepted; matching is already case-insensitive. Results carry durable machine::history-id references, duplicate copies of the same provider message are collapsed, and an offline machine produces a partial-result warning instead of hiding reachable history. Use --machine before the command to scope one machine.",
		examples: []string{"sessions grep -i -C 3 'Google Ads'", "sessions grep --tool claude --role user bolo", "sessions --json grep 'release decision'"}, run: (*app).cmdGrep,
	},
	{
		name: "search", usage: "search <query> [--session ID[,ID...]] [--role user|assistant|tool] [--tool claude|codex|shell] [--name GLOB | --lane GLOB] [--cwd PATH] [--since DATE] [--until DATE] [--context N] [--timeline] [-n N] [--exact | --regex | --ranked] [--json]",
		summary: "search inside recorded conversations", group: dailyCommandGroup, localJSON: true,
		longHelp: "Search chat history across every live and persisted session on this machine and every approved machine by default. The unit of an answer here is the message. To find a conversation rather than a moment inside one — including with no search term at all, by time, provider, or directory — use `sessions history`, which lists conversations newest-first with the command that reopens each one. Ranked recall is the default and it is conjunctive: every bare word must appear, quoted phrases stay exact, and a pasted path matches by its trailing segments. When nothing satisfies all the words the search widens on its own, and the response says which expression actually ran. Bare AND, OR and NOT are ordinary words, not operators -- prefix the whole query with fts: to hand it to the index verbatim, where FTS5 syntax including OR and near(a,b,N) applies, and results include a stable content-derived message bookmark plus optional surrounding turns. --exact uses a case-insensitive contiguous substring; --regex uses a Go regular expression. Filter to real user requests, agent replies, or typed delegation/handoff/automation/status operations with --role; scope by sessions, lane-name glob, workspace, provider, and date. --lane is an accepted alias of --name; supplying both is refused. --timeline merges matching moments chronologically. Use global --machine or --host before the command to search only one daemon.",
		examples: []string{"sessions search 'drafts rollout' --role user --since 2026-07-23", "sessions search 'hello world' --role user --context 3", `sessions search 'near(draft,egress,8) OR "stable session"' --timeline`, "sessions search '{{first_name}}' --exact --session 0123abcd --json"}, run: (*app).cmdSearch,
	},
	{
		name: "usage", usage: "usage [daily|weekly|monthly|session|tag|provider|model] [--mode auto|calculate|display] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--provider claude|codex] [--dimension KEY] [--json]",
		summary: "report local Claude and Codex token usage", group: dailyCommandGroup, localJSON: true,
		longHelp: "Incrementally index the local Claude Code and Codex JSONL stores, then report token usage and estimated cost by day, week, month, session, provider, model, or one session-tag dimension. Reasoning tokens are reported separately but remain a subset of output tokens. auto uses a recorded cost when present and otherwise calculates with pinned ccusage pricing semantics; calculate always prices tokens; display shows recorded costs only. No usage data leaves the daemon.\n\nToken counts are measured; the money column is not. EST COST is a model of what this usage would have cost on the API: prices are pinned in this build with no as-of date, server-side tool use is billed by the provider but never appears in the token stream, and 1-hour cache writes are underpriced. On a Max or ChatGPT subscription the marginal cost is zero. It is printed to the cent because it cannot support more precision than that; the raw float and the pricing provenance are in --json.",
		examples: []string{"sessions usage", "sessions usage weekly --since 2026-07-01", "sessions usage session --mode calculate", "sessions usage tag --dimension product", "sessions usage model", "sessions --json usage monthly"}, run: (*app).cmdUsage,
	},
	{
		name: "resources", usage: "resources [-n N] [--json]",
		summary: "report what live sessions cost this machine", group: dailyCommandGroup, localJSON: true,
		longHelp: "Report the memory and CPU that live sessions are holding on this machine right now: total resident memory, how many sessions and processes it covers, total CPU as a percentage of one core, and the biggest consumers by memory.\n\nEach session's figure is its whole process tree, not one process. A PTY session's runner, the provider it put on the terminal, and everything that provider spawned are all counted together, because that is what the session actually costs. A process that has been reparented away — one whose intermediate parent exited, leaving it adopted by init — leaves the tree and stops being charged to the session that created it.\n\nCPU is a rate measured between two samples seconds apart, not an average over the life of a process. The distinction is the difference between the two numbers a machine will give you for the same PID: cumulative CPU time divided by process age (what `ps %cpu` reports on Linux, and what anyone dividing `ps -o time` by uptime computes) says an agent that worked hard an hour ago is busy now, while `top` says it is idle. This command agrees with `top`. 100% is one core saturated; a session spanning several cores reads above 100.\n\nUnknown is reported as unknown. A session with no live process, and a process this daemon may not inspect, print \"-\" and are excluded from the totals with their count stated, never counted as zero — a fleet reported as costing nothing is exactly the failure this command exists to prevent. Every figure carries the age of the sample it came from, because the daemon measures on a fixed interval and a reader is entitled to know how stale the answer is.\n\nThis is a measurement of the present with no history, which is why it is not a mode of `sessions usage`: that command reports tokens and cost accumulated over a period, and there is no such thing as the resident memory of last Tuesday.",
		examples: []string{"sessions resources", "sessions resources -n 25", "sessions --json resources"}, run: (*app).cmdResources,
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
		longHelp: "Resolve every id or unique prefix before requesting any termination. Sessions durably records whether the caller was another Sessions runtime, a paired device or external owner, or a local user client. --reason adds an optional literal human explanation; Sessions never invents one, and it refuses to swallow a following flag as the explanation, so `--reason --force` is a usage error rather than a recorded reason of '--force' with the force silently dropped. Multi-target calls use one guarded daemon batch and share an operation id. More than three targets are refused unless --force is explicit. A retained runner-gone record has no process to signal; kill closes that record with a durable user-end boundary instead.\n\nResults are reported per target from what the daemon confirmed, never assumed. Each target is killed, closed-lost when a proven-gone runner's retained record was closed, already-exited for a lane that had already finished, failed when the daemon refused or did not confirm it, or unconfirmed when the daemon accepted the request without saying which sessions ended. The command exits 1 when any target failed and 2 when any target could not be confirmed, so a partially refused batch is never reported as success. --json prints {\"items\":[{\"id\",\"status\",\"reason\"}],\"operation_id\"} on stdout with the same statuses, matching the per-target shape used by archive and aside.",
		examples: []string{"sessions kill 0123abcd", `sessions kill 0123abcd 89abcdef --reason "completed rollout batch"`, "sessions --json kill 0123abcd", "sessions kill --json 0123abcd 89abcdef"}, run: (*app).cmdKill,
	},
	{
		name: "recover", usage: "recover [--all | --reopen [--force]]",
		summary: "inspect or reopen recoverable sessions", group: dailyCommandGroup, localJSON: true,
		longHelp: "List actionable recovery recipes. The RESUME column is always the command that actually recovers that conversation. For a record whose provider deleted its own transcript but whose conversation Sessions still holds a copy of, that command is `sessions resume <id>`, not the provider's resume flag, which the provider would refuse; --all labels those records transcript-recovery and --json reports transcriptRecovery on them. --all also shows blocked and unresumable lost records with reasons; blocked means neither the provider nor Sessions still has the conversation. --reopen creates replacement sessions for eligible records; --force overrides the live or moved conversation guard.",
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
		longHelp: "Print the last N terminal lines, defaulting to 50. -n and --lines are the same option and take a positive integer. -f and --follow are the same option and keep streaming new output until interrupted. An unknown option, or -n without a number, is refused rather than ignored, so a mistyped flag never silently prints the default instead of what was asked for.",
		examples: []string{"sessions tail 0123abcd", "sessions tail 0123abcd -n 200 -f", "sessions tail 0123abcd --lines 200 --follow"}, run: (*app).cmdTail,
	},
	{
		name: "cat", usage: "cat <session-id | machine::history-id>",
		summary: "print one durable conversation", group: dailyCommandGroup, localJSON: true,
		longHelp: "Print the complete normalized conversation identified by a live session id, a unique session-id prefix, or a fleet-search reference. An unqualified argument that resolves to a session on this daemon prints that session's transcript, including approval_requested and approval_resolved audit records; otherwise it is treated as a history reference. The machine qualifier selects the approved per-device credential without putting a token in argv. The conversation is read from its source machine; Sessions does not create a second transcript copy merely for search.",
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
		name: "keys", usage: "keys <id> <esc|up|down|left|right|^c|^d|enter|tab|shift-tab>",
		summary: "send a named key to a session", group: dailyCommandGroup,
		longHelp: "Translate a supported key name to terminal bytes and send it to the session. `shift-tab` is Claude's provider-native permission-mode control, so a user or agent can repair a blocked Terminal session without opening the raw terminal or replacing its runner.",
		examples: []string{"sessions keys 0123abcd esc", "sessions keys 0123abcd ^c", "sessions keys PM shift-tab"}, run: (*app).cmdKeys,
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
		name: "resume", aliases: []string{"continue", "resurrect"}, usage: "resume <[machine::]name-or-id> [--with claude|codex] [--permissions inherit|constrained|full] [--terminal [--remote-control] | --structured] [--force] [--source SESSION] [--repair LIVE-SUCCESSOR]",
		summary: "resume one saved conversation", group: dailyCommandGroup, localJSON: true,
		longHelp: "Resume a conversation by its durable Sessions title, full id, id prefix, or exact machine::history-id across the approved fleet. Sessions first recovers a missing Codex identity from the provider's session_meta, then uses the native provider resume. If the provider handle is truly gone but the authored transcript remains, Sessions creates one linked same-provider successor from that transcript instead of losing the conversation. `continue` and `resurrect` remain compatibility aliases. Claude resumes in its native interactive runtime by default; Codex resumes in its Rich app-server runtime. --permissions full reopens a Claude conversation with the exact skip-permissions launch mode; this is the supported repair when a constrained process is blocked, because Claude cannot elevate that live process through its mode cycle. --with creates a linked copy in the other provider. --source links the ended Sessions runtime, and --repair only completes missing records for an already-live successor.",
		examples: []string{"sessions resume db-final-review-sol", "sessions resume PM --permissions full", "sessions resume 'mini::provider-history:claude:00000000-0000-4000-8000-000000000001'", "sessions resume db-final-review-sol --with claude", "sessions --json resume provider:codex:00000000-0000-4000-8000-000000000001"}, run: (*app).cmdContinue,
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
		name: "relay", usage: "relay <status | set URL | disable | install [--listen :8899] [--cert FILE --key FILE] [--allow-file FILE | --directory-url URL --owner-token-file FILE]>",
		summary: "configure or install the optional relay", group: adminCommandGroup, localJSON: true,
		longHelp: "Inspect, set, or disable this daemon's outbound relay fallback, or install sessions-relay as a macOS LaunchAgent. The relay accepts outbound machine tunnels only after an Ed25519 challenge matches either the owner's Somewhere directory or a static allow-list. Put TLS directly on the relay with --cert/--key, or keep its listener behind Tailscale Serve or Caddy. Owner tokens are read from a mode-0600 file rather than command arguments.",
		examples: []string{"sessions relay status", "sessions relay set https://relay.example", "sessions relay disable", "sessions relay install --listen 127.0.0.1:8899 --allow-file ~/.config/sessions/relay-allow.json"}, run: (*app).cmdRelay,
	},
	{
		name: "uninstall", usage: "uninstall",
		summary: "stop and remove the development daemon", group: adminCommandGroup,
		longHelp: "Stop and remove the development sessionsd macOS LaunchAgent.",
		examples: []string{"sessions uninstall"}, run: (*app).cmdUninstall,
	},
	{
		name: "update", usage: "update [--check]",
		summary: "securely update Sessions.app", group: adminCommandGroup, localJSON: true,
		longHelp: "Check or install the latest macOS Sessions release. The updater accepts no URL or key overrides: it fetches only the public Somewhere release manifest, requires the pinned Minisign key, validates the exact immutable GitHub artifact path, and verifies the Developer ID and notarization before an atomic app swap. Only the Sessions UI is restarted; sessionsd and runners are never stopped.",
		examples: []string{"sessions update", "sessions update --check", "sessions --json update --check"}, run: (*app).cmdUpdate,
	},
	{
		name: "pair", usage: "pair [--ttl 10m] [--name NAME]",
		summary: "show a one-time device pairing code", group: adminCommandGroup, localJSON: true,
		longHelp: "Mint and display a QR code, sessions:// application link, and plain browser fallback containing every enabled LAN and Tailscale endpoint in connection order. The random 32-byte ticket is single use, expires after ten minutes by default, and can be shortened with --ttl. Possession is the host's consent: a claiming device immediately receives its own revocable credential without a separate access accept step. The master daemon token is never embedded in the link.",
		examples: []string{"sessions pair", "sessions pair --ttl 5m --name 'Uzair phone'", "sessions --json pair"}, run: (*app).cmdPair,
	},
	{
		name: "account", usage: "account <login [--email EMAIL] [--code CODE] | logout | status | key>",
		summary: "manage the optional Somewhere fleet account", group: adminCommandGroup, localJSON: true,
		longHelp: "Sign this machine in to the optional Somewhere fleet directory with an emailed magic-link code, inspect or revoke that login, or print the machine's Ed25519 public key. Login stores an access/refresh pair and the machine private key in atomic mode-0600 files owned by sessionsd; neither command exposes the private key. Sessions continues to work over LAN and the user's tailnet without an account.",
		examples: []string{"sessions account login", "sessions account login --email owner@example.com", "sessions account status", "sessions account key", "sessions account logout"}, run: (*app).cmdAccount,
	},
	{
		name: "devices", usage: "devices [revoke <id-or-prefix>]",
		summary: "list or revoke paired devices", group: adminCommandGroup, localJSON: true,
		longHelp: "List per-device credentials by id prefix, name, creation time, and last use. Revoke resolves an exact id or unique prefix, reports the matched device, and invalidates its token immediately.",
		examples: []string{"sessions devices", "sessions --json devices", "sessions devices revoke 0123abcd"}, run: (*app).cmdDevices,
	},
	{
		name: "machines", usage: "machines <discover [--timeout D] | connect ENDPOINT-OR-PAIRING-LINK [--lan URL] [--tailnet URL] [--tailnet-ip URL] [--name ALIAS] [--timeout D] | list | forget ALIAS | sync-native>",
		summary: "discover, approve, and save Sessions machines", group: adminCommandGroup, localJSON: true,
		longHelp: "List one merged fleet from saved LAN/Tailscale pairings, currently reachable Bonjour announcements, and the optional Somewhere account directory. Every row names all known transport candidates and the one currently in use. Same-account directory machines receive a per-device credential automatically through a signed challenge; another account still uses pairing or request/accept. Passing the sessions:// or plain /pair/ link printed by `sessions pair` uses the link as host consent and claims immediately, preserving its LAN, Tailscale HTTPS, and direct Tailscale-IP endpoint order. `sessions --machine ALIAS <command>` then runs any daemon-backed CLI command against that saved machine. Discovery reveals no credentials or session data. Nearby HTTP traffic is not encrypted, so connect only on a private network you trust. Direct tailnet-IP HTTP is authenticated and encrypted by Tailscale and remains protected by the Sessions device credential. Forget removes the local credential but does not revoke it on the host. sync-native reconciles the saved set against a native client's machine registry read as JSON on stdin.",
		examples: []string{"sessions machines discover", "sessions machines connect 'sessions://pair?host=…&t=…' --name mini", "sessions machines connect http://192.168.1.20:8787 --name mini", "sessions machines", "sessions --machine mini ls", "sessions machines forget mini"}, run: (*app).cmdMachines,
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
		longHelp: "Enable, disable, or inspect explicit HTTP access from other devices on the same Wi-Fi or Ethernet network. Enabling LAN access also advertises a low-sensitivity Bonjour record for native discovery. Protected routes still require a revocable device or daemon token. LAN HTTP traffic is unencrypted; use it only on a private network you trust.\n\nUnder --json, status always answers with {enabled, verified, url, bonjour} and adds verificationError when a configured listener did not verify — an unreachable or stale listener is reported as enabled with verified:false rather than as a bare error, matching `remote status`. Without --json that same state is a diagnostic on stderr and exit 2.",
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
		longHelp: "Enable, disable, or inspect tailnet-only Sessions access. When Tailscale is installed and signed in, the daemon enables its Tailscale Serve HTTPS endpoint and exact 100.64.0.0/10 interface listener automatically unless disabled. The host must still accept a new device before a revocable credential is issued. Enable turns automatic reachability on; disable turns it off and removes the Sessions-owned Serve root.",
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
	// This preamble is capability framing, not syntax, and it lives here on
	// purpose. help.go generates docs/CLI.md and CI diffs the result, so
	// anything written here cannot silently drift away from the code. A
	// separate document describing the same things has no such gate.
	//
	// Everything claimed below is a property the product actually has and
	// tests hold: a runner outlives the process that created it, the
	// transcript mirror outlives provider pruning, search reaches recorded
	// history rather than live sessions, and the exit codes are the ones
	// writeWaitOutcome and reportFailure emit.
	if _, err := io.WriteString(writer, "sessions — local session fleet CLI\n\nUsage:\n  sessions [global flags]\n  sessions [global flags] <command> [args]\n  sessions help <command>\n\nWith no command, Sessions lists agent sessions and headless lanes. Session ids may be full ids or unique prefixes from `sessions ls`.\n\nWhat this gives an agent that a plain terminal does not:\n  Work outlives you.        A session keeps running after the process that\n                            started it exits or is killed. Dispatch with\n                            `run`, collect from anywhere later with `wait`.\n  Conversations outlive     Providers prune their own transcripts on a timer.\n  the provider.             Sessions keeps its own copy, so `cat`, `search`,\n                            and `resume` still work after they do.\n  You can find what you     `history` browses every recorded Claude and Codex\n  already did.              conversation, on any machine and whatever\n                            directory you started it in — which neither\n                            provider's own resume picker can do — and\n                            `search` reads inside them.\n  Agents can hand off.      A Claude session can drive a Codex one and back;\n                            `send --from` records who asked.\n\nExit codes: 0 satisfied · 1 usage · 2 daemon unreachable · 3 timed out ·\n4 target gone or failed. With --json every command prints exactly one JSON\ndocument, including on failure, and its `code` matches the exit status.\n"); err != nil {
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
	_, err := io.WriteString(writer, "\nDelegating to another agent:\n  Sessions is cross-agent and cross-provider: a Claude session can create and\n  drive a Codex delegate and the reverse, which native subagents cannot do, and\n  a delegate outlives the session that created it. Create one with `sessions new\n  --tool claude|codex` or `sessions run` for a headless lane, hand it work with\n  `sessions ask` or `sessions send --from <your-session>` so the message carries\n  durable attribution back to you, then join the results with `sessions wait\n  <id>` for one, `--any` for the first of several, or `--all` for every one.\n  `sessions list --mine` recovers the delegates you created after a compaction,\n  so ids never have to be remembered. See `sessions help wait`.\n\nGlobal flags:\n  --json           machine-friendly output; may also appear among command options\n  --machine NAME   use a saved machine through the local daemon fleet relay\n  --direct         dial a --machine peer directly (debugging only)\n  --host HOST      low-level sessionsd host; local token stays on loopback\n  --port PORT      sessionsd port (default 8787)\n\nConnection flags must precede the command. Arguments after `sessions run --` always belong to the child command.\n\nRun `sessions help <command>` for one command or `sessions docs` for the complete offline reference.\n")
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
	_, err := io.WriteString(writer, "\n--json may appear before the command or among its options. --machine, --direct, --host, and --port must appear before the command. Arguments after `sessions run --` always belong to the child command.\n")
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
