# Welcome to Jad

Jad is a coding agent you run on your own machine. A small server reads your sessions, profiles, models and tools from files, runs the agent, and streams the result to this page. Everything is a plain JSON file you can read, diff and keep in git.

Pick a session in the left panel to start chatting. This page is shown only at startup, and the message box stays off until you do.

## How the pieces fit

- **Session** is one working directory plus a transcript. It lives in `.jad/sessions/<id>.json` and sets `cwd`, an optional `name`, a `category` for grouping and the `scope` of profiles it may use.
- **Profile** is a way of working in that session: the models, the toolset, the allowed tools and the prompt documents. It lives in `.jad/profiles/<group>/<name>.json`. A session with several profiles shows each as a child row in the left panel, and each profile keeps its own conversation.
- **Model, toolset and MCP server** are shared files in `.jad/models`, `.jad/tools` and `.jad/mcp`. Profiles refer to them by name.
- **State** such as transcripts and checkpoints is kept under `.jad/state`.

## Working in a session

- Click a session row to open it, or a profile row to open that profile's conversation.
- Type `@` in the message box to mention a file from the working directory.
- Attach images or files with the buttons above the box, or drop them onto it.
- **Checkpoints** in the side navigation lists what a run changed and lets you restore it.
- **Notifications** collects activity from sessions you are not looking at.
- **Prompt library** holds prompts, templates and skills you can insert into the box.
- **Search sessions** opens a search over your sessions.

## Changing the setup

Open **Catalog** in the side navigation to create, edit and delete profiles, models, sessions, toolsets and MCP servers. The editor checks every file against its schema and refuses a file the server would not load.

From a terminal, `jad check` reads every catalog and session file the way the server does, starts nothing, and exits 1 when any file is invalid. It needs no API token.

## Running the server

The server listens on `127.0.0.1:8787` by default and requires the `JAD_API_TOKEN` environment variable. Build it from `backend-go/` with `go build -o /tmp/jad-go ./cmd/jad`. This page talks to it over HTTP and server-sent events.

## Coming back here

Choose **README** in the side navigation at any time to show this page again.
