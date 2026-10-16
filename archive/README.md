# Архив веток и спасённых изменений

Создан 16 октября 2026 перед чисткой git. Удаление веток из основного
репозитория необратимо затронуло бы 11 веток, которых **нет на `origin`**:
на удалёнке лежат только 9 из 20.

## Что внутри

`branches-archive.bundle` — полная история всех веток, worktrees и HEAD
на момент архивации (71 ref, 92 объекта). Проверен: восстановление в чистый
клон возвращает все 11 веток, отсутствующих на `origin`, включая
`agent/engineer-ui` (`054c687`) — единственный коммит, которого нет в `main`.

`bvk-db-fix-uncommitted.patch` — незакоммиченный тест, найденный в заброшенном
worktree `/private/tmp/bvk-db-fix` (ветка `fix/database-migration`).
Тест `TestLegacyUnboundTicketDoesNotBlockClaim` проверяет, что заявка без
привязки к топику не блокирует взятие в работу из-за активной аренды
`team_message`. `git bundle` рабочие деревья не сохраняет, поэтому патч вынесен
отдельно. Патч проверен на применимость к текущему дереву
(`git apply --check` — чисто).

## Как восстановить

Ветки внутри bundle лежат в пространстве `refs/remotes/origin/*`, поэтому при
обычном `git clone` они попадают в remote-tracking, а не в локальные ветки.
Смотреть их так:

```bash
git clone archive/branches-archive.bundle /tmp/bvk-restore
cd /tmp/bvk-restore
git log -1 refs/remotes/origin/agent/engineer-ui
```

Сделать нужную ветку локальной:

```bash
git branch agent/engineer-ui refs/remotes/origin/agent/engineer-ui
```

Подтянуть bundle в текущий репозиторий, не клонируя:

```bash
git fetch archive/branches-archive.bundle 'refs/remotes/origin/*:refs/heads/*'
```

Применить спасённый тест поверх актуального `main`:

```bash
git apply archive/bvk-db-fix-uncommitted.patch
```

## Почему архив лежит в репозитории

Он маленький (559 КБ) и должен пережить удаление веток, очистку worktrees и
потерю локальной машины. Если он станет мешать — файлы можно удалить после
того, как содержимое `archive/README.md` перестанет быть нужным.
