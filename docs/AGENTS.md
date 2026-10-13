# Working on BVK Support

Read PLAN.md before changing behavior and consult README.md for current user flows.
The latest explicit user requirements override earlier plans and historical code.
Keep PLAN.md synchronized: separate implemented behavior, verified results, and remaining work.
Do not restore removed features (emergency routing, dispatcher chat, internal-note mode,
mandatory reply controls, or thirty-day cleanup) without a new user request.
Use profile branches from the current shared baseline. Coordinate shared domain
contracts before parallel edits. Review integration and test the complete lifecycle.
Preserve existing records and archives; new completion cleanup must never bulk-delete
topics belonging to historical closures. Report real Telegram pilot separately from mocks.

Apply $telegram-bot-ui when working on the bot interface: messages, button labels,
keyboards, menus, navigation, confirmations, and error states. Read
.agents/skills/telegram-bot-ui/SKILL.md and the relevant bundled references before
making interface changes. Preserve the requirements in PLAN.md and existing
business logic; explicit user instructions take precedence over skill guidelines.
