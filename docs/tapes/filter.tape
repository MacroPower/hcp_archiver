# Filtering, which every list in the browser supports.
#
# Recorded by `task demo:record`, which builds the binary onto PATH, collects
# the demo archive, and points $HCP_ARCHIVER_CONFIG at docs/demo/config.yaml.

Output docs/assets/filter.gif

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

Sleep 1s

# Every workspace the organization owns, whichever project holds it.
Down@350ms
Enter Sleep 2s

# Narrow the list, then descend into what is left.
Type "/" Sleep 750ms
Type@200ms "prod"
Sleep 2s
Enter Sleep 1500ms
Down@350ms 2
Enter Sleep 2s
