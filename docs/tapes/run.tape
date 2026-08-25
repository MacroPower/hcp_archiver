# Archiving an organization with `hcp_archiver run`, and the panel it reports
# through: the phases it walks, the workspaces in flight, the counts it settles,
# and the rate governor adapting to the server's pushback.
#
# Recorded by `task demo:record`, which builds the binary onto PATH and starts
# the stand-in API this archives from. The archive is removed first, so the
# recording collects a whole organization from scratch every time.

Output docs/assets/run.gif

Require hcp_archiver

Set Shell "bash"
Set Padding 20
Set FontSize 18
Set Width 1400
Set Height 460
Set Framerate 24

Hide
  Type "PS1='$ ' && clear" Enter
  Type "export HCP_ARCHIVER_CONFIG=docs/demo/run-config.yaml HCP_TOKEN=demo" Enter
  Type "rm -rf .tmp/demo/run-archive && clear" Enter
Show

Sleep 1s

# One command archives the whole organization; everything below is its panel.
Type@30ms "hcp_archiver run" Enter

# The run itself: org-scoped metadata, projects, the workspaces and their run
# and state history, the registry, and the closing sweep. Waiting on the
# summary rather than on a fixed sleep keeps the recording the length of the
# run, whatever the machine recording it manages.
Wait+Screen@40s /archive complete/

# The summary the run leaves behind once the panel erases itself.
Sleep 3s
