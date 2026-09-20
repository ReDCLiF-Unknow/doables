# Doables

[![Release](https://img.shields.io/github/v/release/ReDCLiF-Unknow/doables)](https://github.com/ReDCLiF-Unknow/doables/releases)
[![Downloads](https://img.shields.io/github/downloads/ReDCLiF-Unknow/doables/total)](https://github.com/ReDCLiF-Unknow/doables/releases)
[![Licence](https://img.shields.io/badge/licence-MIT-blue)](LICENSE)

A small to-do list app you can share: Go, SQLite, `html/template`, and the [Tabler](https://tabler.io) UI + Tabler Icons (loaded from a CDN).
Includes a CLI (Cobra + Resty) that talks to the server's JSON API.

## Install

Download the archive for your system from the
[latest release](https://github.com/ReDCLiF-Unknow/doables/releases/latest), unpack it, and run
`doables-server`. Then open <http://localhost:8080>. There is nothing else to install: the database
is a single SQLite file created on first run.

## Run from source

```
go run ./cmd/server            # http://localhost:8080, database in ./doables.db
go run ./cmd/server -addr localhost:9000 -db /path/to/doables.db
```

SQLite is provided by the pure-Go `modernc.org/sqlite`, so no C compiler is needed.
Databases from earlier versions are upgraded automatically on start-up.

## Working with tasks

- **Edit anything.** Hover a task and click the pencil to change its title, description or due date in place
  (Esc cancels). Click the pencil next to a list's name to rename it.
- **Due dates.** Optional on every task. Tasks show a badge ("Today", "Tomorrow", red "Overdue · Sep 17"), and
  open tasks sort soonest-due first.
- **Today view.** The **Today** item in the sidebar gathers every open task with a due date across all your
  lists: **Overdue**, **Today** and **Next 7 days**. Its badge counts what is overdue or due today (red when
  something is overdue). "Today" is the server's local date.
- **Undo delete.** Deleting a task shows a "Task deleted · Undo" toast. Deleted tasks are kept for a day and
  then purged.
- **Live and in place.** Ticking, adding, editing and deleting don't reload the page, and changes made by other
  people (or the CLI) appear on your screen within a moment, via server-sent events (`/events`). If you're in
  the middle of editing something, that part waits until you're done so nothing you're typing is overwritten.

## On your phone

The layout adapts to the screen: on phones and tablets the sidebar becomes a hamburger menu and a **bottom tab bar**
(Overview, Today with its badge, and your profile) appears; quick-add is a single line with a details button for the
description and due date; touch targets are at least 44px; inputs are 16px so iPhones don't zoom in when you tap them.

It is also **installable**: in Chrome/Edge choose *Install*, on iOS Safari *Share → Add to Home Screen*. You get an icon and a
window without browser chrome. There is no service worker, so it needs a connection to your server (it is not usable
offline). Installing from a non-`localhost` address needs HTTPS.

## Sharing lists with other people

There are no passwords. The first time someone opens the app they pick a **display name**; the server
gives their browser a secret token (in an `HttpOnly` cookie; only a hash is stored server-side).

- Every list you create is **private** to you. Click **Share** on a list to get its **invite link**.
- Anyone who opens the link, picks a name and clicks **Join list** becomes a member: they can add, edit, tick
  off and delete tasks, and rename the list. Tasks show who added them and who finished them.
- The **owner** can remove members, create a new invite link (which stops the old one working) and delete
  the list. Members can leave.
- Lists created before sharing existed (or by the CLI without a token) are **public**: anyone who can reach
  the server sees them. Open one and click **Claim this list** to make it private.
- **Profile** (bottom of the sidebar) lets you rename yourself and shows your token. Paste it on another
  device ("Already use Doables on another device?") to sign in as yourself there. Clearing your cookies
  without saving the token means losing that identity.

Anyone holding an invite link can join, so treat it like a password and reset it if it leaks. If you expose
the server beyond your own network, put it behind HTTPS so tokens and cookies are protected. If you put it
behind a reverse proxy, don't let the proxy buffer `/events` (the server sends `X-Accel-Buffering: no` for nginx).

## CLI

```
go build -o doables ./cmd/doables

doables register "Alex"        # create an identity; prints a token
export DOABLES_TOKEN=<token>   # PowerShell: $env:DOABLES_TOKEN = "<token>"

doables new-list "Groceries"
doables add 1 "Buy milk" -d "2 litres" --due 2026-10-01
doables tasks 1
doables edit 3 --title "Buy oat milk" --due 2026-10-05   # only the flags you give are changed
doables edit 3 --due ""                                  # clear the due date
doables done 1        # or: doables undone 1
doables lists
doables rename-list 1 "Weekly shop"
doables rm 1          # delete task
doables rm-list 1     # delete list and its tasks (owner only)

doables invite 1                       # print the invite link for a list
doables join http://host:8080/join/CODE   # join a list (link or bare code)
doables members 1
doables whoami
```

Use `-s http://host:port` / `DOABLES_SERVER` to pick the server (default `http://localhost:8080`) and
`-t TOKEN` / `DOABLES_TOKEN` for your identity. Without a token you act anonymously and only see public lists.
If you already have a token from the web UI's Profile window, use that one instead of registering.

## JSON API

Send `Authorization: Bearer <token>` to act as a person. Lists you can't see return `404`.
Dates are `YYYY-MM-DD`.

| Method | Path | Body |
|---|---|---|
| POST | `/api/users` | `{"name": "..."}` → `{id, name, token}` |
| GET | `/api/me` | |
| POST | `/api/join/{code}` | |
| GET | `/api/lists` | |
| POST | `/api/lists` | `{"name": "..."}` |
| PATCH | `/api/lists/{id}` | `{"name": "..."}` (any member) |
| DELETE | `/api/lists/{id}` | owner only |
| GET | `/api/lists/{id}/members` | |
| GET | `/api/lists/{id}/tasks` | |
| POST | `/api/lists/{id}/tasks` | `{"title": "...", "description": "...", "due_date": "..."}` |
| PATCH | `/api/tasks/{id}` | any of `{"done": true, "title": "...", "description": "...", "due_date": "..."}`; `"due_date": ""` clears it |
| DELETE | `/api/tasks/{id}` | moves the task to the trash |

## Tests

```
go test ./...
```

## Who is downloading it

Two different numbers, measuring two different things:

**Release downloads** — how many times a built archive was fetched. GitHub counts these permanently
and publicly; the badge at the top is that total. To see the breakdown per file:

```
gh api repos/ReDCLiF-Unknow/doables/releases --jq '.[].assets[] | "\(.download_count)\t\(.name)"'
```

**Clones and views** — people fetching the source or looking at the repo page. GitHub shows these
under *Insights → Traffic*, but only for the last 14 days and only to the repo owner. To check right now:

```
gh api repos/ReDCLiF-Unknow/doables/traffic/clones --jq '"\(.count) clones, \(.uniques) unique"'
gh api repos/ReDCLiF-Unknow/doables/traffic/views  --jq '"\(.count) views, \(.uniques) unique"'
```

The weekly [traffic workflow](.github/workflows/traffic.yml) appends them to
[stats/traffic.csv](stats/traffic.csv) so that history is not lost. It needs a token of its own:
the traffic API requires push access, and the token Actions provides cannot be granted it. Once, to
enable it:

1. Create a token at <https://github.com/settings/tokens> — classic with `repo` scope, or
   fine-grained limited to this repository with *Administration: read-only*.
2. Add it to the repo: `gh secret set TRAFFIC_TOKEN --repo ReDCLiF-Unknow/doables`
   (it will prompt for the value, so the token stays out of your shell history).
3. Check it works: `gh workflow run traffic.yml --repo ReDCLiF-Unknow/doables`

Without that secret the workflow skips quietly instead of failing every week.

Two caveats, so the numbers aren't read as more than they are. Bots clone public repositories
constantly, especially in the hours after one first appears, so early clone counts are mostly
automated rather than people. And `unique` is counted per day, so the same person returning on two
days is counted twice; summed totals are a floor on interest, not a headcount.

Nothing is tracked inside the app itself. A server you run never reports anything to anyone.

## Licence

[MIT](LICENSE) — use it, change it, share it, sell it; just keep the copyright notice.

It stands on other people's open source work: [Tabler](https://tabler.io) and
[Resty](https://github.com/go-resty/resty) (MIT), [Cobra](https://github.com/spf13/cobra) (Apache 2.0),
and [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (BSD-3-Clause).
