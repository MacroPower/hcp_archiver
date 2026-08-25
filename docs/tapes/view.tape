# Browsing to a run's plan output with `hcp_archiver view`.
#
# Recorded by `task demo:record`, which builds the binary onto PATH, collects
# the demo archive, and points $HCP_ARCHIVER_CONFIG at docs/demo/config.yaml.

Output docs/assets/view.gif

Require hcp_archiver

Set Shell "bash"
Set Padding 20
Set FontSize 20
Set Width 1200
Set Height 660
Set Framerate 24

Hide
  Type "PS1='$ ' && clear" Enter
  Type "hcp_archiver view" Enter
  Sleep 4s
Show

# The organization's top-level menu.
Sleep 1500ms

# Projects, then one project's workspaces.
Enter Sleep 1200ms
Down@350ms 2
Enter Sleep 1200ms
Down@350ms
Enter Sleep 1200ms

# The workspace's run history, newest first. The run at the top is still
# planning upstream, so the walk down is to the newest one that finished.
Down@350ms
Enter Sleep 2500ms
Down@350ms 3

# One run's artifacts, and the plan output it produced.
Enter Sleep 2s
Down@350ms
Enter Sleep 1500ms
Down@150ms 8
Sleep 2s
