gvm transport — where a command runs

gvm's commands fall into a few kinds, and the kind decides what a command needs
to run.

Host operations — create, apply, start, stop, status, backup, destroy and the
rest — change or inspect this box. In this build they run in-process, as root,
on the box itself. If you are not root, gvm tells you the one fix: run the same
command with sudo. There is no remote mode in this build; GVM_URL and --remote
are refused with one sentence, because the API client they would need ships
with the dashboard, later.

The console — `gvm console` and `gvm control` — talks to one running server
over a local socket. That socket is readable only by root and by the console
group, so attaching also needs root on this box.

`gvm host init` is the exception that proves the rule: it installs gvm, so it
runs before the control plane exists and has nothing to resolve. It runs
in-process and checks for root itself.

Why gvm will not take a shortcut here
  There is a root-owned control socket on the box that the dashboard's own
  account can reach. A CLI that dialled it to "just run the command" would hand
  the entire root command surface to that internet-facing account — which is
  exactly the escalation the two-service split exists to prevent. So the CLI
  never dials it. When you need root, gvm says so and points you at sudo, and
  nothing in its output names that socket as a way in.
