#!/bin/bash
# Commit and push the watcher's index (not the PDFs) so another machine can read the outbox.
cd "${1:?data dir}" || exit 1
git add -A watch >/dev/null 2>&1   # .gitignore keeps watch/pdf and watch/stop out
git diff --cached --quiet && exit 0
git -c user.name=taktekbot -c user.email=bot@taktek.io commit -qm "lebgazette watch: new items" 
git -c credential.helper= -c credential.helper='!gh auth git-credential' push -q origin main
