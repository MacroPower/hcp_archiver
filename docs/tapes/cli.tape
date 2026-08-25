# Reading an archive from a script: list, show, and extract.
#
# Recorded by `task demo:record`, which builds the binary onto PATH, collects
# the demo archive, and points $HCP_ARCHIVER_CONFIG at docs/demo/config.yaml.

Output docs/assets/cli.gif

Require hcp_archiver

Set Shell "bash"
Set Padding 20
Set FontSize 18
Set Width 1500
Set Height 560
Set Framerate 24

Hide
  Type "PS1='$ ' && clear" Enter
Show

Sleep 1s

# Objects are addressed by org-prefixed archive paths.
Type@30ms "export WS=jacobcolvin-com/projects/edge/workspaces/dns" Enter
Sleep 1s

# One line per object, whichever physical form holds it: rolled up, sealed
# into a bundle, or still loose because the run has not finished.
Type@30ms 'hcp_archiver list $WS/runs | head -12' Enter
Sleep 5s

Type@30ms "clear" Enter
Sleep 500ms

# The exact archived bytes, straight to stdout.
Type@30ms 'hcp_archiver show $WS/workspace.json | head -14' Enter
Sleep 5s

Type@30ms "clear" Enter
Sleep 500ms

# Machine-readable output for the pipeline case, naming the sealed container.
Type@30ms 'hcp_archiver list $WS/state-versions --json | head -2' Enter
Sleep 5s

Type@30ms "clear" Enter
Sleep 500ms

# Back to plain files, bundles and roll-ups expanded.
Type@30ms "hcp_archiver extract jacobcolvin-com --dry-run" Enter
Sleep 3s
Type@30ms "hcp_archiver extract jacobcolvin-com" Enter
Sleep 6s
