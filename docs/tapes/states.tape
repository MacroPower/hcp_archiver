# Reading a workspace's state history, which by then lives in sealed bundles.
#
# The walk down to the workspace is hidden; the recording starts on its menu.
# Recorded by `task demo:record`, which builds the binary onto PATH, collects
# the demo archive, and points $HCP_ARCHIVER_CONFIG at docs/demo/config.yaml.

Output docs/assets/states.gif

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
  Enter Sleep 750ms
  Down@200ms 2
  Enter Sleep 750ms
  Down@200ms
  Enter Sleep 1s
Show

Sleep 1500ms

# Every archived version of the workspace's state.
Down@350ms 2
Enter Sleep 2500ms

# One version: its metadata, and the state documents themselves.
Enter Sleep 2s
Down@350ms
Enter Sleep 1500ms
Down@150ms 6
Sleep 2s
