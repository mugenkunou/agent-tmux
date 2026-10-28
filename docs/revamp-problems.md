---

1. Save the session history to files
- Problem: agent-tmux keeps only the last 2000 lines, in memory. clear, a kill or a crash erases them.
- Change: everything that appears on screen is also written to a file on disk, permanently. Memory still holds the recent part (raised to 50k lines) so screen and scrolling stay fast. The file is the full record.
- Decide: on for every session by default, or only when asked (create --audit)?

2. Record what the agent sends
- Problem: after the fact, you can't easily see exactly which commands the agent typed, or when.
- Change: every send and key goes into a file with the time and who sent it. This includes commands sent inside ssh.
- Decide: nothing. It comes with change 1.

3. Record who had control, and when
- Change: a small event log: "you took control at 14:02, gave it back at 14:05", plus session create, kill and restart.
- Decide: nothing. It comes with change 1.

4. Don't record your own keystrokes
- Why: they include passwords and TOTP codes. Anything you type that appears on screen is already saved by change 1.
- Decide: agree, or do you really want raw keystrokes saved too?

5. Where the files go
- Option A: ~/Sessions/, in the same format as your ws tool, so ws session ls / show / grep / rm work on them for free.
- Option B: a separate agent-tmux folder.
- Decide: A or B? I recommend A.

6. A command to read history: agent-tmux view <name>
- Problem: no good way to scroll and search the whole history.
- Change: opens the history in less, with / to search and q to quit. It reads from the file, so it shows the whole session.
- Decide: nothing, unless you want a different name.

7. [DONE] One escape key that always works: Ctrl-]
- Problem: when ssh hangs, you can't get out or kill anything from the same tab.
- Change: Ctrl-] always works, whether you're watching or in control. It opens a small menu: detach, kill session, restart the shell, send Ctrl-C, view history.
- Built: detach, kill, restart, Ctrl-C, plus `agent-tmux restart <name>`. "View history" comes with change 6. Ctrl-T kept as the toggle for now.
- Decide: is Ctrl-] fine? Should Ctrl-T (take/give control) stay, or move into this menu? Today, Ctrl-T also steals the key from bash and fzf.

8. [DONE] Fix the agent getting locked out
- Problem: if you close the tab while in control, the agent stays blocked until someone runs yield.
- Change: agent-tmux notices the tab is gone and releases control automatically.
- Built: closing the tab releases at once; if the open/take process dies some other way, its claim is cleared on the next send or status.
- Decide: nothing. It's a bug fix.

9. [DONE, behind a flag] Mouse scrolling like a normal terminal
- Problem: the mouse wheel doesn't scroll in a session. tmux can't fix this while you're only watching.
- Change: replace how open connects, so the session shows in your terminal like ssh output. Wheel, selection and Ctrl+F then work natively. This is the biggest change.
- Downside: if two people watch at different window sizes, one sees wrapped lines.
- Built: `agent-tmux open --native <name>`. Plain `open` is unchanged; making --native the default is still open.
- Also built: tmux keeps 50000 lines per new session instead of 2000 (`create --history` to change). The file recording from change 1 is not built.

---
