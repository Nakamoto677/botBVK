# Working on BVK Support

Read `docs/STATE.md` before changing behavior. It is the single source of truth for
the system: user flows, operations, analytics, decisions, known defects and open work.
The latest explicit user requirements override that document and historical code.
Keep `docs/STATE.md` synchronized: separate implemented behavior, verified results,
and remaining work, and never claim the Telegram pilot or the Windows service works
without an actual run.

Testing requires `TEST_DATABASE_URL`; without it every database test skips silently and
a green `go test ./...` proves nothing about persistence. Add `TEST_BACKUP_TOOLS=1`
for backup and restore coverage.

Do not restore removed features (emergency routing, dispatcher chat, internal-note mode,
mandatory reply controls, or thirty-day cleanup) without a new user request.
Preserve existing records and archives; new completion cleanup must never bulk-delete
topics belonging to historical closures. Report real Telegram pilot separately from mocks.

Apply $telegram-bot-ui when working on the bot interface: messages, button labels,
keyboards, menus, navigation, confirmations, and error states. Read
.agents/skills/telegram-bot-ui/SKILL.md and the relevant bundled references before
making interface changes. Preserve the requirements in `docs/STATE.md` and existing
business logic; explicit user instructions take precedence over skill guidelines.

Traps when editing the Telegram layer: an extracted dispatcher branch is not always
behaviour-preserving, because several branches rely on early returns that skip
callback acknowledgement and keyboard cleanup; and every callback acknowledgement
must go through `Adapter.markCallbackAnswered`, otherwise `respondError` answers a
second time and replaces the text the user just received.
