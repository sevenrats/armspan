#!/usr/bin/env bash
# delete-all-remote-tags.sh — delete ALL tags on the github remote.
set -uo pipefail

GITHUB_REMOTE="${GITHUB_REMOTE:-github}"

if ! git remote get-url "$GITHUB_REMOTE" &>/dev/null; then
  echo "ERROR: remote '$GITHUB_REMOTE' not found."
  exit 1
fi

echo "Remote: $(git remote get-url "$GITHUB_REMOTE")"

mapfile -t tags < <(
  git ls-remote --tags "$GITHUB_REMOTE" |
    grep -v '\^{}' |
    awk '{print $2}' |
    sed 's|refs/tags/||'
)

if [[ ${#tags[@]} -eq 0 ]]; then
  echo "No tags found on $GITHUB_REMOTE."
  exit 0
fi

echo "Found ${#tags[@]} tags to delete:"
printf '  %s\n' "${tags[@]}"
echo ""

read -rp "Delete all ${#tags[@]} tags? [y/N] " confirm
if [[ "$confirm" != [yY] ]]; then
  echo "Aborted."
  exit 0
fi

for t in "${tags[@]}"; do
  if git push "$GITHUB_REMOTE" --delete "refs/tags/$t"; then
    echo "  ✅ $t"
  else
    echo "  ❌ $t (failed)"
  fi
done

echo "Done."
